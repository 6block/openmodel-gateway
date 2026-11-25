#!/usr/bin/env python3
"""Verifiable billing: check what you were charged for, byte for byte.

Makes one image generation, one transcription and one speech synthesis, then for each:

  1. reads the worker-signed receipt from the X-Om-Receipt response header and checks its
     ed25519 signature;
  2. checks that the receipt's asset_hash is the sha256 of the bytes you actually received
     (the PNG, the uploaded audio, the returned audio) and that its quantity is what you
     were billed;
  3. with --wait, waits for on-chain settlement and runs the five-step proof (signature,
     Merkle leaf, inclusion proof, committed root, batch on chain) with verify-receipt.py
     from the openmodel-contracts repository.

Usage:
    pip install openai cryptography
    python3 verify_billing.py                       # steps 1-2, immediate
    python3 verify_billing.py --wait --verifier ../openmodel-contracts/verify-receipt.py \\
        --proof-base https://openmodel.filfox.info --rpc https://rpc.ankr.com/filecoin \\
        --contract 0x29b994f63D91e447945a1cDeA2776167B7415c26
"""

import argparse
import base64
import hashlib
import io
import subprocess
import sys
import time
import wave

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

from openmodel_common import Bill, ceil_seconds, client, pick_model, receipt_of


def canonical(r: dict) -> bytes:
    """The exact bytes a worker signs (docs/verification.md in openmodel-contracts)."""
    if int(r.get("quantity") or 0) > 0:
        keys = ["asset_hash", "model", "quantity", "request_id", "request_sha256", "response_sha256", "ts", "v"]
    else:
        keys = ["cached_tokens", "completion_tokens", "model", "prompt_tokens", "pubkey", "request_id",
                "request_sha256", "response_sha256", "ts", "v"]
    import json
    return ("{" + ",".join(f"{json.dumps(k)}:{json.dumps(r[k], ensure_ascii=False)}" for k in keys) + "}").encode()


def check(name, receipt, delivered: bytes, quantity: int) -> bool:
    ok = True
    try:
        Ed25519PublicKey.from_public_bytes(bytes.fromhex(receipt["pubkey"])).verify(
            bytes.fromhex(receipt["sig"]), canonical(receipt))
        sig = "OK"
    except Exception as e:
        sig, ok = f"FAILED ({type(e).__name__})", False
    digest = hashlib.sha256(delivered).hexdigest()
    h = "OK" if receipt.get("asset_hash") == digest else "MISMATCH"
    q = "OK" if int(receipt.get("quantity", 0)) == quantity else f"MISMATCH ({receipt.get('quantity')} vs {quantity})"
    ok = ok and h == "OK" and q.startswith("OK")
    print(f"  {name:<14} signature {sig:<8} asset_hash {h:<8} quantity {receipt.get('quantity')} {q:<4}"
          f"  {receipt['request_id']}")
    return ok


def wav_seconds(b: bytes) -> float:
    with wave.open(io.BytesIO(b)) as w:
        return w.getnframes() / w.getframerate()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--wait", action="store_true", help="wait for settlement and run the on-chain proof")
    ap.add_argument("--verifier", default="verify-receipt.py", help="path to verify-receipt.py")
    ap.add_argument("--proof-base", default="https://openmodel.filfox.info",
                    help="public receipt-proof base URL of the gateway")
    ap.add_argument("--rpc", default="https://rpc.ankr.com/filecoin")
    ap.add_argument("--contract", default="0x29b994f63D91e447945a1cDeA2776167B7415c26")
    ap.add_argument("--verifier-args", default="", help="extra arguments for verify-receipt.py (e.g. --insecure)")
    args = ap.parse_args()

    c = client()
    image, asr, tts = pick_model(c, "image"), pick_model(c, "asr"), pick_model(c, "tts")
    bill = Bill(c)
    results = []

    text = "Every request on this network is signed by the machine that served it."
    raw = c.audio.speech.with_raw_response.create(model=tts, voice="alloy", input=text, response_format="wav")
    speech = raw.content
    results.append(("speech", receipt_of(raw.headers), speech, len(text)))
    bill.add("speech", tts, request_id=raw.headers.get("x-request-id", ""), quantity=len(text))

    raw = c.audio.transcriptions.with_raw_response.create(model=asr, file=("speech.wav", speech))
    secs = ceil_seconds(wav_seconds(speech))
    results.append(("transcription", receipt_of(raw.headers), speech, secs))
    bill.add("transcription", asr, request_id=raw.headers.get("x-request-id", ""), quantity=secs)

    raw = c.images.with_raw_response.generate(model=image, prompt="a brass padlock on an oak table, studio light",
                                              size="512x512", response_format="b64_json")
    png = base64.b64decode(raw.parse().data[0].b64_json)
    open("padlock.png", "wb").write(png)
    results.append(("image", receipt_of(raw.headers), png, 1))
    bill.add("image", image, request_id=raw.headers.get("x-request-id", ""), quantity=1)

    print("Receipts (checked locally):")
    ok = all(check(n, r, b, q) for n, r, b, q in results if r)
    if any(r is None for _, r, _, _ in results):
        print("  some responses carried no receipt"); ok = False
    bill.print()
    if not args.wait:
        sys.exit(0 if ok else 1)

    print("\nOn-chain proof (waits for the next settlement pass):")
    pending = {r["request_id"] for _, r, _, _ in results if r}
    deadline = time.time() + 3 * 3600
    while pending and time.time() < deadline:
        for rid in sorted(pending):
            cmd = [sys.executable, args.verifier, *args.verifier_args.split(), args.proof_base, rid, args.rpc,
                   args.contract]
            p = subprocess.run(cmd, capture_output=True, text=True)
            if p.returncode == 3 and "PENDING" in p.stdout:
                continue
            print(p.stdout.rstrip())
            ok = ok and p.returncode == 0
            pending.discard(rid)
        if pending:
            print(f"  {len(pending)} request(s) not settled yet; checking again in 60 s")
            time.sleep(60)
    sys.exit(0 if ok and not pending else 1)


if __name__ == "__main__":
    main()
