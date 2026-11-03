#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ -f "$REPO_ROOT/scripts/lib/capability.sh" ]]; then
    source "$REPO_ROOT/scripts/lib/capability.sh"
else
    source "$REPO_ROOT/../scripts/lib/capability.sh"
fi
source "$REPO_ROOT/scripts/macos/launchagent.sh"
cap_init "cockpit.app.activate" "Start MemQL.app's worker and menu services for the current user."
cap_spec_param "app" "Absolute installed MemQL.app path"
cap_spec_param "menu" "Start the embedded menu helper (true or false, default true)"
cap_spec_param "ollama-env" "OLLAMA_HOST=<value> or OLLAMA_MODELS=<value> to set in the worker service's environment; repeatable"

# ===========================================================================
# PER-USER PLISTS, UNLESS THE BUILD HAS A TEAM ID (owner's decision, 2026-11-03)
# ===========================================================================
# A build without a Team ID -- every release, which is signed ad-hoc, and
# the local self-signed identity -- runs the worker and the menu from
# per-user plists in ~/Library/LaunchAgents, which this script writes. Only
# a build with a Team ID registers them through SMAppService from the
# plists inside the bundle (memql-cockpit#545), which Login Items lists as
# one MemQL with its icon.
#
# Measured on macOS 27 with a scratch bundle (docs/macos-menu.md has the
# record): Background Task Management keeps ONE record per label for a
# per-user plist and an app registration alike, with the code requirement
# it saw first, and without a Team ID that requirement is the build's hash.
# A later build registered over the record -- an upgrade, with or without an
# unregister first -- reports success and then cannot start: "spawn
# failed", exit 78, "needs LWCR update". A per-user plist carries no such
# pin, and both Macs have upgraded through one. The cost is what #545
# removed: Login Items lists the worker and the menu as two bare
# executables, because macOS ties a per-user plist to its app only through
# a Team ID.
#
# The SMAppService path has not run with a Team ID (no such build exists
# yet). A refusal there, or a worker that does not run after it, puts back
# the per-user plists this run set aside and starts the worker from them
# before the failure is reported, so no answer from macOS leaves the Mac
# without a worker. That replaces #545's rule that a refusal was never
# bypassed by starting the new executable through a per-user plist; the
# owner decided it with the rest.

readonly WORKER_LABEL=com.znasllc.memql-worker
readonly MENU_LABEL=com.znasllc.memql-cockpit-menubar

# merge_service_env sets the given NAME=value pairs in the file and keeps
# its other lines. It fails when nothing changed.
function merge_service_env() {
    local file="$1" stage line pair
    shift
    stage="$(mktemp "$(dirname "$file")/.service-env.XXXXXX")"
    if [[ -f "$file" ]]; then
        while IFS= read -r line || [[ -n "$line" ]]; do
            for pair in "$@"; do [[ "${line%%=*}" != "${pair%%=*}" ]] || continue 2; done
            printf '%s\n' "$line" >> "$stage"
        done < "$file"
    fi
    printf '%s\n' "$@" >> "$stage"
    chmod 600 "$stage"
    if [[ -f "$file" ]] && cmp -s "$stage" "$file"; then
        rm -f "$stage"
        return 1
    fi
    mv "$stage" "$file"
}

# app_team_identifier prints the Team ID the bundle is signed with, or
# nothing. An ad-hoc or self-signed build reads "TeamIdentifier=not set",
# and anything but a ten-character Team ID counts as none: the per-user
# path is the one known to work.
function app_team_identifier() {
    local team
    team="$(codesign --display --verbose=1 "$1" 2>&1 | sed -n 's/^TeamIdentifier=//p' || true)"
    if [[ "$team" =~ ^[A-Z0-9]{10}$ ]]; then printf '%s\n' "$team"; fi
}

# set_aside stops a label's job and moves its per-user plist under
# ~/.memql/backups, remembering both for put_back and the result. It fails
# when the job does not stop, and then moves nothing.
function set_aside() {
    local label="$1" path="$2" kind="$3" backup
    stop_launchagent "gui/$UID_VALUE/$label" || return 5
    mkdir -p "$HOME/.memql/backups" || return 5
    backup="$(mktemp -d "$HOME/.memql/backups/$kind.XXXXXX")" || return 5
    mv "$path" "$backup/$label.plist" || return 5
    ASIDE_LABELS+=("$label")
    ASIDE_PATHS+=("$path")
    ASIDE_FILES+=("$backup/$label.plist")
    cap_changed
}

# put_back copies every plist this run set aside back to its place: the
# definitions the Mac had before. With "start" it also restarts each job;
# with "files" the caller starts them. A plist the new one did not change
# stays as it is. It fails when nothing came back, so a caller can skip
# waiting for a definition it already watched fail.
function put_back() {
    local mode="$1" index label restored=1
    for ((index=0; index<${#ASIDE_FILES[@]}; index++)); do
        label="${ASIDE_LABELS[$index]}"
        ! cmp -s "${ASIDE_FILES[$index]}" "${ASIDE_PATHS[$index]}" || continue
        [[ "$mode" != start ]] || stop_launchagent "gui/$UID_VALUE/$label" || true
        cp -p "${ASIDE_FILES[$index]}" "${ASIDE_PATHS[$index]}" || continue
        restored=0
        [[ "$mode" != start ]] || start_launchagent "gui/$UID_VALUE" "${ASIDE_PATHS[$index]}" "gui/$UID_VALUE/$label" || true
    done
    return "$restored"
}

# set_aside_result adds the plists this run set aside to the result, on
# success and failure alike, so the previous definitions can be found
# whatever happened to the new ones.
function set_aside_result() {
    local file list=""
    for file in "${ASIDE_FILES[@]+"${ASIDE_FILES[@]}"}"; do
        [[ -z "$list" ]] || list+=","
        list+="\"$(cap_json_escape "$file")\""
    done
    cap_result_set_raw previous_service_files "[$list]"
}

# fail is cap_fail once anything may have been set aside.
function fail() {
    [[ -z "$WORKER_STAGE" ]] || rm -f "$WORKER_STAGE"
    set_aside_result
    cap_fail "$1" "$2"
}

# stage_worker_plist writes the worker's per-user plist for this app to
# WORKER_STAGE. An existing plist keeps everything but its executable --
# arguments, log paths, a PATH or variables the owner set; a new one gets
# the definition install-mac.sh writes for a raw worker. Either way
# MEMQL_SERVICE=app has the worker read service.env, as the bundled agent
# does, so this account's Ollama variables have one home.
function stage_worker_plist() {
    local app="$1" plist="$2" service_path
    # launchd's four system directories, then where the app installers put
    # claude and codex -- appended, so nothing the user can write to shadows
    # launchctl. internal/worker/servicepath.go spells the same string.
    service_path="/usr/bin:/bin:/usr/sbin:/sbin:${HOME}/.local/bin:${HOME}/.claude/local:/opt/homebrew/bin:/usr/local/bin"
    if [[ -f "$plist" ]]; then
        [[ "$(/usr/libexec/PlistBuddy -c 'Print :Label' "$plist" 2>/dev/null)" == "$WORKER_LABEL" ]] || fail 3 "worker plist has an unexpected label"
        [[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:1' "$plist" 2>/dev/null)" == worker \
            && "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:2' "$plist" 2>/dev/null)" == run ]] \
            || fail 3 "worker service arguments are not a recognized worker run command"
    fi
    WORKER_STAGE="$(mktemp "$HOME/Library/LaunchAgents/.memql-app-agent.XXXXXX")"
    if [[ -f "$plist" ]]; then
        cp -p "$plist" "$WORKER_STAGE"
        # plutil -replace inserts at an array index on current macOS. Remove
        # then insert so the old executable cannot become an extra argument.
        plutil -remove ProgramArguments.0 "$WORKER_STAGE" >&2
        plutil -insert ProgramArguments.0 -string "$app/Contents/MacOS/MemQL" "$WORKER_STAGE" >&2
        /usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables' "$WORKER_STAGE" >/dev/null 2>&1 \
            || plutil -insert EnvironmentVariables -dictionary "$WORKER_STAGE" >&2
        # A PATH already there is the owner's and stays exactly as it is.
        /usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:PATH' "$WORKER_STAGE" >/dev/null 2>&1 \
            || plutil -insert EnvironmentVariables.PATH -string "$service_path" "$WORKER_STAGE" >&2
    else
        plutil -create xml1 "$WORKER_STAGE" >&2
        plutil -insert Label -string "$WORKER_LABEL" "$WORKER_STAGE" >&2
        plutil -insert ProgramArguments -array "$WORKER_STAGE" >&2
        plutil -insert ProgramArguments.0 -string "$app/Contents/MacOS/MemQL" "$WORKER_STAGE" >&2
        plutil -insert ProgramArguments.1 -string worker "$WORKER_STAGE" >&2
        plutil -insert ProgramArguments.2 -string run "$WORKER_STAGE" >&2
        plutil -insert RunAtLoad -bool true "$WORKER_STAGE" >&2
        plutil -insert KeepAlive -bool true "$WORKER_STAGE" >&2
        plutil -insert StandardOutPath -string "$HOME/.memql/state/worker.log" "$WORKER_STAGE" >&2
        plutil -insert StandardErrorPath -string "$HOME/.memql/state/worker.log" "$WORKER_STAGE" >&2
        plutil -insert EnvironmentVariables -dictionary "$WORKER_STAGE" >&2
        plutil -insert EnvironmentVariables.HOME -string "$HOME" "$WORKER_STAGE" >&2
        plutil -insert EnvironmentVariables.PATH -string "$service_path" "$WORKER_STAGE" >&2
    fi
    plutil -replace EnvironmentVariables.MEMQL_SERVICE -string app "$WORKER_STAGE" >&2
    plutil -remove AssociatedBundleIdentifiers "$WORKER_STAGE" >/dev/null 2>&1 || true
    plutil -insert AssociatedBundleIdentifiers -array "$WORKER_STAGE" >&2
    plutil -insert AssociatedBundleIdentifiers.0 -string com.znasllc.memql-worker "$WORKER_STAGE" >&2
}

# activate_launch_agents runs the worker and the menu from per-user plists.
# A changed definition or app restarts them, and so does a job launchd
# loaded from anywhere else -- an earlier release's SMAppService
# registration, which cannot run this build (see the top). A running,
# unchanged install is left alone. The worker stops last, so a refusal
# before it leaves the previous one running; a worker that does not run
# from the new definition gets the previous plists back before the failure
# is reported.
function activate_launch_agents() {
    local app="$1" menu="$2" fingerprint="$3" helper plist target label legacy output restart=false
    helper="$app/Contents/Library/LoginItems/MemQL Menu.app"
    plist="$HOME/Library/LaunchAgents/$WORKER_LABEL.plist"
    target="gui/$UID_VALUE/$WORKER_LABEL"
    [[ ! -L "$plist" ]] || fail 3 "worker plist must not be a symlink"
    [[ ! -e "$plist" || -f "$plist" && -O "$plist" ]] || fail 3 "worker plist must belong to the current user"
    # Switched off by the person, or by `launchctl disable`: bootstrap would
    # fail, and switching it back on is theirs to do. Nothing has stopped.
    if launchagent_disabled "gui/$UID_VALUE" "$WORKER_LABEL"; then
        cap_error "MemQL's worker is switched off in System Settings > General > Login Items."
        fail 3 "Turn MemQL on in System Settings > General > Login Items, then run the installer again."
    fi
    mkdir -p "$HOME/Library/LaunchAgents"
    stage_worker_plist "$app" "$plist"
    if [[ ! -f "$plist" ]] || ! cmp -s "$WORKER_STAGE" "$plist"; then restart=true; fi
    if [[ ! -f "$MARKER" || "$(cat "$MARKER")" != "$fingerprint" ]]; then restart=true; fi
    if launchctl print "$target" >/dev/null 2>&1 && [[ "$(launchd_field "$target" path)" != "$plist" ]]; then restart=true; fi
    # The pre-rename labels run the same worker under another name.
    for label in com.znasllc.memql-cockpit-worker com.visionarys.memql-cockpit-worker; do
        legacy="$HOME/Library/LaunchAgents/$label.plist"
        if [[ -f "$legacy" && ! -L "$legacy" && -O "$legacy" ]]; then
            set_aside "$label" "$legacy" legacy-agent || fail 5 "could not stop previous service; retry after it exits"
        fi
    done
    if [[ "$restart" == true ]]; then
        rm -f "$MARKER"
        stop_launchagent "gui/$UID_VALUE/$MENU_LABEL" || fail 5 "could not stop previous service; retry after it exits"
        if [[ -f "$plist" ]]; then
            set_aside "$WORKER_LABEL" "$plist" app-activation || fail 5 "could not stop previous service; retry after it exits"
        else
            stop_launchagent "$target" || fail 5 "could not stop previous service; retry after it exits"
        fi
        mv "$WORKER_STAGE" "$plist"
        cap_changed
    else
        rm -f "$WORKER_STAGE"
    fi
    launchctl print "$target" >/dev/null 2>&1 || cap_changed
    if ! start_launchagent "gui/$UID_VALUE" "$plist" "$target" || ! wait_until_running "$target"; then
        cap_error "MemQL's worker did not start from $plist."
        if put_back start && wait_until_running "$target"; then
            fail 5 "MemQL's worker did not start from its new LaunchAgent, so the previous one is back and running; ~/.memql/state/worker.log may say why."
        fi
        fail 5 "MemQL's worker did not start. Run: launchctl print $target, which says why, then run the installer again."
    fi
    if [[ "$menu" == true ]]; then
        if launchagent_disabled "gui/$UID_VALUE" "$MENU_LABEL"; then
            cap_warn "MemQL's menu is switched off in System Settings > General > Login Items; turn it on there to show it."
        else
            output="$(bash "$REPO_ROOT/scripts/macos/install-menubar.sh" --app="$helper" --destination="$helper")" \
                || { printf '%s\n' "$output" >&2; fail 5 "could not start the embedded menu; the worker is running"; }
            printf '%s\n' "$output" >&2
            [[ "$output" != *'"changed":true'* ]] || cap_changed
        fi
    else
        stop_launchagent "gui/$UID_VALUE/$MENU_LABEL" || fail 5 "could not stop previous service; retry after it exits"
        if [[ -e "$HOME/Library/LaunchAgents/$MENU_LABEL.plist" ]]; then
            rm -f "$HOME/Library/LaunchAgents/$MENU_LABEL.plist"
            cap_changed
        fi
    fi
    printf '%s\n' "$fingerprint" > "$MARKER"
    chmod 600 "$MARKER"
}

# A completed unregister can still race macOS's registration bookkeeping.
# Apple DTS documents a short delay as the workaround:
# https://developer.apple.com/forums/thread/783539
# Retry only registration after this invocation successfully unregistered;
# never repeat unregister or retry an approval/signature/unreadable-state error.
function register_app_services() {
    local worker="$1" menu="$2" released="$3" attempt output state worker_state menu_state
    for ((attempt=0; attempt<3; attempt++)); do
        if output="$("$worker" worker service register --menu="$menu" 2>&1)"; then
            printf '%s\n' "$output" >&2
            return 0
        fi
        if ! state="$("$worker" worker service status 2>/dev/null)" \
            || ! worker_state="$(plutil -extract worker raw -o - - <<< "$state" 2>/dev/null)" \
            || ! menu_state="$(plutil -extract menu raw -o - - <<< "$state" 2>/dev/null)"; then
            printf '%s\n' "$output" >&2
            return 5
        fi
        if [[ "$worker_state" == requires_approval || "$menu" == on && "$menu_state" == requires_approval ]]; then
            cap_error "Allow MemQL in System Settings > General > Login Items, then retry."
            return 3
        fi
        if [[ "$released" != true || "$attempt" -eq 2 || "$output" != *"(ServiceManagement error 1)"* \
            || "$worker_state" != not_registered && "$worker_state" != enabled \
            || "$menu_state" != not_registered && "$menu_state" != enabled ]]; then
            printf '%s\n' "$output" >&2
            return 5
        fi
        [[ "$attempt" -ne 0 ]] || cap_info "Waiting for macOS to finish updating MemQL."
        sleep 2
    done
}

# activate_app_services registers the bundle's agents through SMAppService,
# for a build with a Team ID. The per-user plists an earlier install wrote
# are stopped and set aside first, the worker's last: the labels are the
# same. It returns 3 when macOS waits for approval and 5 for any other
# refusal, or for a worker that does not run afterwards; the caller then
# starts the worker from per-user plists.
function activate_app_services() {
    local app="$1" menu="$2" fingerprint="$3" label legacy menu_flag=off released=false
    for label in com.znasllc.memql-cockpit-worker com.visionarys.memql-cockpit-worker "$MENU_LABEL" "$WORKER_LABEL"; do
        legacy="$HOME/Library/LaunchAgents/$label.plist"
        [[ -e "$legacy" || -L "$legacy" ]] || continue
        [[ -f "$legacy" && ! -L "$legacy" && -O "$legacy" ]] || fail 3 "$legacy must be a regular file owned by the current user"
        if ! set_aside "$label" "$legacy" legacy-agent; then
            cap_error "could not stop previous service: $label"
            return 5
        fi
    done
    [[ "$menu" == false ]] || menu_flag=on
    if [[ ! -f "$MARKER" || "$(cat "$MARKER")" != "$fingerprint" ]]; then
        # SMAppService records each program's signature when it registers,
        # and launchd refuses a replaced executable that no longer matches
        # ("Launch Constraint Violation"). A changed app registers afresh;
        # registering again alone answers "already registered". Without a
        # Team ID even this reuses the old record (see the top), which is
        # why such a build never comes here.
        rm -f "$MARKER"
        if ! "$app/Contents/MacOS/MemQL" worker service unregister >&2; then
            cap_error "macOS did not release MemQL's previous services."
            return 5
        fi
        released=true
        cap_changed
    fi
    register_app_services "$app/Contents/MacOS/MemQL" "$menu_flag" "$released" || return $?
    if ! wait_until_running "gui/$UID_VALUE/$WORKER_LABEL"; then
        cap_error "macOS registered MemQL's worker, but it did not start."
        return 5
    fi
    printf '%s\n' "$fingerprint" > "$MARKER"
    chmod 600 "$MARKER"
}

function main() {
    cap_handle_meta "$@"
    OLLAMA_ENV_PAIRS=()
    ASIDE_LABELS=()
    ASIDE_PATHS=()
    ASIDE_FILES=()
    WORKER_STAGE=""
    local arg
    for arg in "$@"; do
        case "$arg" in --ollama-env=*) OLLAMA_ENV_PAIRS+=("${arg#--ollama-env=}") ;; esac
    done
    cap_parse_flags "$@"
    local app menu helper legacy identifier backup build env_file pair status=0
    app="$(cap_param app)"; menu="$(cap_param menu true)"
    [[ "$app" == /* && -x "$app/Contents/MacOS/MemQL" ]] || cap_fail 2 "app must be an installed MemQL bundle"
    [[ "$menu" == true || "$menu" == false ]] || cap_fail 2 "menu must be true or false"
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")"
    [[ "$identifier" == com.znasllc.memql-worker ]] || cap_fail 3 "unexpected worker bundle identity"
    codesign --verify --deep --strict "$app" >&2 || cap_fail 3 "installed bundle signature invalid"
    helper="$app/Contents/Library/LoginItems/MemQL Menu.app"
    [[ -x "$helper/Contents/MacOS/MemQLCockpit" ]] || cap_fail 3 "menu helper is missing"
    [[ -f "$app/Contents/Library/LaunchAgents/$WORKER_LABEL.plist" ]] || cap_fail 3 "app does not carry its service definitions; install a current MemQL"
    UID_VALUE="$(id -u)"
    MARKER="$HOME/.memql/state/app-activation.sha256"
    mkdir -p "$HOME/.memql/state"
    # The installing shell's Ollama variables, so the worker serves from
    # where the setup pulled (memql-cockpit#524). Only these two names.
    for pair in "${OLLAMA_ENV_PAIRS[@]+"${OLLAMA_ENV_PAIRS[@]}"}"; do
        [[ "${pair%%=*}" == OLLAMA_HOST || "${pair%%=*}" == OLLAMA_MODELS ]] || cap_fail 2 "ollama-env takes OLLAMA_HOST or OLLAMA_MODELS"
    done
    if [[ "${#OLLAMA_ENV_PAIRS[@]}" -gt 0 ]]; then
        env_file="$HOME/.memql/state/service.env"
        [[ ! -L "$env_file" && ( ! -e "$env_file" || -f "$env_file" && -O "$env_file" ) ]] || cap_fail 3 "service.env must be a regular file owned by the current user"
        if merge_service_env "$env_file" "${OLLAMA_ENV_PAIRS[@]}"; then cap_changed; fi
    fi
    # What runs, and how it was started: a change to either restarts it.
    build="$(shasum -a 256 "$app/Contents/MacOS/MemQL" "$helper/Contents/MacOS/MemQLCockpit") menu=$menu"
    if [[ -n "$(app_team_identifier "$app")" ]]; then
        activate_app_services "$app" "$menu" "$build service=app" || status=$?
        if [[ "$status" -ne 0 ]]; then
            put_back files || true
            activate_launch_agents "$app" "$menu" "$build service=launch_agent"
            cap_result_set service launch_agent
            if [[ "$status" -eq 3 ]]; then
                fail 3 "Allow MemQL in System Settings > General > Login Items, then run the installer again. The worker runs from per-user LaunchAgents until then."
            fi
            fail 5 "macOS did not start MemQL's services through SMAppService. The worker runs from per-user LaunchAgents instead, so nothing stopped; run the installer again to retry."
        fi
        cap_result_set service app
    else
        activate_launch_agents "$app" "$menu" "$build service=launch_agent"
        cap_result_set service launch_agent
    fi
    # Retire only known old helper bundles; keep the copy for rollback.
    legacy="$HOME/Applications/MemQL Cockpit.app"
    if [[ -d "$legacy" && ! -L "$legacy" && -O "$legacy" ]]; then
        identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$legacy/Contents/Info.plist" 2>/dev/null || true)"
        if [[ "$identifier" == com.znasllc.memql-cockpit-menubar ]]; then
            backup="$(mktemp -d "$HOME/Applications/.memql-legacy-menu.XXXXXX")"
            mv "$legacy" "$backup/previous.app"
            cap_changed
        fi
    fi
    set_aside_result
    cap_result_set app "$app"
    cap_result_set worker "$WORKER_LABEL"
    cap_ok
}
main "$@"
