#!/usr/bin/env bash
# Promote the Abuja Postgres standby after losing Lagos (deploy/ha). Fencing first: the old primary is
# stopped and must not be restarted as a primary; rebuild it as a standby of Abuja (scripts/ha-rejoin.sh).
#   scripts/ha-failover.sh            # Lagos is unreachable or being abandoned
set -euo pipefail
cd "$(dirname "$0")/.."
DC=(docker compose -f deploy/ha/docker-compose.ha.yml)
psql_abuja() { "${DC[@]}" exec -T pg-abuja psql -U spade -d spadeai -Atc "$1"; }

echo "fencing pg-lagos"
"${DC[@]}" stop -t 5 pg-lagos >/dev/null

[ "$(psql_abuja 'select pg_is_in_recovery()')" = t ] || { echo "pg-abuja is already a primary"; exit 0; }
echo "promoting pg-abuja"
psql_abuja 'select pg_promote(true, 60)' >/dev/null
[ "$(psql_abuja 'select pg_is_in_recovery()')" = f ] || { echo "FAIL: pg-abuja did not promote" >&2; exit 1; }
# With Lagos gone there is no synchronous standby; keep accepting writes (asynchronous) until it rejoins.
psql_abuja "alter system set synchronous_standby_names = ''" >/dev/null
psql_abuja 'select pg_reload_conf()' >/dev/null
echo "pg-abuja is primary (timeline $(psql_abuja 'select timeline_id from pg_control_checkpoint()'))"
