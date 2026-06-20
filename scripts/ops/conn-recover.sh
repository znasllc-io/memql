#!/usr/bin/env bash
#
# conn-recover.sh -- diagnose + recover Postgres connection-slot exhaustion
# (SQLSTATE 53300) on the staging Tiger Cloud instance (memql#1817).
#
# The fleet can pin every non-reserved connection slot so hard that even an
# admin client can't get in to inspect pg_stat_activity. This script:
#   capture  -- snapshot max_connections + pg_stat_activity (needs a free slot)
#   recover  -- free slots by scaling the DB-connecting fleet down then back up,
#               then re-capture the snapshot
#
# Owner-run: `recover` mutates the shared `memql` namespace. Read before running.
#
# Requires: tiger (auth login'd), psql, kubectl (context aks-memql-staging).

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

TIGER_SVC="${TIGER_SVC:-xahn9ru4v6}"   # staging Tiger service id
NS="${NS:-memql}"
PGCONNECT_TIMEOUT_S="${PGCONNECT_TIMEOUT_S:-8}"

# DB-connecting node -> steady replica count (restored after recover).
NODE_REPLICAS=(
  "bff=2" "cognition=2" "agent=2" "planner=2"
  "voice=2" "workbench=2" "identity=2" "mcp=1"
)

#=============================================================================
# FUNCTIONS
#=============================================================================

function dsn() {
  tiger db connection-string "$TIGER_SVC" --with-password 2>/dev/null
}

function capture() {
  local d
  d="$(dsn)"
  if [ -z "$d" ]; then
    echo "ERROR: could not resolve Tiger DSN (tiger auth login?)" >&2
    return 1
  fi

  export PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S"

  echo "===== max_connections / reserved ====="
  psql "$d" -At \
    -c "SHOW max_connections;" \
    -c "SHOW superuser_reserved_connections;" 2>&1

  echo "===== total backends ====="
  psql "$d" -At -c "SELECT count(*) FROM pg_stat_activity;" 2>&1

  echo "===== by application_name / state / wait ====="
  psql "$d" -c "SELECT count(*), application_name, state, wait_event_type
                FROM pg_stat_activity
                GROUP BY 2,3,4 ORDER BY 1 DESC LIMIT 40;" 2>&1

  echo "===== oldest sessions (orphan / stuck hunt) ====="
  psql "$d" -c "SELECT pid, application_name, state, client_addr,
                       now()-state_change AS in_state, left(query,50) AS q
                FROM pg_stat_activity
                ORDER BY state_change ASC NULLS FIRST LIMIT 25;" 2>&1
}

function scale_fleet() {
  local replicas="$1" # target replica count per node (0 to drain)
  for pair in "${NODE_REPLICAS[@]}"; do
    local node="${pair%%=*}"
    local desired="${pair##*=}"
    local target="$replicas"
    [ "$replicas" = "restore" ] && target="$desired"
    echo "  kubectl scale deployment/$node -> $target"
    kubectl -n "$NS" scale deployment "$node" --replicas="$target" 2>&1 | sed 's/^/    /'
  done
}

function abort_bff_rollout() {
  # The bff blue-green Rollout left paused at BlueGreenPause holds preview +
  # active (4 pods) indefinitely. Abort drops the preview color.
  echo "  aborting paused bff Rollout (drops preview color)"
  kubectl argo rollouts -n "$NS" abort bff 2>&1 | sed 's/^/    /' \
    || echo "    (argo rollouts plugin not present; promote or abort bff in the console)"
}

function recover() {
  echo "### capture BEFORE recovery (may fail if fully saturated) ###"
  capture
  echo
  echo "### draining DB-connecting fleet to free slots ###"
  abort_bff_rollout
  scale_fleet 0
  echo "  waiting 30s for pools to close..."
  sleep 30
  echo
  echo "### capture WITH fleet drained (this is the diagnostic snapshot) ###"
  capture
  echo
  echo "### restoring fleet to steady replica counts ###"
  scale_fleet restore
  echo
  echo "NOTE: ArgoCD ignores /spec/replicas, so it will NOT self-restore or"
  echo "fight these scales. Verify pods with: kubectl -n $NS get pods"
}

function usage() {
  cat <<EOF
Usage: $0 <capture|recover>

  capture   Snapshot max_connections + pg_stat_activity (needs one free slot).
  recover   Capture, drain the DB-connecting fleet to 0, re-capture the
            pg_stat_activity breakdown, then restore steady replicas.

Env overrides: TIGER_SVC ($TIGER_SVC), NS ($NS), PGCONNECT_TIMEOUT_S.
EOF
}

function main() {
  case "${1:-}" in
    capture) capture ;;
    recover) recover ;;
    *) usage; exit 1 ;;
  esac
}

main "$@"
