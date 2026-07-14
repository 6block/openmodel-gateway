#!/usr/bin/env bash
# restore-state.sh — restore sp-gateway state from a backup-state.sh archive (B3).
#
# Usage:
#   DATA_DIR=/data ./restore-state.sh /backups/openmodel-state-YYYYmmdd-HHMMSS.tar.gz
#
# SAFETY: the gateway MUST be stopped before restoring (it holds the request log open
# and the settler writes the cursor/WAL). This script refuses to overwrite a live
# DATA_DIR unless FORCE=1. It backs up the current state to DATA_DIR/.pre-restore-<ts>
# before extracting, so a bad restore is itself reversible.
#
# After restoring, run a reconciliation to confirm the books are consistent:
#   curl -s -H "Authorization: Bearer $AGENT_ADMIN_TOKEN" localhost:9091/api/v1/reconcile | jq
set -euo pipefail

DATA_DIR="${DATA_DIR:-/data}"
ARCHIVE="${1:-}"

if [ -z "$ARCHIVE" ] || [ ! -f "$ARCHIVE" ]; then
  echo "usage: DATA_DIR=/data $0 <archive.tar.gz>" >&2
  exit 2
fi

# Refuse if the gateway looks like it is running (best-effort check).
if pgrep -f 'sp-state-agent|openmodel-sp-gateway' >/dev/null 2>&1 && [ "${FORCE:-0}" != "1" ]; then
  echo "ERROR: sp-gateway appears to be running. Stop it first, or re-run with FORCE=1." >&2
  exit 1
fi

mkdir -p "$DATA_DIR"
ts="$(date -u +%Y%m%d-%H%M%S)"
pre="${DATA_DIR}/.pre-restore-${ts}"
mkdir -p "$pre"

# Snapshot current state before clobbering it (reversibility).
for f in "$DATA_DIR"/settlement-cursor.json "$DATA_DIR"/settled-total.json \
         "$DATA_DIR"/settlement-debt.json "$DATA_DIR"/pending-settlement.json \
         "$DATA_DIR"/settlement-deadletter.jsonl "$DATA_DIR"/settlements.jsonl \
         "$DATA_DIR"/workers.json "$DATA_DIR"/requests.jsonl*; do
  [ -e "$f" ] && cp -a "$f" "$pre/" || true
done
echo "current state snapshotted to: $pre"

# Extract into a staging dir first, validate, then move into place.
staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT
tar -xzf "$ARCHIVE" -C "$staging"

if [ -f "$staging/BACKUP_MANIFEST.txt" ]; then
  echo "--- restoring from ---"; cat "$staging/BACKUP_MANIFEST.txt"; echo "----------------------"
fi

# Validate JSON state files parse before installing them (a corrupt cursor is worse
# than a missing one). Requires python3; skipped if unavailable.
if command -v python3 >/dev/null 2>&1; then
  for j in "$staging"/*.json; do
    [ -e "$j" ] || continue
    if ! python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$j" 2>/dev/null; then
      echo "ERROR: restored file is not valid JSON: $(basename "$j") — aborting" >&2
      exit 1
    fi
  done
fi

# Install (copy everything except the manifest).
for f in "$staging"/*; do
  base="$(basename "$f")"
  [ "$base" = "BACKUP_MANIFEST.txt" ] && continue
  cp -a "$f" "$DATA_DIR/"
done

echo "restore complete into $DATA_DIR"
echo "NEXT: start the gateway, then run /api/v1/reconcile and confirm within_tolerance=true."
