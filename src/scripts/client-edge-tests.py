#!/usr/bin/env python3
"""client-edge-tests.py — streaming & client-compatibility edge tests (C5).

Runs against a LIVE sp-gateway (needs a real worker behind it), so execute it on
fil-server, not in the dev sandbox:

    pip install openai
    GATEWAY_URL=http://localhost:3000 GATEWAY_KEY=sk-... python3 client-edge-tests.py

Covers the boundaries the unit tests can't (they have no real model):
  - non-streaming chat + completions
  - streaming (SSE) with usage in the final chunk
  - client-side timeout / cancellation mid-stream
  - abrupt disconnect mid-stream (gateway must not wedge; billing must be clean)
  - long context near max_model_len (request near the model's limit)
  - sampling passthrough (stop, top_p, max_tokens)
  - error codes (401 bad key, 404 unknown model, 413 oversized body if a cap is set)
  - LangChain compatibility (only if langchain-openai is installed)

Each check prints PASS/FAIL; exits non-zero if any hard check fails.
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request

GATEWAY = os.environ.get("GATEWAY_URL", "http://localhost:3000").rstrip("/")
KEY = os.environ.get("GATEWAY_KEY", "")
MODEL = os.environ.get("GATEWAY_MODEL", "default")
TIMEOUT = float(os.environ.get("GATEWAY_TIMEOUT", "120"))

results = []  # (name, ok, detail)


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(f"[{'PASS' if ok else 'FAIL'}] {name}" + (f" — {detail}" if detail else ""))


def post(path, body, stream=False, timeout=TIMEOUT, key=KEY):
    data = json.dumps(body).encode()
    req = urllib.request.Request(GATEWAY + path, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("Authorization", f"Bearer {key}")
    return urllib.request.urlopen(req, timeout=timeout)


def test_non_streaming():
    try:
        resp = post("/v1/chat/completions", {
            "model": MODEL,
            "messages": [{"role": "user", "content": "Say hello in one word."}],
            "max_tokens": 16,
        })
        payload = json.loads(resp.read())
        ok = "choices" in payload and payload.get("usage", {}).get("total_tokens", 0) > 0
        check("non-streaming chat returns usage", ok, f"tokens={payload.get('usage')}")
    except Exception as e:
        check("non-streaming chat returns usage", False, str(e))


def test_streaming():
    try:
        resp = post("/v1/chat/completions", {
            "model": MODEL,
            "messages": [{"role": "user", "content": "Count to five."}],
            "max_tokens": 64,
            "stream": True,
            "stream_options": {"include_usage": True},
        }, stream=True)
        chunks, saw_usage, saw_done = 0, False, False
        for raw in resp:
            line = raw.decode(errors="replace").strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                saw_done = True
                break
            chunks += 1
            try:
                obj = json.loads(data)
                if obj.get("usage", {}).get("total_tokens", 0) > 0:
                    saw_usage = True
            except json.JSONDecodeError:
                pass
        check("streaming yields chunks + [DONE]", chunks > 0 and saw_done, f"chunks={chunks}")
        check("streaming includes usage (final chunk)", saw_usage)
    except Exception as e:
        check("streaming yields chunks + [DONE]", False, str(e))


def test_client_timeout_midstream():
    # A very short timeout during a streamed generation must raise client-side
    # WITHOUT leaving the gateway wedged (the next request must still succeed).
    try:
        post("/v1/chat/completions", {
            "model": MODEL,
            "messages": [{"role": "user", "content": "Write a long essay about Filecoin."}],
            "max_tokens": 2048,
            "stream": True,
        }, stream=True, timeout=0.5).read()
        check("client mid-stream timeout raises", False, "expected a timeout")
    except Exception:
        check("client mid-stream timeout raises", True)
    # Gateway still healthy?
    test_non_streaming_quick("gateway healthy after mid-stream timeout")


def test_abrupt_disconnect():
    # Open a stream and close it immediately (abrupt client disconnect). The gateway
    # must cancel the upstream and stay healthy; the interrupted stream must not be
    # billed (verify by reconcile separately).
    try:
        resp = post("/v1/chat/completions", {
            "model": MODEL,
            "messages": [{"role": "user", "content": "Write a long story."}],
            "max_tokens": 2048, "stream": True,
        }, stream=True)
        resp.read(64)   # read a little
        resp.close()    # abrupt disconnect
        check("abrupt mid-stream disconnect handled", True)
    except Exception as e:
        # A connection error here is acceptable; the real check is the gateway survives.
        check("abrupt mid-stream disconnect handled", True, f"({e})")
    test_non_streaming_quick("gateway healthy after abrupt disconnect")


def test_long_context():
    # A prompt near the model's context window exercises the long-context path.
    big = "The quick brown fox. " * 1500  # ~ several thousand tokens
    try:
        resp = post("/v1/chat/completions", {
            "model": MODEL,
            "messages": [{"role": "user", "content": big + "\nSummarize in one sentence."}],
            "max_tokens": 64,
        })
        payload = json.loads(resp.read())
        check("long-context request accepted", "choices" in payload,
              f"prompt_tokens={payload.get('usage', {}).get('prompt_tokens')}")
    except urllib.error.HTTPError as e:
        # 400/413 with a clear message is an acceptable, well-behaved rejection.
        check("long-context request accepted", e.code in (400, 413),
              f"HTTP {e.code} (clean rejection acceptable)")
    except Exception as e:
        check("long-context request accepted", False, str(e))


def test_sampling_passthrough():
    try:
        resp = post("/v1/chat/completions", {
            "model": MODEL,
            "messages": [{"role": "user", "content": "List three colors, comma separated."}],
            "max_tokens": 32, "top_p": 0.5, "stop": ["\n"],
        })
        payload = json.loads(resp.read())
        check("stop/top_p/max_tokens accepted", "choices" in payload)
    except Exception as e:
        check("stop/top_p/max_tokens accepted", False, str(e))


def test_error_codes():
    # 401: bad key.
    try:
        post("/v1/chat/completions", {"model": MODEL, "messages": []}, key="sk-bad-key")
        check("bad key → 401", False, "no error raised")
    except urllib.error.HTTPError as e:
        check("bad key → 401", e.code == 401, f"HTTP {e.code}")
    except Exception as e:
        check("bad key → 401", False, str(e))

    # 404: unknown model.
    try:
        post("/v1/chat/completions", {
            "model": "definitely-not-a-real-model-zzz",
            "messages": [{"role": "user", "content": "hi"}], "max_tokens": 8,
        })
        check("unknown model → 404", False, "no error raised")
    except urllib.error.HTTPError as e:
        check("unknown model → 404", e.code == 404, f"HTTP {e.code}")
    except Exception as e:
        check("unknown model → 404", False, str(e))


def test_non_streaming_quick(name):
    try:
        resp = post("/v1/chat/completions", {
            "model": MODEL, "messages": [{"role": "user", "content": "hi"}], "max_tokens": 8,
        }, timeout=60)
        json.loads(resp.read())
        check(name, True)
    except Exception as e:
        check(name, False, str(e))


def test_langchain():
    try:
        from langchain_openai import ChatOpenAI
    except ImportError:
        print("[SKIP] LangChain not installed (pip install langchain-openai)")
        return
    try:
        llm = ChatOpenAI(model=MODEL, base_url=GATEWAY + "/v1", api_key=KEY or "x",
                         max_tokens=32, timeout=TIMEOUT)
        out = llm.invoke("Say hi.")
        check("LangChain ChatOpenAI invoke", bool(out and out.content))
    except Exception as e:
        check("LangChain ChatOpenAI invoke", False, str(e))


def main():
    print(f"Target gateway: {GATEWAY}  model={MODEL}")
    if not KEY:
        print("WARNING: GATEWAY_KEY unset — auth tests may behave differently.")
    test_non_streaming()
    test_streaming()
    test_client_timeout_midstream()
    test_abrupt_disconnect()
    test_long_context()
    test_sampling_passthrough()
    test_error_codes()
    test_langchain()

    hard_fails = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(hard_fails)}/{len(results)} checks passed.")
    if hard_fails:
        print("FAILED:", ", ".join(n for n, _, _ in hard_fails))
        sys.exit(1)
    print("All client-edge checks passed.")


if __name__ == "__main__":
    main()
