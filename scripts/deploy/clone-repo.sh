#!/usr/bin/env bash
#
# scripts/deploy/clone-repo.sh
# ============================
#
# Capability: deploy.cloneRepo -- check out a repository working tree at a
# pinned ref so a build runs against a known version.
#
# Backend for the `cloneRepoAtVersion` deploy action (I8, #2222). Capability
# script: non-interactive, structured params in (JSON on stdin via
# --params-stdin, or --flag=value), one JSON result envelope on stdout, human
# logs on stderr, honest exit codes.
# Contract: docs/internal/design/capability-script-contract.md
#
# Idempotent: checking out a ref already at HEAD is a no-op (changed=false).
# dryRun (default true) reports the intended checkout without touching the tree
# -- the cockpit/runner no-op surface for local replay.
#
# Exit codes: 0 ok | 2 bad param | 4 git absent (when applying) | 5 checkout failed
#
# Refs: #2222 #2221

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/capability.sh
source "${SCRIPT_DIR}/../lib/capability.sh"

cap_init "deploy.cloneRepo" "Check out a repository working tree at a pinned ref."
cap_spec_param "workdir" "absolute path of the repository working tree"
cap_spec_param "ref"     "git ref (branch, tag, or SHA) to check out"
cap_spec_param "dryRun"  "report the intended checkout without performing it"
function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    local workdir ref dry
    workdir="$(cap_param workdir "")"
    ref="$(cap_param ref "")"
    dry="$(cap_param dryRun "true")"
    cap_require workdir "$workdir"
    cap_require ref "$ref"

    cap_result_set     workdir "$workdir"
    cap_result_set     ref     "$ref"

    if [[ "$dry" != "false" ]]; then
        cap_info "[dry-run] would checkout ${ref} in ${workdir}"
        cap_result_set_raw dryRun true
        cap_ok
    fi

    if ! command -v git &>/dev/null; then
        cap_fail 4 "git is not installed on the runner"
    fi
    cap_info "Fetching and checking out ${ref} in ${workdir}..."
    git -C "$workdir" fetch --all --tags >&2 || cap_fail 5 "git fetch failed in ${workdir}"
    git -C "$workdir" checkout "$ref" >&2     || cap_fail 5 "git checkout ${ref} failed in ${workdir}"
    cap_changed
    cap_result_set_raw dryRun false
    cap_ok
}

main "$@"
