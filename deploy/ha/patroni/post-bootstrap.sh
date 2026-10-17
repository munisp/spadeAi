#!/bin/sh
# Run once by Patroni on the node that initialises the cluster; $1 is a superuser connection string.
set -eu
for db in spadeai temporal temporal_visibility keycloak permify; do
  psql "$1" -v ON_ERROR_STOP=1 -c "CREATE DATABASE $db OWNER spade"
done
