# OpenModel Gateway — Routing, Billing & On-Chain Settlement

The coordinator of the OpenModel network: a single Go service that aggregates
many SP worker machines (each running the
[openmodel](https://github.com/6block/openmodel) stack) behind **one
OpenAI-compatible endpoint**, routes around mining windows, meters every
request, and settles usage on-chain (Filecoin FEVM) in batches.

**v2.0.0** adds the settlement layer on top of the v1 routing gateway:
self-service API-key registration, prepaid balance gating, batch settlement to a
smart contract, and independently verifiable billing. With
`settlement.enabled: false` it behaves exactly like v1 — a pure router.

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
| `http://36.189.235.195:18019` | OpenAI-compatible API + self-service registration (`/v1/register`) |
| `http://36.189.235.195:18020` | Public read-only queries: billing proofs (`/api/v1/receipt-proof/…`), SP earnings |

```bash
# 1. Register: prove wallet ownership with an EIP-191 signature, get an API key
#    (message format and a signing example: docs/settlement-api.md §1)
curl -X POST http://36.189.235.195:18019/v1/register \
  -H "Content-Type: application/json" \
  -d '{"wallet":"0x…","issued_at":<unix seconds>,"signature":"0x…"}'

# 2. Deposit a SMALL amount of FIL from that wallet to the settlement contract
#    on Filecoin MAINNET: 0x1BB694BD2759eC88Bc04595D9677cb1065fa7D1f
#    (depositFIL(); this is real FIL — without a deposit, billable calls return 402)

# 3. Call the API with your key
curl http://36.189.235.195:18019/v1/chat/completions \
  -H "Authorization: Bearer sk-om-…" -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'

# 4. Audit any charge: take request_id from the X-Om-Receipt response header, then
curl http://36.189.235.195:18020/api/v1/receipt-proof/<request_id>
```

> ⚠️ **Trial limitations.** This is an early, incomplete version of the
> self-service experience, offered for evaluation only. The complete
> registration/account system (key rotation and recovery, multiple keys per
> wallet, registration rate-limiting, a user dashboard) is scheduled for the M4
> milestone. The endpoints run plain HTTP with no SLA and may change without
> notice — **use small deposits only**, amounts you are comfortable treating as
> test spend.

## Repository layout

```
openmodel-gateway/
├── README.md                  # This document (deployment guide)
├── docker-compose.yml         # Compose file (pre-built image)
├── .env.example               # Environment variable template
├── config/                    # Gateway configuration samples
├── docs/
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
| Workers | One or more machines running the openmodel SP stack (scheduler :9090 + inference :8000) |
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
  -d '{"model":"default","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'
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
| **3001** | SPs / users | None — read-only billing-proof and SP-earnings queries; rate-limited; responses carry no client identity | Public OK |

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
  an EIP-191 signature, receive an API key bound to that wallet
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
| Contract repo | [openmodel-contracts](https://github.com/6block/openmodel-contracts) v1.0.0 |
| Filecoin Calibration | `0x83c264c95e7Ad4b30Caa5Bc60e75E317bf109E4F` |
| Filecoin Mainnet | `0x1BB694BD2759eC88Bc04595D9677cb1065fa7D1f` (trial: fee 0%) |

The contract is non-upgradeable; the gateway pins its interface internally. A
contract change means a new address and a new gateway release.

## API documentation

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

- **v2.0.0** (this release): settlement layer + verifiable billing + public
  query port; routing refinements (predictive de-prioritization, transparent
  stream resume, honest Retry-After).
- v1.0.0: routing gateway.

Recommended workers: openmodel v1.2.0+ (receipt signing and stream continuation
are negotiated per worker via `/health`; older workers keep working with those
features dormant).
