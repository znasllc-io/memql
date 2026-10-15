#!/usr/bin/env bash
#
# scripts/deploy/conn-headroom-check.sh
# =====================================
#
# Deploy gate (memql#1820, follow-up to the #1817 connection-exhaustion spike):
# block a deploy/promotion whose projected Postgres connection demand would
# exceed the instance budget, so a fleet-growth or pool-size change can never
# silently re-introduce the SQLSTATE 53300 storm.
#
# KEPT THROUGH THE TIGER CUTOVER (epic memql#3842 / #3848), unlike its two
# sibling scripts, and the difference is worth stating. conn-surge-watch.sh and
# deployer-pool-reap.sh managed TIGER'S SCARCITY -- a per-tier ceiling of ~59
# usable slots and a managed-pooler artifact -- and the scarcity went away with
# the provider. This gate is about OUR budget: the arithmetic below holds for
# any Postgres, and self-hosting changed the NUMBER on the right-hand side
# rather than the inequality. Losing it would have removed the one thing
# standing between a replica-count bump and the storm it caused in #1817.
#
# What changed with the cutover: `max_connections` is ours to set (200 for the
# entry preset, 400 for the local overlay and the mid and top presets, in
# deploy/k8s/components/cnpg-db), the reserved slots are
# Postgres's own superuser reservation rather than Tiger's ops overhead, and
# the DSN no longer has a `tiger` CLI fallback -- there is no Tiger service to
# ask.
#
# Budget contract (from the spike findings,
# docs/internal/ops/conn-exhaustion-53300-spike.md, and
# docs/public/operate/db-connection-budget.md):
#
#     per_process = main + direct + migration + gate
#     Sigma(replicas x per_process) + rollSurge + blueGreenOverlap
#         <= max_connections - reserved
#
# A PROCESS OPENS FOUR POOLS against the one max_connections (memql#6022), and
# every one counts:
#   - main: the request pool, MEMORY_NODES_DATABASE_MAX_OPEN_CONNS -- the
#     memory-nodes database reads its knobs under that prefix, so the bare
#     MAX_OPEN_CONNS is NOT read -- from the container's env, else from a
#     ConfigMap it takes env from (memql-db-pool), else the code's default;
#   - direct: the pool opened from MEMORY_NODES_DATABASE_DIRECT_DSN, fixed in
#     code. The DSN lives in memql-secrets, which no manifest shows, and every
#     shipped install seeds it (scripts/k3d/seed-secrets.sh copies the main DSN
#     when no direct one is given), so it is counted unless --direct-pool=0;
#   - migration: the one connection a process's startup migration opens;
#   - gate: the write-gate pool, MEMQL_DB_GATE_POOL_SIZE (container env, else a
#     ConfigMap, else the code's default).
#
# It reads every DB-connecting Deployment under the manifests dir, takes each
# one's replicas, its per-process connections (the four above), and
# RollingUpdate maxSurge, adds a blue-green overlap term for bff, and compares
# the steady + peak totals against (max_connections - reserved). The defaults
# are component/database's; scripts/deploy/conn_headroom_test.go holds them to
# the code.
#
# Exits non-zero (gate FAIL) when the PEAK would exceed budget. A steady-state
# overage is also a FAIL; a steady total above the soft threshold warns.
#
# Function-based per the Skills+Scripts convention (CLAUDE.md). set -uo pipefail.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

#=============================================================================
# CONFIGURATION (env-overridable; staging defaults from the live #1817 capture)
#=============================================================================

# The SMALLEST max_connections we run anywhere (the entry preset), so the
# default is conservative: local, staging and production set 400, and --live
# reads the real value off the instance anyway.
DEFAULT_MAX_CONNECTIONS="${MAX_CONNECTIONS:-200}"   # deploy/k8s/components/cnpg-db
# superuser_reserved_connections (3 by default) + CNPG's own instance-manager
# and monitoring connections. No Tiger ops overhead to leave room for.
DEFAULT_RESERVED="${RESERVED_CONNECTIONS:-10}"      # superuser + operator connections
DEFAULT_MAX_OPEN="${MAX_OPEN_CONNS:-10}"            # main pool default (database.go, DefaultRequestPoolSize)
DEFAULT_DIRECT_POOL="${DIRECT_POOL_CONNS:-4}"       # direct pool, fixed (database.go, DirectPoolSize)
DEFAULT_MIGRATION_POOL="${MIGRATION_POOL_CONNS:-1}" # startup migration (database.go, MigrationPoolSize)
DEFAULT_GATE_POOL="${GATE_POOL_CONNS:-4}"           # write-gate pool default (gate_pool.go, DefaultGatePoolSize)
DEFAULT_MANIFESTS="${MANIFESTS_DIR:-$REPO_ROOT/deploy/k8s/base}"

# --live (memql#1958): the projected math can't see what is ALREADY on the
# instance -- a non-fleet leak (e.g. the `deployer` Tiger control-plane pool,
# #1822). With --live the gate reads the REAL max_connections and subtracts
# live FOREIGN (non-memql) backends from the budget, so a deploy into an
# already-near-full instance FAILS FAST instead of cold-starting into a storm
# (the 0.9.88 failure mode). No-op without --live (pure manifest projection).
LIVE=false
DSN=""
PGCONNECT_TIMEOUT_S="${PGCONNECT_TIMEOUT_S:-8}"
LIVE_FOREIGN=0

#=============================================================================
# FUNCTIONS
#=============================================================================

function show_help() {
    cat <<EOF
Usage: $0 [options]

Block a deploy when projected DB connections exceed the instance budget:
  per_process = main + direct + migration + gate
  Sigma(replicas x per_process) + surge + blue-green overlap <= max_connections - reserved

Options:
  --max-connections=N   Instance max_connections (default $DEFAULT_MAX_CONNECTIONS / env MAX_CONNECTIONS)
  --reserved=N          Reserved slots (default $DEFAULT_RESERVED / env RESERVED_CONNECTIONS)
  --default-max-open=N  Main pool when neither the container nor its ConfigMaps set
                        MEMORY_NODES_DATABASE_MAX_OPEN_CONNS (default $DEFAULT_MAX_OPEN)
  --direct-pool=N       Direct pool per process; 0 for an install with no direct DSN (default $DEFAULT_DIRECT_POOL)
  --migration-pool=N    Startup-migration connections per process (default $DEFAULT_MIGRATION_POOL)
  --default-gate-pool=N Write-gate pool when neither the container nor its ConfigMaps set
                        MEMQL_DB_GATE_POOL_SIZE (default $DEFAULT_GATE_POOL)
  --manifests-dir=DIR   Where the node Deployment YAMLs live (default deploy/k8s/base)
  --live                ALSO subtract LIVE non-fleet backends from the budget +
                        read the real max_connections (needs a DSN). Catches a
                        deploy into an already-near-full instance (memql#1958).
  --dsn=DSN             DB DSN for --live (default: DIRECT_DSN /
                        MEMORY_NODES_DATABASE_DIRECT_DSN / MEMQL_DATABASE_DSN)
  --help

Exit: 0 = within budget, 1 = PEAK or steady exceeds budget (gate FAIL).
EOF
}

function parse_arguments() {
    MAX_CONNECTIONS="$DEFAULT_MAX_CONNECTIONS"
    RESERVED="$DEFAULT_RESERVED"
    DEF_MAX_OPEN="$DEFAULT_MAX_OPEN"
    DIRECT_POOL="$DEFAULT_DIRECT_POOL"
    MIGRATION_POOL="$DEFAULT_MIGRATION_POOL"
    DEF_GATE_POOL="$DEFAULT_GATE_POOL"
    MANIFESTS="$DEFAULT_MANIFESTS"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --max-connections=*) MAX_CONNECTIONS="${1#*=}";;
            --reserved=*)        RESERVED="${1#*=}";;
            --default-max-open=*) DEF_MAX_OPEN="${1#*=}";;
            --direct-pool=*)     DIRECT_POOL="${1#*=}";;
            --migration-pool=*)  MIGRATION_POOL="${1#*=}";;
            --default-gate-pool=*) DEF_GATE_POOL="${1#*=}";;
            --manifests-dir=*)   MANIFESTS="${1#*=}";;
            --live)              LIVE=true;;
            --dsn=*)             DSN="${1#*=}";;
            --help) show_help; exit 0;;
            *) echo "ERROR: unknown option: $1"; show_help; exit 2;;
        esac
        shift
    done
}

# resolve_live (only with --live): read the instance's REAL max_connections and
# the count of FOREIGN (non-memql) live backends, so the budget reflects reality
# (incl. the deployer leak) rather than the static default.
function resolve_live() {
    [[ "$LIVE" == true ]] || return 0
    if ! command -v psql >/dev/null 2>&1; then
        echo "CONN-HEADROOM-WARN: --live needs psql; skipping live adjustment"; return 0
    fi
    if [[ -z "$DSN" ]]; then
        DSN="${DIRECT_DSN:-${MEMORY_NODES_DATABASE_DIRECT_DSN:-${MEMQL_DATABASE_DSN:-}}}"
    fi
    if [[ -z "$DSN" ]]; then
        echo "CONN-HEADROOM-WARN: --live found no DSN; skipping live adjustment"; return 0
    fi
    local livemax foreign
    livemax="$(PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S" psql "$DSN" -At -c "SHOW max_connections;" 2>/dev/null)"
    [[ "$livemax" =~ ^[0-9]+$ ]] && MAX_CONNECTIONS="$livemax"
    # FOREIGN = backends NOT opened by the mesh (application_name not LIKE memql%)
    # and not our own probe -- i.e. the deployer leak, exporters, etc.
    foreign="$(PGCONNECT_TIMEOUT="$PGCONNECT_TIMEOUT_S" psql "$DSN" -At -c \
      "SELECT count(*) FROM pg_stat_activity WHERE datname IS NOT NULL AND coalesce(application_name,'') NOT LIKE 'memql%' AND pid <> pg_backend_pid();" 2>/dev/null)"
    [[ "$foreign" =~ ^[0-9]+$ ]] && LIVE_FOREIGN="$foreign"
    echo "INFO: --live: max_connections=$MAX_CONNECTIONS, foreign(non-mesh) live backends=$LIVE_FOREIGN (subtracted from budget)"
}

# compute_and_check delegates the YAML walk + arithmetic to python3 (already a
# dep of the release tooling). Prints the per-node table + verdict; returns the
# gate exit code.
function compute_and_check() {
    MAX_CONNECTIONS="$MAX_CONNECTIONS" RESERVED="$RESERVED" \
    DEF_MAX_OPEN="$DEF_MAX_OPEN" DIRECT_POOL="$DIRECT_POOL" \
    MIGRATION_POOL="$MIGRATION_POOL" DEF_GATE_POOL="$DEF_GATE_POOL" \
    MANIFESTS="$MANIFESTS" LIVE_FOREIGN="$LIVE_FOREIGN" \
    python3 - <<'PY'
import os, sys, glob
try:
    import yaml
except Exception:
    print("CONN-HEADROOM-FAIL: python yaml unavailable"); sys.exit(2)

maxc = int(os.environ["MAX_CONNECTIONS"])
reserved = int(os.environ["RESERVED"])
def_open = int(os.environ["DEF_MAX_OPEN"])
direct = int(os.environ["DIRECT_POOL"])
migration = int(os.environ["MIGRATION_POOL"])
def_gate = int(os.environ["DEF_GATE_POOL"])
mdir = os.environ["MANIFESTS"]
foreign = int(os.environ.get("LIVE_FOREIGN", "0") or 0)
budget = maxc - reserved - foreign

# The variables the process reads for its two configurable pools.
MAIN_KEY = "MEMORY_NODES_DATABASE_MAX_OPEN_CONNS"
GATE_KEY = "MEMQL_DB_GATE_POOL_SIZE"

docs = []
for path in sorted(glob.glob(os.path.join(mdir, "*.yaml"))):
    try:
        docs.extend((path, d) for d in yaml.safe_load_all(open(path)) if d)
    except Exception:
        continue

# ConfigMaps a container may take its env from (envFrom configMapRef).
configmaps = {}
for _, d in docs:
    if d.get("kind") == "ConfigMap":
        name = (d.get("metadata", {}) or {}).get("name")
        if name:
            configmaps[name] = {k: str(v) for k, v in (d.get("data", {}) or {}).items()}

def as_int(value, default):
    try:
        return int(value)
    except (TypeError, ValueError):
        return default

rows = []
for path, d in docs:
    if d.get("kind") not in ("Deployment", "Rollout"):
        continue
    spec = d.get("spec", {}) or {}
    tmpl = (spec.get("template", {}) or {}).get("spec", {}) or {}
    containers = tmpl.get("containers", []) or []
    # DB-connecting = mounts the shared memql-secrets (carries the DSN).
    connects = False
    max_open, gate = def_open, def_gate
    for c in containers:
        # The container's env as Kubernetes resolves it: envFrom sources in
        # order, then env, each overriding what came before.
        env = {}
        for ef in (c.get("envFrom", []) or []):
            if (ef.get("secretRef", {}) or {}).get("name") == "memql-secrets":
                connects = True
            ref = (ef.get("configMapRef", {}) or {}).get("name")
            if ref in configmaps:
                env.update(configmaps[ref])
        for e in (c.get("env", []) or []):
            if e.get("name") and e.get("value") not in (None, ""):
                env[e["name"]] = str(e["value"])
        if MAIN_KEY in env:
            max_open = as_int(env[MAIN_KEY], max_open)
        if GATE_KEY in env:
            # The process takes a size below one as the default (gate_pool.go).
            gate = as_int(env[GATE_KEY], gate)
            if gate < 1:
                gate = def_gate
    if not connects:
        continue
    name = (d.get("metadata", {}) or {}).get("name", path)
    replicas = int(spec.get("replicas", 1) or 1)
    # surge
    surge = 0
    strat = spec.get("strategy", {}) or {}
    ru = strat.get("rollingUpdate", {}) or {}
    ms = ru.get("maxSurge", 0)
    if isinstance(ms, str) and ms.endswith("%"):
        surge = (replicas * int(ms[:-1]) + 99)//100
    elif ms not in (None, "",):
        try: surge = int(ms)
        except ValueError: surge = 0
    rows.append({"name": name, "replicas": replicas, "max_open": max_open,
                 "gate": gate, "surge": surge})

# Collapse the bff variants (the plain Deployment + the blue-green
# bff-blue/bff-green) into ONE logical "bff" node -- only one strategy is live
# at a time. Keep the max replicas seen; the blue-green overlap (preview+active
# both holding pools) is added as a peak term below, so we don't double-count.
def norm(name):
    if name.startswith("bff"):
        return "bff"
    return name
by_name = {}
for r in rows:
    key = norm(r["name"])
    cur = by_name.get(key)
    if cur is None or r["replicas"] > cur["replicas"]:
        by_name[key] = {**r, "name": key}
rows = sorted(by_name.values(), key=lambda r: r["name"])

steady = 0
peak = 0
print(f"{'node':<14}{'replicas':>9}{'main':>6}{'direct':>7}{'migr':>6}{'gate':>6}{'perProc':>8}{'surge':>7}{'steady':>9}{'peak':>7}")
for r in rows:
    n, rep, mo, gt, sg = r["name"], r["replicas"], r["max_open"], r["gate"], r["surge"]
    per = mo + direct + migration + gt
    s = rep * per
    p = (rep + sg) * per
    if n.startswith("bff"):
        p += rep * per  # blue-green overlap: preview color holds a second set of pools
    steady += s
    peak += p
    print(f"{n:<14}{rep:>9}{mo:>6}{direct:>7}{migration:>6}{gt:>6}{per:>8}{sg:>7}{s:>9}{p:>7}")

if foreign:
    print(f"\nmax_connections={maxc}  reserved={reserved}  foreign(live non-mesh)={foreign}  budget(usable)={budget}")
else:
    print(f"\nmax_connections={maxc}  reserved={reserved}  budget(usable)={budget}")
print(f"steady total={steady}  peak total(surge+blue-green)={peak}")

fail = False
if peak > budget:
    print(f"CONN-HEADROOM-FAIL: peak {peak} > budget {budget} "
          f"-- a roll would exhaust connections (SQLSTATE 53300). "
          f"Lower MEMORY_NODES_DATABASE_MAX_OPEN_CONNS or MEMQL_DB_GATE_POOL_SIZE, cut replicas/surge, add a pooler, or raise max_connections.")
    fail = True
elif steady > budget:
    print(f"CONN-HEADROOM-FAIL: steady {steady} > budget {budget} -- already oversubscribed.")
    fail = True
elif steady > int(budget * 0.8):
    print(f"CONN-HEADROOM-WARN: steady {steady} > 80% of budget {budget} -- little headroom.")
else:
    print("CONN-HEADROOM-OK: within budget.")

sys.exit(1 if fail else 0)
PY
}

function main() {
    parse_arguments "$@"
    echo "INFO: connection-headroom gate (manifests: $MANIFESTS)"
    resolve_live
    compute_and_check
}

main "$@"
