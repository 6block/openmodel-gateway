# OpenModel Gateway — Routing, Billing & On-Chain Settlement

The coordinator of the OpenModel network: a single Go service that aggregates
many SP worker machines (each running the
[openmodel](https://github.com/6block/openmodel) stack) behind **one
OpenAI-compatible endpoint**, routes around mining windows, meters every
request, and settles usage on-chain (Filecoin FEVM) in batches.

The gateway ships self-service API-key registration, prepaid balance gating,
batch settlement to a smart contract, independently verifiable billing, SP
self-registration with certificate issuance, and a built-in web chat UI. With
`settlement.enabled: false` it behaves as a pure router.

```
Clients (curl / OpenAI SDK / LangChain)
        │  API key
        ▼
┌──────────────────────────────────────────────┐
│  sp-gateway                                   │
│  :3000  OpenAI API + /v1/register             │
│  :9091  Admin API + Prometheus metrics        │
│  :3001  Public read-only queries (no auth)    │
└───────┬───────────────────────┬───────────────┘
        │ polls health/ready    │ batch settlement
        ▼                       ▼
   SP workers (openmodel)   Settlement contract (FEVM)
```

## Try the hosted trial

A trial instance is live for hands-on experience:

| Endpoint | Purpose |
|---|---|
| **`https://openmodel.filfox.info`** | **Everything** — OpenAI-compatible API, self-service registration, the web chat UI, and public billing queries, one origin behind a publicly-trusted certificate |


```bash
# 1. Register: prove wallet ownership with an EIP-191 signature, get an API key
#    (exact message bytes + an ethers signing example: docs/settlement-api.md,
#     section "Registration & API keys")
curl "https://openmodel.filfox.info/v1/register/message?wallet=0x…"   # returns {message, issued_at}
# sign `message` with the wallet (EIP-191 personal_sign), then:
curl -X POST https://openmodel.filfox.info/v1/register \
  -H "Content-Type: application/json" \
  -d '{"wallet":"0x…","issued_at":<from step above>,"signature":"0x…"}'

# 2. Deposit a SMALL amount from that wallet to the settlement contract on
#    Filecoin MAINNET: 0x60D41baEaBe1ABE061AE82c44425debc35bA524A
#      FIL   — depositFIL(), payable
#      USDFC — approve() on 0x80B98d3aa09ffff255c3ba4A241111Ff1262F045, then depositToken()
#    This is real money. Without a deposit, billable calls return 402. A USDFC
#    balance is spent before FIL, so a stablecoin deposit keeps your credit's
#    purchasing power fixed while FIL's floats with the exchange rate.

# 3. Call the API with your key  (<model> = a model id from GET /v1/models; the "default" alias is not accepted)
curl https://openmodel.filfox.info/v1/chat/completions \
  -H "Authorization: Bearer sk-om-…" -H "Content-Type: application/json" \
  -d '{"model":"<model>","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'

# 4. Audit any charge: take request_id from the X-Om-Receipt response header, then
curl https://openmodel.filfox.info/api/v1/receipt-proof/<request_id>
```

> ⚠️ **Trial limitations.** The hosted endpoints are an invite-scale alpha:
> no SLA, and parameters (pricing, limits, addresses) may change with notice in
> this README — **use small deposits only**, amounts you are comfortable
> treating as test spend. Accounts are fully self-service (wallet-signed
> registration, up to 10 keys per wallet, IP rate-limited, keys stored hashed;
> lost keys are replaced, not recovered; web dashboard on the HTTPS endpoint).
> The hosted entry is the domain above.

## Repository layout

```
openmodel-gateway/
├── README.md                  # This document (deployment guide)
├── docker-compose.yml         # Compose file (pre-built image)
├── .env.example               # Environment variable template
├── config/                    # Gateway configuration samples
├── docs/
│   ├── USER-GUIDE.md          # Full consumer reference: keys, funding, billing, refunds
│   ├── getting-started.md     # Third-party developer guide: zero to first billed call
│   ├── inference-api.md       # Inference API (chat/completions, streaming, workers admin)
│   └── settlement-api.md      # Registration, billing, settlement admin, receipts
├── src/                       # Source code (Go module + Dockerfile + ops assets)
└── release/                   # Staging area for image tarballs (uploaded to GitHub Releases)
```

## Requirements

| Item | Requirement |
|---|---|
| OS | Ubuntu 22.04+ or compatible Linux |
| Docker | 24+ with Compose v2 |
| Workers | One or more machines running the openmodel SP stack (self-registered v1.3 workers expose TLS fronts :38443/:39443; operator-configured plaintext workers use :8000/:9090) |
| Settlement (optional) | A Filecoin RPC endpoint, the settlement contract address, and a funded operator wallet |

## Deploy from images

```bash
# 1. Download the image tarball from GitHub Releases, verify, and load
sha256sum -c SHA256SUMS.txt
docker load -i openmodel-sp-gateway.tar.gz

# 2. Configure
cp .env.example .env               # CLIENT_TOKEN, AGENT_ADMIN_TOKEN,
                                   # OPERATOR_PRIVATE_KEY (only if settlement on)
vi config/sp-state-agent.yaml      # worker polling, routing, settlement section

# 3. Launch
docker compose up -d

# 4. Register each worker
curl -X POST http://localhost:9091/api/v1/workers/register \
  -H "Authorization: Bearer $AGENT_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"id":"sp-1","endpoint":"http://<worker>:8000","scheduler_url":"http://<worker>:9090",
       "gpu_count":8,"miner_address":"t0xxxx","auth_token":"<per-worker token>"}'

# 5. Use it
curl http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_TOKEN" -H "Content-Type: application/json" \
  -d '{"model":"<model>","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'
```

Settlement is **off by default**. To enable it, set `settlement.enabled: true`
plus `contract_address`, `rpc_urls`, `sp_address_map` (miner → payout address)
in the config, and provide `OPERATOR_PRIVATE_KEY`. Details and every endpoint:
[docs/settlement-api.md](docs/settlement-api.md).

## Ports & trust boundaries

| Port | Audience | Auth | Public exposure |
|---|---|---|---|
| **3000** | Clients | API key (Bearer); `/v1/register` is open by design | Public, behind TLS |
| **9091** | Operator only | Admin token — full control (workers, settlement, pricing) | **Never expose** |
| **3001** | SPs / users | None — read-only billing-proof, SP-earnings and network-stats queries; rate-limited; responses carry no client identity | Public OK |
| **3002** | Workers + browsers | TLS (`sp_registration.https_port`): SP self-registration & worker direction under the registration CA, plus the web UI and the same read-only queries same-origin | Public, when self-registration / web UI is on |

## Features

**Routing**
- Weighted load balancing by GPU count and in-flight load; model-aware routing
  (`supported_models`) with automatic model switching
- **Mining-aware**: workers yield to WindowPoSt/WinningPoSt; requests queue
  through short mining windows instead of failing; workers about to yield are
  de-prioritized ahead of time; `503` responses carry an honest `Retry-After`
  derived from real resume estimates
- **Transparent stream resume**: a stream interrupted by mining continues on
  another worker — the client sees one uninterrupted stream, and is billed only
  for delivered output
- Session affinity (`X-Session-Id`) for prefix-cache reuse; per-key rate and
  concurrency limits; request-size cap; graceful drain on shutdown

**Billing & settlement**
- **Self-service onboarding**: `POST /v1/register` — prove wallet ownership with
  an EIP-191 signature, receive an API key bound to that wallet; up to 10 keys
  per wallet via signed `/v1/keys` (create/list/delete), keys stored hashed
- **Thinking mode**: per-request `enable_thinking` surfaces reasoning models'
  chain of thought as `reasoning_content` (billed as output; off by default)
- **SP self-registration & capability auditing**: miners join with one
  miner-signed challenge (`/v1/sp/register`, certificates issued at
  registration); a background auditor probes claimed models and an admission
  gate withholds routing until a claim passes
- **Public network stats**: `GET /api/v1/network-stats` — aggregate provider /
  model / developer counts plus on-chain cumulative request & token counters
- **Prepaid balance gate**: users deposit FIL/stablecoins into the settlement
  contract; each request is pre-reserved against on-chain balance minus pending
  spend and corrected to actual usage afterwards (`402` when insufficient;
  failed requests are never billed)
- **Batch settlement**: usage is aggregated per (user, SP, token) and submitted
  on-chain every settlement interval — crash-safe and idempotent (replays cannot
  double-charge); multiple RPC endpoints with automatic failover; FIL price feed
  that defers settlement while stale; stablecoin depeg protection
- **Verifiable billing**: workers sign per-request receipts; every settlement
  batch commits a Merkle root over per-request leaves into the on-chain record —
  anyone holding a `request_id` can verify the exact charge against the chain
  without trusting the operator. Verifier and byte-level spec:
  [openmodel-contracts](https://github.com/6block/openmodel-contracts)
- Continuous three-way reconciliation (billed vs settled vs pending) with drift
  alerting; debt tracking with automatic service suspension
- Prometheus metrics and alert rules; request log with rotation; state
  backup/restore tooling

## Settlement contract

| | |
|---|---|
| Contract repo | [openmodel-contracts](https://github.com/6block/openmodel-contracts) v1.3.0 |
| Filecoin Calibration | `0x97a3d202CfF60dD369cdf8F7D514dAe36b469852` |
| Filecoin Mainnet | `0x60D41baEaBe1ABE061AE82c44425debc35bA524A` (trial: fee 0%) |

The contract is non-upgradeable; the gateway pins its interface internally. A
contract change means a new address and a new gateway release. The addresses
above are the ones the hosted trial settles against — they run contract v1.3,
whose `submitSettlement` carries per-batch request/token counts. **This release
speaks the v1.3 contract** (`contract_schema: 3`, the sample-config default:
per-batch request/token stats plus cumulative counters) and stays compatible
with v1.2-era deployments via `contract_schema: 2`. Deposits, balances and
billing are unaffected either way — only the settlement call differs.

## API documentation

- [docs/getting-started.md](docs/getting-started.md) — third-party developer
  guide: register, fund, first call, verify your bill.
- [docs/USER-GUIDE.md](docs/USER-GUIDE.md) — the full consumer reference:
  accounts & keys, funding, billing semantics, verification, refunds,
  troubleshooting.
- [docs/inference-api.md](docs/inference-api.md) — inference API: chat/completions,
  streaming, model names, error codes, worker admin, stats, metrics
- [docs/settlement-api.md](docs/settlement-api.md) — key registration, balance gate
  and billing rules, receipts and billing proofs, settlement admin API,
  `settlement-cli`, public query port

## Build from source

```bash
cd src
go build ./... && go test ./...
docker build -t openmodel-sp-gateway:latest .
```

## Versions

- **v2.1.0** (this release): v1.3 contract schema (per-batch stats),
  multi-key accounts, thinking mode, SP self-registration + certificate
  issuance + capability auditing, network-stats endpoint, trusted-proxy
  client-IP resolution (`gateway.trusted_proxies`), web chat UI,
  model-switch resilience fixes.
- v2.0.0: settlement layer + verifiable billing + public
  query port; routing refinements (predictive de-prioritization, transparent
  stream resume, honest Retry-After).
- v1.0.0: routing gateway.

Recommended workers: openmodel v1.3.0+ (self-registration, certificates, model
claims); v1.2.0 workers still serve (receipt signing and stream continuation
are negotiated per worker via `/health`; older workers keep working with those
features dormant).
