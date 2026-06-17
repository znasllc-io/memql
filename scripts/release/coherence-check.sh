#!/usr/bin/env bash
#
# scripts/release/coherence-check.sh
# ==================================
#
# Validate a release lockfile (deployment-v2 Phase 4, znasllc-io/memql#702).
# A lockfile (releases/<version>.yaml) pins all release components by @sha256: digest;
# this is the CI gate that keeps it honest:
#
#   1. Every expected component is present.
#   2. Every digest is a real sha256: digest (no floating tags).
#   3. CROSS-REPO COHERENCE: the carrier (memql-bff-copresent) and the SPA
#      (copresent) each record builtAgainstEngine == the lockfile engineVersion.
#      This replaces the implicit "remember to rebuild the BFF against the new
#      engine" -- an incoherent set fails CI instead of shipping.
#
# Usage: coherence-check.sh releases/<version>.yaml [more.yaml ...]
#        coherence-check.sh --all      # every releases/*.yaml
#
# Function-based per the Skills+Scripts convention (CLAUDE.md). set -uo pipefail.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# REQUIRED = present in every lockfile. OPTIONAL = allowed (digest-checked when
# present) but NOT required, so historical lockfiles cut before a node existed
# still validate. memql-mcp is optional: introduced in 0.9.61 (memql#1554), so
# pre-0.9.61 lockfiles legitimately don't carry it; the assemble script always
# emits it for new releases.
REQUIRED_COMPONENTS="memql-identity memql-cognition memql-voice memql-agent memql-planner memql-workbench memql-bff-copresent copresent"
OPTIONAL_COMPONENTS="memql-mcp"

function info() { echo "INFO: $*"; }
function fail() { echo "COHERENCE-FAIL: $*"; }

# check_one FILE -> 0 ok / 1 fail. Delegates the YAML parse to python3.
function check_one() {
    local file="$1"
    info "checking $file"
    REQUIRED="$REQUIRED_COMPONENTS" OPTIONAL="$OPTIONAL_COMPONENTS" python3 - "$file" <<'PY'
import os, sys, re
try:
    import yaml
except Exception:
    print("COHERENCE-FAIL: python yaml unavailable"); sys.exit(2)
path = sys.argv[1]
with open(path) as f:
    doc = yaml.safe_load(f)
required = os.environ["REQUIRED"].split()
optional = os.environ.get("OPTIONAL", "").split()
allowed = set(required) | set(optional)
errs = []
engine_version = str(doc.get("engineVersion", "")).strip()
if not engine_version:
    errs.append("missing engineVersion")
comps = doc.get("components", {}) or {}
# REQUIRED components must be present.
for name in required:
    if name not in comps:
        errs.append(f"missing component: {name}")
# Every PRESENT allowed component (required + any optional present) must be
# digest-pinned; the carrier + SPA must be coherent with the engine version.
for name, c in comps.items():
    if name not in allowed:
        continue
    c = c or {}
    digest = str(c.get("digest", ""))
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        errs.append(f"{name}: digest not a sha256: pin -> {digest!r}")
    if name in ("memql-bff-copresent", "copresent"):
        ba = str(c.get("builtAgainstEngine", "")).strip()
        if ba != engine_version:
            errs.append(f"{name}: builtAgainstEngine={ba!r} != engineVersion={engine_version!r} (incoherent set)")
extra = [n for n in comps if n not in allowed]
if extra:
    errs.append(f"unexpected components: {extra}")
if errs:
    for e in errs:
        print("COHERENCE-FAIL:", e)
    sys.exit(1)
print(f"INFO: OK -- {path}: {len(comps)} components digest-pinned + coherent (engine {engine_version})")
PY
}

function main() {
    local files=()
    if [ "${1:-}" = "--all" ]; then
        local f
        for f in "$REPO_ROOT"/releases/*.yaml; do
            [ -e "$f" ] && files+=("$f")
        done
    else
        files=("$@")
    fi
    if [ "${#files[@]}" -eq 0 ]; then
        echo "usage: $0 releases/<version>.yaml [...]  |  $0 --all"; exit 2
    fi
    local rc=0 f
    for f in "${files[@]}"; do
        check_one "$f" || rc=1
    done
    [ "$rc" -eq 0 ] && info "all lockfiles coherent." || fail "one or more lockfiles failed."
    return "$rc"
}

main "$@"
