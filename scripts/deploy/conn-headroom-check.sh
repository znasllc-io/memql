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
# What changed with the cutover: `max_connections` is ours to set (200 local,
# 400 staging/prod, in deploy/k8s/components/cnpg-db), the reserved slots are
# Postgres's own superuser reservation rather than Tiger's ops overhead, and
# the DSN no longer has a `tiger` CLI fallback -- there is no Tiger service to
# ask.
#
# Budget contract (from the spike findings,
# docs/internal/ops/conn-exhaustion-53300-spike.md):
#
#     Sigma(replicas x MAX_OPEN_CONNS) + rollSurge + blueGreenOverlap
#         <= max_connections - reserved
#
# It reads every DB-connecting Deployment under the manifests dir, takes each
# one's replicas, per-pod MAX_OPEN_CONNS (env on the container, else the code
# default), and RollingUpdate maxSurge, adds a blue-green overlap term for bff,
# and compares the steady + peak totals against (max_connections - reserved).
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

# The SMALLEST max_connections we run anywhere (the local/entry preset), so
# the default is conservative: staging and production set 400, and --live
# reads the real value off the instance anyway.
DEFAULT_MAX_CONNECTIONS="${MAX_CONNECTIONS:-200}"   # deploy/k8s/components/cnpg-db
# superuser_reserved_connections (3 by default) + CNPG's own instance-manager
# and monitoring connections. No Tiger ops overhead to leave room for.
DEFAULT_RESERVED="${RESERVED_CONNECTIONS:-10}"      # superuser + operator connections
DEFAULT_MAX_OPEN="${MAX_OPEN_CONNS:-10}"            # per-pod pool default (database.go)
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
  Sigma(replicas x MAX_OPEN_CONNS) + surge + blue-green overlap <= max_connections - reserved

Options:
  --max-connections=N   Instance max_connections (default $DEFAULT_MAX_CONNECTIONS / env MAX_CONNECTIONS)
  --reserved=N          Reserved slots (default $DEFAULT_RESERVED / env RESERVED_CONNECTIONS)
  --default-max-open=N  Per-pod pool when the container sets no MAX_OPEN_CONNS (default $DEFAULT_MAX_OPEN)
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
    MANIFESTS="$DEFAULT_MANIFESTS"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --max-connections=*) MAX_CONNECTIONS="${1#*=}";;
            --reserved=*)        RESERVED="${1#*=}";;
            --default-max-open=*) DEF_MAX_OPEN="${1#*=}";;
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
    DEF_MAX_OPEN="$DEF_MAX_OPEN" MANIFESTS="$MANIFESTS" \
    LIVE_FOREIGN="$LIVE_FOREIGN" \
    python3 - <<'PY'
import os, sys, glob
try:
    import yaml
except Exception:
    print("CONN-HEADROOM-FAIL: python yaml unavailable"); sys.exit(2)

maxc = int(os.environ["MAX_CONNECTIONS"])
reserved = int(os.environ["RESERVED"])
def_open = int(os.environ["DEF_MAX_OPEN"])
mdir = os.environ["MANIFESTS"]
foreign = int(os.environ.get("LIVE_FOREIGN", "0") or 0)
budget = maxc - reserved - foreign

rows = []
for path in sorted(glob.glob(os.path.join(mdir, "*.yaml"))):
    try:
        docs = [d for d in yaml.safe_load_all(open(path)) if d]
    except Exception:
        continue
    for d in docs:
        if d.get("kind") not in ("Deployment", "Rollout"):
            continue
        spec = d.get("spec", {}) or {}
        tmpl = (spec.get("template", {}) or {}).get("spec", {}) or {}
        containers = tmpl.get("containers", []) or []
        # DB-connecting = mounts the shared memql-secrets (carries the DSN).
        connects = False
        max_open = def_open
        for c in containers:
            for ef in (c.get("envFrom", []) or []):
                if (ef.get("secretRef", {}) or {}).get("name") == "memql-secrets":
                    connects = True
            for e in (c.get("env", []) or []):
                if e.get("name") == "MAX_OPEN_CONNS" and e.get("value"):
                    try: max_open = int(e["value"])
                    except ValueError: pass
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
                     "surge": surge})

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
print(f"{'node':<14}{'replicas':>9}{'maxOpen':>9}{'surge':>7}{'steady':>9}{'peak':>7}")
for r in rows:
    n, rep, mo, sg = r["name"], r["replicas"], r["max_open"], r["surge"]
    s = rep * mo
    p = (rep + sg) * mo
    if n.startswith("bff"):
        p += rep * mo  # blue-green overlap: preview color holds a second pool
    steady += s
    peak += p
    print(f"{n:<14}{rep:>9}{mo:>9}{sg:>7}{s:>9}{p:>7}")

if foreign:
    print(f"\nmax_connections={maxc}  reserved={reserved}  foreign(live non-mesh)={foreign}  budget(usable)={budget}")
else:
    print(f"\nmax_connections={maxc}  reserved={reserved}  budget(usable)={budget}")
print(f"steady total={steady}  peak total(surge+blue-green)={peak}")

fail = False
if peak > budget:
    print(f"CONN-HEADROOM-FAIL: peak {peak} > budget {budget} "
          f"-- a roll would exhaust connections (SQLSTATE 53300). "
          f"Lower MAX_OPEN_CONNS, cut replicas/surge, add a pooler, or raise max_connections.")
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
