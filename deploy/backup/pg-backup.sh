#!/usr/bin/env bash
# Nightly dump of the Xenos control-plane database.
#
# Losing this database is worse than losing a VM: it is the record of who owns
# what and who paid. The dump is verified before it counts, kept for
# RETENTION_DAYS, and mirrored off-host.
#
# Configuration (environment, usually /etc/xenos/backup.env):
#   DATABASE_URL     postgres connection URL of the database to dump (required)
#   BACKUP_DIR       local staging/retention directory   (default /var/backups/xenos)
#   RETENTION_DAYS   how many days of dumps to keep       (default 14)
#   REMOTE           rsync target for the off-host copy, e.g.
#                    u123456@u123456.your-storagebox.de:xenos-db/   (optional but expected in production)
#   RSYNC_SSH        ssh command for rsync, e.g. "ssh -p 23 -i /etc/xenos/storagebox_key"
#   HEALTHCHECK_URL  pinged on success so a missed run is noticed (optional; e.g. a healthchecks.io URL)
set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/xenos}"
RETENTION_DAYS="${RETENTION_DAYS:-14}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
file="$BACKUP_DIR/xenos-$stamp.dump"

log() { printf '%s pg-backup: %s\n' "$(date -u +%FT%TZ)" "$*"; }
die() { log "FAILED: $*"; exit 1; }

umask 077
mkdir -p "$BACKUP_DIR"

# Dump to a temp name so a half-written file is never mistaken for a backup.
log "dumping to $file"
pg_dump --format=custom --no-owner --dbname="$DATABASE_URL" --file="$file.partial" || { rm -f "$file.partial"; die "pg_dump failed"; }

# A dump we cannot read back is not a backup.
pg_restore --list "$file.partial" >/dev/null || { rm -f "$file.partial"; die "dump is unreadable"; }
mv "$file.partial" "$file"
(cd "$BACKUP_DIR" && sha256sum "$(basename "$file")" > "$(basename "$file").sha256")
log "ok: $(du -h "$file" | cut -f1)"

# Retention: delete dumps (and their checksums) older than RETENTION_DAYS, always keeping the newest.
find "$BACKUP_DIR" -maxdepth 1 -name 'xenos-*.dump*' -mtime "+$RETENTION_DAYS" -not -name "$(basename "$file")*" -delete

# Off-host copy. --delete mirrors the retention above, so the remote never grows without bound.
if [[ -n "${REMOTE:-}" ]]; then
  log "syncing to $REMOTE"
  rsync -a --delete ${RSYNC_SSH:+-e "$RSYNC_SSH"} "$BACKUP_DIR"/ "$REMOTE" || die "rsync to $REMOTE failed"
else
  log "WARNING: REMOTE is not set; this backup is only on this machine"
fi

if [[ -n "${HEALTHCHECK_URL:-}" ]]; then
  curl -fsS -m 10 --retry 3 "$HEALTHCHECK_URL" >/dev/null || log "WARNING: could not ping HEALTHCHECK_URL"
fi
log "done"
