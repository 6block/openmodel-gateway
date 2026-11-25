"""Shared helpers for the OpenModel multimodal examples.

Every example talks to OpenModel through the official OpenAI Python SDK; the only
OpenModel-specific parts are here:

  * client()          an OpenAI client pointed at OPENMODEL_BASE_URL with OPENMODEL_API_KEY
  * pick_model()      the first model of a kind (text / vision / image / asr / tts) that
                      GET /v1/models lists, or the one named in OPENMODEL_<KIND>_MODEL
  * receipt_of()      the worker-signed receipt carried in the X-Om-Receipt response header
  * Bill              a running cost table priced from GET /v1/catalog
"""

from __future__ import annotations

import base64
import json
import math
import os
import sys
import urllib.request

from openai import OpenAI

BASE_URL = os.environ.get("OPENMODEL_BASE_URL", "https://openmodel.filfox.info/v1").rstrip("/")


def client() -> OpenAI:
    key = os.environ.get("OPENMODEL_API_KEY")
    if not key:
        sys.exit("Set OPENMODEL_API_KEY to your API key (see the gateway README, \"Register\").")
    # Retries cover the short 503s a worker returns while its GPU is busy with a
    # Filecoin proof; the SDK honours the Retry-After header the gateway sends.
    return OpenAI(base_url=BASE_URL, api_key=key, max_retries=4, timeout=600)


def pick_model(c: OpenAI, kind: str) -> str:
    """A model id of the given kind (modality), or the OPENMODEL_<KIND>_MODEL override."""
    override = os.environ.get(f"OPENMODEL_{kind.upper()}_MODEL")
    if override:
        return override
    for m in c.models.list().data:
        if (m.model_extra or {}).get("modality") == kind:
            return m.id
    sys.exit(f"No {kind} model is being served right now (GET /v1/models lists none).")


def receipt_of(headers) -> dict | None:
    """The decoded worker receipt of a response, or None when the response has none."""
    raw = headers.get("x-om-receipt")
    if not raw:
        return None
    return json.loads(base64.b64decode(raw))


def _catalog_key(model_id: str) -> str:
    # /v1/models lists "Org/Name"; /v1/catalog keys the same model as "/models/Org--Name".
    s = model_id[len("/models/"):] if model_id.startswith("/models/") else model_id
    return s.replace("--", "/").lower()


class Bill:
    """Collects one row per request and prices it from /v1/catalog."""

    def __init__(self, c: OpenAI):
        req = urllib.request.Request(BASE_URL + "/catalog",
                                     headers={"Authorization": f"Bearer {c.api_key}"})
        with urllib.request.urlopen(req, timeout=60) as r:
            cat = json.load(r)
        self.prices = {_catalog_key(m["id"]): m for m in cat.get("data", [])}
        self.rows: list[dict] = []

    def add(self, step: str, model: str, *, request_id: str = "", tokens: tuple[int, int] | None = None,
            quantity: float | None = None) -> dict:
        p = self.prices.get(_catalog_key(model), {})
        unit = p.get("price_unit", "token")
        if tokens is not None:
            pin = float(p.get("input_price_usd_per_1m") or 0)
            pout = float(p.get("output_price_usd_per_1m") or 0)
            usd = (tokens[0] * pin + tokens[1] * pout) / 1e6
            qty = f"{tokens[0]}+{tokens[1]} tok"
        else:
            usd = float(quantity or 0) * float(p.get("unit_price_usd") or 0)
            qty = f"{quantity:g} {unit}"
        row = {"step": step, "model": model, "quantity": qty, "usd": usd, "request_id": request_id}
        self.rows.append(row)
        return row

    def print(self) -> None:
        w = max(len(r["step"]) for r in self.rows)
        print()
        for r in self.rows:
            print(f"  {r['step']:<{w}}  {r['quantity']:>18}  ${r['usd']:.6f}  {r['request_id']}")
        print(f"  {'total':<{w}}  {'':>18}  ${sum(r['usd'] for r in self.rows):.6f}")
        print("  (prices from GET /v1/catalog; the settled amount is what the receipts commit)")


def ceil_seconds(seconds: float) -> int:
    """Transcription billing: input duration rounded up to whole seconds, minimum 1."""
    return max(1, math.ceil(seconds - 1e-9))
