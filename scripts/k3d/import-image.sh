#!/usr/bin/env bash
#
# scripts/k3d/import-image.sh
# ===========================
#
# Capability: k3d.importImage -- import a built engine node image into the
# local k3d cluster (the development-environment placement step).
#
# Backend for the `importImageToK3d` deploy action (I8, #2222). dryRun
# (default true) reports the intended import without invoking k3d -- the
# cockpit/runner no-op surface for local replay.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 k3d absent (when applying) | 5 import failed
#
# Refs: #2222 #2221 #2061

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "k3d.importImage" "Import a built image into the local k3d cluster."
cap_spec_param "nodeType" "engine node type whose image to import (memql-<type>:<version>)"
cap_spec_param "image"    "explicit image ref to import (alternative to nodeType; e.g. a downstream frontend image)"
cap_spec_param "cluster"  "target k3d cluster name"
cap_spec_param "version"  "image version tag to import (nodeType form only)"
cap_spec_param "dryRun"   "report the intended import without performing it"
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local nodeType cluster version dry image
    nodeType="$(cap_param nodeType "")"
    image="$(cap_param image "")"
    cluster="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    version="$(cap_param version "dev")"
    dry="$(cap_param dryRun "true")"
    cap_require cluster "$cluster"
    if [ -z "$nodeType" ] && [ -z "$image" ]; then
        cap_fail 2 "one of nodeType or image is required"
    fi

    # nodeType names an engine image; --image imports any locally-built ref
    # (the seam a downstream stack uses for its own images, e.g. a frontend).
    if [ -z "$image" ]; then
        image="memql-${nodeType}:${version}"
    fi
    cap_result_set nodeType "$nodeType"
    cap_result_set cluster  "$cluster"
    cap_result_set image    "$image"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would import ${image} into k3d cluster ${cluster}"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v k3d &>/dev/null; then
        cap_fail 4 "k3d is not installed on the runner"
    fi
    cap_info "Importing ${image} into k3d cluster ${cluster}..."
    k3d image import "$image" -c "$cluster" >&2 || cap_fail 5 "k3d image import of ${image} failed"
    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
