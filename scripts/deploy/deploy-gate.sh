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
# to ${workdir}/scripts/deploy/post-deploy-gate.sh -- which post-decouple is the
# downstream product pack checkout, not this engine tree -- and fails cleanly
# (exit 5) when that gate is absent from the workdir.
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
cap_spec_param "provider" "deploy target: docker-local | azure (default: azure)"
cap_spec_param "workdir"  "absolute path of the checked-out repository"
cap_spec_param "dryRun"  "no-op gate that reports passed=true"
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local provider workdir dry gate
    provider="$(cap_param provider "azure")"
    workdir="$(cap_param workdir "")"
    dry="$(cap_param dryRun "true")"
    cap_require workdir "$workdir"

    cap_result_set provider "$provider"
    cap_result_set workdir  "$workdir"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] no-op gate for ${provider}: passed=true"
        cap_result_set_raw passed true
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if [[ "$provider" == "docker-local" ]]; then
        # Local gate target (#2380): the k3d mesh litmus. Selecting the
        # health-check backend BY the provider param is mechanical dispatch
        # (which deploy TARGET this is, not which environment -- epic
        # memql#3943 removed the latter); the litmus itself is the k3d.status
        # capability script (deployments + per-pod unique MEMQL_NODE_ID -- the
        # `make status` check).
        litmus_script="${workdir%/}/scripts/k3d/status.sh"
        if [[ ! -f "$litmus_script" ]]; then
            cap_fail 5 "docker-local gate litmus not found at ${litmus_script}"
        fi
        cap_info "Running the docker-local gate (k3d mesh litmus)..."
        litmus_json="$(bash "$litmus_script" 2> >(cat >&2) || true)"
        if printf '%s' "$litmus_json" | grep -q '"meshHealthy": *true'; then
            cap_result_set_raw passed true
        else
            cap_result_set_raw passed false
        fi
        cap_result_set_raw dryRun false
        cap_ok
    fi

    gate="${workdir%/}/scripts/deploy/post-deploy-gate.sh"
    if [[ ! -x "$gate" && ! -f "$gate" ]]; then
        cap_fail 5 "post-deploy gate not found at ${gate}"
    fi
    cap_info "Running the post-deploy gate..."
    if bash "$gate" >&2; then
        cap_result_set_raw passed true
    else
        cap_result_set_raw passed false
    fi
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
