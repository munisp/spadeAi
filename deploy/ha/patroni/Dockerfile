# Postgres 16 + Patroni (PGDG Debian package) for the deploy/ha rehearsal.
FROM postgres:16
RUN apt-get update && apt-get install -y --no-install-recommends patroni python3-etcd python3-psycopg2 curl \
 && rm -rf /var/lib/apt/lists/*
USER postgres
ENTRYPOINT ["/usr/bin/patroni", "/etc/patroni.yml"]
