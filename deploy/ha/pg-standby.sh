#!/bin/sh
# Standby: clone the primary once (pg_basebackup -R writes primary_conninfo + standby.signal), then run
# as a hot standby. After a promotion this node is a primary and simply keeps running as one.
set -eu
PGDATA=${PGDATA:-/var/lib/postgresql/data}
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  mkdir -p "$PGDATA" && chown postgres:postgres "$PGDATA" && chmod 700 "$PGDATA"
  until PGPASSWORD="$SPADE_REPLICATION_PASSWORD" su-exec postgres pg_basebackup -d "host=$PRIMARY_HOST user=replicator application_name=abuja" -D "$PGDATA" -R -X stream -S abuja; do
    echo "waiting for primary"; sleep 2; rm -rf "$PGDATA"/*
  done
fi
exec su-exec postgres postgres -c hot_standby=on -c wal_level=replica -c max_wal_senders=10 -c max_replication_slots=10
