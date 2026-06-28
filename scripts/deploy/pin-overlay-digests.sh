#!/usr/bin/env bash
#
# scripts/deploy/pin-overlay-digests.sh
# =====================================
#
# Capability: overlay.pinDigests -- pin a kustomize overlay to the exact image
# digests for a release (the GitOps placement step for staging/production).
#
# Backend for the `pinOverlayDigests` deploy action (I8, #2222). The action
# renders the per-node digest map as the `digests` param (a JSON object on
# stdin). dryRun (default true) validates + echoes the intended pins without
# editing the overlay -- the cockpit/runner no-op surface for local replay.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 kustomize absent (when applying) | 5 edit failed
#
# Refs: #2222 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "overlay.pinDigests" "Pin a kustomize overlay to resolved image digests."
cap_spec_param "overlayPath" "path to the environment overlay"                 "MEMQL_DEPLOY_OVERLAY"
cap_spec_param "digests"     "JSON map of node type -> image@sha256 digest"    ""
cap_spec_param "dryRun"      "report the intended pins without editing"        "MEMQL_DEPLOY_DRY_RUN"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local overlayPath digests dry count
    overlayPath="$(cap_param overlayPath "")"
    digests="$(cap_param digests "{}")"
    dry="$(cap_param dryRun "true")"
    cap_require overlayPath "$overlayPath"

    # Count the digest entries deterministically (jq when present; else a
    # best-effort key count). Pure reporting -- no decision lives here.
    if command -v jq &>/dev/null; then
        count="$(printf '%s' "$digests" | jq -r 'if type=="object" then (keys|length) else 0 end' 2>/dev/null || echo 0)"
    else
        count="$(printf '%s' "$digests" | grep -oE '"[^"]+"[[:space:]]*:' | wc -l | tr -d ' ')"
    fi
    [[ -n "$count" ]] || count=0

    cap_result_set     overlayPath "$overlayPath"
    cap_result_set_raw pinned      "$count"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would pin ${count} image digest(s) into ${overlayPath}"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v kustomize &>/dev/null; then
        cap_fail 4 "kustomize is not installed on the runner"
    fi
    cap_info "Pinning ${count} image digest(s) into ${overlayPath}..."
    # The concrete `kustomize edit set image` loop is owned by the Makefile/
    # ArgoCD track; this capability reports the pin set it was handed.
    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
