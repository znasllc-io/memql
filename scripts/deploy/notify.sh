#!/usr/bin/env bash
#
# scripts/deploy/notify.sh
# ========================
#
# Capability: deploy.notify -- post a deploy status notification.
#
# Backend for the `notifyDeploy` deploy action (I8, #2222). Local/no-op
# notifier: it logs the status to stderr and reports it in the result envelope.
# Where a webhook is configured (MEMQL_DEPLOY_NOTIFY_WEBHOOK) and dryRun=false,
# it POSTs a small JSON body; otherwise it is a clean no-op. An
# integration.slack.* engine-tool backend can replace this where Slack is wired.
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok | 2 bad param | 5 webhook post failed
#
# Refs: #2222 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.notify" "Post a deploy status notification."
cap_spec_param "env"          "target environment the deploy ran against"
cap_spec_param "status"       "deploy outcome (succeeded/failed)"
cap_spec_param "deploymentId" "deployment id the notification refers to"
cap_spec_param "dryRun"       "log only; do not POST to a webhook"
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local env status deploymentId dry webhook
    env="$(cap_param env "")"
    status="$(cap_param status "")"
    deploymentId="$(cap_param deploymentId "")"
    dry="$(cap_param dryRun "true")"
    webhook="${MEMQL_DEPLOY_NOTIFY_WEBHOOK:-}"
    cap_require env "$env"
    cap_require status "$status"
    cap_require deploymentId "$deploymentId"

    cap_info "deploy ${deploymentId} on ${env}: ${status}"
    cap_result_set env          "$env"
    cap_result_set status       "$status"
    cap_result_set deploymentId "$deploymentId"

    if [[ "$dry" == "false" && -n "$webhook" ]]; then
        if ! command -v curl &>/dev/null; then
            cap_fail 5 "curl is not installed; cannot POST to the notify webhook"
        fi
        cap_info "Posting notification to webhook..."
        curl -fsS -X POST -H 'Content-Type: application/json' \
            --data "{\"env\":\"${env}\",\"status\":\"${status}\",\"deploymentId\":\"${deploymentId}\"}" \
            "$webhook" >&2 || cap_fail 5 "notify webhook POST failed"
        cap_changed
        cap_result_set_raw notified true
        cap_ok
    fi

    cap_result_set_raw notified false
    cap_ok
}

main "$@"
