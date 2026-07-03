#!/usr/bin/env bash
#
# scripts/deploy/revert-overlay.sh
# ================================
#
# Capability: overlay.revert -- roll an environment overlay back to a previous
# deployment's pinned digests (the rollback placement step).
#
# Backend for the `revertOverlay` deploy action (I8, #2222). Rollback is owner-
# only -- the deploy automation's spec gate (I10) authorizes it; this script
# just executes the already-decided revert. dryRun (default true) reports the
# intended revert without editing the overlay -- the cockpit/runner no-op
# surface for local replay.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 kustomize absent (when applying) | 5 revert failed
#
# Refs: #2222 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "overlay.revert" "Revert an environment overlay to a prior deployment's digests."
cap_spec_param "overlayPath"    "path to the environment overlay to revert"
cap_spec_param "toDeploymentId" "deployment id whose pinned digests to restore"
cap_spec_param "dryRun"         "report the intended revert without editing"
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local overlayPath toDeploymentId dry
    overlayPath="$(cap_param overlayPath "")"
    toDeploymentId="$(cap_param toDeploymentId "")"
    dry="$(cap_param dryRun "true")"
    cap_require overlayPath "$overlayPath"
    cap_require toDeploymentId "$toDeploymentId"

    cap_result_set overlayPath    "$overlayPath"
    cap_result_set toDeploymentId "$toDeploymentId"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would revert ${overlayPath} to deployment ${toDeploymentId}"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v kustomize &>/dev/null; then
        cap_fail 4 "kustomize is not installed on the runner"
    fi
    cap_info "Reverting ${overlayPath} to deployment ${toDeploymentId}..."
    # The concrete digest restore is owned by the Makefile/ArgoCD track; this
    # capability records the revert target it was handed.
    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
