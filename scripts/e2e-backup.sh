#!/usr/bin/env bash
# Backup/restore check on a docker-in-docker "VM": deploy static-site, write v1, back up (restic, encrypted,
# off-host REST repo), overwrite with v2 plus an extra file, restore, expect exactly v1 again.
# Also checks tenant isolation: each service has its own append-only, private-repo login, and retention
# runs from the control plane. Needs api/worker/gateway running with the settings printed by
# scripts/backup-repo-user.sh (gateway SPADE_BACKUP_REPO=rest:http://172.31.250.1:8000, GATEWAY_HOSTS
# including 172.31.250.1); compose `backups` and `backups-admin` up.
set -euo pipefail
cd "$(dirname "$0")/.."
API=${API:-http://localhost:8080}
SECRET=${SPADE_INTERNAL_SECRET:?export the same SPADE_INTERNAL_SECRET used by spade-api}
INTERNAL=${INTERNAL:-http://127.0.0.1:8081}
GW_IN=${GW_IN:-https://172.31.250.1:8443}
V=spade-e2e-backup
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
sql() { docker compose -f deploy/compose/docker-compose.yml exec -T postgres psql -U spade -d spadeai -Atc "$1"; }
key() { openssl rand -hex 16; }
TOKEN=$(curl -sf -d client_id=spade-cli -d grant_type=password -d username=dev@spadeai.local -d password=dev-password -d scope=openid \
  http://localhost:8180/realms/spadeai/protocol/openid-connect/token | j "d['access_token']")
H=(-H "Authorization: Bearer $TOKEN")
topup() { local body="{\"org_id\":\"$1\",\"provider\":\"paystack\",\"provider_ref\":\"$2\",\"amount_minor\":$3}"
  curl -sf -H "X-Spade-Signature: $(printf %s "$body" | openssl dgst -sha256 -hmac "$SECRET" | cut -d' ' -f2)" -d "$body" "$INTERNAL/internal/payments/verified" >/dev/null; }
wait_op() { local s=""; for _ in $(seq 300); do s=$(curl -sf "${H[@]}" "$API/v1/operations/$1" | j "d['status']"); [ "$s" = succeeded ] && return; [ "$s" = failed ] && break; sleep 1; done
  curl -sf "${H[@]}" "$API/v1/operations/$1" >&2; echo >&2; docker exec "$V" tail -20 /var/log/agent.log >&2 || true; fail "operation $1 $s"; }
cleanup() { [ -n "${KEEP:-}" ] && return; docker rm -f "$V" >/dev/null 2>&1 || true; docker network rm spade-e2e >/dev/null 2>&1 || true; }
trap cleanup EXIT

CTX=$(mktemp -d)
cp rust/target/x86_64-unknown-linux-musl/release/spade-agent .spade/ca/ca.crt "$(command -v nebula)" "$(command -v nebula-cert)" "$CTX/"
docker build -q -t spade-e2e-vm -f deploy/e2e/vm.Dockerfile "$CTX" >/dev/null
docker network rm spade-e2e >/dev/null 2>&1 || true
docker network create --subnet 172.31.250.0/24 --gateway 172.31.250.1 spade-e2e >/dev/null
docker rm -f "$V" >/dev/null 2>&1 || true
docker run -d --privileged --name "$V" --network spade-e2e spade-e2e-vm >/dev/null
for _ in $(seq 60); do docker exec "$V" docker info >/dev/null 2>&1 && break; sleep 1; done

ORG=$(curl -sf "${H[@]}" -d '{"name":"Backup E2E","billing_mode":"prepaid"}' "$API/v1/orgs" | j "d['id']")
topup "$ORG" "bk-$(key)" 20000000
PRJ=$(curl -sf "${H[@]}" -d "{\"name\":\"bk-$RANDOM\",\"technical_email\":\"ops@e2e.ng\",\"overlay\":\"wireguard\"}" "$API/v1/orgs/$ORG/projects" | j "d['id']")
OP=$(curl -sf "${H[@]}" -H "Idempotency-Key: $(key)" -d '{"template":"static-site","display_name":"site","size":"byovm:SMALL-1C-2G"}' "$API/v1/projects/$PRJ/services" | j "d['id']")
BOOT=""; for _ in $(seq 30); do BOOT=$(curl -sf "${H[@]}" "$API/v1/operations/$OP" | j "(d.get('response') or {}).get('bootstrap','')"); [ -n "$BOOT" ] && break; sleep 1; done
ET=$(sed -E 's/.*SPADE_ENROL_TOKEN=([0-9a-f]+).*/\1/' <<<"$BOOT")
docker exec "$V" spade-agent enrol --gateway "$GW_IN" --ca /etc/spade/ca.pem --token "$ET" --state-dir /etc/spade >/dev/null
docker exec -d "$V" sh -c "spade-agent run --gateway $GW_IN --state-dir /etc/spade --stacks-dir /var/lib/spade/stacks >/var/log/agent.log 2>&1"
wait_op "$OP"
SVC=$(curl -sf "${H[@]}" "$API/v1/operations/$OP" | j "d['response']['service_id']")
C="spade-${SVC:0:8}-web-1"
page() { docker exec "$V" curl -s http://127.0.0.1:18081/; }
docker exec "$V" docker exec "$C" sh -c 'echo v1 > /usr/share/nginx/html/index.html'
[ "$(page)" = v1 ] || fail "could not write v1"

BOP=$(curl -sf "${H[@]}" -X POST -H "Idempotency-Key: $(key)" "$API/v1/services/$SVC/backups" | j "d['id']")
wait_op "$BOP"
BID=$(curl -sf "${H[@]}" "$API/v1/operations/$BOP" | j "d['response']['backup_id']")
SNAP=$(curl -sf "${H[@]}" "$API/v1/services/$SVC/backups" | j "[b for b in d['items'] if b['id']=='$BID'][0]['restic_snapshot_id']")
[ -n "$SNAP" ] || fail "backup has no restic snapshot id"
[ "$(page)" = v1 ] || fail "app not serving after quiesced backup"
echo "PASS backup: encrypted restic snapshot $SNAP stored off-host, app restarted"

U="s-$SVC"
HT=${SPADE_BACKUP_HTPASSWD:-.spade/backup-auth/htpasswd}
grep -q "^$U:\$2" "$HT" || fail "no bcrypt login for $U in $HT"
docker compose -f deploy/compose/docker-compose.yml exec -T backups test -f "/data/$U/config" || fail "repository not under /$U"
[ "$(sql "SELECT status FROM operation WHERE id='$BOP'")" = succeeded ] || fail "backup op not succeeded"
[ "$(sql "SELECT coalesce(response->>'prune_error','') FROM operation WHERE id='$BOP'")" = "" ] || fail "retention failed: $(sql "SELECT response->>'prune_error' FROM operation WHERE id='$BOP'")"
code() { docker exec "$V" curl -s -o /dev/null -w '%{http_code}' "$@"; }
[ "$(code http://172.31.250.1:8000/$U/config)" = 401 ] || fail "anonymous request to the tenant repository was not refused"
[ "$(code --max-time 3 http://172.31.250.1:8001/ || true)" = 000 ] || fail "retention endpoint reachable from a tenant host"
# A second tenant login (as another service would get) must not reach this service's repository, and a
# tenant login cannot delete snapshots even in its own repository.
OTHER="s-$(python3 -c 'import uuid;print(uuid.uuid4())')"; OPW=$(key)
docker run --rm httpd:2.4-alpine htpasswd -nbB "$OTHER" "$OPW" >> "$HT"
for _ in $(seq 40); do [ "$(code -u "$OTHER:$OPW" -X POST "http://172.31.250.1:8000/$OTHER/?create=true")" = 200 ] && break; sleep 1; done
[ "$(code -u "$OTHER:$OPW" -X POST "http://172.31.250.1:8000/$OTHER/?create=true")" = 200 ] || fail "test login never accepted"
[ "$(code -u "$OTHER:$OPW" "http://172.31.250.1:8000/$U/config")" = 401 ] || fail "another tenant's login can read $U"
[ "$(code -u "$OTHER:$OPW" -X DELETE "http://172.31.250.1:8000/$U/snapshots/$SNAP")" = 401 ] || fail "another tenant's login can delete in $U"
docker exec "$V" sh -c "printf x | curl -s -o /dev/null -u '$OTHER:$OPW' -X POST --data-binary @- http://172.31.250.1:8000/$OTHER/keys/0000000000000000000000000000000000000000000000000000000000000000" >/dev/null
[ "$(code -u "$OTHER:$OPW" -X DELETE "http://172.31.250.1:8000/$OTHER/keys/0000000000000000000000000000000000000000000000000000000000000000")" = 403 ] || fail "append-only: a tenant login could delete from its own repository"
sed -i "/^$OTHER:/d" "$HT"
echo "PASS isolation: per-service login, repository under /$U, other tenants refused, deletes refused (append-only), retention endpoint unreachable from hosts"

docker exec "$V" docker exec "$C" sh -c 'echo v2 > /usr/share/nginx/html/index.html; echo junk > /usr/share/nginx/html/extra.txt'
[ "$(page)" = v2 ] || fail "could not write v2"
ROP=$(curl -sf "${H[@]}" -X POST -H "Idempotency-Key: $(key)" "$API/v1/services/$SVC/backups/$BID/restore" | j "d['id']")
wait_op "$ROP"
for _ in $(seq 20); do [ "$(page)" = v1 ] && break; sleep 1; done
[ "$(page)" = v1 ] || fail "restore did not bring back v1 (got: $(page))"
docker exec "$V" curl -sf http://127.0.0.1:18081/extra.txt >/dev/null && fail "file created after the snapshot survived the restore"
[ "$(curl -sf "${H[@]}" "$API/v1/services/$SVC" | j "d['state']")" = running ] || fail "service not running after restore"
echo "PASS restore: data back to the snapshot exactly, service running"

SOP=$(curl -sf "${H[@]}" -X POST -H "Idempotency-Key: $(key)" "$API/v1/services/$SVC/actions/stop" | j "d['id']")
wait_op "$SOP"
B2=$(curl -sf "${H[@]}" -X POST -H "Idempotency-Key: $(key)" "$API/v1/services/$SVC/backups" | j "d['id']")
wait_op "$B2"
[ -z "$(docker exec "$V" docker compose -p "spade-${SVC:0:8}" ps -q --status running)" ] || fail "backup of a stopped service started its containers"
echo "PASS backup of a stopped service leaves it stopped"

# The repository password (if SPADE_BACKUP_REPO carries one) must not leak into payloads or results.
PW=$(sed -nE 's#.*https?://[^:/@]+:([^@]+)@.*#\1#p' <<<"${SPADE_BACKUP_REPO:-}")
if [ -n "$PW" ]; then
  [ "$(sql "SELECT count(*) FROM agent_command WHERE payload::text LIKE '%$PW%' OR coalesce(result::text,'') LIKE '%$PW%'")" = 0 ] \
    || fail "backup repository password leaked into agent_command"
fi
[ "$(sql "SELECT count(*) FROM agent_command WHERE verb LIKE 'backup_%' AND (payload ? 'restic' OR payload::text ILIKE '%password%')")" = 0 ] \
  || fail "backup credentials were persisted in the command queue"
echo "PASS backup credentials never stored in agent_command"

DOP=$(curl -sf "${H[@]}" -X DELETE -H "Idempotency-Key: $(key)" "$API/v1/services/$SVC" | j "d['id']")
wait_op "$DOP"
grep -q "^$U:" "$HT" && fail "backup login for deleted service $U still present"
echo "PASS deleting the service revokes its backup login"
echo "BACKUP E2E OK"
