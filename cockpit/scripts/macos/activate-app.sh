#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ -f "$REPO_ROOT/scripts/lib/capability.sh" ]]; then
    source "$REPO_ROOT/scripts/lib/capability.sh"
else
    source "$REPO_ROOT/../scripts/lib/capability.sh"
fi
source "$REPO_ROOT/scripts/macos/launchagent.sh"
cap_init "cockpit.app.activate" "Register MemQL.app's worker and menu services for the current user."
cap_spec_param "app" "Absolute installed MemQL.app path"
cap_spec_param "menu" "Start the embedded menu helper (true or false, default true)"
cap_spec_param "ollama-env" "OLLAMA_HOST=<value> or OLLAMA_MODELS=<value> to set in the worker service's environment; repeatable"

# MemQL.app REGISTERS ITS OWN SERVICES (memql-cockpit#545). Both agent plists
# live inside the signed bundle and the app's executable registers them
# through SMAppService, so Login Items lists one MemQL with the app's icon.
# The per-user plists this used to write showed there as two bare
# executables: macOS ties such a plist to an app only through a Team ID, and
# an ad-hoc build has none. What a plist in the bundle cannot carry -- this
# account's Ollama variables -- goes to service.env, which the worker reads
# when the bundled agent starts it (internal/worker/app_service.go).

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

function main() {
    cap_handle_meta "$@"
    OLLAMA_ENV_PAIRS=()
    local arg
    for arg in "$@"; do
        case "$arg" in --ollama-env=*) OLLAMA_ENV_PAIRS+=("${arg#--ollama-env=}") ;; esac
    done
    cap_parse_flags "$@"
    local app menu helper uid_value backup legacy label identifier fingerprint marker env_file pair menu_flag=off released=false status legacy_backups=""
    app="$(cap_param app)"; menu="$(cap_param menu true)"
    [[ "$app" == /* && -x "$app/Contents/MacOS/MemQL" ]] || cap_fail 2 "app must be an installed MemQL bundle"
    [[ "$menu" == true || "$menu" == false ]] || cap_fail 2 "menu must be true or false"
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")"
    [[ "$identifier" == com.znasllc.memql-worker ]] || cap_fail 3 "unexpected worker bundle identity"
    codesign --verify --deep --strict "$app" >&2 || cap_fail 3 "installed bundle signature invalid"
    helper="$app/Contents/Library/LoginItems/MemQL Menu.app"
    [[ -x "$helper/Contents/MacOS/MemQLCockpit" ]] || cap_fail 3 "menu helper is missing"
    [[ -f "$app/Contents/Library/LaunchAgents/com.znasllc.memql-worker.plist" ]] || cap_fail 3 "app does not carry its service definitions; install a current MemQL"
    uid_value="$(id -u)"
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
    # The per-user plists an earlier install wrote. The services keep their
    # labels inside the app, so the old jobs stop and their files leave
    # first; each is kept under ~/.memql/backups.
    for label in com.znasllc.memql-worker com.znasllc.memql-cockpit-menubar com.znasllc.memql-cockpit-worker com.visionarys.memql-cockpit-worker; do
        legacy="$HOME/Library/LaunchAgents/$label.plist"
        [[ -e "$legacy" || -L "$legacy" ]] || continue
        [[ -f "$legacy" && ! -L "$legacy" && -O "$legacy" ]] || cap_fail 3 "$legacy must be a regular file owned by the current user"
        mkdir -p "$HOME/.memql/backups"
        backup="$(mktemp -d "$HOME/.memql/backups/legacy-agent.XXXXXX")"
        stop_launchagent "gui/$uid_value/$label" || cap_fail 5 "could not stop previous service; retry after it exits"
        mv "$legacy" "$backup/$label.plist"
        [[ -z "$legacy_backups" ]] || legacy_backups+=","
        legacy_backups+="\"$(cap_json_escape "$backup/$label.plist")\""
        cap_changed
    done
    # Return recovery evidence on both success and failure. The caller restores
    # the previous app before starting these services; registration refusal is
    # never bypassed by starting the new executable through a legacy plist.
    cap_result_set_raw previous_service_files "[$legacy_backups]"
    [[ "$menu" == false ]] || menu_flag=on
    marker="$HOME/.memql/state/app-activation.sha256"
    fingerprint="$(shasum -a 256 "$app/Contents/MacOS/MemQL" "$helper/Contents/MacOS/MemQLCockpit") menu=$menu"
    if [[ ! -f "$marker" || "$(cat "$marker")" != "$fingerprint" ]]; then
        # SMAppService records each program's signature when it registers,
        # and launchd refuses a replaced executable that no longer matches
        # ("Launch Constraint Violation"). A changed app registers afresh;
        # registering again alone answers "already registered".
        rm -f "$marker"
        "$app/Contents/MacOS/MemQL" worker service unregister >&2 || cap_fail 5 "macOS did not release MemQL's previous services"
        released=true
        cap_changed
    fi
    if register_app_services "$app/Contents/MacOS/MemQL" "$menu_flag" "$released"; then
        :
    else
        status=$?
        if [[ "$status" -eq 3 ]]; then
            cap_fail 3 "Allow MemQL in System Settings > General > Login Items, then retry."
        fi
        cap_fail 5 "macOS could not start MemQL. Previous service files are retained for recovery."
    fi
    printf '%s\n' "$fingerprint" > "$marker"
    chmod 600 "$marker"
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
    cap_result_set app "$app"
    cap_result_set worker "com.znasllc.memql-worker"
    cap_ok
}
main "$@"
