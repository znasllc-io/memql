#!/usr/bin/env bash
#
# scripts/deploy/deploy-gate.sh
# =============================
#
# Capability: deploy.gate -- run the post-deploy gate for an environment and
# report a structured pass/fail result.
#
# Backend for the `runDeployGate` deploy action (I8, #2222). The result's
# `passed` boolean is the input to the pure logic deployGateGreen
# (dsl/deployment/logic.memql), which the deploy automation (I10) branches
# success/rollback on. dryRun (default true) returns passed=true for a no-op
# target so a local replay exercises the success branch; an apply run delegates
# to scripts/deploy/post-deploy-gate.sh when present.
#
# NOTE: this capability REPORTS the gate outcome; the decision (auto-promote vs
# rollback) lives in DSL logic, not here (contract rule 7).
#
# Capability-script contract: docs/internal/design/capability-script-contract.md
# Exit codes: 0 ok (gate ran; see result.passed) | 2 bad param | 5 gate errored
#
# Refs: #2222 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.gate" "Run the post-deploy gate and report a structured pass/fail result."
cap_spec_param "env"     "target environment (development/staging/production)"
cap_spec_param "workdir" "absolute path of the checked-out repository"
cap_spec_param "dryRun"  "no-op gate that reports passed=true"
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local env workdir dry gate
    env="$(cap_param env "")"
    workdir="$(cap_param workdir "")"
    dry="$(cap_param dryRun "true")"
    cap_require env "$env"
    cap_require workdir "$workdir"

    cap_result_set env     "$env"
    cap_result_set workdir "$workdir"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] no-op gate for ${env}: passed=true"
        cap_result_set_raw passed true
        cap_result_set_raw dryRun true
        cap_ok
    fi

    gate="${workdir%/}/scripts/deploy/post-deploy-gate.sh"
    if [[ ! -x "$gate" && ! -f "$gate" ]]; then
        cap_fail 5 "post-deploy gate not found at ${gate}"
    fi
    cap_info "Running post-deploy gate for ${env}..."
    if bash "$gate" --env="$env" >&2; then
        cap_result_set_raw passed true
    else
        cap_result_set_raw passed false
    fi
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
