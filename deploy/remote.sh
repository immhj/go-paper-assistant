#!/usr/bin/env bash
# Server-side helper invoked by scripts/release.ps1 over ssh. Run from the deploy directory
# (default /opt/paper-assistant), which holds .env, both compose files and deploy/.
#
#   remote.sh load <archive>        docker load an uploaded image tarball, then delete it
#   remote.sh activate <tag>        point IMAGE_TAG at <tag>, restart, wait for app healthy
#   remote.sh prune <keep> <tag>    keep the newest <keep> image tags, never removing <tag>
#   remote.sh status                show services and the active tag
set -euo pipefail
cd "$(dirname "$0")/.."

# shellcheck disable=SC2086 # word splitting of COMPOSE_ARGS is intended
compose() { docker compose ${COMPOSE_ARGS:--f docker-compose.yml -f docker-compose.prod.yml} "$@"; }

case "${1:-}" in
load)
	archive=${2:?archive path required}
	docker load -i "$archive"
	rm -f "$archive"
	;;
activate)
	tag=${2:?tag required}
	for repo in paper-assistant-app paper-assistant-paperscraper; do
		docker image inspect "$repo:$tag" >/dev/null 2>&1 || { echo "$repo:$tag is not loaded on this host" >&2; exit 1; }
	done
	if grep -q '^IMAGE_TAG=' .env; then
		sed -i "s/^IMAGE_TAG=.*/IMAGE_TAG=$tag/" .env
	else
		echo "IMAGE_TAG=$tag" >>.env
	fi
	compose up -d --remove-orphans
	for _ in $(seq 1 36); do
		status=$(docker inspect -f '{{.State.Health.Status}}' "$(compose ps -q app)" 2>/dev/null || echo starting)
		if [ "$status" = healthy ]; then
			echo "app healthy on $tag"
			exit 0
		fi
		sleep 5
	done
	echo "app did not become healthy on $tag; recent logs:" >&2
	compose logs --tail 80 app >&2
	exit 1
	;;
prune)
	keep=${2:?keep count required}
	current=${3:?current tag required}
	for repo in paper-assistant-app paper-assistant-paperscraper; do
		docker images "$repo" --format '{{.CreatedAt}}|{{.Tag}}' | sort -r | tail -n +"$((keep + 1))" | cut -d'|' -f2 |
			while read -r old; do
				if [ "$old" != "$current" ]; then docker rmi "$repo:$old" || true; fi
			done
	done
	;;
status)
	grep '^IMAGE_TAG=' .env || true
	compose ps
	;;
*)
	sed -n '2,9p' "$0"
	exit 2
	;;
esac
