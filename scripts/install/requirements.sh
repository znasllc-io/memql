#!/usr/bin/env bash
#
# scripts/install/requirements.sh
# ===============================
#
# Capability: install.requirements -- refuse to create a cluster on a machine
# below the one-machine minimum requirements.
#
# A local install runs the whole of MemQL here: the cluster and the local models
# that answer chat, embeddings, speech recognition and speech. On a machine
# below the minimum (docs/public/operate/minimum-requirements.md) the install
# used to run to the end and fail, or run badly, once the models loaded. This
# step runs right after install.detect and before anything changes the machine,
# so the answer arrives while saying no still costs nothing.
#
# THE FIGURES AND PROBES ARE NOT HERE. They live in scripts/lib/requirements.sh,
# which install.detect reports from and scripts/k3d/up.sh refuses from. A
# threshold written twice is two opinions about one machine.
#
# A MACHINE THAT ALREADY RUNS THE CLUSTER IS WARNED, NEVER REFUSED. A repair or
# a continued setup runs this graph too, and refusing it would strand a cluster
# that is already here. Which cluster is the --cluster param, the same name
# k3d.up creates.
#
# THE OVERRIDE IS EXPLICIT. --allow-below-minimum (default from
# MEMQL_ALLOW_BELOW_MINIMUM) continues below the minimum with a warning. It
# exists for the CI lanes, whose runners have no GPU, and for a developer who
# accepts a slow machine knowingly. Nothing sets it implicitly.
#
# READ-ONLY. This step measures and never changes anything, so `changed` stays
# false.
#
# This is a CAPABILITY SCRIPT: non-interactive, structured params in, a single
# JSON result envelope on stdout, human logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Usage:
#   scripts/install/requirements.sh
#   scripts/install/requirements.sh --cluster=memql --path="$HOME"
#   scripts/install/requirements.sh --allow-below-minimum
#   scripts/install/requirements.sh --print-spec
#
# Exit codes: 0 ok | 2 bad param | 3 refused (below the minimum, no cluster yet)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/platform.sh
source "${SCRIPT_DIR}/../lib/platform.sh"
# shellcheck source=../lib/requirements.sh
source "${SCRIPT_DIR}/../lib/requirements.sh"

cap_init "install.requirements" "Check this machine against the minimum requirements before a cluster is created (read-only)."
cap_spec_param "cluster" "the cluster this install creates (default: memql); a machine that already runs it is warned, never refused"
cap_spec_param "path" "filesystem path to measure free space on (default: \$HOME)"
cap_spec_param "allow-below-minimum" "continue below the minimum with a warning (default: \$MEMQL_ALLOW_BELOW_MINIMUM, else false)"

# cluster_exists <name> -- 0 when k3d already has a cluster of that name.
function cluster_exists() {
    local name _rest
    command -v k3d &>/dev/null || return 1
    while read -r name _rest; do
        [[ "$name" == "$1" ]] && return 0
    done < <(k3d cluster list --no-headers 2>/dev/null || true)
    return 1
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local cluster path allow line
    cluster="$(cap_param cluster "${MEMQL_K3D_CLUSTER:-memql}")"
    path="$(cap_param path "${HOME:-/}")"
    allow="$(cap_bool_str allow-below-minimum "${MEMQL_ALLOW_BELOW_MINIMUM:-false}")"
    cap_require cluster "$cluster"
    cap_require path "$path"

    cap_step "checking this computer against the minimum requirements"
    requirements_evaluate "$path"
    while IFS= read -r line; do
        [[ -n "$line" ]] && cap_info "$line"
    done <<< "$REQ_SUMMARY"

    cap_result_set_raw requirements "$(requirements_json)"
    cap_result_set_raw meetsMinimum "$REQ_MEETS_MINIMUM"

    if [[ "$REQ_MEETS_MINIMUM" == "true" ]]; then
        cap_result_set_raw ready true
        cap_ok
    fi

    local shortfalls
    shortfalls="$(requirements_shortfalls_line)"
    if cluster_exists "$cluster"; then
        cap_warn "this computer is below the minimum requirements: ${shortfalls}"
        cap_warn "cluster '${cluster}' is already here, so the install continues; expect it to run slowly."
        cap_result_set_raw ready true
        cap_result_set_raw clusterExists true
        cap_ok
    fi
    if [[ "$allow" == "true" ]]; then
        cap_warn "this computer is below the minimum requirements: ${shortfalls}"
        cap_warn "continuing because allow-below-minimum is set."
        cap_result_set_raw ready true
        cap_result_set_raw allowedBelowMinimum true
        cap_ok
    fi

    cap_result_set_raw ready false
    # 3, not 4. A few shortfalls have a fix the person can make (Docker's
    # memory, free disk, a GPU driver) and the message names it, but memory and
    # the GPU itself cannot be installed, so "supply the missing piece and
    # retry" would be a false promise for most of them.
    cap_fail 3 "This computer does not meet the minimum requirements for MemQL. ${shortfalls} See ${REQ_DOCS_URL}"
}

main "$@"
