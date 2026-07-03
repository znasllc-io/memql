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
# shellcheck source=../lib/engine_build_args.sh
source "${SCRIPT_DIR}/../lib/engine_build_args.sh"

cap_init "deploy.buildImage" "Build a single engine node image at a version."
cap_spec_param "nodeType" "engine node type (identity/cognition/voice/agent/...)"
cap_spec_param "version"  "version tag to build"
cap_spec_param "workdir"  "absolute path of the checked-out repository"
cap_spec_param "dryRun"   "report the intended build without performing it"
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

    # Per-node build args (memql#2379): BUILD_TAGS selects the node-type
    # binary; voice needs CGO + the voice-runtime stage. Shared mapping with
    # scripts/k3d/dev.sh via scripts/lib/engine_build_args.sh -- without it
    # every node type built as the bff-default binary.
    engine_build_args_for_node "$nodeType"
    cap_result_set buildTags "$nodeType"
    cap_result_set target "$ENGINE_BUILD_TARGET"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would build ${image} from ${workdir} (BUILD_TAGS=${nodeType}, target=${ENGINE_BUILD_TARGET})"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v docker &>/dev/null; then
        cap_fail 4 "docker is not installed on the runner"
    fi
    cap_info "Building ${image} from ${workdir} (BUILD_TAGS=${nodeType}, target=${ENGINE_BUILD_TARGET})..."
    ( cd "$workdir" && docker build "${ENGINE_BUILD_ARGS[@]}" --target "${ENGINE_BUILD_TARGET}" -t "$image" . ) >&2 || cap_fail 5 "docker build of ${image} failed"
    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
