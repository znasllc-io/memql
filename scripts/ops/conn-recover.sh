#!/usr/bin/env bash
#
# conn-recover.sh -- diagnose + recover Postgres connection-slot exhaustion
# (SQLSTATE 53300) on the staging Tiger Cloud instance (memql#1817).
#
# The fleet can pin every non-reserved connection slot so hard that even an
# admin client can't get in to inspect pg_stat_activity. This script:
#   capture           -- snapshot max_connections + pg_stat_activity (needs a free slot)
#   recover           -- free slots by scaling the DB-connecting fleet down then back
#                        up, then re-capture the snapshot
#   deployer-inspect  -- snapshot the `deployer` tooling-role backends (memql#1861)
#   deployer-reclaim  -- terminate leaked idle `deployer` backends AND install a
#                        permanent server-side idle reaper on the role (memql#1861)
#
# The app fleet connects as the Tiger master role (`tsdbadmin`) and reaps its
# own idle connections client-side (CONN_MAX_IDLE_TIME_MS + the per-session
# idle_session_timeout, memql#1817). The `deployer` role is a SEPARATE
# least-privilege credential used by deploy tooling (gate / migrate / promote /
# exporters); it has no client-side reaper, so a tool that opens a pool and
# never closes it leaves idle backends pinned for hours and permanently eats a
# chunk of the mesh connection budget (memql#1861). `deployer-reclaim` fixes
# that at the role level so it is leak-proof regardless of which client misbehaves.
#
# Owner-run: `recover` mutates the shared `memql` namespace; `deployer-reclaim`
# mutates the shared Tiger instance (terminates backends + ALTER ROLE). Read
# before running.
#
# Requires: tiger (auth login'd), psql, kubectl (context aks-memql-staging).

set -uo pipefail

#=============================================================================
# CONFIGURATION
#=============================================================================

TIGER_SVC="${TIGER_SVC:-xahn9ru4v6}"   # staging Tiger service id
NS="${NS:-memql}"
PGCONNECT_TIMEOUT_S="${PGCONNECT_TIMEOUT_S:-8}"

# deployer-role reaper (memql#1861). The tooling role that leaks idle pools.
DEPLOYER_ROLE="${DEPLOYER_ROLE:-deployer}"
# Reap deployer backends idle longer than this (server-side backstop + the
# one-time terminate threshold). 5min comfortably outlives any legitimate
# gate/migrate/promote run while killing the 8h+ leaks (#1861).
DEPLOYER_IDLE_TIMEOUT_MS="${DEPLOYER_IDLE_TIMEOUT_MS:-300000}"

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

function deployer_inspect() {
  # Snapshot the deployer tooling-role backends so the leaking client can be
  # identified by application_name / client_addr / age (memql#1861).
  local d
  d="$(dsn)"
  if [ -z "$d" ]; then
    echo "ERROR: could not resolve Tiger DSN (tiger auth login?)" >&2
    return 1
  fi
  export PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S"

  echo "===== '$DEPLOYER_ROLE' backends: count by state ====="
  psql "$d" -c "SELECT count(*), state, application_name
                FROM pg_stat_activity
                WHERE usename = '$DEPLOYER_ROLE'
                GROUP BY 2,3 ORDER BY 1 DESC;" 2>&1

  echo "===== '$DEPLOYER_ROLE' backends: oldest first (leak hunt) ====="
  psql "$d" -c "SELECT pid, application_name, client_addr, state,
                       now()-state_change AS in_state, left(query,40) AS q
                FROM pg_stat_activity
                WHERE usename = '$DEPLOYER_ROLE'
                ORDER BY state_change ASC NULLS FIRST LIMIT 40;" 2>&1

  echo "===== role-level idle reaper currently set on '$DEPLOYER_ROLE' ====="
  psql "$d" -At -c "SELECT unnest(rolconfig) FROM pg_roles
                    WHERE rolname = '$DEPLOYER_ROLE'
                      AND rolconfig IS NOT NULL;" 2>&1
  echo "(blank above = no role-level idle_session_timeout backstop yet)"
}

function deployer_reclaim() {
  # Two parts (memql#1861):
  #   1. Install the PERMANENT server-side backstop on the role itself, so a
  #      leaked deployer pool is reaped no matter which client opened it. Only
  #      affects NEW connections.
  #   2. Terminate the EXISTING leaked idle / idle-in-transaction backends to
  #      reclaim the slots right now.
  local d
  d="$(dsn)"
  if [ -z "$d" ]; then
    echo "ERROR: could not resolve Tiger DSN (tiger auth login?)" >&2
    return 1
  fi
  export PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S"

  local secs=$(( DEPLOYER_IDLE_TIMEOUT_MS / 1000 ))

  echo "### BEFORE ###"
  deployer_inspect
  echo

  echo "### installing permanent role-level idle reaper on '$DEPLOYER_ROLE' (${DEPLOYER_IDLE_TIMEOUT_MS}ms) ###"
  # idle_session_timeout reaps plain idle sessions; idle_in_transaction guards a
  # wedged client holding a slot mid-transaction. Both are PG14+ / Tiger-supported.
  psql "$d" \
    -c "ALTER ROLE \"$DEPLOYER_ROLE\" SET idle_session_timeout = '${DEPLOYER_IDLE_TIMEOUT_MS}ms';" \
    -c "ALTER ROLE \"$DEPLOYER_ROLE\" SET idle_in_transaction_session_timeout = '${DEPLOYER_IDLE_TIMEOUT_MS}ms';" 2>&1

  echo "### terminating existing '$DEPLOYER_ROLE' backends idle > ${secs}s ###"
  psql "$d" -c "SELECT pid,
                       pg_terminate_backend(pid) AS terminated,
                       now()-state_change AS was_idle_for
                FROM pg_stat_activity
                WHERE usename = '$DEPLOYER_ROLE'
                  AND state IN ('idle', 'idle in transaction')
                  AND state_change < now() - interval '${secs} seconds'
                  AND pid <> pg_backend_pid();" 2>&1

  echo
  echo "### AFTER ###"
  deployer_inspect
  echo
  echo "NOTE: the ALTER ROLE backstop is permanent + leak-proof. Once the"
  echo "reclaimed slots stay free, the per-pod MAX_OPEN_CONNS (cut to 4 under"
  echo "#1858) can be raised back toward the default 10 -- see"
  echo "docs/public/operate/db-connection-budget.md (#1861)."
}

function usage() {
  cat <<EOF
Usage: $0 <capture|recover|deployer-inspect|deployer-reclaim>

  capture           Snapshot max_connections + pg_stat_activity (needs one free slot).
  recover           Capture, drain the DB-connecting fleet to 0, re-capture the
                    pg_stat_activity breakdown, then restore steady replicas.
  deployer-inspect  Snapshot the '$DEPLOYER_ROLE' tooling-role backends to find a
                    leaked idle pool (memql#1861). Read-only.
  deployer-reclaim  Install a permanent role-level idle reaper on '$DEPLOYER_ROLE'
                    and terminate its existing leaked idle backends (memql#1861).

Env overrides: TIGER_SVC ($TIGER_SVC), NS ($NS), PGCONNECT_TIMEOUT_S,
               DEPLOYER_ROLE ($DEPLOYER_ROLE), DEPLOYER_IDLE_TIMEOUT_MS ($DEPLOYER_IDLE_TIMEOUT_MS).
EOF
}

function main() {
  case "${1:-}" in
    capture) capture ;;
    recover) recover ;;
    deployer-inspect) deployer_inspect ;;
    deployer-reclaim) deployer_reclaim ;;
    *) usage; exit 1 ;;
  esac
}

main "$@"
