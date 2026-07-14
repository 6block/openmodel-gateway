# Backup & Restore Drill Runbook

If sp-gateway's money-critical state is lost or corrupted, the result is
**double-billing, missed debits, or reconciliation failure**. This runbook covers
what to back up, how to restore, and how to drill the restore regularly so it is
known to actually work.

## State that must be backed up (all under `DATA_DIR`, default `/data`)

| File | Consequence if lost |
|------|---------------------|
| `settlement-cursor.json` | cursor lost → re-scan from the beginning (double-billing) or skip ahead (missed debits) |
| `settled-total.json` | reconciliation baseline lost → drift detection becomes unreliable |
| `settlement-debt.json` | debt ledger lost → overdrawn usage can never be recovered (and debt-suspension state is lost) |
| `pending-settlement.json` | settlement WAL; if the crash happened mid-settlement → the replay basis is gone |
| `settlement-deadletter.jsonl` | unparsed billable records lost → SPs miss their share |
| `workers.json` | worker registry → workers must re-register after a restart before routing resumes |
| `requests.jsonl[.N]` | **the billing source of truth**; losing it = billing data for that window is gone for good |

## Backing up

`deploy/backup/backup-state.sh` packs the files above into a timestamped tar.gz
and prunes old backups by retention days.

```bash
# manual
DATA_DIR=/data ./deploy/backup/backup-state.sh /backups

# scheduled (every 15 min — keep it SHORTER than the settlement interval,
# so a restore can never lose a full settlement cycle)
*/15 * * * * DATA_DIR=/data /opt/openmodel/backup-state.sh /backups >> /var/log/openmodel-backup.log 2>&1
```

Frequency rule: **backup interval < settlement interval**. Otherwise one restore
may roll back an entire un-backed-up settlement cycle.

Off-host copy: sync `/backups` to another host or object storage
(`rsync` / `aws s3 sync`), so a single dead disk cannot take the primary data
and the backups with it.

## Restoring

```bash
# 1. stop the gateway (the script refuses to overwrite a running instance unless FORCE=1)
systemctl stop openmodel-gateway   # or: docker compose stop sp-gateway

# 2. restore (snapshots the current state to DATA_DIR/.pre-restore-<ts> first — reversible)
DATA_DIR=/data ./deploy/backup/restore-state.sh /backups/openmodel-state-YYYYmmdd-HHMMSS.tar.gz

# 3. start the gateway
systemctl start openmodel-gateway

# 4. post-restore self-check: state integrity + three-way reconciliation
curl -s -H "Authorization: Bearer $AGENT_ADMIN_TOKEN" localhost:9091/api/v1/state-check | jq
curl -s -H "Authorization: Bearer $AGENT_ADMIN_TOKEN" localhost:9091/api/v1/reconcile   | jq
```

Before installing anything, the restore script:
- refuses to overwrite a running instance (`FORCE=1` to override);
- snapshots the current state to `.pre-restore-<ts>` (the restore itself can be rolled back);
- validates that every JSON file parses, and aborts on corruption (a broken cursor is never installed).

## Reading the post-restore self-check

`GET /api/v1/state-check` (implemented by `Settler.VerifyState`):
- `ok: true` → cursor readable and within bounds, settled-total parses, the WAL (if present) parses.
- `ok: false` + `problems[...]` → **do not leave it running**. Common cases:
  `cursor offset N exceeds request-log size M` (the log was replaced but the
  cursor was not reset → debits would be skipped), `pending WAL unparseable`
  (corrupted WAL).

`GET /api/v1/reconcile`: `within_tolerance: true` means "billed = settled +
outstanding" — the books balance. Drift right after a restore means the restored
snapshot disagrees with on-chain state (e.g. a cursor older than the chain →
already-settled records get re-scanned; the on-chain `processedBatches` dedup
blocks any re-submission, so this is safe — pending just reads high for a cycle
until it settles back down).

## Regular drills (actually run them — backups alone prove nothing)

Monthly, in a **staging environment**:
1. `restore-state.sh` a production backup into the staging DATA_DIR;
2. start the gateway, run `state-check` and `reconcile`, confirm `ok=true` / `within_tolerance=true`;
3. send a few inference requests + one `settle-now`, confirm the cursor advances and settlement succeeds;
4. record the restore time (RTO) and the acceptable data-loss window (RPO = backup interval).

Automated coverage: `internal/settlement/statecheck_test.go` pins the core
invariants of backup → restore → self-check (cursor offset and settled-total
survive into a rebuilt settler; an out-of-bounds cursor and a corrupted WAL are
detected). It runs in CI, so the restore logic cannot silently regress.
