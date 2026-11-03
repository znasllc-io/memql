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

# launchd_field prints one of the job's own fields as `launchctl print`
# shows it (`<tab>key = value`; nested blocks are indented further), or
# nothing when the job is not loaded or has no such field.
function launchd_field() {
    local target="$1" key="$2" output line tab=$'\t'
    output="$(launchctl print "$target" 2>/dev/null)" || return 0
    while IFS= read -r line; do
        if [[ "$line" == "$tab$key = "* && "$line" != "$tab$tab"* ]]; then
            printf '%s\n' "${line#"$tab$key = "}"
            return 0
        fi
    done <<< "$output"
}

# wait_until_running gives a started job twenty seconds to run, and the
# process it finds one second more. bootstrap and kickstart both succeed for
# a job launchd cannot start, which reads "spawn failed" or "spawn
# scheduled", and a build that exits at once reads "running" for an instant
# between respawns (measured on macOS 27).
function wait_until_running() {
    local target="$1" attempt pid
    for ((attempt=0; attempt<=80; attempt++)); do
        if [[ "$(launchd_field "$target" state)" == running ]]; then
            pid="$(launchd_field "$target" pid)"
            sleep 1
            if [[ -n "$pid" && "$(launchd_field "$target" state)" == running && "$(launchd_field "$target" pid)" == "$pid" ]]; then
                return 0
            fi
        fi
        [[ "$attempt" -eq 80 ]] || sleep 0.25
    done
    return 5
}

# launchagent_disabled reports whether launchd keeps the label switched off:
# the person turned it off in System Settings > General > Login Items, or
# something ran `launchctl disable`. Turning it back on is theirs to do.
function launchagent_disabled() {
    local domain="$1" label="$2" output
    output="$(launchctl print-disabled "$domain" 2>/dev/null)" || return 1
    [[ "$output" == *"\"$label\" => disabled"* || "$output" == *"\"$label\" => true"* ]]
}
