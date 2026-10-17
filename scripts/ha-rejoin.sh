#!/usr/bin/env bash
# After scripts/ha-failover.sh: rebuild the fenced Lagos Postgres as a synchronous standby of Abuja.
# Its old data is discarded (it may hold writes Abuja never saw), then cloned fresh from Abuja.
set -euo pipefail
cd "$(dirname "$0")/.."
DC=(docker compose -f deploy/ha/docker-compose.ha.yml)
psql_abuja() { "${DC[@]}" exec -T pg-abuja psql -U spade -d spadeai -Atc "$1"; }
[ "$(psql_abuja 'select pg_is_in_recovery()')" = f ] || { echo "pg-abuja is not primary; run ha-failover.sh first" >&2; exit 1; }
"${DC[@]}" stop -t 5 pg-lagos >/dev/null
psql_abuja "select pg_drop_replication_slot('lagos') from pg_replication_slots where slot_name='lagos'" >/dev/null
docker run --rm --network spade-ha_ha -v spade-ha_pg-lagos:/var/lib/postgresql/data \
  -e PGPASSWORD="${SPADE_REPLICATION_PASSWORD:-replication-local-dev}" postgres:16-alpine sh -c '
  rm -rf /var/lib/postgresql/data/* &&
  su-exec postgres pg_basebackup -d "host=pg-abuja user=replicator application_name=lagos" \
    -D /var/lib/postgresql/data -R -X stream -C -S lagos'
"${DC[@]}" start pg-lagos >/dev/null
for _ in $(seq 60); do
  st=$(psql_abuja "select state from pg_stat_replication where application_name='lagos'")
  [ "$st" = streaming ] && break; sleep 1
done
[ "${st:-}" = streaming ] || { echo "FAIL: pg-lagos did not start streaming from pg-abuja" >&2; exit 1; }
psql_abuja "alter system set synchronous_standby_names = 'ANY 1 (lagos)'" >/dev/null
psql_abuja 'select pg_reload_conf()' >/dev/null
echo "pg-lagos is a synchronous standby of pg-abuja"
