#!/usr/bin/env bash
# The same operand checks run in a native image build and against a loaded
# development image. This script starts only its own disposable local database.
set -euo pipefail

cleanup() {
    local result=$?
    if [[ "$result" != 0 ]]; then
        tail -n 80 "$smoke_dir/init.log" "$smoke_dir/server.log" 2>/dev/null || true
    fi
    if [[ -f "$smoke_dir/data/postmaster.pid" ]]; then
        pg_ctl -D "$smoke_dir/data" -m immediate -w -t 30 stop >/dev/null 2>&1 || result=1
    fi
    rm -rf -- "$smoke_dir"
    exit "$result"
}
run() { psql -X -h "$smoke_dir" -U postgres -d postgres -v ON_ERROR_STOP=1 -q "$@"; }
val() { psql -X -h "$smoke_dir" -U postgres -d postgres -v ON_ERROR_STOP=1 -tAc "$1"; }
pu() { psql -X -h "$smoke_dir" -U postgres -d upgrade_probe -v ON_ERROR_STOP=1 -q "$@"; }
pv() { psql -X -h "$smoke_dir" -U postgres -d upgrade_probe -v ON_ERROR_STOP=1 -tAc "$1"; }

function main() {
    PG_MAJOR="${1:?PostgreSQL major is required}"
    TS_CURRENT="${2:?current TimescaleDB version is required}"
    TS_PREVIOUS="${3:?previous TimescaleDB version is required}"
    [[ "$PG_MAJOR" =~ ^[1-9][0-9]*$ ]]
    [[ "$TS_CURRENT" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
    [[ "$TS_PREVIOUS" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
    [[ "$TS_CURRENT" != "$TS_PREVIOUS" ]]
    [[ "$(id -u)" == 26 ]]

    test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb.so"
    test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TS_CURRENT}.so"
    test -f "/usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TS_PREVIOUS}.so"
    test -f "/usr/share/postgresql/${PG_MAJOR}/extension/vector.control"

    export PATH="/usr/lib/postgresql/${PG_MAJOR}/bin:$PATH"
    smoke_dir="$(mktemp -d /tmp/memql-db-smoke.XXXXXXXX)"
    trap cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    initdb -D "$smoke_dir/data" -U postgres >"$smoke_dir/init.log" 2>&1
    printf "shared_preload_libraries = 'timescaledb'\n" >> "$smoke_dir/data/postgresql.conf"
    pg_ctl -D "$smoke_dir/data" -l "$smoke_dir/server.log" \
        -o "-k $smoke_dir -c listen_addresses=" -w -t 60 start >/dev/null

    run -c 'CREATE EXTENSION timescaledb;' -c 'CREATE EXTENSION vector;'
    [[ "$(val 'SHOW timescaledb.license')" == timescale ]]
    [[ "$(val "SELECT extversion FROM pg_extension WHERE extname='timescaledb'")" == "$TS_CURRENT" ]]
    run <<'SQL'
CREATE TABLE probe(ts timestamptz NOT NULL, v double precision);
SELECT create_hypertable('probe', 'ts');
CREATE MATERIALIZED VIEW probe_1m WITH (timescaledb.continuous) AS
  SELECT time_bucket('1 minute', ts) AS bucket, avg(v) FROM probe GROUP BY 1 WITH NO DATA;
ALTER TABLE probe SET (timescaledb.compress);
SELECT add_retention_policy('probe', INTERVAL '7 days');
SQL

    run -c 'CREATE DATABASE upgrade_probe;'
    pu -c "CREATE EXTENSION timescaledb VERSION '${TS_PREVIOUS}';"
    [[ "$(pv "SELECT extversion FROM pg_extension WHERE extname='timescaledb'")" == "$TS_PREVIOUS" ]]
    pu -c 'CREATE TABLE t(ts timestamptz NOT NULL, v int);' \
        -c "SELECT create_hypertable('t','ts');" >/dev/null
    pu -c "ALTER EXTENSION timescaledb UPDATE TO '${TS_CURRENT}';"
    [[ "$(pv "SELECT extversion FROM pg_extension WHERE extname='timescaledb'")" == "$TS_CURRENT" ]]
    pv 'SELECT count(*) FROM t' >/dev/null
    printf 'SUCCESS: PostgreSQL %s, TimescaleDB Community %s -> %s and pgvector smoke checks passed\n' \
        "$PG_MAJOR" "$TS_PREVIOUS" "$TS_CURRENT"
}

main "$@"
