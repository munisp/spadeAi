#!/usr/bin/env bash
# Creates (or rotates) the restic REST server user and prints the SPADE_BACKUP_REPO values to export.
# The password is generated here and only printed to your terminal; it is never written to the repo.
set -euo pipefail
cd "$(dirname "$0")/.."
user=${BACKUP_REPO_USER:-spade}
[[ $user =~ ^[A-Za-z0-9_.-]{1,64}$ ]] || { echo "BACKUP_REPO_USER must match [A-Za-z0-9_.-]{1,64}" >&2; exit 1; }
pw=$(openssl rand -hex 24)
compose="docker compose -f deploy/compose/docker-compose.yml"
$compose exec -T backups sh -c 'delete_user "$1" >/dev/null 2>&1 || true; create_user "$1" "$2"' _ "$user" "$pw" >/dev/null
echo "local dev only: this repo is plain HTTP, so the gateway also needs SPADE_BACKUP_ALLOW_PLAINTEXT=1 (production: rest:https://)" >&2
echo "gateway (as seen from hosts): SPADE_BACKUP_REPO=rest:http://$user:$pw@${BACKUP_REPO_HOST:-172.31.250.1}:8000"
echo "api (capability probe):       SPADE_BACKUP_REPO=rest:http://$user:$pw@127.0.0.1:8000"
