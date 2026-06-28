#!/usr/bin/env bash
#
# scripts/deploy/build-image.sh
# =============================
#
# Capability: deploy.buildImage -- build one engine node image at a version.
#
# Backend for the `buildEngineImage` deploy action (I8, #2222). The AUTHORITATIVE
# release build runs on the GitHub build server (OIDC -> ACR); this is the
# local / no-op surface that the cockpit runner drives for development and
# replay. dryRun (default true) reports the intended build without invoking
# docker.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 docker absent (when applying) | 5 build failed
#
# Refs: #2222 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.buildImage" "Build a single engine node image at a version."
cap_spec_param "nodeType" "engine node type (identity/cognition/voice/agent/...)" "MEMQL_DEPLOY_NODE_TYPE"
cap_spec_param "version"  "version tag to build"                                  "MEMQL_DEPLOY_VERSION"
cap_spec_param "workdir"  "absolute path of the checked-out repository"           "MEMQL_DEPLOY_WORKDIR"
cap_spec_param "dryRun"   "report the intended build without performing it"       "MEMQL_DEPLOY_DRY_RUN"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local nodeType version workdir dry image
    nodeType="$(cap_param nodeType "")"
    version="$(cap_param version "")"
    workdir="$(cap_param workdir "")"
    dry="$(cap_param dryRun "true")"
    cap_require nodeType "$nodeType"
    cap_require version "$version"
    cap_require workdir "$workdir"

    image="memql-${nodeType}:${version}"
    cap_result_set nodeType "$nodeType"
    cap_result_set version  "$version"
    cap_result_set image    "$image"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would build ${image} from ${workdir}"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v docker &>/dev/null; then
        cap_fail 4 "docker is not installed on the runner"
    fi
    cap_info "Building ${image} from ${workdir}..."
    ( cd "$workdir" && docker build -t "$image" . ) >&2 || cap_fail 5 "docker build of ${image} failed"
    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
