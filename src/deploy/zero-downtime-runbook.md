# Zero-Downtime Deploy & Rollback Runbook

How to upgrade / restart the OpenModel sp-gateway without dropping in-flight
requests or double-settling, and how to roll back safely.

## What the gateway does on shutdown (built-in)

On `SIGTERM`/`SIGINT` the gateway runs an ordered graceful shutdown
(`cmd/agent/main.go`):

1. **Drain.** `Gateway.BeginDrain(8s)` flips the gateway into draining mode:
   every NEW `/v1/*` proxy request is answered with `503 + Retry-After: 5`, while
   requests already in flight are given up to 8s to finish. The
   `openmodel_gateway_draining` gauge goes to 1 for the window.
2. **Admin server stop.** Registration/metrics endpoints stop accepting.
3. **HTTP server `Shutdown`.** Waits for connections to close (bounded, 10s).
4. **Background cancel.** Poller, balance refresh, pricer, and the settler stop.
   The settler is **WAL-protected**: if a cycle was mid-submit, the write-ahead
   `pending-settlement.json` is replayed on next start and on-chain dedup
   (`processedBatches[detailsHash]`) prevents a double charge. So a hard stop at
   any point is safe — no lost and no duplicated settlement.

Result: clients see a brief, retriable 503 rather than a connection reset, and
billing is exactly-once across the restart.

## Rolling restart (single coordinator host)

The gateway is a single coordinator process. For a true no-gap restart, run two
instances behind a TCP load balancer / reverse proxy and restart them one at a
time:

```
# Assuming systemd unit openmodel-gateway@1 / @2 behind nginx/HAProxy on :3000.
# 1. Drain + restart instance 1; the LB sends new traffic to instance 2.
systemctl restart openmodel-gateway@1
#    ... wait for it to report healthy ...
curl -fsS localhost:9091/health        # instance 1 admin health
# 2. Then instance 2.
systemctl restart openmodel-gateway@2
```

Important constraint: **only ONE instance may run the settler** (settlement must be
single-writer to the cursor/WAL). Run the second instance with `settlement.enabled:
false`, or front settlement with a leader lock. Both instances may serve inference
and write to the SAME request log only if that log is per-instance; otherwise keep
settlement + request-log ownership on instance 1 and let instance 2 be inference-only.

Single-instance hosts (today's default) cannot be truly zero-downtime, but the drain
makes the gap a ~8s window of retriable 503s rather than dropped connections. Tell
clients to retry on 503 (the OpenAI SDKs do by default).

## Health gating

- `GET :9091/health` — liveness (always 200 once the process is up).
- `GET :9091/ready` — readiness (worker(s) reachable). Gate the LB on `/ready` so a
  freshly-started instance only receives traffic once it has live workers.

## Rollback

Before every deploy, tag the running image as a dated backup so a rollback
target always exists (`docker tag openmodel-sp-gateway:latest
openmodel-sp-gateway:pre-<date>`).

```
# Roll back to the previous known-good image:
docker tag openmodel-sp-gateway:pre-<date> openmodel-sp-gateway:latest
docker compose up -d sp-gateway
```

Settlement-safety during rollback:

- The on-chain contract is **not upgradeable**. A rollback only swaps the gateway
  binary; it does not change contract state. The cursor, WAL, and `processedBatches`
  dedup make a binary rollback safe — a batch already confirmed on-chain is skipped
  on replay.
- Do NOT roll back to a build whose settlement record/JSON formats are incompatible
  with the on-disk `pending-settlement.json` / `settlement-cursor.json`. If the WAL
  schema changed between versions, let the current version finish settling (WAL
  empty, `openmodel_settlement_pending_wal == 0`) BEFORE rolling back.
- If a contract-level bug is found, there is no in-place fix: pause settlement
  (`settlement.enabled: false` + redeploy), migrate to a new contract, and re-point
  `settlement.contract_address`. Funds already deposited stay in the old contract
  until withdrawn/refunded.

## Pre-restart checklist

- [ ] `openmodel_settlement_pending_wal == 0` (no interrupted settlement) — or accept
      that it will be replayed by the new process (safe, but confirm version compat).
- [ ] `openmodel_settlement_operator_balance_low == 0` (operator has gas to finish).
- [ ] LB gated on `/ready`, clients configured to retry on 503.
- [ ] New image tagged and the previous image retained for rollback.
