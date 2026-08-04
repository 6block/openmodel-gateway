# OpenModel Gateway — Billing & Settlement API

The OpenModel Gateway includes a **prepaid, on-chain settlement layer**. Users deposit FIL or stablecoins into an FVM smart contract; the gateway meters usage per request and a background engine batches consumption into periodic on-chain settlements that credit SPs and the platform.

For the inference API (`/v1/chat/completions`, streaming, model routing) see **inference-api.md**. The settlement layer adds new client-facing behaviors on that same endpoint — per-key rate/concurrency limits (**429**), a request-body cap (**413**), a debt-suspension path (**402 "account suspended"**), and a graceful-drain rejection (**503** on shutdown) — documented in §2.1 below. Beyond those, this document covers: **self-service registration (getting an API key)**, the **balance gate**, the **settlement Admin API**, and the **`settlement-cli`** tool.

---

## Registration: getting an API key (wallet binding)

> ⚠️ **v1 — to be extended in M4**: hashed key storage, key rotation/recovery, multiple keys per wallet, and rate-limiting `/v1/register` against abuse. Put it behind HTTPS in production.

A user self-registers an API key bound to their own EVM wallet — usage on that key is then billed and settled to that wallet. Wallet ownership is proven by an **Ethereum signature**, so nobody can register with someone else's address (which would bill that person).

**`POST /v1/register`** — no existing key required; this is the self-service entry
point. It is served on every listener the gateway exposes (the plain API port and,
when configured, the TLS port that also serves the web UI), so use whichever
address the operator published.

Request body:
```json
{
  "wallet": "0x<your EVM address>",
  "issued_at": 1782800000,
  "signature": "0x<EIP-191 signature over the message below>",
  "name": "optional label"
}
```

The signed message is **fixed byte-for-byte** (EIP-191 personal_sign); the server reconstructs it in the same format and verifies the signature recovers to `wallet`:
```
OpenModel API key registration
wallet: <EIP-55 checksummed address>
issued_at: <unix seconds>
```

Success **200**:
```json
{"api_key": "sk-om-…", "wallet": "0x…", "name": "user-…"}
```

Then call `/v1/chat/completions` with `Authorization: Bearer sk-om-…`; usage is billed to that `wallet`, which **must be funded first** or the balance gate returns 402 (see §2). Registration immediately adds the wallet to on-chain balance refresh (surviving restarts), so **a funded wallet can spend within seconds of registering**.

**Security & constraints:**

| Case | Response |
|---|---|
| Recovered signer ≠ wallet | 401 |
| `issued_at` outside the ±5-minute window | 400 |
| Same signature replayed within the window | 409 |
| Wallet already registered (incl. wallets already in config) | 409 |
| Malformed wallet / non-POST | 400 / 405 |

Four things stop replay: the signed message spells out its purpose and wallet, so a signature made for anything else won't fit; a signature is only valid for ±5 minutes; within that window the same signature is accepted only once; and each wallet gets exactly one key. **Registration completes in a single request — no server-issued nonce round-trip**; and under production HTTPS an attacker never sees the signature in the first place.

**Signing example (ethers v6):**
```js
const issued = Math.floor(Date.now() / 1000);
const msg = `OpenModel API key registration\nwallet: ${wallet.address}\nissued_at: ${issued}`;
const signature = await wallet.signMessage(msg); // EIP-191 personal_sign
// POST /v1/register  body: { wallet: wallet.address, issued_at: issued, signature }
```

---

## 1. Payment model

```
deposit (on-chain)  →  balance gate (per request)  →  batched settlement (on-chain)  →  SP/platform withdraw
```

- **Deposit** — users call the contract's `depositFIL()` / `depositToken()` to fund a prepaid balance.
- **Balance gate** — on every billable request the gateway reserves the estimated cost (`max_tokens × model price`) against `available = on-chain balance − pendingSpend`. Insufficient funds → **HTTP 402**. After the request, the reservation is reconciled to the actual token usage (failed / interrupted requests are not billed).
- **Settlement** — every `interval_minutes`, the engine groups billable requests per `(wallet, SP, token)`, converts USD → token amount (stablecoin at par, FIL at the current FIL/USD rate), and submits batches to the contract. Each batch is written to a WAL before submission and recognized on-chain by its `detailsHash` — so if the process crashes and retries, the same batch can never be charged twice.
- **Pricing** — `model_prices_usd` is **USD per 1,000,000 tokens** per model (with a `default`). The FIL/USD rate is `manual` or `auto` (CoinGecko → Binance fallback).

Settlement is off by default (`settlement.enabled: false`); when off, the gateway behaves as a pure routing gateway.

---

## 2. Balance gate (on the inference endpoint)

When settlement is enabled and the API key is bound to a `wallet`, `POST /v1/chat/completions` (and `/v1/completions`) enforce a balance check **before** routing:

```json
{
  "error": {
    "message": "insufficient balance",
    "type": "gateway_error"
  }
}
```

**HTTP Status:** 402 Payment Required.

Billing rules:

| Outcome | Billed |
|---|---|
| Success (200) | actual `usage.total_tokens` |
| Retried then succeeded | actual usage, once |
| All retries failed (503) | **not billed** (reservation reversed) |
| Stream interrupted by mining → **transparently resumed** | billed by the **gateway's own count of delivered tokens** (the re-fed prefix of a continuation segment is never double-billed) |
| Stream interrupted and **not resumable** (degrades to an error event) | billed for tokens delivered; the server-fault interruption itself is not billed |
| **Client abandons** a stream mid-flight | billed for tokens **delivered before the disconnect** (no free-riding by hanging up) |
| Stream with no usage chunk | billed by the gateway's own delivered-frame count (no longer depends on the worker reporting usage) |
| 402 / 401 / 404 / 400 | not billed |

> Since transparent stream resume and the stream-billing fix, streaming is metered against the
> **gateway's own count of delivered content frames**: worker-reported usage is preferred
> when present, with the self-count as the fallback for missing/interrupted usage — so
> "no usage → 0 tokens" under-billing is gone, and clients are never billed for
> server-side faults.

A request without a valid Bearer token returns **401**; an API key with no `wallet` bypasses the gate (no billing).

### 2.1 Error codes & request limits (inference endpoint)

The settlement layer adds the following client-facing responses on `/v1/chat/completions` and `/v1/completions`, on top of the base inference error surface (see inference-api.md). All are config-gated and (except the suspension 402) only occur when the operator has enabled the corresponding control.

| Status | Condition | Body `error.message` contains | Headers |
|---|---|---|---|
| 400 | Request body is not valid JSON (checked before routing) | `request body is not valid JSON` | — |
| 400 | Body carries the reserved internal field `om_continuation` (gateway-internal) | `om_continuation is a reserved internal field` | — |
| 402 | Insufficient prepaid balance | `insufficient balance` | — |
| 402 | **Account suspended for unpaid debt** | `account suspended` | — |
| 413 | Request body exceeds `max_request_bytes` | `request body exceeds limit of N bytes` | — |
| 429 | Per-key request rate exceeded | `rate limit exceeded for this API key` | `Retry-After: 1` |
| 429 | Per-key concurrency limit reached | `too many concurrent requests for this API key` | `Retry-After: 1` |
| 503 | Gateway draining (graceful shutdown) | `server is shutting down, please retry` | `Retry-After: 5` |
| 503 | All workers mining and queue timed out / full | `all workers are mining/offline` | `Retry-After: <honest resume estimate>` (smallest mining worker's estimate, clamped [5,120]; fixed 30 when no estimate) |

Notes:
- **Two distinct 402s.** The original `insufficient balance` means the prepaid balance can't cover this request's estimate. `account suspended` is the debt-suspension path: a wallet whose carried (under-funded) debt has reached `debt_suspend_usd` is refused all requests until the next settlement collects the debt after a top-up. A client should treat the latter as "top up, then wait one settlement cycle", not "retry now".
- **429** is per API key (token-bucket rate + concurrent in-flight cap). Honor `Retry-After`. One noisy key cannot affect another key's budget.
- **413** rejects rather than truncates — an oversized body is never silently cut (which would corrupt the JSON and zero the token count). Raise `max_request_bytes` if your prompts are legitimately large.
- **503 (draining)** appears only during a graceful shutdown/restart window; the request was not started, so retrying against the (restarted) gateway or another instance is safe. Distinct from the routing `503` "all workers mining/offline".
- None of these are billed (they reject before or without a settled reservation).

### 2.2 Verifiable billing: signed receipts (client-facing)

Every successful inference response carries an **ed25519 receipt signed by the worker
(SP)** binding the request hash / response hash / token triple / model under the SP's own
signature — the SP cannot later deny the usage it reported. How clients obtain it:

| Request type | Receipt location |
|---|---|
| Non-streaming | Response header `X-Om-Receipt` (base64 JSON with `v`, `request_id`, `model`, `request_sha256`, `response_sha256`, `prompt_tokens`, `completion_tokens`, `cached_tokens`, `ts`, `pubkey`, `sig`) |
| Streaming | **Not delivered by default** (strict SSE parsers are unaffected); send **`X-OM-Receipt-Req: 1`** and the gateway forwards one `data: {"om_receipt": {...}}` event before `[DONE]` |

With the `request_id` from the receipt, the charge can be **audited offline,
independently of the gateway**:

```bash
# public read-only port (:3001), no token needed
curl http://<host>:3001/api/v1/receipt-proof/<request_id>
# one-shot five-step verification (signature / leaf / merkle inclusion / combined hash /
# on-chain processedBatches) — verify-receipt.py ships in the openmodel-contracts repo:
python3 verify-receipt.py http://<host>:3001 <request_id> \
    <FEVM RPC> <settlement contract>
# → RESULT: VERIFIED ✔
```

How it works: each settlement batch commits `detailsHash = sha256(legacy batch hash ‖
MerkleRoot(per-request leaves))` on-chain; every request is one leaf. `receipt-proof`
returns the leaf, the inclusion proof and the batch's on-chain identity, so anyone can
verify "this request entered that on-chain settlement at exactly the usage the SP
signed". Note: **the proof only becomes available once the request has settled on-chain
and cleared the confirmation depth** — typically within ~25 minutes of the request; until
then the endpoint returns **404**, as it does for old charges settled before Merkle
commitments existed. Each request's signed receipt is stored separately at settlement time
and returned along with the proof, so verifying a signature never depends on the request
log — logs can rotate away and old charges stay verifiable. The one exception is the very
oldest Merkle batches, from before these copies were kept: their proofs are still complete
and verifiable, but the response carries no billing record (`record` is null).

### 2.3 GET /v1/catalog (model catalog & pricing)

Clients can discover the **currently available models, prices and specs** (Bearer key
required):

```bash
curl http://<host>:3000/v1/catalog -H "Authorization: Bearer $KEY"
```

Returns `{"object":"list","data":[...]}` with per model: `id`, `available` (loaded /
supported by a currently routable worker), `input_price_usd_per_1m` (falls back to the
output price when no explicit input price), `output_price_usd_per_1m` (from
`model_prices_usd`), `cache_read_price_usd_per_1m`, `context_window`, `max_output` (the
latter three from `model_catalog`). Models with a catalog entry are billed with the
**input / output / cache-hit three-way split**; others bill flat `total_tokens × output
price`.

The response also carries a top-level **`fil_price_usd`** — the FIL/USD rate billing
currently uses (the same number settlement converts with), so you can translate the USD
prices above into FIL and estimate how much balance a call will burn. Omitted when
settlement is disabled.

---

## 3. Settlement Admin API

Served on the admin port (**9091**, alongside the worker-management endpoints).

**Authentication:** Bearer token (`AGENT_ADMIN_TOKEN`).

```
Authorization: Bearer <AGENT_ADMIN_TOKEN>
```

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/v1/revenue` | All SP earnings (on-chain), plus current FIL price |
| GET | `/api/v1/revenue/:sp` | Earnings for one SP (miner or EVM address) |
| GET | `/api/v1/balances` | All known user balances + pending spend |
| GET | `/api/v1/balances/:addr` | One user's balance + pending spend |
| GET | `/api/v1/settlements` | Total settled batch count (on-chain nonce) |
| GET | `/api/v1/settlements/:id` | **On-chain record for a batch + local audit cross-reference** |
| POST | `/api/v1/settle-now` | Trigger an immediate settlement cycle |
| GET | `/api/v1/operator-balance` | Operator wallet FIL balance (gas) |
| GET / PUT | `/api/v1/fil-price` | Query or set the FIL/USD rate |
| GET / POST | `/api/v1/reconcile` | Three-way billing reconciliation (billed vs settled+pending+debt); **409 on drift** |
| GET | `/api/v1/state-check` | Verify integrity of persisted settlement state (post-restore); **409 on problems** |
| GET | `/api/v1/sp-earnings-detail/:sp` | **Per-request earnings for one SP** (each inference request: earning + settled/pending + on-chain tx) |
| GET | `/api/v1/receipt-proof/:request_id` | **Verifiable-billing proof for one request** (signed receipt + Merkle inclusion proof + on-chain batch; also on public port :3001, see §2.2) |

### GET /api/v1/settlements/:id

Returns the **on-chain settlement record** for batch `:id` (the on-chain batch number, 1-based), cross-referenced against the local audit log (`settlements.jsonl`). This is the data source for `settlement-cli verify`.

```bash
curl http://<host>:9091/api/v1/settlements/3 \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN"
```

```json
{
  "batch_id": 3,
  "on_chain": {
    "details_hash": "0x77ed5255b3db03b27f62335b60ee01e13b5f74623b424b76bef5bdb162e43d04",
    "total_amount": "2000000000000000000",
    "settled_count": 3,
    "failed_count": 0,
    "timestamp": 1717329600,
    "processed": true
  },
  "local_audit": {
    "found": true,
    "tx_hash": "0xdea4...",
    "block_number": 8,
    "gas_used": 123456,
    "item_count": 3
  }
}
```

- `on_chain` is read directly from the contract (`getSettlement(batchId)` + `processedBatches(detailsHash)`). `total_amount` is in the token's smallest unit (string, to avoid precision loss); `settled_count` / `failed_count` reflect partial-settlement skips.
- `local_audit` is the matching entry from the operator's local settlement log (`tx_hash`, `block_number`, `gas_used`, `item_count`). `found: false` means the on-chain batch has **no local audit record** — an audit-log gap worth investigating.
- The `local_audit` object is omitted entirely if the settlement engine is not attached.

**Errors:**

| Condition | Status | Body |
|---|---|---|
| `:id` is not a positive integer | 400 | `{"error":{"message":"invalid batch ID (must be a positive integer)", ...}}` |
| `:id` greater than the latest batch | 404 | `{"error":{"message":"batch 99 not found (latest batch is 5)", ...}}` |

### Other endpoints (brief)

```bash
# Revenue (all SPs)
curl http://<host>:9091/api/v1/revenue -H "Authorization: Bearer $TOK"
# => {"providers":[{"miner_address":"t0182063","evm_address":"0x..","earnings":{"USDC":"79.800000"}}],"fil_price_usd":"2.0000"}

# One user's balance
curl http://<host>:9091/api/v1/balances/0x7099... -H "Authorization: Bearer $TOK"
# => {"address":"0x7099...","balances":{"FIL":"90.000000"},"pending_spend_usd":"0.000000"}

# Settled batch count
curl http://<host>:9091/api/v1/settlements -H "Authorization: Bearer $TOK"
# => {"total_batches":5}

# Trigger settlement now
curl -X POST http://<host>:9091/api/v1/settle-now -H "Authorization: Bearer $TOK"
# => {"triggered":true,"message":"settlement cycle queued"}

# Operator gas balance (+ current active RPC endpoint; multi-endpoint failover observability)
curl http://<host>:9091/api/v1/operator-balance -H "Authorization: Bearer $TOK"
# => {"active_rpc_endpoint":"https://api.calibration.node.glif.io/rpc/v1",
#     "address":"0xf39F...","balance":"4.980000 FIL"}

# FIL price (get / set)
curl http://<host>:9091/api/v1/fil-price -H "Authorization: Bearer $TOK"
# => {"fil_price_usd":"2.0000"}
curl -X PUT http://<host>:9091/api/v1/fil-price -H "Authorization: Bearer $TOK" \
  -H "Content-Type: application/json" -d '{"fil_price_usd":"3.50"}'
# => {"fil_price_usd":"3.5000","updated":true}
```

### GET / POST /api/v1/reconcile

Runs the **three-way billing reconciliation** and returns the report. The reconciler keeps its own **request-log cursor** and a **durable running total of everything billed** (`reconcile-cursor.json` / `reconcile-state.json` in the data dir): each pass reads only the **newly-appended** billable records and adds them to the total, so log rotation deleting old backups never causes under-counting. The first pass records the then-current settled/pending/debt as a **baseline**; every pass after that checks one equation:

```
cumulative billed  ==  (settled − baseline) + (pending − baseline) + (debt − baseline)
```

The report's numbers follow the same logic: `billed_usd` is everything billed since reconciliation started; `settled_usd`/`pending_usd`/`debt_usd` are what has been added on top of the baseline (not global totals). Reconciliation also runs automatically every `reconcile_interval_minutes`. **To reset the baseline**, delete `reconcile-state.json` and `reconcile-cursor.json` in the data dir and restart.

```bash
curl http://<host>:9091/api/v1/reconcile -H "Authorization: Bearer $TOK"
```

```json
{
  "timestamp": "2026-06-29T12:00:00Z",
  "billed_usd": "30.000000",
  "settled_usd": "20.000000",
  "pending_usd": "10.000000",
  "debt_usd": "0.000000",
  "drift_usd": "0.000000",
  "drift_abs_usd": "0.000000",
  "within_tolerance": true,
  "tolerance_usd": "0.010000",
  "billable_count": 3,
  "dead_letters": 0
}
```

- **HTTP 200** when `within_tolerance` is true; **409 Conflict** when drift exceeds the tolerance (so a deploy/CI gate can fail on drift); **503** when settlement is disabled; **500** on a read error.
- `drift_usd = cumulative billed − (settled delta + pending delta + debt delta)`. A positive drift means under-settlement (potential lost revenue); negative means over-settlement (potential double counting). Investigate any non-zero drift beyond tolerance.
- **Why incremental**: the old implementation re-summed whatever request log was still on disk to get an all-time "billed" figure. As soon as rotation deleted old backups — or the chain held settlements from before the reconciler ever started — the numbers stopped matching, and a steadily growing **false drift** appeared (we measured −7.36 USD in long-run testing), drowning out real alerts. Counting increments against a baseline makes the same scenario read drift=0.

### GET /api/v1/state-check

Verifies the integrity of the persisted settlement state — intended to be run **after a backup restore, before starting to settle**. It checks that the cursor is readable and not beyond the request log, the cumulative settled total parses, and the WAL (if present) parses.

```bash
curl http://<host>:9091/api/v1/state-check -H "Authorization: Bearer $TOK"
```

```json
{
  "ok": true,
  "cursor_offset": 4096,
  "settled_usd": "20.000000",
  "debt_entries": 0,
  "dead_letters": 0,
  "wal_present": false,
  "wal_confirmed_batches": 0,
  "wal_total_batches": 0,
  "problems": []
}
```

- **HTTP 200** when `ok` is true; **409 Conflict** when problems are found (e.g. `cursor offset N exceeds request-log size M`, `pending WAL unparseable`) — a restore script / CI gate should fail on 409; **503** when settlement is disabled.

### GET /api/v1/sp-earnings-detail/:sp

**Per-request earnings for one SP** — lets a Storage Provider see, for each individual inference request it served, how much it earned and whether that request has been settled on-chain (and in which tx). This is the per-request complement to `/api/v1/revenue/:sp` (which only gives the on-chain aggregate total).

- `:sp` may be a **miner address** (resolved via `sp_address_map`) or an **EVM address**.
- Query params: `since` (unix seconds — only requests at/after this time), `limit` (max items, newest first, default 200).
- The earning per request is computed from the request log with the **exact same pricing logic settlement uses**, minus the on-chain platform fee (`platform_fee_bps`, read live from the contract) — so the detail always matches how billing actually works.
- Settlement status (`settled` + `tx_hash`/`block_number`) comes straight from the **Merkle commitment ledger**: a request that made it into a confirmed on-chain batch shows `settled: true` along with that batch's tx; one that hasn't yet shows `settled: false` (pending). Historical requests settled before Merkle commitments were enabled (pre-2026-07-03) also show `settled: false` — the same line receipt-proof draws by returning 404 for them.
- **Totals cover only the returned page**: `total/settled/pending_earning_usd` and both counts describe just the items in this response — that's what `"scope": "returned_items"` is saying. For all-time settled totals use `/api/v1/revenue/:sp`, which keeps its own running total and answers instantly. The reason is simple: billing history keeps growing, so the endpoint walks from the newest records backwards and stops as soon as it has `limit` items — a query stays fast no matter how much history piles up, at the cost of totals that only reflect the page you fetched.

```bash
curl "http://<host>:9091/api/v1/sp-earnings-detail/0x3C44...?since=1782700000&limit=100" \
  -H "Authorization: Bearer $TOK"
```

```json
{
  "sp": "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC",
  "platform_fee_bps": 300,
  "scope": "returned_items",
  "total_earning_usd": "14.55000000",
  "settled_earning_usd": "14.55000000",
  "pending_earning_usd": "0.00000000",
  "settled_count": 2,
  "pending_count": 0,
  "items": [
    {
      "request_id": "req-abc",
      "timestamp": "2026-06-30T12:00:00Z",
      "model": "Qwen2.5-3B",
      "total_tokens": 10,
      "prompt_tokens": 4,
      "cached_tokens": 0,
      "earning_usd": "9.70000000",
      "settled": true,
      "tx_hash": "0xdea4...",
      "details_hash": "0x77ed...",
      "block_number": 8
    }
  ]
}
```

- `total_earning_usd` = `settled_earning_usd` + `pending_earning_usd` (both page-scoped, see above). Per-item `earning_usd` stays consistent with the chain: **summing any page's settled items equals what those requests credited the SP on-chain** (within rounding) — that's the cross-check an SP uses to trust the numbers; for all-time reconciliation compare `/api/v1/revenue/:sp` against on-chain `spEarnings`.

#### Public read-only query port (SP self-service)

The endpoint above lives on the admin port (**9091**) and needs the admin token — but that token is operator god-mode (register/deregister workers, force settlement, pause the contract), so it **must not be handed to a third-party SP**. For SP self-service there is a **separate, public, read-only port** that exposes ONLY two read-only routes: `sp-earnings-detail` and `receipt-proof` (verifiable billing, see §2.2):

- **Port**: **3001** in the reference deploy (grouped with the client gateway on 3000 as the other public-facing port; the code default is 9092, but 9092 is commonly taken by Prometheus).
- **No auth**: anyone may query; just put the `:sp` address in the path. Open querying is intentional — the on-chain `spEarnings` mapping is already public, so exposing the off-chain per-request detail is just one more step of transparency, and it lets SPs cross-check that the operator settles fairly.
- **No client identity**: the response carries only request_id / model / tokens / earning / tx — **never the paying client's wallet or api-key**. SP-earnings transparency is not client-activity transparency.
- **Rate-limited**: anyone can hit this port, so a global token bucket (`rate_per_sec` / `rate_burst`) keeps the volume in check; over the limit you get **429**. `/health` is never rate-limited. Both routes are cheap to serve: `receipt-proof` jumps straight to the right ledger line through an index, and `sp-earnings-detail` walks from newest to oldest and stops once its page is full — however large the ledger grows, no single query ever reads all of it.
- **Plain HTTP**: fine for an invite-only, small-amount trial; front it with a TLS reverse proxy before exposing it to untrusted networks at scale (to stop path tampering/impersonation of this money-adjacent response — on-chain `getSPEarnings` is still the unforgeable backstop).
- Requires `settlement.enabled`; the port does not start when `public_query.enabled: false`.

```bash
# no token needed
curl "http://<host>:3001/api/v1/sp-earnings-detail/0x3C44...?limit=100"
```

Config (`sp-state-agent.yaml`):

```yaml
public_query:
  enabled: true
  port: 3001
  rate_per_sec: 20      # global sustained rate (req/sec); 0 = default 20
  rate_burst: 40        # token-bucket burst; 0 = 2×rate
```

---

## 4. settlement-cli

A thin CLI over the Admin API. Configured via environment:

```bash
export ADMIN_URL=http://localhost:9091        # default
export AGENT_ADMIN_TOKEN=<admin token>
```

```
settlement-cli <command> [args]

  balance <address>         Query user balance (on-chain + pending spend)
  balances                  List all user balances
  earnings <sp_address>     Query SP earnings
  revenue                   All SP revenue summary
  settlements               List settlement batches (count)
  verify <batch_id>         Reconcile an on-chain batch vs the local audit log
  settle-now                Trigger immediate settlement
  operator-balance          Operator wallet gas balance
  fil-price [set <price>]   Query or set FIL/USD price
  revenue-report            Formatted revenue report
  sp-detail <sp> [since_unix] [limit]
                            Per-request earnings for one SP (each request:
                            earning + settled/pending + on-chain tx)
```

### settlement-cli verify <batch_id>

Reconciles an on-chain settlement batch against the operator's local audit log. It calls `GET /api/v1/settlements/:id`, prints the raw response, then prints a one-line **verdict**:

```bash
settlement-cli verify 3
```

```json
{
  "batch_id": 3,
  "on_chain": { "processed": true, "settled_count": 3, "details_hash": "0x77ed…3d04", ... },
  "local_audit": { "found": true, "tx_hash": "0xdea4…", "block_number": 8, ... }
}

VERDICT: OK — on-chain record matches the local audit log
```

Verdicts:

| On-chain `processed` | Local audit | Verdict |
|:---:|:---:|---|
| true | found | `OK — on-chain record matches the local audit log` |
| true | not found | `WARN — on-chain shows processed, but NO local audit record (audit-log gap)` |
| false | found | `WARN — local audit record exists, but batch is NOT marked processed on-chain` |
| false | not found | `WARN — batch not processed on-chain and no local audit record` |
| true | (no engine) | `OK (on-chain) — batch processed on-chain (no local audit log available to cross-check)` |

Use `verify` to spot-check any batch when reconciling revenue, or to investigate a disputed settlement: it tells you whether the chain and the operator's records agree.

---

## 5. Settlement configuration (reference)

```yaml
settlement:
  enabled: true
  rpc_url: https://api.calibration.node.glif.io/rpc/v1
  rpc_urls:                          # failover endpoints (optional). rpc_url is always
    - https://rpc.ankr.com/filecoin_testnet      # tried first; when the active endpoint fails a
    - https://calibration.filfox.info/rpc/v1     # health probe it rotates within ~30s. Active
                                     # endpoint is shown by GET /api/v1/operator-balance.
  chain_id: 314159
  contract_address: "0x..."
  operator_private_key: ${SETTLEMENT_PRIVATE_KEY}
  interval_minutes: 15
  max_batch_size: 50
  confirmation_depth: 5             # reorg safety: blocks of finality before the cursor advances; 0 = mined==final
  model_prices_usd:                 # USD per 1,000,000 tokens
    "Qwen/Qwen2.5-3B-Instruct": "0.10"
    "default": "0.20"
  fil_price_usd: "3.50"             # manual rate, or initial value for auto
  fil_price_source: "auto"          # "manual" | "auto"
  fil_price_sources: ["coingecko", "binance"]
  model_catalog:                    # optional: per-model input/cache-read split pricing + catalog metadata
    "Qwen/Qwen2.5-3B-Instruct": { input: "0.04", cache_read: "0.01", context_window: 32768, max_output: 4096 }
    # models with a catalog entry bill input/output/cache-hit; others bill total×output (backward compatible)
  # stablecoin depeg protection (optional; unset = stablecoin pinned at $1)
  stablecoin_symbol: USDC
  stablecoin_price_sources: [coingecko, binance]   # empty = not monitored
  stablecoin_depeg_bps: 200         # >2% off $1 → depegged: settlement skips it (falls to FIL) and it stops counting toward spendable credit
  stablecoin_price_refresh_sec: 300
  supported_tokens:
    - { symbol: "FIL",  address: "0x0000000000000000000000000000000000000000", decimals: 18 }
    - { symbol: "USDC", address: "0x...",                                        decimals: 6  }
  deduction_priority: ["USDC", "FIL"]
  min_balance_fil: "0.001"          # reserve buffer kept un-spendable (absorbs estimate-vs-settlement drift)
  max_pending_spend_fil: "10"       # per-wallet unsettled-spend credit cap; 0 = off
  operator_min_balance_fil: "0.1"   # operator gas alert threshold
  debt_suspend_usd: "1.0"           # suspend a wallet at this carried-debt USD; "" = disabled, "0" = any positive debt
  reconcile_interval_minutes: 30    # auto-reconcile cadence; 0 = default 30
  reconcile_tolerance_usd: "0.01"   # drift tolerated before flagged; empty = 1 cent
  sp_address_map:
    "t0182063": "0x..."             # MinerAddress → EVM payout address
```

API keys carry the user wallet that the balance gate and settlement use; the `gateway` block also holds the optional per-key abuse controls:

```yaml
gateway:
  api_keys:
    - { key: "sk-user1", name: "user1", wallet: "0x7099..." }
  # Abuse controls — all optional, 0/unset = that dimension is unlimited:
  rate_per_sec_per_key: 0       # sustained requests/sec per API key (token bucket) → 429
  rate_burst_per_key: 0         # token-bucket burst; 0 = ceil(rate) or 1
  max_concurrent_per_key: 0     # max in-flight requests per API key → 429
  max_request_bytes: 0          # max request body size in bytes; 0 = default 10 MiB → 413
  stream_resume: true           # transparently continue mining-interrupted streams on another worker (default false)

# SP fraud-detection sampling (optional; unset = off, zero cost)
verification:
  sample_rate: 0.02             # fraction of request/response pairs retained as offline audit evidence
  sample_log_path: /data/verify-samples.jsonl
  sample_max_mb: 50             # per-file rotation cap
  sample_backups: 5
```
