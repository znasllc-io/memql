#!/usr/bin/env bash
#
# scripts/release/assemble-lockfile.sh
# ====================================
#
# Assemble a release lockfile from per-component digests (deployment-v2 Phase 4,
# znasllc-io/memql#702). Each repo's CI emits its built image digest; an
# assembly step (the release-lockfile workflow, or an operator) collects the release
# digests and writes releases/<version>.yaml. The lockfile is then PR'd; the
# coherence-check gate validates it.
#
# Digests are supplied via env vars (so CI can export them from its build
# outputs) or --<component>=sha256:... flags:
#   DIGEST_memql_identity, DIGEST_memql_cognition, DIGEST_memql_voice,
#   DIGEST_memql_agent, DIGEST_memql_planner, DIGEST_memql_workbench,
#   DIGEST_memql_bff_copresent, DIGEST_copresent
#
# Usage: assemble-lockfile.sh --version=X.Y.Z --engine-version=X.Y.Z \
#            [--gate=deep] [--out=releases/X.Y.Z.yaml] [--<comp>=sha256:...]
#
# Function-based per the Skills+Scripts convention (CLAUDE.md). set -uo pipefail.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

COMPONENTS="memql-identity memql-cognition memql-voice memql-mcp memql-agent memql-planner memql-workbench memql-bff-copresent copresent"

function info() { echo "INFO: $*"; }

function repo_for() {
    case "$1" in
        memql-bff-copresent) echo "visionarys-io/memql-bff-copresent" ;;
        copresent)           echo "visionarys-io/copresent" ;;
        *)                   echo "znasllc-io/memql" ;;
    esac
}

# digest_for COMP -> resolves from --flag override (DIGEST_OVERRIDE_<comp>) or
# the DIGEST_<comp_with_underscores> env var.
function digest_for() {
    local comp="$1" var ov
    ov="DIGEST_OVERRIDE_${comp//-/_}"
    [ -n "${!ov:-}" ] && { echo "${!ov}"; return; }
    var="DIGEST_${comp//-/_}"
    echo "${!var:-}"
}

function parse_arguments() {
    VERSION=""; ENGINE_VERSION=""; GATE="deep"; OUT=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --version=*)        VERSION="${1#*=}"; shift ;;
            --engine-version=*) ENGINE_VERSION="${1#*=}"; shift ;;
            --gate=*)           GATE="${1#*=}"; shift ;;
            --out=*)            OUT="${1#*=}"; shift ;;
            --*=*)
                # --<component>=digest override, e.g. --copresent=sha256:...
                local key="${1%%=*}"; key="${key#--}"
                local val="${1#*=}"
                export "DIGEST_OVERRIDE_${key//-/_}=$val"
                shift ;;
            *) echo "ERROR: unknown option: $1"; exit 2 ;;
        esac
    done
    [ -z "$VERSION" ] && { echo "ERROR: --version required"; exit 2; }
    [ -z "$ENGINE_VERSION" ] && ENGINE_VERSION="$VERSION"
    [ -z "$OUT" ] && OUT="$REPO_ROOT/releases/$VERSION.yaml"
}

function write_lockfile() {
    {
        echo "# Release lockfile (deployment-v2 Phase 4, #702). Assembled by"
        echo "# scripts/release/assemble-lockfile.sh from per-repo CI digests."
        echo "version: \"$VERSION\""
        echo "validatedAt: \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\""
        echo "gate: \"$GATE\""
        echo "engineVersion: \"$ENGINE_VERSION\""
        echo "components:"
        local comp d
        for comp in $COMPONENTS; do
            d="$(digest_for "$comp")"
            [ -z "$d" ] && { echo "ERROR: no digest for $comp (set DIGEST_${comp//-/_})" >&2; exit 1; }
            echo "  $comp:"
            echo "    repo: $(repo_for "$comp")"
            echo "    digest: $d"
            if [ "$comp" = "memql-bff-copresent" ] || [ "$comp" = "copresent" ]; then
                echo "    builtAgainstEngine: \"$ENGINE_VERSION\""
            fi
        done
    } > "$OUT"
    info "wrote $OUT"
}

function main() {
    parse_arguments "$@"
    write_lockfile
    info "validating the assembled lockfile..."
    bash "$SCRIPT_DIR/coherence-check.sh" "$OUT"
}

main "$@"
