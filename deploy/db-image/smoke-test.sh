#!/usr/bin/env bash
#
# Smoke test for the MemQL database operand image (epic memql#3842, task
# memql#3844).
#
# Run against a BUILT image, before it is pushed anywhere. One script so the
# release lane (.github/workflows/build-db-image.yml) and the local dev build
# (scripts/db-image/build.sh) assert exactly the same things -- two copies would
# drift, and the copy that drifts is the one that stops catching anything.
#
# WHAT IT ASSERTS, AND WHY EACH ONE EARNS ITS PLACE
#
#   1. Both extensions create.               The image's entire purpose.
#   2. `SHOW timescaledb.license` = timescale
#                                            The Community/TSL build is what
#                                            carries continuous aggregates,
#                                            compression, and retention. An
#                                            Apache build installs cleanly and
#                                            creates the extension happily --
#                                            it fails LATER, at the first
#                                            migration, with an error about a
#                                            function that does not exist. This
#                                            is the one check that separates
#                                            the two builds up front.
#   3. Continuous aggregate + compression + retention actually run.
#                                            Not a version string but the three
#                                            features the schema requires,
#                                            verified in
#                                            component/database/memory-nodes/migrations/.
#                                            Asserting the license alone would
#                                            still pass on a build where they
#                                            were unavailable for another reason.
#   4. N-1 -> N upgrade choreography.        The image ships two versioned
#                                            `.so` files so a rolling restart
#                                            can land before ALTER EXTENSION
#                                            runs. That is invisible until an
#                                            upgrade, when getting it wrong is
#                                            an outage rather than a bug, so it
#                                            is exercised here: create the
#                                            extension AT the previous minor,
#                                            serve a query, then UPDATE.
#
# Usage:
#   deploy/db-image/smoke-test.sh <image-ref> [pg-major] [current-version] [previous-version]

set -euo pipefail

IMAGE="${1:?usage: smoke-test.sh <image-ref> [pg-major] [current] [previous]}"
PG_MAJOR="${2:-16}"
TS_CURRENT="${3:-2.29.1}"
TS_PREVIOUS="${4:-2.28.3}"

function info() { printf 'INFO:  %s\n' "$*" >&2; }
function fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# The libraries have to be on disk before anything else is worth checking.
function assert_libraries_present() {
    info "checking both TimescaleDB libraries are present..."
    docker run --rm --entrypoint bash "$IMAGE" -c "
        set -e
        test -f /usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb.so
        test -f /usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TS_CURRENT}.so
        test -f /usr/lib/postgresql/${PG_MAJOR}/lib/timescaledb-${TS_PREVIOUS}.so
        test -f /usr/share/postgresql/${PG_MAJOR}/extension/vector.control
    " || fail "the image is missing a required library (loader, ${TS_CURRENT}, ${TS_PREVIOUS}, or pgvector)"
    info "  loader + ${TS_CURRENT} + ${TS_PREVIOUS} + pgvector: present"
}

# CNPG operand images are driven by the instance manager rather than an
# entrypoint that starts Postgres, so the test brings its own server up with
# initdb + pg_ctl on a unix socket in /tmp. uid 26 is the postgres user in a
# CNPG image; running as root would make initdb refuse.
function assert_runtime_behaviour() {
    info "starting Postgres inside the image and exercising it..."
    docker run --rm --user 26 --entrypoint bash "$IMAGE" -c "
        set -e
        export PATH=/usr/lib/postgresql/${PG_MAJOR}/bin:\$PATH
        export PGDATA=/tmp/pgdata
        initdb -D \"\$PGDATA\" -U postgres >/dev/null 2>&1
        echo \"shared_preload_libraries = 'timescaledb'\" >> \"\$PGDATA/postgresql.conf\"
        pg_ctl -D \"\$PGDATA\" -o '-k /tmp -c listen_addresses=' -w -t 60 start >/dev/null 2>&1

        run() { psql -h /tmp -U postgres -d postgres -v ON_ERROR_STOP=1 -q \"\$@\"; }
        val() { psql -h /tmp -U postgres -d postgres -tAc \"\$1\"; }

        # (1) both extensions create
        run -c 'CREATE EXTENSION timescaledb;' -c 'CREATE EXTENSION vector;'

        # (2) Community/TSL build, not Apache
        lic=\$(val 'SHOW timescaledb.license')
        if [ \"\$lic\" != 'timescale' ]; then
            echo \"ERROR: timescaledb.license is '\$lic', want 'timescale' -- this is an Apache build and the schema will fail at migration time\" >&2
            exit 1
        fi
        echo \"  timescaledb.license = \$lic\"
        echo \"  timescaledb         = \$(val \"SELECT extversion FROM pg_extension WHERE extname='timescaledb'\")\"
        echo \"  pgvector            = \$(val \"SELECT extversion FROM pg_extension WHERE extname='vector'\")\"

        # (3) the three TSL features the schema actually requires
        run <<'SQL'
CREATE TABLE probe(ts timestamptz NOT NULL, v double precision);
SELECT create_hypertable('probe', 'ts');
CREATE MATERIALIZED VIEW probe_1m WITH (timescaledb.continuous) AS
  SELECT time_bucket('1 minute', ts) AS bucket, avg(v) FROM probe GROUP BY 1 WITH NO DATA;
ALTER TABLE probe SET (timescaledb.compress);
SELECT add_retention_policy('probe', INTERVAL '7 days');
SQL
        echo '  continuous aggregate + compression + retention: OK'

        # (4) the N-1 -> N choreography, on a second database so the checks above
        #     stay independent of it
        run -c 'CREATE DATABASE upgrade_probe;'
        pu() { psql -h /tmp -U postgres -d upgrade_probe -v ON_ERROR_STOP=1 -q \"\$@\"; }
        pv() { psql -h /tmp -U postgres -d upgrade_probe -tAc \"\$1\"; }
        pu -c \"CREATE EXTENSION timescaledb VERSION '${TS_PREVIOUS}';\"
        got=\$(pv \"SELECT extversion FROM pg_extension WHERE extname='timescaledb'\")
        [ \"\$got\" = '${TS_PREVIOUS}' ] || { echo \"ERROR: pinned extension is \$got, want ${TS_PREVIOUS}\" >&2; exit 1; }
        # serving a real query on N-1 is the part that proves the old .so is
        # both present and reachable through the loader
        pu -c 'CREATE TABLE t(ts timestamptz NOT NULL, v int);' -c \"SELECT create_hypertable('t','ts');\" >/dev/null
        pu -c \"ALTER EXTENSION timescaledb UPDATE TO '${TS_CURRENT}';\"
        got=\$(pv \"SELECT extversion FROM pg_extension WHERE extname='timescaledb'\")
        [ \"\$got\" = '${TS_CURRENT}' ] || { echo \"ERROR: after UPDATE the extension is \$got, want ${TS_CURRENT}\" >&2; exit 1; }
        pv 'SELECT count(*) FROM t' >/dev/null
        echo '  upgrade choreography ${TS_PREVIOUS} -> ${TS_CURRENT}: OK'

        pg_ctl -D \"\$PGDATA\" -m immediate stop >/dev/null 2>&1
    " || fail "runtime smoke test failed"
}

function main() {
    info "smoke-testing ${IMAGE} (pg${PG_MAJOR}, timescaledb ${TS_PREVIOUS} -> ${TS_CURRENT})"
    assert_libraries_present
    assert_runtime_behaviour
    info "SUCCESS: ${IMAGE} passed every smoke check"
}

main "$@"
