#!/usr/bin/env bash
# Run every capability script's meta paths under REAL bash 3.2.
#
# WHY THIS EXISTS. macOS ships bash 3.2.57 as /bin/bash and always will (bash 4
# went GPLv3). scripts/lib/platform.sh lists darwin/arm64 as supported, so every
# script the installer can reach has to run there -- and every CI job in this
# repo runs on ubuntu-latest, where bash is 5.x. `local -n` in
# scripts/install/seed-bootstrap.sh therefore shipped and broke 100% of macOS
# installs while every lane stayed green.
#
# WHY IT IS NOT ENOUGH TO LINT. scripts/lib/bash_portability_test.go scans for
# the constructs we know about. This runs the interpreter, which is the only
# thing that catches the construct nobody thought to add to a list.
#
# WHY IT IS NOT A macOS RUNNER. GitHub's macOS runners have no usable Docker, so
# the cluster lane cannot run there at all; the bash:3.2.57 image is the same
# interpreter on the ubuntu runner for a fraction of the minutes.
#
# WHAT IT COVERS, HONESTLY. `--print-spec` and `--help` reach cap_init,
# cap_spec_param and cap_handle_meta -- the shared prologue of every capability
# -- plus whatever each script does at source time. It does NOT execute the
# bodies; only seed-bootstrap.sh, whose body is the one that broke, is driven
# far enough to build its argument list. Anything a body does past that point is
# still uncovered here and is covered by the static gate.
#
# NOT A CAPABILITY SCRIPT: it is a CI reporter, not a DSL-callable action, so it
# does not source lib/capability.sh and emits no envelope.
#
# Portability: this file runs under bash 3.2 too.

# `-e` is deliberately absent: a reporter must run every script and print every
# failure, not stop at the first one.
set -uo pipefail

FAILURES=0

function fail() {
    printf 'FAIL: %s\n' "$*" >&2
    FAILURES=$((FAILURES + 1))
}

function note() {
    printf 'INFO: %s\n' "$*" >&2
}

# capability_scripts -- every .sh under scripts/ that opts into the contract by
# SOURCING lib/capability.sh. Same set as
# scripts/lib/capability_contract_test.go, so the two cover one corpus.
#
# THE MATCH IS ON A `source` STATEMENT, NOT A MENTION, and that is not
# fastidiousness: a bare `grep -q lib/capability.sh` matches this file -- which
# names the path in its own prose and in the pattern itself -- so this script
# discovered ITSELF, ran itself with --print-spec, and fork-bombed the
# container. Measured, not imagined. The self-skip below is the belt to that
# brace: it documents the trap where the next person will hit it.
function capability_scripts() {
    local f self
    self="$(basename "$0")"
    find scripts -type f -name '*.sh' | sort | while IFS= read -r f; do
        if [[ "$(basename "$f")" == "$self" ]]; then
            continue
        fi
        if grep -qE '^[[:space:]]*(source|\.)[[:space:]].*lib/capability\.sh' "$f"; then
            printf '%s\n' "$f"
        fi
    done
}

# run_bounded <seconds> <cmd>... -- run with a deadline when one is available.
#
# A capability that HANGS on --print-spec would otherwise burn the whole job
# timeout and report nothing, which is the silent failure this lane exists to
# replace. `timeout` is coreutils: present in the CI image, absent on macOS,
# where someone will reasonably run this by hand -- so it is used when found
# and skipped when not, exactly as scripts/lib/docker.sh guards its own.
function run_bounded() {
    local secs="$1"
    shift
    if command -v timeout &>/dev/null; then
        timeout "$secs" "$@"
        return $?
    fi
    "$@"
}

# meta_paths_answer <script> -- --print-spec and --help must both exit 0. They
# are the paths cap_handle_meta serves before a script does any work, so a
# failure here is the shared prologue failing, not the capability.
function meta_paths_answer() {
    local script="$1" flag out
    for flag in --print-spec --help; do
        if ! out="$(run_bounded 30 bash "$script" "$flag" 2>&1)"; then
            fail "$script $flag exited non-zero under bash ${BASH_VERSION}: ${out}"
            continue
        fi
        if [[ -z "$out" ]]; then
            fail "$script $flag printed nothing"
        fi
    done
}

# seed_bootstrap_dry_run -- the regression that motivated this lane.
#
# Reaches the argument-building loop with NO cluster: main() runs cap_handle_meta,
# cap_parse_flags, the cap_param reads, three pure-bash validators and
# stage_provider_key (which returns early with no provider) before the first
# add_literal. The kubectl preflight is inside the `if [[ -z "$dry_run" ]]`
# branch, so nothing here touches an API server.
function seed_bootstrap_dry_run() {
    local script="scripts/install/seed-bootstrap.sh" out
    if [[ ! -f "$script" ]]; then
        fail "$script is missing -- this lane's one body-level case cannot run"
        return
    fi
    out="$(run_bounded 60 bash "$script" --dry-run \
        --domain=memql.localhost \
        --owner-email=owner@example.com \
        --owner-first-name=Ada \
        --owner-last-name="Lovelace King" \
        --org-name="Acme Corp Inc" \
        --registration-mode=open 2>&1)"
    case "$out" in
        *'"ok":true'*)
            note "seed-bootstrap.sh --dry-run: ok"
            ;;
        *)
            fail "seed-bootstrap.sh --dry-run did not report ok:true under bash ${BASH_VERSION}: ${out}"
            ;;
    esac
}

function main() {
    note "bash ${BASH_VERSION}"
    case "${BASH_VERSION}" in
        3.2.*) ;;
        *)
            fail "this lane must run under bash 3.2 -- got ${BASH_VERSION}. Running it on a newer bash proves nothing, so it fails rather than passing vacuously."
            printf '%s\n' "FAILURES: ${FAILURES}" >&2
            exit 1
            ;;
    esac

    local script count=0
    while IFS= read -r script; do
        count=$((count + 1))
        meta_paths_answer "$script"
    done < <(capability_scripts)

    if [[ "$count" -eq 0 ]]; then
        fail "found no capability scripts -- the discovery rule is broken, and a broken walk reports a clean bill of health about nothing"
    else
        note "checked ${count} capability script(s)"
    fi

    seed_bootstrap_dry_run

    if [[ "$FAILURES" -gt 0 ]]; then
        printf 'FAILURES: %s\n' "$FAILURES" >&2
        exit 1
    fi
    note "every capability script answers its meta paths under bash 3.2"
    return 0
}

main "$@"
