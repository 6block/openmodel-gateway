# OpenModel User Guide — accounts, funding, usage, verification, refunds

The complete reference for API consumers: everything between "I have a wallet"
and "I got my unused deposit back". For the ten-minute happy path, read
**getting-started.md** first; this guide goes deeper into every step and into
what to do when something does not behave.

Hosted mainnet deployment (alpha) used in every example:

| | |
|---|---|
| API + web UI + public queries (publicly-trusted certificate) | `https://openmodel.filfox.info` |
| Settlement contract (Filecoin mainnet, verified on [Filfox](https://filfox.info/en/address/0x60D41baEaBe1ABE061AE82c44425debc35bA524A)) | `0x60D41baEaBe1ABE061AE82c44425debc35bA524A` |
| USDFC token (18 decimals) | `0x80B98d3aa09ffff255c3ba4A241111Ff1262F045` |

Everything is served on one origin behind a publicly-trusted certificate.
The web UI at the
same address covers registration, deposits and usage interactively — this
guide documents the API/contract path that works without a browser.

---

## 1. Account & keys

Your account **is** your EVM wallet. Registration proves you own it (one EIP-191
signature) and mints an API key; usage on any of your keys is billed to the
wallet's prepaid balance in the settlement contract.

### 1.1 First key

```bash
curl "https://openmodel.filfox.info/v1/register/message?wallet=0xYOURWALLET"
```

Sign the returned `message` (personal_sign — MetaMask, ethers, anything), then:

```bash
curl -X POST https://openmodel.filfox.info/v1/register \
  -H "Content-Type: application/json" \
  -d '{"wallet":"0xYOURWALLET","issued_at":<from step 1>,"signature":"0x…","name":"my-app"}'
```

`api_key` in the response is shown **once** and stored only as a hash. A wallet
registers once (409 afterwards); more keys come from `/v1/keys`.

### 1.2 Managing keys

Up to **10 keys per wallet**; every management action is a signed request, same
pattern as registration (fetch canonical text → sign → POST):

```bash
# the text to sign (action: create | list | delete)
curl "https://openmodel.filfox.info/v1/keys/message?wallet=0xYOURWALLET&action=create&name=ci-bot"
```

```bash
curl -X POST https://openmodel.filfox.info/v1/keys \
  -H "Content-Type: application/json" \
  -d '{"wallet":"0xYOURWALLET","action":"create","issued_at":…,"signature":"0x…","name":"ci-bot"}'
```

- **create** → `{api_key, key:{id,name,display,created_at}}` — `display` is a
  clipped preview (`sk-om-bd34…a3ed`) for recognising keys in lists.
- **list** → ids, names, display previews. Never plaintext keys.
- **delete** (needs `key_id`) → revocation is immediate; the next request on
  that key gets 401. A lost key cannot be recovered — delete it and create a
  new one. Deleting a key never touches the wallet balance.

### 1.3 Your account at a glance

```bash
curl https://openmodel.filfox.info/v1/me -H "Authorization: Bearer sk-om-YOURKEY"
```

| Field | Meaning |
|---|---|
| `key` | which key you authenticated with (`id`, `name`) |
| `wallet` | the wallet this key bills to |
| `balance.chain_fil` | your FIL deposit as last read from the contract |
| `balance.tokens` | per-token detail (FIL, USDFC) in human units |
| `balance.fil_price_usd` | the FIL/USD rate billing currently uses |
| `balance.pending_usd` | cost reserved for requests in flight / not yet settled |
| `balance.available_usd` | what the payment gate sees: deposits valued in USD minus pending |
| `recent_usage` | your last 20 requests on this key |

`available_usd` is computed by the **same** multi-token, buffer-adjusted rule
the 402 gate uses: if it is positive, your next request passes.

---

## 2. Funding your wallet

Usage is prepaid: deposit first, spend it down, refund what you do not use
(§6). Deposits must come **from the registered wallet itself** — the contract
credits `msg.sender`.

### 2.1 FIL

Send FIL straight to the contract address from your wallet (a plain transfer
credits your balance — the contract's receive hook records it), or call
`depositFIL()` with value. From MetaMask: a normal send to
`0x60D41baEaBe1ABE061AE82c44425debc35bA524A` is enough.

### 2.2 USDFC (stablecoin)

Two transactions, standard ERC-20 flow:

1. `approve(0x60D41baE…, amount)` on the USDFC token contract
2. `depositToken(0x80B98d3a…, amount)` on the settlement contract

USDFC has **18 decimals** (not 6): 1 USDFC = `1000000000000000000` units.
When you hold both tokens, billing draws **USDFC first, then FIL** — the
stablecoin buys price stability, the FIL sits as buffer.

### 2.3 From an exchange or a native Filecoin wallet

Those senders need the f410 form of an address. Derive yours:

```bash
curl "https://openmodel.filfox.info/v1/f4addr?wallet=0xYOURWALLET"
```

**Withdraw to your own f410 address first**, then move funds from your wallet
to the contract. Never point an exchange withdrawal directly at the contract:
the deposit would be credited to the exchange's wallet, not yours.

### 2.4 Confirming arrival

The gateway refreshes balances within seconds of on-chain confirmation:

```bash
curl https://openmodel.filfox.info/v1/me -H "Authorization: Bearer sk-om-YOURKEY"
```

`balance.available_usd > 0` → you can spend. If a deposit does not show up,
see §7 "I deposited but still get 402".

---

## 3. Using the API

OpenAI-compatible: point any OpenAI SDK or framework at the gateway and change
nothing else. Everything below runs against the hosted deployment as-is.

### 3.1 What you can call

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /v1/models` | Bearer key | Models servable *right now* (live, verified workers — changes as providers join or switch) |
| `GET /v1/catalog` | Bearer key | The models with per-model USD prices and the live FIL rate |
| `POST /v1/chat/completions` | Bearer key | Chat (primary; streaming via `"stream": true`) |
| `POST /v1/completions` | Bearer key | Legacy text completions |
| `GET /v1/me` | Bearer key | Account self-view: balance, pending spend, recent usage (§1.3) |
| `GET /v1/register/message`, `POST /v1/register` | open | Wallet-signed registration (§1.1) |
| `GET /v1/keys/message`, `POST /v1/keys` | open (signed) | Key management (§1.2) |
| `GET /v1/f4addr` | open | f410 form of your address for exchange withdrawals (§2.3) |
| `GET /api/v1/receipt-proof/<request_id>` | none | Public inclusion proof for one charge (§5) |
| `GET /api/v1/network-stats` | none | Public aggregate network statistics |

### 3.2 Chat

```bash
curl https://openmodel.filfox.info/v1/chat/completions \
  -H "Authorization: Bearer sk-om-YOURKEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "/models/Qwen--Qwen3-4B-Instruct-2507",
    "messages": [{"role": "user", "content": "What is Filecoin?"}],
    "max_tokens": 200,
    "temperature": 0.7
  }'
```

`model` must be a specific id from `GET /v1/models` (path form
`/models/Org--Name` and HF form `Org/Name` both work; the `"default"` alias
returns 400). A model no worker currently serves returns 404. Supported
parameters: `messages`, `max_tokens`, `temperature`, `top_p`, `stop`,
`stream`, `n` (only `n=1`); `tools`/`functions` and `response_format` are not
supported. Reasoning models want `max_tokens` ≥ 256 (§7).

### 3.3 Streaming

```bash
curl -sN https://openmodel.filfox.info/v1/chat/completions \
  -H "Authorization: Bearer sk-om-YOURKEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"/models/Qwen--Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"Hello"}],"max_tokens":100,"stream":true}' \
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

Standard OpenAI SSE: `data:` chunks with `delta.content`, a final chunk with
`finish_reason` and `usage`, then `data: [DONE]`.

### 3.4 Thinking mode

Reasoning models (Qwen3-8B/32B, Qwen3.8-27B, gpt-oss-20b) can show their
chain of thought — opt in per request; the reasoning arrives in
`reasoning_content`, separate from the answer, and is billed as output:

```bash
curl https://openmodel.filfox.info/v1/chat/completions \
  -H "Authorization: Bearer sk-om-YOURKEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"/models/Qwen--Qwen3.8-27B-FP8","enable_thinking":true,"messages":[{"role":"user","content":"Which is larger, 9.11 or 9.9?"}],"max_tokens":512}'
```

In streaming, deltas carry `reasoning_content` first, then `content`. Models
without a thinking template ignore the flag.

### 3.5 SDKs

```python
from openai import OpenAI
client = OpenAI(base_url="https://openmodel.filfox.info/v1", api_key="sk-om-YOURKEY")

r = client.chat.completions.create(
    model="/models/Qwen--Qwen3-4B-Instruct-2507",
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=100,
)
print(r.choices[0].message.content)

for chunk in client.chat.completions.create(
    model="/models/Qwen--Qwen3-4B-Instruct-2507",
    messages=[{"role": "user", "content": "Hello!"}],
    max_tokens=100, stream=True,
):
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

```python
from langchain_openai import ChatOpenAI
llm = ChatOpenAI(base_url="https://openmodel.filfox.info/v1",
                 api_key="sk-om-YOURKEY",
                 model="/models/Qwen--Qwen3-4B-Instruct-2507", max_tokens=200)
print(llm.invoke("Explain Filecoin in simple terms.").content)
```

```javascript
const r = await fetch("https://openmodel.filfox.info/v1/chat/completions", {
  method: "POST",
  headers: { "Authorization": "Bearer sk-om-YOURKEY",
             "Content-Type": "application/json" },
  body: JSON.stringify({
    model: "/models/Qwen--Qwen3-4B-Instruct-2507",
    messages: [{ role: "user", content: "Hi" }],
    max_tokens: 50,
  }),
});
console.log((await r.json()).choices[0].message.content);
```

### 3.6 Session affinity

```bash
curl https://openmodel.filfox.info/v1/chat/completions \
  -H "Authorization: Bearer sk-om-YOURKEY" \
  -H "X-Session-Id: my-conversation-42" \
  -H "Content-Type: application/json" \
  -d '{"model":"/models/Qwen--Qwen3-4B-Instruct-2507","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'
```

Send the same `X-Session-Id` for every turn of one conversation and it stays
on the same provider (prefix-cache reuse → faster long chats). Best effort:
if that provider leaves to mine, you fail over transparently.

### 3.7 Mining windows

Providers are Filecoin miners; when one leaves to prove storage your request
fails over automatically. Non-streaming requests retry on another provider;
interrupted streams resume mid-generation on another worker without the
client noticing. If *every* provider is mining, requests queue briefly (503
with `Retry-After` only if the wait exceeds the window).

---

## 4. Billing: what you pay and when

- **Pricing** is USD per million tokens, per model — read it from
  `GET /v1/catalog`. Prompt and completion tokens both count.
- **Reservation**: before a request runs, its worst-case cost
  (`max_tokens` × price) is reserved against your balance; after it runs the
  reservation settles to actual usage. This is why `pending_usd` briefly
  exceeds the final cost, and why a tiny balance can 402 on a large
  `max_tokens` — lower `max_tokens` if you are running the balance down.
- **Thinking is billed as output**: with `enable_thinking` on, reasoning
  tokens count as completion tokens at the model's normal output rate — a
  thinking answer can cost several times a direct one.
- **Failures are not billed**: a request that errors, is rejected, or dies
  mid-stream before completing costs nothing (delivered tokens on a resumed
  stream are billed once, not twice).
- **Settlement**: every ~20 minutes the gateway batches finished requests into
  one on-chain settlement per (wallet, provider, token). Your deposit
  decreases on-chain only at these batch points; between them the spend shows
  in `pending_usd`.
- The FIL/USD rate is snapshotted per batch; `fil_price_usd` in `/v1/me` and
  `/v1/catalog` shows the live value.

---

## 5. Verifying what you were charged

You do not have to trust the operator's numbers.

1. Every response carries `X-Request-ID`. Non-streaming responses also carry
   `X-Om-Receipt` — a receipt for exactly your request, **signed by the worker
   that served it** (ed25519). Streaming: send `X-OM-Receipt-Req: 1` and the
   receipt arrives as a final `om_receipt` SSE event.
2. Once its batch settles (≤ ~20 min), fetch the public inclusion proof —
   no auth, anyone can:

```bash
curl "https://openmodel.filfox.info/api/v1/receipt-proof/req-YOURREQUESTID"
```

3. Verify offline — five independent checks (worker signature, leaf hash,
   Merkle inclusion, details-hash binding, on-chain batch record):

```bash
python3 verify-receipt.py https://openmodel.filfox.info req-YOURREQUESTID \
  https://rpc.ankr.com/filecoin 0x60D41baEaBe1ABE061AE82c44425debc35bA524A
```

(`verify-receipt.py` ships in the
[openmodel-contracts](https://github.com/6block/openmodel-contracts) repo;
requires `pip install cryptography`.)
`RESULT: VERIFIED` means the charge the chain saw is byte-bound to what the
worker attested about your request. Receipts contain hashes of your
request/response, never the content itself.

You can also read the network's totals straight from the contract:
`cumulativeRequests()` / `cumulativeTokens()` on the Filfox "Read Contract"
tab, and aggregate stats at `GET /api/v1/network-stats` (no auth).

---

## 6. Refunds: getting unused deposit back

Deposits are yours; withdrawal is a two-step, time-locked flow (the delay is
the operator's protection against spend-then-drain races — on the hosted
deployment it is **3600 s / 1 hour**). All three functions are callable from
the [Filfox contract page](https://filfox.info/en/address/0x60D41baEaBe1ABE061AE82c44425debc35bA524A)
("Write Contract", connect the registered wallet) or from any EVM tooling.

1. **Request**: `requestRefund(token, amount)` — `token` is
   `0x0000000000000000000000000000000000000000` for FIL (or the USDFC address),
   `amount` in 18-decimal units. This **marks** the amount for refund (you can
   only mark free balance: deposit minus amounts already marked) and returns a
   `requestId` (also in the `RefundRequested` event, with `claimableAt`).
   Your deposit itself is NOT reduced yet — during the waiting window your
   usage keeps billing against the full balance as normal, so requesting a
   refund is not a way to use the service for free.
2. **Wait** for `claimableAt` (1 hour on this deployment).
3. **Claim**: `claimRefund(requestId)` — deducts the amount from your deposit
   and transfers it to your wallet. Or **cancel**: `cancelRefund(requestId)`
   simply releases the mark (e.g. you decided to keep using the service).

Notes:

- Claiming requires the amount to still be on your balance. If you kept using
  the service during the window and spent below the marked amount, the claim
  reverts with `balance already settled` — the funds went to usage; call
  `cancelRefund` to clear the now-meaningless mark.
- Refund state survives everything — requests are on-chain, so a gateway
  restart or outage never loses them.
- If the operator pauses the contract in an emergency, refunds keep working:
  pausing blocks new spending, not user exits.

Worked example with ethers v6 (FIL, 0.05):

```js
const { ethers } = require("ethers");
const c = new ethers.Contract("0x60D41baEaBe1ABE061AE82c44425debc35bA524A", [
  "function requestRefund(address,uint256) returns (uint256)",
  "function claimRefund(uint256)",
  "event RefundRequested(uint256 indexed requestId, address indexed user, address indexed token, uint256 amount, uint256 claimableAt)",
], wallet);
const tx = await c.requestRefund(ethers.ZeroAddress, ethers.parseEther("0.05"));
const rc = await tx.wait();
const id = rc.logs.map(l => { try { return c.interface.parseLog(l); } catch { return null; } })
  .find(e => e && e.name === "RefundRequested").args.requestId;
// …one hour later…
await (await c.claimRefund(id)).wait();
```

---

## 7. Troubleshooting

**400 "a specific model id is required"** — the request had no `model` or used
the retired `"default"` alias; pick a name from `GET /v1/models`.

**401 on every call** — key deleted, mistyped, or the `Bearer ` prefix is
missing. Keys are hashes server-side: nobody can look yours up, replace it
(§1.2).

**402 with money in the wallet** — three usual causes, in order of likelihood:
(1) the reservation, not the balance, is short: lower `max_tokens`;
(2) the deposit came from a different address than the registered wallet
(exchange withdrawal straight to the contract — see §2.3; the credit went to
the exchange's wallet);
(3) a pending refund request marked the amount (§6): marked funds still back
your usage during the window, but a large mark can make the *free* balance too
small for new marks — spending is unaffected; `cancelRefund` clears it.
`/v1/me` shows the gate's own numbers; `available_usd` is the truth the gate
uses.

**402 "account suspended"** — the wallet accumulated debt (edge cases like
mid-flight balance exhaustion) past the suspension threshold. Deposit to cover
the debt, then wait one settlement cycle (~20 min): the suspension lifts when
the next settlement collects the debt, not on the next balance refresh.

**Deposit not showing after several minutes** — confirm the transaction
actually landed on Filecoin mainnet (chainId 314) and targeted the settlement
contract; then re-check `/v1/me`. The gateway cross-checks multiple RPC
endpoints precisely so that one flaky endpoint cannot show you a fake zero.

**404 unknown model** — the model is not currently served. Pick from
`GET /v1/models`; the set changes as providers switch loads.

**409 on register** — the wallet already has an account; use `/v1/keys` for
additional keys.

**413** — request body over the size cap (10 MiB default). Trim the prompt.

**429** — per-key rate/concurrency limit, or per-IP registration limit.
Back off and retry; limits are per key, so parallel workloads can use
multiple keys (§1.2).

**502 upstream error** — the serving worker failed mid-request. Non-streaming:
already retried automatically before you see it, so a surfaced 502 means
retries were exhausted — retry later. Streaming: resumed transparently when
possible; a surfaced error means no provider could continue the stream.

**503 all providers busy/mining** — the queue wait exceeded its window.
`Retry-After` says when to come back; usually under a minute.

**Streaming stops mid-answer with no error** — check whether a `finish_reason`
of `length` arrived: you hit your own `max_tokens`, not a failure.

**`content` is empty and `reasoning_content` has text you did not ask for** —
`finish_reason` is `length`. Reasoning models (`gpt-oss-20b` in particular)
think before they answer, and a small `max_tokens` can be spent entirely
inside that thinking phase, before any answer text exists. Rather than hand
back nothing for tokens you are billed for, the thinking is returned in
`reasoning_content`. The fix is to raise `max_tokens` — **256 minimum** for
reasoning models, more for anything non-trivial. Measured on `gpt-oss-20b`: a
one-line arithmetic question needs ~76 completion tokens end to end, so
`max_tokens: 64` never reaches the answer while `max_tokens: 256` answers
normally. Tokens spent thinking are billed whether or not an answer fits,
exactly as they are when thinking mode is on.

---

## 8. Privacy

- Registration stores your wallet address and key hash — no email, no name.
- Request logs keep token counts, hashes and routing metadata for billing;
  receipts commit to `sha256(request)` / `sha256(response)`, never plaintext.
- Public endpoints (`receipt-proof`, `sp-earnings-detail`, `network-stats`)
  expose aggregates and per-request billing facts, never your key, wallet
  balance, or content.

## Where to go deeper

- **getting-started.md** — the ten-minute first call.
- **inference-api.md** — every endpoint and parameter.
- **settlement-api.md** — billing internals, receipts, settlement design.
- **[openmodel-contracts](https://github.com/6block/openmodel-contracts)** —
  contract source, deployment records, `verify-receipt.py`.
