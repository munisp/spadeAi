#!/bin/sh
# Primary: replication role, physical slot for the Abuja standby, and pg_hba access for it.
set -eu
psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v pw="$SPADE_REPLICATION_PASSWORD" <<'SQL'
CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD :'pw';
SELECT pg_create_physical_replication_slot('abuja');
-- Not on the command line: the entrypoint's temporary init server would then block on the standby.
ALTER SYSTEM SET synchronous_standby_names = 'ANY 1 (abuja)';
SQL
echo "host replication replicator all scram-sha-256" >> "$PGDATA/pg_hba.conf"
