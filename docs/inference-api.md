# OpenModel Gateway — Inference API

## Overview

The OpenModel Gateway provides an **OpenAI-compatible API** that routes inference requests across multiple Filecoin SP Workers. When a Worker begins mining (WindowPoSt/WinningPoSt), it is automatically removed from the routing pool, and requests are transparently redirected to available Workers.

**Base URL:** `http://<gateway-host>:3000`

**Hosted mainnet deployment (alpha):** `https://openmodel.filfox.info` —
publicly-trusted certificate, also serves the web UI and the public billing
queries on the same origin. Get a key via self-registration (settlement-api.md,
"Registration & API keys") and fund the wallet before calling.

**Authentication:** All requests require a Bearer token in the `Authorization` header.

```
Authorization: Bearer <CLIENT_TOKEN>
```

Each response includes an `X-Request-ID` header for request tracing. Additionally (when the
feature is enabled): `X-Session-Key` (with `X-Session-Id`, the session-key fingerprint —
see "Session affinity"); `X-Om-Receipt` (the signed billing receipt on non-streaming — see
settlement-api.md §2.2).

---

## Supported Endpoints

### Chat Completions (Primary)

```
POST /v1/chat/completions
```

Fully compatible with the [OpenAI Chat Completions API](https://platform.openai.com/docs/api-reference/chat).

**Example:**

```bash
curl http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen/Qwen3-4B-Instruct-2507",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "What is Filecoin?"}
    ],
    "max_tokens": 200,
    "temperature": 0.7
  }'
```

**Response:**

```json
{
  "id": "cmpl-abc123",
  "object": "chat.completion",
  "model": "Qwen/Qwen3-4B-Instruct-2507",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Filecoin is a decentralized storage network..."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 24,
    "completion_tokens": 65,
    "total_tokens": 89
  }
}
```

### Text Completions (Legacy)

```
POST /v1/completions
```

Supported for backward compatibility. Same parameters as OpenAI's legacy completions API.

### List Models

```
GET /v1/models
```

Returns the list of available models across all active Workers.

---

## Supported Parameters

| Parameter | Supported | Notes |
|-----------|:---------:|-------|
| `model` | Yes | A specific model id from `GET /v1/models` (e.g. `"Qwen/Qwen3-4B-Instruct-2507"`). The `"default"` alias is **not accepted** — it returns 400 |
| `messages` | Yes | Standard chat message array |
| `max_tokens` | Yes | |
| `temperature` | Yes | 0.0 - 2.0 |
| `top_p` | Yes | 0.0 - 1.0 |
| `stop` | Yes | String or array of stop sequences |
| `stream` | Yes | SSE streaming supported |
| `n` | Partial | Only `n=1` is reliably supported |
| `tools` / `functions` | **No** | Silently ignored |
| `response_format` | **No** | `json_object` mode is not enforced |
| `seed` | Partial | Passed through but reproducibility is not guaranteed |

---

## Streaming

SSE streaming is fully supported. Set `"stream": true` to receive token-by-token output:

```bash
curl -N http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"Hello"}],"max_tokens":100,"stream":true}'
```

Each chunk is an SSE event:

```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":10,"total_tokens":13}}

data: [DONE]
```

To display readable streaming output in terminal:

```bash
curl -sN http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"Hello"}],"max_tokens":100,"stream":true}' \
  | python3 -c "
import sys, json
for line in sys.stdin:
    line = line.strip()
    if line.startswith('data: ') and line != 'data: [DONE]':
        delta = json.loads(line[6:]).get('choices',[{}])[0].get('delta',{})
        print(delta.get('content',''), end='', flush=True)
print()
"
```

### Streaming receipts (opt-in)

Non-streaming responses carry the worker-signed billing receipt in the
`X-Om-Receipt` header (settlement-api.md §2.2). A streaming response cannot use a
trailer, so the receipt arrives as a dedicated SSE event instead — but only when
the client asks for it, by sending the header:

```
X-OM-Receipt-Req: 1
```

The gateway then forwards one extra event before `[DONE]`:

```
data: {"om_receipt": {"v":1, "request_id":"…", "model":"…", "request_sha256":"…", "response_sha256":"…", "prompt_tokens":…, "completion_tokens":…, "cached_tokens":…, "ts":…, "pubkey":"…", "sig":"…"}}
```

Without the header the event is stripped and the stream is byte-identical to
plain OpenAI SSE — existing clients parse nothing new. Verification of the
receipt (five offline checks against the on-chain batch) is described in
settlement-api.md §2.2.

### Thinking mode (opt-in)

Reasoning-capable models (Qwen3-8B, Qwen3-32B-AWQ, Qwen3.8-27B-FP8,
openai/gpt-oss-20b) can think before answering. Off by default — answers,
latency and bills stay unchanged unless a request opts in:

```json
{"model": "Qwen/Qwen3.8-27B-FP8", "enable_thinking": true, "messages": [...]}
```

(`"chat_template_kwargs": {"enable_thinking": true}` is accepted as the
OpenAI-ecosystem spelling of the same switch. Models without a thinking
template silently ignore both.)

With thinking on, the reasoning arrives separated from the answer, matching the
convention used across OpenAI-compatible reasoning APIs:

- **Non-streaming**: `choices[0].message.reasoning_content` carries the chain
  of thought; `content` stays clean.
- **Streaming**: deltas carry `reasoning_content` first, then `content`.

Billing: reasoning tokens are generated tokens — they are billed as output at
the model's normal rate, and a thinking answer can cost several times a direct
one. The signed receipt hashes the raw generated text (reasoning included), so
verifiability is unchanged.

**One case surfaces `reasoning_content` even with thinking off.** `gpt-oss-20b`
reasons on every request — its template has no off switch — so by default that
reasoning is withheld and only the answer is returned. If `max_tokens` runs out
*before* any answer text exists, withholding it too would return an entirely
empty response for tokens you are billed for. Instead the reasoning is
returned in `reasoning_content` with `content: ""` and `finish_reason:
"length"` — read that pair as "raise `max_tokens`" (256 is a sane floor for
reasoning models). Requests that do produce an answer are unaffected: the
reasoning stays hidden unless you asked for it.

---

## Model Names

The following model names all route to the same backend:

| Model Name | Example |
|-----------|---------|
| `default` | **Not accepted** — the gateway returns 400 (`a specific model id is required`); pick a name from `GET /v1/models` |
| Full HuggingFace ID | `Qwen/Qwen3-4B-Instruct-2507` |
| Local path | `/models/Qwen--Qwen3-4B-Instruct-2507` |

A missing or `"default"` model returns **400** (the request must name a model); a named model that no worker has loaded or supports returns **404**:

```json
{"error":{"message":"no worker supports model \"gpt-nonexistent\"","type":"gateway_error"}}
```

The authoritative list of available models is `GET /v1/catalog` (see
settlement-api.md).

---

## Client Integration Examples

### Python (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://<host>:3000/v1",
    api_key="<CLIENT_TOKEN>"
)

# Non-streaming
response = client.chat.completions.create(
    model="Qwen/Qwen3-4B-Instruct-2507",
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=100,
)
print(response.choices[0].message.content)

# Streaming
for chunk in client.chat.completions.create(
    model="Qwen/Qwen3-4B-Instruct-2507",
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=100,
    stream=True,
):
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

### Python (LangChain)

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    base_url="http://<host>:3000/v1",
    api_key="<CLIENT_TOKEN>",
    model="Qwen/Qwen3-4B-Instruct-2507",
    max_tokens=200,
)

response = llm.invoke("Explain Filecoin in simple terms.")
print(response.content)
```

### cURL

```bash
curl http://<host>:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"Hi"}],"max_tokens":50}'
```

### JavaScript (fetch)

```javascript
const response = await fetch("http://<host>:3000/v1/chat/completions", {
  method: "POST",
  headers: {
    "Authorization": "Bearer <CLIENT_TOKEN>",
    "Content-Type": "application/json"
  },
  body: JSON.stringify({
    model: "Qwen/Qwen3-4B-Instruct-2507",
    messages: [{ role: "user", content: "Hello" }],
    max_tokens: 100
  })
});
const data = await response.json();
console.log(data.choices[0].message.content);
```

---

## Token Usage

Token counts (`usage.prompt_tokens`, `usage.completion_tokens`) are computed by the vLLM inference engine from actual tokenized input/output. Values are accurate and suitable for billing.

`finish_reason` is accurate: `"stop"` when the model finishes naturally, `"length"` when truncated by `max_tokens`.

---

## Model-Aware Routing

When Workers are registered with `supported_models`, the gateway routes requests based on the `model` field:

1. **Priority 1**: Route to a Worker that already has the model loaded (no switch, lowest latency)
2. **Priority 2**: Route to an idle Worker that lists the model in `supported_models` (triggers model switch)
3. **Priority 3** (internal only): requests without a usable model preference — e.g. gateway-initiated stream-resume continuations — route to any available Worker. Client requests can never reach this branch: a missing or `"default"` model is rejected with 400 at the door

When all Workers that have the requested model loaded are overloaded (active requests exceed `gpu_count * model_switch_load_factor`), the gateway automatically routes to an idle Worker that supports the model, triggering a model switch to distribute load. This is configurable; set `model_switch_load_factor: 0` to disable.

**Mining-window awareness**: within the model-priority rules above, the gateway
**soft-de-prioritizes** workers **about to yield to mining** — the scheduler's `/ready`
emits `seconds_until_change` (seconds until the on-chain WindowPoSt graceful yield), and a
worker inside the 60s yield window has its weight multiplied by 0.05 (decaying with the
estimate). This is a soft preference, not a hard exclusion: when all workers are about to
yield they are still served (transparent stream resume is the backstop). The goal is to avoid handing a
long stream to a machine about to mine.

### Session affinity (prefix-cache reuse)

Send `X-Session-Id: <your-session-id>` and the gateway **prefers to stick subsequent
requests of that session to the same worker**, reusing vLLM's prefix cache (a multi-turn
conversation's shared prefix isn't recomputed — faster and cheaper). Affinity is a
**preference, not a pin**: if the sticky worker is gone / busy elsewhere / switched
models, it transparently falls back to normal weighted routing, never blocking the
request. The response header `X-Session-Key` returns an opaque fingerprint of the session
key (so isolation is observable: same key+session-id → same value; different api keys with
the same session-id → different values).

If no Worker supports the requested model:

```json
{
  "error": {
    "message": "no worker supports model \"llama-70b\"",
    "type": "gateway_error"
  }
}
```

**HTTP Status:** 404

The gateway automatically normalizes model name formats. All of these match the same model:
- `Qwen/Qwen3-4B-Instruct-2507` (HuggingFace ID)
- `/models/Qwen--Qwen3-4B-Instruct-2507` (local path)
- `Qwen--Qwen3-4B-Instruct-2507` (basename)

---

## Error Handling

### During Mining (All Workers Unavailable)

When all Workers are mining or loading models, requests are queued for up to `queue_timeout_sec` (default 60s, configurable to longer). If no Worker recovers within the timeout:

```json
{
  "error": {
    "message": "no available worker — all workers are mining or offline",
    "type": "gateway_error"
  }
}
```

**HTTP Status:** 503. The `Retry-After` header is an **honest resume estimate**: the
smallest resume-estimate (seconds) among the currently-mining workers — derived by the
scheduler from the on-chain WindowPoSt deadline and exposed on `/ready` — clamped to
[5,120]; it falls back to a fixed 30 when no estimate is available. Clients can retry at
the real recovery time instead of blindly waiting 30s.

If the queue is full (`max_queue_size` exceeded):

```json
{
  "error": {
    "message": "request rejected — queue full (100/100)",
    "type": "gateway_error"
  }
}
```

### Automatic Failover (Non-Streaming)

With 2+ Workers, if a Worker returns 503 mid-generation (e.g., WinningPoSt interrupts), the gateway automatically retries on a different Worker (up to 3 attempts). This is completely transparent to the client.

### Streaming interruption & transparent resume

If mining starts mid-stream and the gateway has `stream_resume` on (recommended), the
gateway **transparently continues the stream on another worker that advertises resume
support**: it re-feeds the already-delivered text as a prefix (`om_continuation`) plus the
remaining `max_tokens` to a new worker and keeps streaming — **the client sees no error
event** and still receives `data: [DONE]` normally. The continuation segment is billed by
the gateway's own count; the re-fed prefix is never double-billed. A single stream may
resume multiple times (bounded by a budget).

Only when a stream **cannot** be resumed (no capable worker / resume budget exhausted)
does it degrade to the legacy behavior — an error event and no `[DONE]`:

```
data: {"error": {"message": "Engine paused during generation — request aborted", "type": "server_error"}}
```

With `stream_resume` off (the default) it is always the legacy behavior. Separately, when a
client **abandons** a stream, it is billed for the tokens delivered before the disconnect
(see the billing rules in settlement-api.md).

### Model required (400)

A request without a `model`, or with the retired `"default"` alias, is rejected
before routing:

```json
{"error":{"message":"a specific model id is required (the \"default\" alias is no longer accepted); call GET /v1/models for the live list","type":"gateway_error"}}
```

### Upstream failure (502)

The serving worker failed mid-request. Non-streaming requests are retried on
another worker automatically before you ever see a 502 — a surfaced one means
retries were exhausted; retry later. For streams, see "Streaming interruption &
transparent resume" above.

### Unsupported Endpoints

```json
{
  "error": {
    "message": "endpoint /v1/embeddings is not supported",
    "type": "gateway_error"
  }
}
```

**HTTP Status:** 404. Unsupported endpoints: `/v1/embeddings`, `/v1/images/*`, `/v1/audio/*`.

---

## Management API (sp-gateway)

The gateway provides a REST API on port **9091** for Worker management.

**Authentication:** Bearer token (`AGENT_ADMIN_TOKEN`)

> **Operator-internal.** This admin API is for the gateway operator's own
> tooling; its token must never be handed out. A third-party Storage Provider
> joins through **self-registration** instead — the worker's scheduler signs a
> gateway challenge with the miner's own key and receives its credentials and
> TLS certificate automatically. That flow needs nothing from this section; see
> ONBOARDING-ALPHA.md in the [openmodel](https://github.com/6block/openmodel)
> repository.

### Register a Worker

```bash
curl -X POST http://<host>:9091/api/v1/workers/register \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "sp-my-worker",
    "endpoint": "http://<worker-ip>:8000",
    "scheduler_url": "http://<worker-ip>:9090",
    "gpu_count": 1,
    "miner_address": "t0182063",
    "supported_models": ["Qwen/Qwen3-4B-Instruct-2507", "Qwen/Qwen3-8B"],
    "auth_token": "<secret the worker stack validates>"
  }'
```

Worker ID rules: 1-64 characters, starting alphanumeric, then alphanumeric plus `-`, `_`, `.`

Notes:
- `gpu_count` is an initial value. After the first poll, it is automatically updated from the Worker's actual `engine_count` to reflect true inference capacity.
- `supported_models` is optional but strongly recommended: the gateway routes model-aware, and a worker with no claimed (or loaded) models is never matched to client requests.
- `miner_address` is the Worker's miner address (mapped to an EVM payout address via `sp_address_map` at settlement).
- `auth_token` is optional, a **per-worker auth secret**: the gateway attaches it when forwarding inference requests, and the worker stack validates that requests really came from this gateway (preventing free-riding by connecting to the Worker directly, bypassing the gateway). It **must survive restarts** — if a poll gets a 401, the gateway marks that Worker offline.
- `loaded_model` is auto-detected from the Worker's `/health` endpoint — no need to specify.

### List Workers

```bash
curl http://<host>:9091/api/v1/workers \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN"
```

### Deregister a Worker

```bash
curl -X DELETE http://<host>:9091/api/v1/workers/<worker-id> \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN"
```

### Fleet Statistics

```bash
curl http://<host>:9091/api/v1/stats \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN"
```

### Health / Readiness

```bash
# Liveness (no auth required)
curl http://<host>:9091/health

# Readiness — checks if any workers can serve requests (no auth required)
curl http://<host>:9091/ready
```

### Prometheus Metrics

```bash
curl http://<host>:9091/metrics
```

Key metrics:
- `openmodel_workers_total{state}` — Worker count by state (idle, busy, mining, loading, offline)
- `openmodel_polls_total{result}` — Poll success/failure count
- `openmodel_poll_duration_seconds` — Poll latency histogram
- `openmodel_gateway_requests_total{status,worker_id}` — Gateway request count
- `openmodel_gateway_request_duration_seconds{worker_id}` — Request latency histogram
- `openmodel_gateway_queued_requests` — Currently queued requests
- `openmodel_worker_state_transitions_total{from,to}` — State transition counter
