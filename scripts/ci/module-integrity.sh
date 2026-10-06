#!/usr/bin/env bash
# CI reporter, not a DSL capability: check the complete tracked module tree.
# The same command can run in a Workbench Job or a developer checkout.
set -uo pipefail

function show_help() {
    printf '%s\n' 'Usage: scripts/ci/module-integrity.sh [--help]' \
        'Checks workspace membership, standalone build/vet/tidy, version sync and package coverage.' \
        'go work sync may update module files; any resulting drift fails this check.'
}

function module_dirs() {
    git ls-files -z -- go.mod '**/go.mod' | python3 -c '
import os, sys
for name in sorted(set(os.path.dirname(p) or "." for p in sys.stdin.read().split("\0") if p)):
    print(name)
'
}

function check_workspace() {
    local tracked="$1" workspace
    workspace="$(go work edit -json | python3 -c 'import json,sys; print("\n".join(sorted(u["DiskPath"].removeprefix("./") for u in json.load(sys.stdin)["Use"])))')" || return 1
    if [[ -z "$tracked" || "$tracked" != "$workspace" ]]; then
        printf '%s\n' 'Module files and go.work disagree, or the module inventory is empty.' >&2
        diff <(printf '%s\n' "$workspace") <(printf '%s\n' "$tracked") >&2 || true
        return 1
    fi
}

function check_module() {
    local dir="$1" status=0
    printf 'Checking standalone module %s\n' "$dir" >&2
    (cd "$dir" && GOWORK=off go build ./...) || status=1
    (cd "$dir" && GOWORK=off go vet ./...) || status=1
    (cd "$dir" && GOWORK=off go mod tidy -diff) || status=1
    return "$status"
}

function check_versions() {
    go work sync || return 1
    git diff --exit-code -- go.mod go.sum go.work go.work.sum '**/go.mod' '**/go.sum'
}

function check_package_coverage() {
    local packages count
    packages="$(go list github.com/znasllc-io/memql/...)" || return 1
    count="$(printf '%s\n' "$packages" | wc -l | tr -d ' ')"
    printf 'Workspace packages: %s\n' "$count" >&2
    [[ "$count" -ge 181 ]]
}

function main() {
    case "${1:-}" in
        --help) show_help; return 0 ;;
        '') ;;
        *) show_help >&2; return 2 ;;
    esac
    [[ "$#" -eq 0 ]] || return 2
    cd "$(dirname "${BASH_SOURCE[0]}")/../.." || return 1
    local dirs dir status=0
    dirs="$(module_dirs)" || return 1
    check_workspace "$dirs" || return 1
    while IFS= read -r dir; do
        check_module "$dir" || status=1
    done <<< "$dirs"
    check_versions || status=1
    check_package_coverage || status=1
    return "$status"
}

main "$@"
