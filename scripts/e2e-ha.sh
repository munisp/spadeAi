#!/usr/bin/env bash
# Site-loss drill for deploy/ha, with no operator step: write continuously to Postgres and TigerBeetle
# and run a Temporal workflow whose timer spans the outage, then kill the whole Lagos site (Postgres
# leader, etcd member, TigerBeetle replica, HAProxy). Patroni must promote Abuja by itself, and:
#   - no acknowledged Postgres row or TigerBeetle transfer is lost, and writes resume;
#   - the Temporal workflow completes, and Keycloak logins and Permify writes/checks work afterwards
#     (all three reach Postgres only through the floating HAProxy address);
#   - Lagos returns as a replica of Abuja (rewound, not a second primary).
# Then a partition drill: cut the current leader off from the network and check it demotes itself.
set -euo pipefail
cd "$(dirname "$0")/.."
DC=(docker compose -f deploy/ha/docker-compose.ha.yml)
psql_at() { "${DC[@]}" exec -T "$1" psql -U spade -d spadeai -Atc "$2"; }
role() { curl -s -o /dev/null -w '%{http_code}' "http://$1:8008/primary"; }
leader() { "${DC[@]}" exec -T "$1" curl -sf http://127.0.0.1:8008/cluster | python3 -c 'import json,sys; print(next((m["name"] for m in json.load(sys.stdin)["members"] if m["role"]=="leader"),""))'; }
sync_of() { "${DC[@]}" exec -T "$1" curl -sf http://127.0.0.1:8008/cluster | python3 -c 'import json,sys; print(",".join(m["name"] for m in json.load(sys.stdin)["members"] if m["role"]=="sync_standby"))'; }
wait_for() { local n=$1; shift; for _ in $(seq "$n"); do "$@" >/dev/null 2>&1 && return 0; sleep 2; done; "$@"; }
fail() { echo "FAIL $*" >&2; exit 1; }
export PGPASSWORD=spade-local-dev
KC=http://localhost:8190 PERMIFY=http://localhost:3486/v1/tenants/t1
kc_login() { curl -sf -d client_id=spade-cli -d grant_type=password -d username=dev@spadeai.local -d password=dev-password \
  -d scope=openid "$KC/realms/spadeai/protocol/openid-connect/token" | grep -q access_token; }
permify_rw() { # write a tuple for doc $1 and check it is visible
  curl -sf -XPOST "$PERMIFY/data/write" -d '{"metadata":{"schema_version":""},"tuples":[{"entity":{"type":"doc","id":"'"$1"'"},"relation":"owner","subject":{"type":"user","id":"u1"}}]}' >/dev/null &&
  curl -sf -XPOST "$PERMIFY/permissions/check" -d '{"metadata":{"schema_version":"","snap_token":"","depth":20},"entity":{"type":"doc","id":"'"$1"'"},"permission":"view","subject":{"type":"user","id":"u1"}}' | grep -q CHECK_RESULT_ALLOWED; }

mkdir -p .spade/backup-abuja && touch .spade/backup-abuja/htpasswd
"${DC[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
"${DC[@]}" up -d --build >/dev/null 2>&1
is_sync() { [ "$(sync_of pg-lagos)" = abuja ] && [ "$(leader pg-lagos)" = lagos ]; }
wait_for 90 is_sync || fail "patroni: lagos is not leader with abuja as synchronous standby"
wait_for 90 kc_login || fail "keycloak login before failover"
wait_for 60 curl -sf -XPOST "$PERMIFY/schemas/write" -d '{"schema":"entity user {}\nentity doc {\n relation owner @user\n permission view = owner\n}"}' || fail "permify schema"
wait_for 30 permify_rw before || fail "permify before failover"
echo "PASS setup: patroni leader lagos, synchronous standby abuja, etcd 3 sites, tigerbeetle 3 replicas; keycloak and permify on the floating address"

go build -o bin/spade-hadrill ./cmd/spade-hadrill
export SPADE_DATABASE_URL='postgres://spade:spade-local-dev@127.0.0.1:5452/spadeai?sslmode=disable&connect_timeout=2'
export TB_ADDRESSES=3011,3012,3013
out=$(mktemp -d)/drill.json
bin/spade-hadrill run -for 60s -temporal 127.0.0.1:7243 -out "$out" > "$out.log" 2>&1 &
drill=$!
sleep 10
echo "losing the whole Lagos site (pg-lagos, etcd-lagos, tb-lagos, haproxy-lagos); no operator action"
t0=$(date +%s)
for s in pg-lagos etcd-lagos tb-lagos haproxy-lagos; do docker kill "$("${DC[@]}" ps -q "$s")" >/dev/null; done
wait_for 60 bash -c '[ "$(curl -s -o /dev/null -w %{http_code} http://172.31.240.11:8008/primary)" = 200 ]' >/dev/null || fail "abuja was not promoted automatically"
echo "abuja promoted by patroni $(( $(date +%s) - t0 ))s after losing lagos"
wait "$drill" || { cat "$out.log"; fail "drill"; }
grep -v message_bus "$out.log"
bin/spade-hadrill verify -in "$out" 2> >(grep -v message_bus >&2) || fail "acknowledged writes lost or workflow not completed"
recent=$(psql_at pg-abuja "select count(*) from ha_drill where at > now() - interval '20 seconds'")
[ "$recent" -gt 0 ] || fail "no postgres writes after failover"
wait_for 60 kc_login || fail "keycloak login after failover"
wait_for 30 permify_rw after || fail "permify write/check after failover"
echo "PASS failover: automatic, $recent postgres writes in the last 20s, ledger on 2 of 3 replicas, temporal workflow completed, keycloak login and permify write ok"

"${DC[@]}" start etcd-lagos pg-lagos tb-lagos haproxy-lagos >/dev/null 2>&1
lagos_replica() { [ "$(sync_of pg-abuja)" = lagos ] && [ "$(psql_at pg-lagos 'select pg_is_in_recovery()')" = t ]; }
wait_for 90 lagos_replica || fail "lagos did not rejoin as abuja's synchronous standby"
psql_at pg-abuja "insert into ha_drill (id) values (-1)" >/dev/null
wait_for 15 bash -c "[ \"\$(PGPASSWORD=$PGPASSWORD psql -h 127.0.0.1 -p 5441 -U spade -d spadeai -Atc 'select count(*) from ha_drill where id=-1')\" = 1 ]" || fail "lagos does not replicate from abuja"
echo "PASS rejoin: old Lagos primary rewound and following Abuja as synchronous standby (no split brain)"

# Partition: cut the leader (Abuja) off. It loses etcd, so it must demote itself; Lagos is promoted.
docker network disconnect spade-ha_ha "$("${DC[@]}" ps -q pg-abuja)"
docker network disconnect spade-ha_ha "$("${DC[@]}" ps -q etcd-abuja)"
demoted() { [ "$("${DC[@]}" exec -T pg-abuja psql -U spade -d spadeai -Atc 'select pg_is_in_recovery()')" = t ]; }
wait_for 45 demoted || fail "isolated abuja kept acting as primary (split brain)"
wait_for 45 bash -c '[ "$(curl -s -o /dev/null -w %{http_code} http://172.31.240.10:8008/primary)" = 200 ]' >/dev/null || fail "lagos not promoted after partition"
docker network connect --ip 172.31.240.41 spade-ha_ha "$("${DC[@]}" ps -q etcd-abuja)"
docker network connect --ip 172.31.240.11 spade-ha_ha "$("${DC[@]}" ps -q pg-abuja)"
abuja_replica() { [ "$(sync_of pg-lagos)" = abuja ]; }
wait_for 90 abuja_replica || fail "abuja did not rejoin after the partition healed"
echo "PASS partition: isolated leader demoted itself, majority side promoted, healed node rejoined as standby"
echo "HA E2E OK"
