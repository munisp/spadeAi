#!/usr/bin/env bash
# Site-loss drill for deploy/ha: write continuously to Postgres and TigerBeetle, lose the whole Lagos site
# (Postgres primary + its TigerBeetle replica), fail over to Abuja, and prove that no acknowledged write
# was lost and that writes resumed. Then bring Lagos back as a standby and check it replicates again.
set -euo pipefail
cd "$(dirname "$0")/.."
DC=(docker compose -f deploy/ha/docker-compose.ha.yml)
psql_at() { "${DC[@]}" exec -T "$1" psql -U spade -d spadeai -Atc "$2"; }
fail() { echo "FAIL $*" >&2; exit 1; }

mkdir -p .spade/backup-abuja && touch .spade/backup-abuja/htpasswd
"${DC[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
"${DC[@]}" up -d >/dev/null 2>&1
for _ in $(seq 60); do
  [ "$(psql_at pg-lagos "select sync_state from pg_stat_replication where application_name='abuja'" 2>/dev/null)" = quorum ] && break; sleep 2
done
[ "$(psql_at pg-lagos "select sync_state from pg_stat_replication where application_name='abuja'")" = quorum ] || fail "abuja is not a synchronous standby"
echo "PASS setup: pg-abuja streams synchronously from pg-lagos; tigerbeetle 3 replicas (lagos, abuja, witness)"

go build -o bin/spade-hadrill ./cmd/spade-hadrill
export SPADE_DATABASE_URL='postgres://spade:spade-local-dev@127.0.0.1:5441,127.0.0.1:5442/spadeai?sslmode=disable&target_session_attrs=read-write&connect_timeout=2'
export TB_ADDRESSES=3011,3012,3013
out=$(mktemp -d)/drill.json
bin/spade-hadrill run -for 45s -out "$out" > "$out.log" 2>&1 &
drill=$!
sleep 10
echo "losing the Lagos site (pg-lagos, tb-lagos)"
docker kill "$("${DC[@]}" ps -q tb-lagos)" >/dev/null
scripts/ha-failover.sh
wait "$drill" || { cat "$out.log"; fail "drill"; }
cat "$out.log"
grep -q 'acknowledged:' "$out.log" || fail "drill produced no result"
bin/spade-hadrill verify -in "$out" || fail "acknowledged writes lost"

# Writes must have resumed after the failover, not just before it.
recent=$(psql_at pg-abuja "select count(*) from ha_drill where at > now() - interval '20 seconds'")
[ "$recent" -gt 0 ] || fail "no postgres writes after failover"
echo "PASS failover: Lagos lost, Abuja promoted, $recent postgres writes in the last 20s, ledger writable on 2 of 3 replicas"

"${DC[@]}" start tb-lagos >/dev/null
scripts/ha-rejoin.sh
psql_at pg-abuja "insert into ha_drill (id) values (-1)" >/dev/null
for _ in $(seq 20); do [ "$(psql_at pg-lagos "select count(*) from ha_drill where id=-1")" = 1 ] && break; sleep 1; done
[ "$(psql_at pg-lagos "select count(*) from ha_drill where id=-1")" = 1 ] || fail "lagos does not replicate from abuja"
[ "$(psql_at pg-lagos 'select pg_is_in_recovery()')" = t ] || fail "lagos came back as a primary (split brain)"
echo "PASS rejoin: Lagos rebuilt as a synchronous standby of Abuja, tigerbeetle replica restarted"
echo "HA E2E OK"
