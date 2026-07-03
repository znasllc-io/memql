#!/usr/bin/env bash
#
# scripts/deploy/argo-sync.sh
# ===========================
#
# Capability: argocd.sync -- trigger an ArgoCD application sync to a target
# revision.
#
# Backend for the `argoSync` deploy action (I8, #2222). dryRun (default true)
# reports the intended sync without contacting ArgoCD -- the cockpit/runner
# no-op surface for local replay.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 4 argocd absent (when applying) | 5 sync failed
#
# Refs: #2222 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "argocd.sync" "Trigger an ArgoCD application sync to a target revision."
cap_spec_param "app"      "ArgoCD application name"
cap_spec_param "revision" "git revision (tag/SHA) to sync to"
cap_spec_param "dryRun"   "report the intended sync without performing it"
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local app revision dry
    app="$(cap_param app "")"
    revision="$(cap_param revision "")"
    dry="$(cap_param dryRun "true")"
    cap_require app "$app"
    cap_require revision "$revision"

    cap_result_set app      "$app"
    cap_result_set revision "$revision"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would sync ArgoCD app ${app} to ${revision}"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v argocd &>/dev/null; then
        cap_fail 4 "argocd CLI is not installed on the runner"
    fi
    cap_info "Syncing ArgoCD app ${app} to ${revision}..."
    argocd app sync "$app" --revision "$revision" >&2 || cap_fail 5 "argocd sync of ${app} failed"
    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
