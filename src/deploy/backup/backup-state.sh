#!/usr/bin/env bash
# backup-state.sh — back up the sp-gateway's fund-critical state (B3).
#
# What it backs up (everything that, if lost, causes lost/duplicate billing or a
# wrong fleet view):
#   - settlement-cursor.json   request-log read position (lost → re-bill or skip)
#   - settled-total.json       cumulative settled USD (reconciliation baseline)
#   - settlement-debt.json     carried under-funded debt ledger
#   - pending-settlement.json  in-flight settlement WAL (if a cycle was mid-flight)
#   - settlement-deadletter.jsonl  unresolved billable records
#   - settlement-items.jsonl   per-item settlement ledger (what each batch paid)
#   - merkle-batches.jsonl, receipt-records.jsonl, receipt-index.jsonl
#                              billing proofs: without them receipt-proof cannot answer
#   - reconcile-cursor.json, reconcile-state.json  reconciliation position and totals
#   - registrations.json       self-registered API keys (hashed) and their wallets
#   - workers.json             worker registry (fleet membership)
#   - requests.jsonl[.N]       the billing source log + retained rotations
#
# Usage:
#   DATA_DIR=/data REQUEST_LOG=/data/requests.jsonl ./backup-state.sh /backups
#
# Produces /backups/openmodel-state-YYYYmmdd-HHMMSS.tar.gz and prunes backups older
# than BACKUP_RETENTION_DAYS (default 14). Run from cron, e.g. every 15 min:
#   */15 * * * * DATA_DIR=/data /opt/openmodel/backup-state.sh /backups >> /var/log/openmodel-backup.log 2>&1
# Run it as root (or the owner of DATA_DIR): some state files are mode 0600.
set -euo pipefail

DATA_DIR="${DATA_DIR:-/data}"
REQUEST_LOG="${REQUEST_LOG:-${DATA_DIR}/requests.jsonl}"
DEST="${1:-${BACKUP_DEST:-/backups}}"
BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}"

ts="$(date -u +%Y%m%d-%H%M%S)"
mkdir -p "$DEST"
staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT

# Copy state files that exist. cp -a preserves mtimes. Missing files are skipped
# (e.g. no WAL when settlement is idle — that is normal); a file that exists but
# cannot be read is an ERROR, never a silent skip — the gateway writes
# registrations.json mode 0600 as its own user (root in the container), so a
# backup run as another user would otherwise lose every self-registered key.
copy_if() {
  [ -e "$1" ] || return 0
  if ! cp -a "$1" "$staging/"; then
    echo "ERROR: cannot read $1 — run the backup as the user that owns DATA_DIR (e.g. root / sudo)" >&2
    exit 1
  fi
}

for f in settlement-cursor.json settled-total.json settlement-debt.json \
         pending-settlement.json settlement-deadletter.jsonl settlements.jsonl \
         settlement-items.jsonl merkle-batches.jsonl receipt-records.jsonl \
         receipt-index.jsonl reconcile-cursor.json reconcile-state.json \
         registrations.json workers.json; do
  copy_if "${DATA_DIR}/${f}"
done

# Request log + all numbered rotations (the billing source of truth).
copy_if "$REQUEST_LOG"
for bk in "${REQUEST_LOG}".*; do
  copy_if "$bk"
done

# Record provenance so a restore knows where it came from.
cat > "$staging/BACKUP_MANIFEST.txt" <<EOF
created_utc=$ts
data_dir=$DATA_DIR
request_log=$REQUEST_LOG
host=$(hostname)
files=$(cd "$staging" && ls -1 | tr '\n' ' ')
EOF

archive="${DEST}/openmodel-state-${ts}.tar.gz"
tar -czf "$archive" -C "$staging" .
echo "backup written: $archive ($(du -h "$archive" | cut -f1))"

# Prune old backups.
find "$DEST" -name 'openmodel-state-*.tar.gz' -type f -mtime "+${BACKUP_RETENTION_DAYS}" -print -delete || true
