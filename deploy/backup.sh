#!/usr/bin/env bash
# Nightly backup: consistent MySQL dump + private/shared full-text volume, kept locally for
# $KEEP_DAYS days and optionally uploaded to Aliyun OSS.
#
# Install on the server (as the deploy user, which must be in the docker group):
#   crontab -e
#   30 3 * * * /opt/paper-assistant/deploy/backup.sh >> /opt/paper-assistant/backups/backup.log 2>&1
#
# OSS upload is enabled when OSS_BUCKET is set in .env and `ossutil` is configured
# (https://help.aliyun.com/zh/oss/developer-reference/ossutil).
set -euo pipefail
cd "$(dirname "$0")/.."

KEEP_DAYS=${KEEP_DAYS:-7}
# COMPOSE_ARGS lets the script be exercised against the dev stack: COMPOSE_ARGS='-f docker-compose.yml'
# shellcheck disable=SC2086 # word splitting of COMPOSE_ARGS is intended
compose() { docker compose ${COMPOSE_ARGS:--f docker-compose.yml -f docker-compose.prod.yml} "$@"; }
OSS_BUCKET=$(grep -E '^OSS_BUCKET=' .env | cut -d= -f2- || true)

stamp=$(date +%Y%m%d-%H%M%S)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p backups

# --single-transaction gives a consistent InnoDB snapshot without locking writers. The password
# is read inside the container from its own environment, never placed on this command line.
compose exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot \
	--single-transaction --routines --triggers --set-gtid-purged=OFF --databases "$MYSQL_DATABASE"' \
	| gzip >"$work/mysql.sql.gz"
gzip -t "$work/mysql.sql.gz"

# Full-text files live in the app's volume; stream them out through the running container.
compose exec -T app tar -C /app/data -czf - fulltext >"$work/fulltext.tar.gz"

archive="backups/paper-assistant-$stamp.tar"
tar -C "$work" -cf "$archive" mysql.sql.gz fulltext.tar.gz
chmod 600 "$archive"
echo "$(date -Is) backup ok: $archive ($(du -h "$archive" | cut -f1))"

if [ -n "$OSS_BUCKET" ]; then
	ossutil cp "$archive" "oss://$OSS_BUCKET/paper-assistant/$(basename "$archive")"
	echo "$(date -Is) uploaded to oss://$OSS_BUCKET/paper-assistant/"
fi

find backups -name 'paper-assistant-*.tar' -mtime +"$KEEP_DAYS" -delete
