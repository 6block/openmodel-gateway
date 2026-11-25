# Getting Started — third-party developer guide

From zero to your first billed inference call on the hosted OpenModel mainnet
deployment. Time: about ten minutes, most of it waiting for a deposit to land.

What you need:

- an EVM wallet you control (MetaMask or any tool that can produce an EIP-191
  `personal_sign` signature),
- a small amount of **FIL** or **USDFC** on Filecoin mainnet to fund usage
  (fractions of a FIL are plenty to start: a typical chat call bills well under
  $0.001).

Endpoints used throughout (alpha):

| | |
|---|---|
| API + web UI (publicly-trusted certificate) | `https://openmodel.filfox.info` |
| Settlement contract (Filecoin mainnet) | `0x29b994f63D91e447945a1cDeA2776167B7415c26` |

The web UI at the same address can do steps 1-2 interactively (connect wallet,
register, deposit) and offers a chat with every model kind — text, image
understanding, image generation and speech; the steps below are the API path.

---

## Step 1 — register your wallet, get an API key

Fetch the exact text to sign:

```bash
curl "https://openmodel.filfox.info/v1/register/message?wallet=0xYOURWALLET"
```

Sign the returned `message` with your wallet (EIP-191 personal_sign) and post it
back. With ethers v6, the whole thing is:

```js
const { ethers } = require("ethers");
const GW = "https://openmodel.filfox.info";
const wallet = new ethers.Wallet(process.env.PRIVATE_KEY);
const m = await (await fetch(`${GW}/v1/register/message?wallet=${wallet.address}`)).json();
const signature = await wallet.signMessage(m.message);
const res = await fetch(`${GW}/v1/register`, {
  method: "POST", headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ wallet: m.wallet, issued_at: m.issued_at, signature, name: "my-app" }),
});
console.log(await res.json()); // { api_key: "sk-om-…", wallet, name }
```

Save `api_key` — it is shown once and stored only as a hash. One wallet
registers once; more keys (up to 10) via `/v1/keys`, and lost keys are replaced,
not recovered. Full reference: settlement-api.md, "Registration & API keys".

## Step 2 — fund the wallet

Usage is prepaid against the settlement contract. Deposit **from the same wallet
the key is bound to**:

- **From MetaMask / any EVM tool:** send FIL directly to the contract
  (`0x29b994f63D91e447945a1cDeA2776167B7415c26`) — a plain transfer credits your
  balance. For USDFC (`0x80B98d3aa09ffff255c3ba4A241111Ff1262F045`, 18
  decimals): `approve()` then `depositToken()`.
- **From an exchange or Lotus wallet:** those need the f410 form of your
  address:

```bash
curl "https://openmodel.filfox.info/v1/f4addr?wallet=0xYOURWALLET"
```

Deposit to your own f410 address first, then move funds to the contract from
there.

Check that the gateway sees the balance (refreshes within seconds):

```bash
curl https://openmodel.filfox.info/v1/me -H "Authorization: Bearer sk-om-YOURKEY"
```

`balance.available_usd > 0` means the next call will pass the payment gate.

## Step 3 — call the API

The gateway is OpenAI-compatible, so the official OpenAI SDKs **are** the client
SDK — point them at the gateway and change nothing else:

```python
from openai import OpenAI

client = OpenAI(base_url="https://openmodel.filfox.info/v1", api_key="sk-om-YOURKEY")
resp = client.chat.completions.create(
    model="Qwen/Qwen3-4B-Instruct-2507",
    messages=[{"role": "user", "content": "Hello from Filecoin!"}],
    max_tokens=64,
)
print(resp.choices[0].message.content)
```

Or curl:

```bash
curl https://openmodel.filfox.info/v1/chat/completions \
  -H "Authorization: Bearer sk-om-YOURKEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"Hello"}],"max_tokens":64}'
```

List what is servable right now (the answer changes as providers join and
switch):

```bash
curl https://openmodel.filfox.info/v1/models -H "Authorization: Bearer sk-om-YOURKEY"
```

Streaming (`"stream": true`), model routing, mining-window failover and session
affinity all behave as documented in inference-api.md — a request that lands
while every provider is mining queues briefly instead of failing.

## Step 3b — images and speech

Each entry of `/v1/models` names its `modality`, the `endpoint` that serves it
and its billing unit (`price_unit`):

| `modality` | Endpoint | Billed per (`price_unit`) |
|---|---|---|
| `text` | `/v1/chat/completions` | `token` |
| `vision` | `/v1/chat/completions` with an `image_url` part in the message | `token` (images count as prompt tokens) |
| `image` | `/v1/images/generations` | `image` delivered |
| `asr` | `/v1/audio/transcriptions` | `audio_second` of input, rounded up |
| `tts` | `/v1/audio/speech` | `character` of input (Unicode code points) |

Pick a model by kind, then call its endpoint. A round trip through speech
synthesis and recognition:

```bash
BASE=https://openmodel.filfox.info
KEY=sk-om-YOURKEY
TTS=$(curl -s "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | jq -r '.data[] | select(.modality=="tts") | .id' | head -1)
ASR=$(curl -s "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | jq -r '.data[] | select(.modality=="asr") | .id' | head -1)
curl -s "$BASE/v1/audio/speech" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d "{\"model\":\"$TTS\",\"input\":\"Hello, this is a speech test.\"}" -o hello.wav
curl -s "$BASE/v1/audio/transcriptions" -H "Authorization: Bearer $KEY" \
  -F model="$ASR" -F file=@hello.wav
```

And an image:

```bash
IMG=$(curl -s "$BASE/v1/models" -H "Authorization: Bearer $KEY" \
  | jq -r '.data[] | select(.modality=="image") | .id' | head -1)
curl -s "$BASE/v1/images/generations" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d "{\"model\":\"$IMG\",\"prompt\":\"a lighthouse on a cliff at dusk, watercolor\",\"size\":\"1024x1024\"}" \
  | jq -r '.data[0].b64_json' | base64 --decode > image.png
```

Request fields, limits (image size, audio formats, text length) and an
image-understanding example are in inference-api.md. The OpenAI SDKs work
unchanged on every endpoint, and complete multimodal programs are in
[`examples/`](../examples/README.md).

**What it costs.** Prices are per unit and live in `GET /v1/catalog`: token
models carry `input_price_usd_per_1m` / `output_price_usd_per_1m`, the others
`unit_price_usd` (USD per image, per second of audio, or per character), next
to the FIL/USD rate in `fil_price_usd`:

```bash
curl -s "$BASE/v1/catalog" -H "Authorization: Bearer $KEY" \
  | jq '.data[] | {id, modality, price_unit, unit_price_usd, input_price_usd_per_1m, output_price_usd_per_1m}'
```

A transcription bills its audio duration rounded up to whole seconds (4.05 s
→ 5); speech bills the characters of `input`; image generation bills the
images actually delivered.

## Step 4 — watch usage, verify your bill

`GET /v1/me` returns your balance, pending spend and the last 20 requests on the
key, each with its billed amount (tokens, or `quantity` + `unit`) and
`cost_usd`. Each response also carries an `X-Request-ID`; once that request has been
settled on-chain, anyone can fetch its inclusion proof:

```bash
curl "https://openmodel.filfox.info/api/v1/receipt-proof/req-YOURREQUESTID"
```

and verify it offline (worker signature → Merkle inclusion → on-chain batch)
with `verify-receipt.py` from
[openmodel-contracts](https://github.com/6block/openmodel-contracts). You do not
have to trust the gateway's numbers: the contract's
`cumulativeRequests()` / `cumulativeVolume(i)` (per unit: 0 token, 1 image,
2 audio_second, 3 character) and each batch's `detailsHash` are public, and
settlement-api.md §2.2 walks the whole chain of custody.

---

## About the SDK

OpenModel does not ship its own client library, on purpose: the API is
OpenAI-compatible, so the **official OpenAI SDKs (Python, JS) and every
framework built on them (LangChain, LlamaIndex, …) work as-is** by setting
`base_url`. The only OpenModel-specific client code you will ever write is the
ten-line registration snippet in Step 1 — one signature, once. Worked examples
for Python, LangChain, curl and fetch are in inference-api.md, "Client
Integration Examples".

## Error quick-reference

| Code | Meaning | What to do |
|---|---|---|
| 401 | missing/invalid key, or signature did not recover to the wallet | check the Bearer header / re-sign |
| 400 | missing `model`, or the retired `"default"` alias | name a model from `/v1/models` |
| 400 | model sent to the wrong endpoint, bad image `size`, audio format not accepted, unknown voice | the message says what to change; `/v1/models` lists each model's `endpoint` |
| 402 | balance below the estimated request cost, or account suspended for debt | deposit (Step 2); check `/v1/me` |
| 404 | named model not currently served | pick from `/v1/models` |
| 409 | wallet already registered / key limit / signature replay | use `/v1/keys`; fetch a fresh message |
| 413 | request body over the size cap | trim the prompt |
| 429 | per-key or per-IP rate limit | back off and retry |
| 502 | upstream worker failed mid-request | retry; non-streaming retries are automatic |
| 503 | all providers mining and the wait exceeded the queue window | retry after `Retry-After` seconds |

Full semantics: settlement-api.md §2.1 (limits, suspension) and inference-api.md
"Error Handling" (mining windows, failover, stream resume).

## Going deeper

- **inference-api.md** — every endpoint (chat, vision, images, audio), parameter, streaming, routing, failover.
- **settlement-api.md** — registration & keys, payment model, balance gate,
  receipts, settlement internals.
- **[openmodel](https://github.com/6block/openmodel)** — run a provider yourself
  (GETTING-STARTED-SP.md).
- **[openmodel-contracts](https://github.com/6block/openmodel-contracts)** — the
  settlement contract, deployment records, offline bill verification.
