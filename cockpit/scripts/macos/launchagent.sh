#!/usr/bin/env bash
# Shared launchd lifecycle helpers. bootout can return before teardown finishes.
# This library emits diagnostics on stderr and leaves capability results to callers.
function stop_launchagent() {
    local target="$1" attempt
    launchctl print "$target" >/dev/null 2>&1 || return 0
    launchctl bootout "$target" >/dev/null 2>&1 || true
    for ((attempt=0; attempt<=40; attempt++)); do
        launchctl print "$target" >/dev/null 2>&1 || return 0
        [[ "$attempt" -lt 40 ]] && sleep 0.25
    done
    printf 'ERROR: service did not stop within 10 seconds: %s\n' "$target" >&2
    return 5
}

function start_launchagent() {
    local domain="$1" plist="$2" target="$3" attempt status diagnostics
    if ! launchctl print "$target" >/dev/null 2>&1; then
        diagnostics="$(mktemp "${TMPDIR:-/tmp}/memql-launch.XXXXXX")" || return 5
        for ((attempt=0; attempt<=40; attempt++)); do
            if launchctl bootstrap "$domain" "$plist" >"$diagnostics" 2>&1; then
                break
            else
                status=$?
            fi
            # launchd can briefly reject bootstrap with EIO after removing a job.
            # Do not retry authorization/configuration errors or overwrite a job.
            if [[ "$status" != 5 || "$attempt" == 40 ]] || launchctl print "$target" >/dev/null 2>&1; then
                cat "$diagnostics" >&2
                rm -f "$diagnostics"
                return 5
            fi
            sleep 0.25
        done
        rm -f "$diagnostics"
    fi
    launchctl kickstart "$target" >&2
}
