#!/usr/bin/env bash
# Prove a database backup restores. Run it before launch and then regularly
# (a monthly timer is sensible): an untested backup is a hope, not a backup.
#
# Restores the newest dump (or the file given as $1) into a scratch database on
# the server at ADMIN_URL, checks the schema and data are present, then drops it.
#
#   ADMIN_URL   connection URL of a server where we may create a database (required)
#   BACKUP_DIR  where dumps live (default /var/backups/xenos)
#   LIVE_URL    optional; if set, row counts are compared with the live database
set -euo pipefail

: "${ADMIN_URL:?ADMIN_URL is required}"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/xenos}"
dump="${1:-$(ls -1t "$BACKUP_DIR"/xenos-*.dump 2>/dev/null | head -n1 || true)}"
[[ -n "$dump" && -f "$dump" ]] || { echo "restore-test: no dump found in $BACKUP_DIR" >&2; exit 1; }

if [[ -f "$dump.sha256" ]]; then
  (cd "$(dirname "$dump")" && sha256sum --check --quiet "$(basename "$dump").sha256") \
    || { echo "restore-test: checksum mismatch for $dump" >&2; exit 1; }
fi

scratch="xenos_restore_test_$(date -u +%Y%m%d%H%M%S)"
# Swap the database name in the URL, keeping host, credentials and query string.
scratch_url="$(printf '%s' "$ADMIN_URL" | sed -E "s#(://[^/]+/)[^?]*#\1$scratch#")"
cleanup() { psql "$ADMIN_URL" -qAt -c "DROP DATABASE IF EXISTS \"$scratch\"" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "restore-test: restoring $(basename "$dump") into $scratch"
psql "$ADMIN_URL" -qAt -c "CREATE DATABASE \"$scratch\"" >/dev/null
pg_restore --no-owner --exit-on-error --dbname="$scratch_url" "$dump"

q() { psql "$1" -qAt -c "$2"; }
fail=0
for t in users sessions ssh_keys plans templates vms ip_addresses jobs usage_charges conversions goose_db_version; do
  if [[ "$(q "$scratch_url" "SELECT to_regclass('public.$t') IS NOT NULL")" != "t" ]]; then
    echo "restore-test: MISSING table $t" >&2; fail=1
  fi
done
(( fail == 0 )) || { echo "restore-test: FAILED, schema incomplete" >&2; exit 1; }

printf 'restore-test: %-16s %10s %10s\n' table restored live
for t in users vms usage_charges conversions; do
  restored="$(q "$scratch_url" "SELECT count(*) FROM $t")"
  live="-"
  [[ -n "${LIVE_URL:-}" ]] && live="$(q "$LIVE_URL" "SELECT count(*) FROM $t")"
  printf 'restore-test: %-16s %10s %10s\n' "$t" "$restored" "$live"
done
echo "restore-test: OK (migration version $(q "$scratch_url" "SELECT max(version_id) FROM goose_db_version"))"
