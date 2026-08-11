#!/usr/bin/env bash
# Capability: install.dockerAccess -- can this user actually drive Docker?
#
# WHY IT IS A STEP AND NOT A FOOTNOTE (memql#3549). `install.detect` has always
# reported whether Docker answers, and NOTHING CONSUMED IT. No step depended on
# the answer, so an install on a machine whose Docker was unreachable ran the
# whole first half regardless: it downloaded and installed k3d, kubectl and
# mkcert, edited /etc/hosts, put a certificate authority into the operator's
# browser trust store, cloned the stack -- five mutating steps, two of them
# privileged -- and only then failed at `clusterUp`, on a blocker that was fully
# known before any of it started.
#
# This step exists to be depended on. Everything that changes the machine now
# waits on it, so an unusable Docker costs an operator a sentence instead of a
# trust-store entry they have to go and remove.
#
# READ-ONLY, ALWAYS. It never installs Docker, never starts the daemon and never
# edits a group. That is deliberate: the remedy for two of the three failure
# states needs root, and this runs unprivileged as part of an automated graph.
# What it does instead is name the exact command, which the wizard offers to run
# in a terminal the operator controls (memql#3551). A gate that quietly acquired
# root to fix itself would be a much larger thing than a gate.
#
# The classification is scripts/lib/docker.sh, shared with `install.detect` so
# the inventory and the gate cannot disagree about the same machine.
#
# Exit codes:
#   0 ok (Docker is reachable)   2 bad param
#   4 prerequisite missing (Docker missing, stopped, or refusing this user)
#
# Usage:
#   scripts/install/docker-access.sh
#   scripts/install/docker-access.sh --print-spec
#
# Refs: #3549 #3551 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"
# shellcheck source=../lib/docker.sh
source "${SCRIPT_DIR}/../lib/docker.sh"

cap_init "install.dockerAccess" "Confirm this user can reach the Docker daemon before anything is installed."

#=============================================================================
# MAIN
#=============================================================================

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    cap_step "checking whether Docker is reachable by $(id -un)"

    local state remedy explanation
    state="$(docker_access_state)"
    remedy="$(docker_access_remedy "$state")"
    explanation="$(docker_access_explanation "$state")"

    cap_result_set     state       "$state"
    cap_result_set     remedy      "$remedy"
    cap_result_set     explanation "$explanation"

    if [[ "$state" == "ok" ]]; then
        cap_result_set_raw ready true
        cap_info "docker is reachable"
        # No cap_changed: this step inspects and never acts. Always.
        cap_ok
    fi

    cap_result_set_raw ready false
    cap_error "$explanation"
    [[ -n "$remedy" ]] && cap_error "Fix it with: ${remedy}"

    # 4, not 5: nothing was attempted and nothing was left half-done, and the
    # remedy is a prerequisite the operator can supply. The wizard renders 4 as
    # "Something this step needs is not on this machine ... Nothing has been
    # left half-done", which is exactly true here.
    cap_fail 4 "${explanation}${remedy:+ Fix it with: ${remedy}}"
}

main "$@"
