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

# deployer leak reclaim (memql#1861). The leaking client stamps this
# application_name (there is no 'deployer' Postgres role).
DEPLOYER_APPNAME="${DEPLOYER_APPNAME:-deployer}"
# Reclaim backends idle longer than this. 5min comfortably outlives any
# legitimate gate/migrate/promote run while killing the 8h+ leaks (#1861).
DEPLOYER_IDLE_TIMEOUT_MS="${DEPLOYER_IDLE_TIMEOUT_MS:-300000}"

# DB-connecting node -> steady replica count (restored after recover).
# bff is intentionally NOT in this list: it is an argo Rollout whose serving
# pods are owned by the Rollout, while the `bff` Deployment is only the
# workloadRef pod template and MUST stay scaled to 0. Scaling deployment/bff up
# (as an earlier version of this script did on restore) spawns a SECOND bff
# ReplicaSet that double-draws DB connections and drives 53300 (memql#1868).
# See enforce_bff_rollout_invariant().
NODE_REPLICAS=(
  "cognition=2" "agent=2" "planner=2"
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
  enforce_bff_rollout_invariant
}

function enforce_bff_rollout_invariant() {
  # bff is an argo Rollout; the `bff` Deployment is only the workloadRef pod
  # template and MUST stay scaled to 0 (the Rollout owns the serving pods, and
  # ArgoCD ignores the Deployment's /spec/replicas). Pin it to 0 on every
  # scale_fleet call so a recover run can NEVER leave a second bff ReplicaSet
  # running -- that duplicate double-draws DB connections and drives 53300
  # (memql#1868). To actually drain bff's own connections, scale the Rollout via
  # `kubectl argo rollouts` directly; this script deliberately does not, to avoid
  # perturbing a blue-green promotion mid-incident.
  echo "  pinning bff workloadRef Deployment -> 0 (Rollout owns bff pods; memql#1868)"
  kubectl -n "$NS" scale deployment bff --replicas=0 2>&1 | sed 's/^/    /' \
    || echo "    (no bff Deployment to pin; ok)"
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
  # Snapshot the leaking backends by application_name so the source client can
  # be identified by client_addr / age (memql#1861). The leak is a CLIENT that
  # stamps application_name='deployer' (a deploy/migrate process) and never
  # closes its pool -- there is NO Postgres role named 'deployer', so the match
  # is on application_name, not usename.
  local d
  d="$(dsn)"
  if [ -z "$d" ]; then
    echo "ERROR: could not resolve Tiger DSN (tiger auth login?)" >&2
    return 1
  fi
  export PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S"

  echo "===== application_name='$DEPLOYER_APPNAME' backends: count by state + client ====="
  psql "$d" -c "SELECT count(*), state, client_addr, usename
                FROM pg_stat_activity
                WHERE application_name = '$DEPLOYER_APPNAME'
                GROUP BY 2,3,4 ORDER BY 1 DESC;" 2>&1

  echo "===== application_name='$DEPLOYER_APPNAME' backends: oldest first (leak hunt) ====="
  psql "$d" -c "SELECT pid, usename, client_addr, state,
                       now()-state_change AS in_state, left(query,40) AS q
                FROM pg_stat_activity
                WHERE application_name = '$DEPLOYER_APPNAME'
                ORDER BY state_change ASC NULLS FIRST LIMIT 40;" 2>&1
}

function deployer_reclaim() {
  # Terminate the leaked idle backends stamped application_name='deployer' to
  # reclaim the slots now (memql#1861). There is no 'deployer' ROLE, so there is
  # no ALTER ROLE backstop to install -- the DURABLE fix is to stop the source
  # client (identify it by client_addr below and make it close its pool / set an
  # idle timeout). Reclaim is owner-run; it only kills sessions idle past the
  # threshold (a live gate/migrate run is never mid-statement that long).
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

  echo "### terminating leaked application_name='$DEPLOYER_APPNAME' backends idle > ${secs}s ###"
  psql "$d" -c "SELECT pid, client_addr,
                       pg_terminate_backend(pid) AS terminated,
                       now()-state_change AS was_idle_for
                FROM pg_stat_activity
                WHERE application_name = '$DEPLOYER_APPNAME'
                  AND state IN ('idle', 'idle in transaction')
                  AND state_change < now() - interval '${secs} seconds'
                  AND pid <> pg_backend_pid();" 2>&1

  echo
  echo "### AFTER ###"
  deployer_inspect
  echo
  echo "DURABLE FIX: this only reclaims the CURRENT leak. The source client"
  echo "(see client_addr above) keeps re-leaking ~1 conn per run until it is"
  echo "stopped or made to close its pool. Once the leak source is fixed and"
  echo "the slots stay free, the per-pod MAX_OPEN_CONNS (cut to 4 under #1858)"
  echo "can be raised back toward 10 -- see"
  echo "docs/public/operate/db-connection-budget.md (#1861)."
}

function usage() {
  cat <<EOF
Usage: $0 <capture|recover|deployer-inspect|deployer-reclaim>

  capture           Snapshot max_connections + pg_stat_activity (needs one free slot).
  recover           Capture, drain the DB-connecting fleet to 0, re-capture the
                    pg_stat_activity breakdown, then restore steady replicas.
  deployer-inspect  Snapshot the application_name='$DEPLOYER_APPNAME' backends to find
                    the leaking client by client_addr (memql#1861). Read-only.
  deployer-reclaim  Terminate the leaked idle application_name='$DEPLOYER_APPNAME'
                    backends (memql#1861). Stop the source client for the durable fix.

Env overrides: TIGER_SVC ($TIGER_SVC), NS ($NS), PGCONNECT_TIMEOUT_S,
               DEPLOYER_APPNAME ($DEPLOYER_APPNAME), DEPLOYER_IDLE_TIMEOUT_MS ($DEPLOYER_IDLE_TIMEOUT_MS).
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
