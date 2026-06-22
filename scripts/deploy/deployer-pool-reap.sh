#!/usr/bin/env bash
#
# deployer-pool-reap.sh -- detect and (guarded) reap the leaked
# `application_name='deployer'` idle connection pool on the Tiger Cloud
# Postgres instance (memql#1933, a #1861 recurrence).
#
# WHAT THIS FIXES
#   A deploy/migrate-class client connecting as the `postgres` superuser and
#   stamping `application_name='deployer'` opens connections and never closes
#   them. With NO client-side idle reaper, those backends stay `idle` for hours
#   and re-accumulate ~1 per ~30 min, eating roughly a third of the usable
#   direct connection budget -- the dominant residual SQLSTATE 53300 driver
#   during the 0.9.87 incident. There is NO Postgres role named `deployer`
#   (the match is on application_name, not usename), so there is no shared role
#   to hang a server-side reaper on; the durable fix is to (a) stop / bound the
#   leaking client and (b) keep this guarded reap available as a runbook step.
#
#   The in-cluster deploy + migration cycle is ALREADY leak-free: the mesh
#   pods connect as `tsdbadmin` and reap their own idle connections client-side
#   (CONN_MAX_IDLE_TIME_MS + idle_session_timeout), and the one-shot
#   `memql migrate` Job (deploy/k8s/base/migrate-job.yaml) closes its pool on
#   exit (Database.Stop -> db.Close), keeps only one short-lived idle conn, and
#   is TTL-reaped. This script targets the residual OUT-OF-CLUSTER leak.
#
# SAFETY (this is the SAFE version of the incident's /tmp reaper)
#   - DEFAULT action is `inspect` -- read-only, terminates nothing.
#   - `reap` prints the exact target pids (a dry-run plan) and refuses to
#     terminate anything UNLESS `--confirm` is passed.
#   - The terminate set is triple-bounded and can NEVER touch a live deploy:
#       application_name = '<appname>'                  (only the leak)
#       AND state IN ('idle','idle in transaction')     (never 'active')
#       AND state_change < now() - interval '<idle>'    (older than threshold)
#       AND pid <> pg_backend_pid()                     (never our own session)
#     Active backends and every non-deployer backend are untouched by design.
#   - Owner-run: terminating `postgres`-owned sessions requires a SUPERUSER DSN
#     (only a superuser may terminate a superuser's sessions). `inspect` works
#     with the read-only `tsdbadmin` recovery DSN.
#
# Requires: psql, and one of SUPERUSER_DSN / MEMORY_NODES_DATABASE_DSN, or the
#           `tiger` CLI (auth login'd) to resolve a DSN for TIGER_SVC.

set -euo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

# The leaking client stamps this application_name (there is no 'deployer' role).
DEPLOYER_APPNAME="${DEPLOYER_APPNAME:-deployer}"

# Reap backends idle longer than this Postgres interval. 5 minutes comfortably
# outlives any legitimate gate/migrate/promote run (those are never idle that
# long mid-flight) while killing the 8h+ leaks (#1861).
IDLE_THRESHOLD="${IDLE_THRESHOLD:-5 minutes}"

# DSN resolution order: explicit SUPERUSER_DSN (needed to terminate
# postgres-owned sessions) -> MEMORY_NODES_DATABASE_DSN -> `tiger` CLI.
SUPERUSER_DSN="${SUPERUSER_DSN:-}"
TIGER_SVC="${TIGER_SVC:-xahn9ru4v6}"     # staging Tiger service id
PGCONNECT_TIMEOUT_S="${PGCONNECT_TIMEOUT_S:-8}"

CONFIRM=false

#=============================================================================
# FUNCTIONS
#=============================================================================

function require_psql() {
    if command -v psql >/dev/null 2>&1; then
        return 0
    fi
    if [ -x "/opt/homebrew/opt/libpq/bin/psql" ]; then
        export PATH="/opt/homebrew/opt/libpq/bin:$PATH"
        return 0
    fi
    echo "ERROR: psql not found (on macOS libpq is keg-only: brew install libpq)" >&2
    return 1
}

function resolve_dsn() {
    if [ -n "$SUPERUSER_DSN" ]; then
        printf '%s' "$SUPERUSER_DSN"
        return 0
    fi
    if [ -n "${MEMORY_NODES_DATABASE_DSN:-}" ]; then
        printf '%s' "$MEMORY_NODES_DATABASE_DSN"
        return 0
    fi
    if command -v tiger >/dev/null 2>&1; then
        tiger db connection-string "$TIGER_SVC" --with-password 2>/dev/null
        return 0
    fi
    return 0
}

# psql_target prints the resolved DSN or fails loudly. Centralizes the
# "no DSN" error so every subcommand reports it the same way.
function psql_target() {
    local d
    d="$(resolve_dsn)"
    if [ -z "$d" ]; then
        echo "ERROR: no DSN. Set SUPERUSER_DSN (to terminate postgres-owned" >&2
        echo "       sessions) or MEMORY_NODES_DATABASE_DSN, or 'tiger auth login'." >&2
        return 1
    fi
    printf '%s' "$d"
}

function inspect() {
    local d
    d="$(psql_target)" || return 1
    export PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S"

    echo "===== application_name='$DEPLOYER_APPNAME' backends: count by state + client ====="
    psql "$d" -c "SELECT count(*), state, client_addr, usename
                  FROM pg_stat_activity
                  WHERE application_name = '$DEPLOYER_APPNAME'
                  GROUP BY 2,3,4 ORDER BY 1 DESC;"

    echo "===== application_name='$DEPLOYER_APPNAME' backends: oldest first (leak hunt) ====="
    psql "$d" -c "SELECT pid, usename, client_addr, state,
                         now()-state_change AS in_state, left(query,40) AS last_query
                  FROM pg_stat_activity
                  WHERE application_name = '$DEPLOYER_APPNAME'
                  ORDER BY state_change ASC NULLS FIRST LIMIT 40;"
}

# plan prints the EXACT set of pids the reap would terminate -- read-only, so an
# operator can eyeball the target before passing --confirm.
function plan() {
    local d
    d="$(psql_target)" || return 1
    export PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S"

    echo "===== reap PLAN: idle '$DEPLOYER_APPNAME' backends older than '$IDLE_THRESHOLD' ====="
    psql "$d" -c "SELECT pid, usename, client_addr, state,
                         now()-state_change AS idle_for, left(query,40) AS last_query
                  FROM pg_stat_activity
                  WHERE application_name = '$DEPLOYER_APPNAME'
                    AND state IN ('idle', 'idle in transaction')
                    AND state_change < now() - interval '$IDLE_THRESHOLD'
                    AND pid <> pg_backend_pid()
                  ORDER BY state_change ASC NULLS FIRST;"
    echo "(no backends are terminated by 'plan'; run 'reap --confirm' to act)"
}

function reap() {
    local d
    d="$(psql_target)" || return 1
    export PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S"

    echo "### TARGET SET (dry-run plan) ###"
    plan
    echo

    if [ "$CONFIRM" != true ]; then
        echo "REFUSING to terminate: --confirm not passed."
        echo "Re-run with: $0 reap --confirm   (optionally --idle '<interval>')"
        return 0
    fi

    echo "### terminating idle application_name='$DEPLOYER_APPNAME' backends older than '$IDLE_THRESHOLD' ###"
    # Only idle deployer-stamped backends past the threshold, never our own
    # session, never an 'active' backend, never a non-deployer client.
    psql "$d" -c "SELECT pid, client_addr,
                         pg_terminate_backend(pid) AS terminated,
                         now()-state_change AS was_idle_for
                  FROM pg_stat_activity
                  WHERE application_name = '$DEPLOYER_APPNAME'
                    AND state IN ('idle', 'idle in transaction')
                    AND state_change < now() - interval '$IDLE_THRESHOLD'
                    AND pid <> pg_backend_pid();"

    echo
    echo "### AFTER ###"
    inspect
    echo
    echo "NOTE: this reclaims the CURRENT leak only. The source client keeps"
    echo "re-leaking until it is stopped or made to close its pool / set an idle"
    echo "timeout. Identify it by the client_addr above. See"
    echo "docs/internal/ops/conn-exhaustion-53300-spike.md (memql#1933/#1861)."
}

function usage() {
    cat <<EOF
Usage: $0 <inspect|plan|reap> [--confirm] [--idle '<pg interval>']

  inspect   (default) Read-only snapshot of application_name='$DEPLOYER_APPNAME'
            backends grouped by state/client + oldest-first. Terminates nothing.
  plan      Read-only. Print the EXACT pids a reap would terminate (idle,
            deployer-stamped, older than the idle threshold). Terminates nothing.
  reap      Print the target plan, then terminate it ONLY if --confirm is passed.
            Bounded to idle + application_name='$DEPLOYER_APPNAME' + older than
            the threshold + not our own session. Never touches active or
            non-deployer backends.

Flags:
  --confirm           Required for 'reap' to actually terminate.
  --idle '<interval>' Override the idle threshold (default: '$IDLE_THRESHOLD').

Env: SUPERUSER_DSN (required to terminate postgres-owned sessions),
     MEMORY_NODES_DATABASE_DSN, TIGER_SVC ($TIGER_SVC),
     DEPLOYER_APPNAME ($DEPLOYER_APPNAME), IDLE_THRESHOLD ('$IDLE_THRESHOLD'),
     PGCONNECT_TIMEOUT_S ($PGCONNECT_TIMEOUT_S).
EOF
}

function parse_flags() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --confirm) CONFIRM=true ;;
            --idle)
                shift
                if [ $# -eq 0 ]; then
                    echo "ERROR: --idle requires a value (e.g. '5 minutes')" >&2
                    return 1
                fi
                IDLE_THRESHOLD="$1"
                ;;
            --idle=*) IDLE_THRESHOLD="${1#*=}" ;;
            *)
                echo "ERROR: unknown flag: $1" >&2
                usage >&2
                return 1
                ;;
        esac
        shift
    done
}

function main() {
    require_psql || exit 1

    local cmd="${1:-inspect}"
    [ $# -gt 0 ] && shift || true
    parse_flags "$@" || exit 1

    case "$cmd" in
        inspect) inspect ;;
        plan)    plan ;;
        reap)    reap ;;
        -h|--help|help) usage ;;
        *) usage >&2; exit 1 ;;
    esac
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
