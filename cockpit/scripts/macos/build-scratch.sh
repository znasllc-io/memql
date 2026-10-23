#!/usr/bin/env bash

# Source after capability.sh. This owns only the directory mktemp allocates
# for this invocation. Success cleans before cap_ok emits its result; failure
# chains the capability runtime's result trap with the original exit status.
COCKPIT_BUILD_SCRATCH=""

function create_build_scratch() {
    trap '_build_scratch_on_exit' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    COCKPIT_BUILD_SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/${1:?scratch prefix required}.XXXXXX")"
}

function cleanup_build_scratch() {
    [[ -n "$COCKPIT_BUILD_SCRATCH" ]] || return 0
    if ! rm -rf -- "$COCKPIT_BUILD_SCRATCH"; then
        cap_error "could not remove build scratch: $COCKPIT_BUILD_SCRATCH"
        return 5
    fi
    [[ ! -e "$COCKPIT_BUILD_SCRATCH" && ! -L "$COCKPIT_BUILD_SCRATCH" ]] || return 5
    COCKPIT_BUILD_SCRATCH=""
}

function _build_scratch_on_exit() {
    local code=$?
    trap - EXIT
    if ! cleanup_build_scratch; then
        [[ "$code" -ne 0 ]] || code=5
    fi
    # The capability trap reads $?. Disable errexit only while handing it
    # that status; the trap must emit the missing failure envelope itself.
    set +e
    (exit "$code")
    _cap_on_exit
    exit "$code"
}
