#!/usr/bin/env bash
#
# conn-monitor.sh -- periodic connection-budget monitor + leak detector for the
# shared Tiger Cloud Postgres (memql#1958, follow-up to the 0.9.88 deploy storm).
#
# WHY
#   The 0.9.88 deploy stormed because nothing watched the direct connection
#   budget: an external `deployer` superuser pool (Tiger control plane, #1822)
#   had silently consumed ~60 of ~88 direct slots, and the deploy cold-started
#   into the remainder. We must SEE connection pressure building -- total vs
#   budget AND per-application_name -- so a leak (ours or Tiger's) is caught
#   while it's small, before it can set up the next storm.
#
# WHAT IT DOES (read-only; terminates / changes nothing)
#   One pass over pg_stat_activity on the DIRECT endpoint (which sees every
#   backend on the instance -- mesh direct pools, migrations, AND any non-mesh
#   client like `deployer` or the pooler's server pool). Emits a single
#   structured line plus, on threshold breach, WARN/CRIT lines that a log-based
#   alert can key on. Designed to run every ~5 min as a k8s CronJob
#   (deploy/k8s/base/conn-monitor-cronjob.yaml) and ad-hoc by an operator.
#
# THRESHOLDS (env-overridable)
#   total backends > WARN_PCT% of budget  -> WARN
#   total backends > CRIT_PCT% of budget  -> CRIT
#   any NON-mesh application_name holding > LEAK_MIN connections -> WARN (leak)
#       (mesh = application_name LIKE 'memql%'; everything else is "foreign":
#        deployer, postgres_exporter, ForgeExplorer, psql, ...)
#
# EXIT CODES: 0 = ok, 1 = WARN, 2 = CRIT (or unreachable). So a CronJob's
#   last-run status + logs surface the state; a human/gate can branch on it.
#
# DSN: DIRECT_DSN -> MEMORY_NODES_DATABASE_DIRECT_DSN -> MEMORY_NODES_DATABASE_DSN
#      -> `tiger db connection-string $TIGER_SVC`.
#
# Status-reporter: set -uo pipefail (NO -e). Function-based per CLAUDE.md.

set -uo pipefail

#=============================================================================
# CONFIGURATION (env-overridable; staging defaults)
#=============================================================================

RESERVED_CONNECTIONS="${RESERVED_CONNECTIONS:-17}"  # superuser(12) + Tiger ops(5)
WARN_PCT="${WARN_PCT:-70}"
CRIT_PCT="${CRIT_PCT:-90}"
LEAK_MIN="${LEAK_MIN:-30}"                           # a foreign app holding > this = WARN
TIGER_SVC="${TIGER_SVC:-xahn9ru4v6}"
PGCONNECT_TIMEOUT_S="${PGCONNECT_TIMEOUT_S:-8}"

DSN=""

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

One-pass connection-budget monitor + leak detector (read-only). Intended for a
*/5 CronJob and ad-hoc operator use.

Options:
  --warn-pct=N     Warn when total backends exceed N% of budget (default $WARN_PCT)
  --crit-pct=N     Crit when total backends exceed N% of budget (default $CRIT_PCT)
  --leak-min=N     Warn when a NON-mesh application_name holds > N conns (default $LEAK_MIN)
  --reserved=N     Reserved slots (default $RESERVED_CONNECTIONS)
  --dsn=DSN        DB DSN (default: DIRECT_DSN / MEMORY_NODES_DATABASE_DIRECT_DSN
                   / MEMORY_NODES_DATABASE_DSN / tiger CLI for \$TIGER_SVC)
  --help

Exit: 0 ok, 1 WARN, 2 CRIT/unreachable.
EOF
}

function parse_arguments() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --warn-pct=*)  WARN_PCT="${1#*=}"; shift ;;
            --crit-pct=*)  CRIT_PCT="${1#*=}"; shift ;;
            --leak-min=*)  LEAK_MIN="${1#*=}"; shift ;;
            --reserved=*)  RESERVED_CONNECTIONS="${1#*=}"; shift ;;
            --dsn=*)       DSN="${1#*=}"; shift ;;
            --help)        show_help; exit 0 ;;
            *) echo "ERROR: unknown option: $1" >&2; show_help; exit 2 ;;
        esac
    done
}

function require_psql() {
    command -v psql >/dev/null 2>&1 || { echo "CONN-MONITOR CRIT: psql not found" >&2; exit 2; }
}

function resolve_dsn() {
    [[ -n "$DSN" ]] && return 0
    [[ -n "${DIRECT_DSN:-}" ]] && { DSN="$DIRECT_DSN"; return 0; }
    [[ -n "${MEMORY_NODES_DATABASE_DIRECT_DSN:-}" ]] && { DSN="$MEMORY_NODES_DATABASE_DIRECT_DSN"; return 0; }
    [[ -n "${MEMORY_NODES_DATABASE_DSN:-}" ]] && { DSN="$MEMORY_NODES_DATABASE_DSN"; return 0; }
    if command -v tiger >/dev/null 2>&1; then
        DSN="$(tiger db connection-string "$TIGER_SVC" --with-password 2>/dev/null)"
        [[ -n "$DSN" ]] && return 0
    fi
    echo "CONN-MONITOR CRIT: no DSN (set DIRECT_DSN / MEMORY_NODES_DATABASE_DIRECT_DSN / MEMORY_NODES_DATABASE_DSN or auth tiger CLI)" >&2
    exit 2
}

function psql_q() {
    PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S" psql "$DSN" -At -F '|' -c "$1" 2>/dev/null
}

# evaluate runs the single forensic query and prints the status + any WARN/CRIT.
# Returns the worst exit code (0/1/2).
function evaluate() {
    local maxc total
    maxc="$(psql_q "SHOW max_connections;")"
    if [[ ! "$maxc" =~ ^[0-9]+$ ]]; then
        echo "CONN-MONITOR CRIT: instance unreachable (or at max_connections)"
        return 2
    fi
    local budget=$(( maxc - RESERVED_CONNECTIONS ))

    total="$(psql_q "SELECT count(*) FROM pg_stat_activity WHERE datname IS NOT NULL;")"
    [[ "$total" =~ ^[0-9]+$ ]] || { echo "CONN-MONITOR CRIT: count query failed"; return 2; }

    local pct=$(( budget > 0 ? total * 100 / budget : 0 ))

    # Per-application_name, split mesh (memql*) vs foreign, biggest first.
    local breakdown
    breakdown="$(psql_q "SELECT coalesce(nullif(application_name,''),'(none)') AS app,
                                count(*) AS n,
                                count(*) FILTER (WHERE state LIKE 'idle%') AS idle,
                                coalesce(host(min(client_addr))::text,'-') AS addr
                         FROM pg_stat_activity WHERE datname IS NOT NULL
                         GROUP BY 1 ORDER BY n DESC LIMIT 20;")"

    local rc=0
    # Total-budget thresholds.
    if (( pct >= CRIT_PCT )); then
        echo "CONN-MONITOR CRIT: backends ${total}/${budget} (${pct}%) >= ${CRIT_PCT}% of budget (max_connections=${maxc})"
        rc=2
    elif (( pct >= WARN_PCT )); then
        echo "CONN-MONITOR WARN: backends ${total}/${budget} (${pct}%) >= ${WARN_PCT}% of budget (max_connections=${maxc})"
        (( rc < 1 )) && rc=1
    fi

    # Foreign-leak detector: any non-mesh app_name over LEAK_MIN.
    while IFS='|' read -r app n idle addr; do
        [[ -z "${app:-}" ]] && continue
        case "$app" in
            memql*|"(none)") continue ;;  # mesh pods (+ unattributable system rows)
        esac
        if [[ "$n" =~ ^[0-9]+$ ]] && (( n > LEAK_MIN )); then
            echo "CONN-MONITOR WARN: foreign app '${app}' holds ${n} conns (${idle} idle) from ${addr} -- possible leak (> ${LEAK_MIN}); see memql#1822/#1958"
            (( rc < 1 )) && rc=1
        fi
    done <<< "$breakdown"

    # Always emit the structured one-liner (greppable; the steady-state record).
    local top
    top="$(printf '%s\n' "$breakdown" | head -5 | awk -F'|' '{printf "%s=%s ",$1,$2}')"
    echo "CONN-MONITOR OK status: backends=${total}/${budget} (${pct}%) max_connections=${maxc} top: ${top}"

    return "$rc"
}

function main() {
    parse_arguments "$@"
    require_psql
    resolve_dsn
    evaluate
    exit $?
}

main "$@"
