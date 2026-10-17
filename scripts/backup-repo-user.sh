#!/usr/bin/env bash
# Creates (or rotates) the admin login on the retention endpoint (compose `backups-admin`, loopback only)
# and prints the backup settings to export. Per-service logins on the tenant server are created by the
# worker itself. The password is only printed to your terminal; it is never written to the repo.
set -euo pipefail
cd "$(dirname "$0")/.."
user=${BACKUP_ADMIN_USER:-spade-admin}
[[ $user =~ ^[A-Za-z0-9_.-]{1,64}$ ]] || { echo "BACKUP_ADMIN_USER must match [A-Za-z0-9_.-]{1,64}" >&2; exit 1; }
mkdir -p .spade/backup-auth .spade/backup-admin
touch .spade/backup-auth/htpasswd
pw=$(openssl rand -hex 24)
compose="docker compose -f deploy/compose/docker-compose.yml"
$compose up -d backups backups-admin >/dev/null
$compose exec -T backups-admin sh -c 'delete_user "$1" >/dev/null 2>&1 || true; create_user "$1" "$2"' _ "$user" "$pw" >/dev/null
echo "local dev only: the tenant server is plain HTTP, so gateway and worker also need SPADE_BACKUP_ALLOW_PLAINTEXT=1 (production: rest:https://)" >&2
echo "gateway (as seen from hosts): SPADE_BACKUP_REPO=rest:http://${BACKUP_REPO_HOST:-172.31.250.1}:8000"
echo "worker + api:                 SPADE_BACKUP_REPO=rest:http://127.0.0.1:8000"
echo "worker:                       SPADE_BACKUP_HTPASSWD=$PWD/.spade/backup-auth/htpasswd"
echo "worker + api:                 SPADE_BACKUP_ADMIN_REPO=rest:http://$user:$pw@127.0.0.1:8001"
if [ "${BACKUP_OFFSITE:-0}" = 1 ]; then
  # Second-site copy target from deploy/ha (compose `backups-abuja`); same admin login, worker only.
  mkdir -p .spade/backup-abuja && touch .spade/backup-abuja/htpasswd
  ha="docker compose -f deploy/ha/docker-compose.ha.yml"
  $ha up -d backups-abuja >/dev/null
  $ha exec -T backups-abuja sh -c 'delete_user "$1" >/dev/null 2>&1 || true; create_user "$1" "$2"' _ "$user" "$pw" >/dev/null
  echo "worker + api:                 SPADE_BACKUP_REPLICA_REPO=rest:http://$user:$pw@127.0.0.1:8002"
fi
