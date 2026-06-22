#!/usr/bin/env bash
#
# conn-surge-watch.sh -- watch Postgres backend usage on the Tiger Cloud
# instance during a deploy surge, to PROVE the connection storm (SQLSTATE
# 53300) is gone under the transaction pooler (epic memql#1925, child #1935).
#
# WHY
#   Before the hybrid pooler split, a full-stack roll (blue-green bff + rolling
#   mesh) briefly ran old + new pods together; pods x MAX_OPEN_CONNS + the
#   leaked deployer pool blew past Tiger's ~88 usable direct slots -> 53300
#   storm that wedged 0.9.87. With bulk traffic on the TRANSACTION POOLER, a
#   client surge no longer maps 1:1 to Postgres backends, so the backend count
#   on the instance should stay comfortably under the ceiling THROUGHOUT a roll.
#
# WHAT IT DOES (read-only; terminates / changes nothing)
#   Polls pg_stat_activity on the underlying Postgres instance every INTERVAL
#   seconds for DURATION seconds (0 = until Ctrl-C). Each tick prints the total
#   backend count vs the budget, plus the breakdown by application_name and by
#   state. Tracks the PEAK total across the run and prints a final summary line
#   suitable for pasting into the #1935 acceptance ("peak backends vs budget").
#
#   Query the instance through the DIRECT endpoint: pg_stat_activity there shows
#   EVERY backend on the instance -- the mesh pods' direct (advisory-lock /
#   migration) connections AND the pooler's server-side pool -- i.e. the
#   authoritative total that must stay under max_connections.
#
# DSN resolution (read-only is enough): DIRECT_DSN -> MEMORY_NODES_DATABASE_DIRECT_DSN
#   -> MEMORY_NODES_DATABASE_DSN -> `tiger db connection-string $TIGER_SVC`.
#
# Requires: psql, and one of the DSNs above or the `tiger` CLI (auth login'd).
#
# Status-reporter script: set -uo pipefail (NO -e -- one failed poll must not
# abort the watch). Function-based per the Skills+Scripts convention (CLAUDE.md).

set -uo pipefail

#=============================================================================
# CONFIGURATION (env-overridable; staging defaults from the #1817/#1820 capture)
#=============================================================================

INTERVAL_S="${INTERVAL_S:-5}"                       # seconds between polls
DURATION_S="${DURATION_S:-0}"                       # total run; 0 = until Ctrl-C
MAX_CONNECTIONS="${MAX_CONNECTIONS:-105}"           # SHOW max_connections (staging Tiger 2GB)
RESERVED_CONNECTIONS="${RESERVED_CONNECTIONS:-17}"  # superuser(12) + Tiger ops(5)
TIGER_SVC="${TIGER_SVC:-xahn9ru4v6}"                # staging Tiger service id
PGCONNECT_TIMEOUT_S="${PGCONNECT_TIMEOUT_S:-8}"

DSN=""
BUDGET=0
PEAK=0
PEAK_AT="-"

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

Watch Postgres backend usage during a deploy surge (read-only) to prove the
53300 storm is gone under the transaction pooler (memql#1935).

Options:
  --interval=N          Seconds between polls (default $INTERVAL_S / env INTERVAL_S)
  --duration=N          Total seconds to watch; 0 = until Ctrl-C (default $DURATION_S / env DURATION_S)
  --max-connections=N   Instance max_connections (default $MAX_CONNECTIONS / env MAX_CONNECTIONS)
  --reserved=N          Reserved slots (default $RESERVED_CONNECTIONS / env RESERVED_CONNECTIONS)
  --dsn=DSN             DB DSN to query (default: DIRECT_DSN / MEMORY_NODES_DATABASE_DIRECT_DSN
                        / MEMORY_NODES_DATABASE_DSN / tiger CLI for \$TIGER_SVC)
  --help

Budget = max_connections - reserved. PASS = peak backends stays under budget
with zero SQLSTATE 53300 in the pod logs throughout the roll.
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --interval=*)        INTERVAL_S="${1#*=}"; shift ;;
            --duration=*)        DURATION_S="${1#*=}"; shift ;;
            --max-connections=*) MAX_CONNECTIONS="${1#*=}"; shift ;;
            --reserved=*)        RESERVED_CONNECTIONS="${1#*=}"; shift ;;
            --dsn=*)             DSN="${1#*=}"; shift ;;
            --help)              show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1" >&2; show_help; exit 1 ;;
        esac
    done
}

function require_psql() {
    if ! command -v psql >/dev/null 2>&1; then
        echo "ERROR: psql not found on PATH." >&2
        exit 1
    fi
}

function resolve_dsn() {
    if [[ -n "$DSN" ]]; then
        return 0
    fi
    if [[ -n "${DIRECT_DSN:-}" ]]; then
        DSN="$DIRECT_DSN"; return 0
    fi
    if [[ -n "${MEMORY_NODES_DATABASE_DIRECT_DSN:-}" ]]; then
        DSN="$MEMORY_NODES_DATABASE_DIRECT_DSN"; return 0
    fi
    if [[ -n "${MEMORY_NODES_DATABASE_DSN:-}" ]]; then
        DSN="$MEMORY_NODES_DATABASE_DSN"; return 0
    fi
    if command -v tiger >/dev/null 2>&1; then
        DSN="$(tiger db connection-string "$TIGER_SVC" --with-password 2>/dev/null)"
        [[ -n "$DSN" ]] && return 0
    fi
    echo "ERROR: no DSN. Set DIRECT_DSN / MEMORY_NODES_DATABASE_DIRECT_DSN / MEMORY_NODES_DATABASE_DSN, pass --dsn, or auth the tiger CLI." >&2
    exit 1
}

# psql_q runs a single-value/tabular query, returns "" on connection error so a
# transient blip mid-roll doesn't abort the watch.
function psql_q() {
    PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S" psql "$DSN" -At -F '|' -c "$1" 2>/dev/null
}

function preflight() {
    BUDGET=$(( MAX_CONNECTIONS - RESERVED_CONNECTIONS ))
    # Prefer the instance's real max_connections when reachable.
    local live
    live="$(psql_q "SHOW max_connections;")"
    if [[ "$live" =~ ^[0-9]+$ ]]; then
        MAX_CONNECTIONS="$live"
        BUDGET=$(( MAX_CONNECTIONS - RESERVED_CONNECTIONS ))
    fi
    echo "INFO: watching $TIGER_SVC -- max_connections=$MAX_CONNECTIONS reserved=$RESERVED_CONNECTIONS budget=$BUDGET"
    echo "INFO: interval=${INTERVAL_S}s duration=$( [[ "$DURATION_S" == "0" ]] && echo 'until Ctrl-C' || echo "${DURATION_S}s" )"
    echo
}

function timestamp() { date -u +%H:%M:%S; }

# poll_once prints one status line + the application_name breakdown, and updates
# PEAK. Returns the total (or empty on a failed poll).
function poll_once() {
    local total
    total="$(psql_q "SELECT count(*) FROM pg_stat_activity WHERE datname IS NOT NULL;")"
    local now; now="$(timestamp)"

    if [[ ! "$total" =~ ^[0-9]+$ ]]; then
        echo "$now  (poll failed -- instance unreachable; retrying)"
        return 0
    fi

    if (( total > PEAK )); then
        PEAK="$total"; PEAK_AT="$now"
    fi

    local pct=$(( BUDGET > 0 ? total * 100 / BUDGET : 0 ))
    local flag=""
    (( total > BUDGET )) && flag="  *** OVER BUDGET ***"

    printf '%s  backends=%s / budget=%s (%s%%)%s\n' "$now" "$total" "$BUDGET" "$pct" "$flag"

    # Top application_name contributors (the #1817 attribution), idle vs active.
    psql_q "SELECT coalesce(nullif(application_name,''),'(none)') AS app,
                   count(*) FILTER (WHERE state='active')               AS active,
                   count(*) FILTER (WHERE state LIKE 'idle%')           AS idle,
                   count(*)                                             AS total
            FROM pg_stat_activity
            WHERE datname IS NOT NULL
            GROUP BY 1 ORDER BY total DESC LIMIT 8;" \
        | awk -F'|' '{ printf "        %-28s active=%-4s idle=%-4s total=%s\n", $1, $2, $3, $4 }'
}

function watch_loop() {
    local elapsed=0
    # Trap Ctrl-C so the summary still prints.
    trap 'print_summary; exit 0' INT
    while true; do
        poll_once
        if [[ "$DURATION_S" != "0" ]] && (( elapsed >= DURATION_S )); then
            break
        fi
        sleep "$INTERVAL_S"
        elapsed=$(( elapsed + INTERVAL_S ))
    done
}

function print_summary() {
    echo
    echo "============================================================"
    echo "  SURGE SUMMARY (memql#1935)"
    echo "============================================================"
    echo "  peak backends : $PEAK (at $PEAK_AT)"
    echo "  budget        : $BUDGET  (max_connections=$MAX_CONNECTIONS - reserved=$RESERVED_CONNECTIONS)"
    if (( PEAK > 0 )) && (( PEAK <= BUDGET )); then
        local head=$(( BUDGET - PEAK ))
        echo "  headroom      : $head slots"
        echo "  RESULT        : PASS (peak stayed under budget). Confirm pod logs show ZERO SQLSTATE 53300."
    elif (( PEAK > BUDGET )); then
        echo "  RESULT        : FAIL (peak exceeded budget -- the surge still pressures direct slots)."
    else
        echo "  RESULT        : INCONCLUSIVE (no successful polls captured a backend count)."
    fi
    echo "  NOTE          : the authoritative 53300 signal is the pod logs (grep '53300' / 'remaining connection slots')."
    echo "============================================================"
}

function main() {
    parse_arguments "$@"
    require_psql
    resolve_dsn
    preflight
    watch_loop
    print_summary
}

main "$@"
