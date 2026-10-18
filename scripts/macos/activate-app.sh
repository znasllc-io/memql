#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ -f "$REPO_ROOT/scripts/lib/capability.sh" ]]; then
    source "$REPO_ROOT/scripts/lib/capability.sh"
else
    source "$REPO_ROOT/../memql/scripts/lib/capability.sh"
fi
source "$REPO_ROOT/scripts/macos/launchagent.sh"
cap_init "cockpit.app.activate" "Point the current user's existing worker and menu services at MemQL.app."
cap_spec_param "app" "Absolute installed MemQL.app path"
cap_spec_param "menu" "Start the embedded menu helper (true or false, default true)"
cap_spec_param "ollama-env" "OLLAMA_HOST=<value> or OLLAMA_MODELS=<value> to set in the worker service's environment; repeatable"

function main() {
    cap_handle_meta "$@"
    OLLAMA_ENV_PAIRS=()
    local arg
    for arg in "$@"; do
        case "$arg" in --ollama-env=*) OLLAMA_ENV_PAIRS+=("${arg#--ollama-env=}") ;; esac
    done
    cap_parse_flags "$@"
    local app menu plist helper stage uid_value backup legacy identifier fingerprint marker service_path
    app="$(cap_param app)"; menu="$(cap_param menu true)"
    [[ "$app" == /* && -x "$app/Contents/MacOS/MemQL" ]] || cap_fail 2 "app must be an installed MemQL bundle"
    [[ "$menu" == true || "$menu" == false ]] || cap_fail 2 "menu must be true or false"
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")"
    [[ "$identifier" == com.znasllc.memql-worker ]] || cap_fail 3 "unexpected worker bundle identity"
    codesign --verify --deep --strict "$app" >&2 || cap_fail 3 "installed bundle signature invalid"
    helper="$app/Contents/Library/LoginItems/MemQL Menu.app"
    [[ -x "$helper/Contents/MacOS/MemQLCockpit" ]] || cap_fail 3 "menu helper is missing"
    uid_value="$(id -u)"
    # launchd's four system directories, then where the app installers put
    # claude and codex -- appended, so nothing the user can write to shadows
    # launchctl. internal/worker/servicepath.go spells the same string.
    service_path="/usr/bin:/bin:/usr/sbin:/sbin:${HOME}/.local/bin:${HOME}/.claude/local:/opt/homebrew/bin:/usr/local/bin"
    plist="$HOME/Library/LaunchAgents/com.znasllc.memql-worker.plist"
    [[ ! -L "$plist" ]] || cap_fail 3 "worker plist must not be a symlink"
    [[ ! -e "$plist" || -f "$plist" && -O "$plist" ]] || cap_fail 3 "worker plist must belong to the current user"
    mkdir -p "$(dirname "$plist")" "$HOME/.memql/state"
    # Preserve the enrollment, environment, arguments, log paths and policies;
    # replace only the executable and its associated native bundle identity.
    stage="$(mktemp "$HOME/Library/LaunchAgents/.memql-app-agent.XXXXXX")"
    if [[ -f "$plist" ]]; then
        [[ "$(/usr/libexec/PlistBuddy -c 'Print :Label' "$plist")" == com.znasllc.memql-worker ]] || cap_fail 3 "worker plist has an unexpected label"
        [[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:1' "$plist")" == worker && "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:2' "$plist")" == run ]] || cap_fail 3 "worker service arguments are not a recognized worker run command"
        cp -p "$plist" "$stage"
        # plutil -replace inserts at an array index on current macOS. Remove
        # then insert so the old executable cannot become an extra CLI arg.
        plutil -remove ProgramArguments.0 "$stage" >&2
        plutil -insert ProgramArguments.0 -string "$app/Contents/MacOS/MemQL" "$stage" >&2
        # A plist written before the service carried a PATH gets one, or the
        # worker finds no app on this machine. A PATH already there is the
        # owner's and stays exactly as it is.
        if ! /usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables:PATH' "$stage" >/dev/null 2>&1; then
            /usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables' "$stage" >/dev/null 2>&1 \
                || plutil -insert EnvironmentVariables -dictionary "$stage" >&2
            plutil -insert EnvironmentVariables.PATH -string "$service_path" "$stage" >&2
        fi
    else
        plutil -create xml1 "$stage" >&2
        plutil -insert Label -string com.znasllc.memql-worker "$stage" >&2
        plutil -insert ProgramArguments -array "$stage" >&2
        plutil -insert ProgramArguments.0 -string "$app/Contents/MacOS/MemQL" "$stage" >&2
        plutil -insert ProgramArguments.1 -string worker "$stage" >&2
        plutil -insert ProgramArguments.2 -string run "$stage" >&2
        plutil -insert RunAtLoad -bool true "$stage" >&2
        plutil -insert KeepAlive -bool true "$stage" >&2
        plutil -insert StandardOutPath -string "$HOME/.memql/state/worker.log" "$stage" >&2
        plutil -insert StandardErrorPath -string "$HOME/.memql/state/worker.log" "$stage" >&2
        plutil -insert EnvironmentVariables -dictionary "$stage" >&2
        plutil -insert EnvironmentVariables.HOME -string "$HOME" "$stage" >&2
        plutil -insert EnvironmentVariables.PATH -string "$service_path" "$stage" >&2
        chmod 600 "$stage"
    fi
    # The installing shell's Ollama variables, so the worker serves from
    # where the setup pulled (memql-cockpit#524). Only these two names.
    local pair key value
    for pair in "${OLLAMA_ENV_PAIRS[@]+"${OLLAMA_ENV_PAIRS[@]}"}"; do
        key="${pair%%=*}"; value="${pair#*=}"
        [[ "$key" == OLLAMA_HOST || "$key" == OLLAMA_MODELS ]] || cap_fail 2 "ollama-env takes OLLAMA_HOST or OLLAMA_MODELS"
        /usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables' "$stage" >/dev/null 2>&1 \
            || plutil -insert EnvironmentVariables -dictionary "$stage" >&2
        plutil -replace "EnvironmentVariables.${key}" -string "$value" "$stage" >&2
    done
    plutil -remove AssociatedBundleIdentifiers "$stage" >/dev/null 2>&1 || true
    plutil -insert AssociatedBundleIdentifiers -array "$stage" >&2
    plutil -insert AssociatedBundleIdentifiers.0 -string com.znasllc.memql-worker "$stage" >&2
    marker="$HOME/.memql/state/app-activation.sha256"
    fingerprint="$(shasum -a 256 "$app/Contents/MacOS/MemQL" "$helper/Contents/MacOS/MemQLCockpit") menu=$menu"
    if [[ ! -f "$plist" ]] || ! cmp -s "$stage" "$plist" || [[ ! -f "$marker" || "$(cat "$marker")" != "$fingerprint" ]]; then
        mkdir -p "$HOME/.memql/backups"
        backup="$(mktemp -d "$HOME/.memql/backups/app-activation.XXXXXX")"
        if [[ -f "$plist" ]]; then cp -p "$plist" "$backup/com.znasllc.memql-worker.plist"; fi
        rm -f "$marker"
        stop_launchagent "gui/$uid_value/com.znasllc.memql-cockpit-menubar" || cap_fail 5 "could not stop previous service; retry after it exits"
        stop_launchagent "gui/$uid_value/com.znasllc.memql-worker" || cap_fail 5 "could not stop previous service; retry after it exits"
        mv "$stage" "$plist"
        start_launchagent "gui/$uid_value" "$plist" "gui/$uid_value/com.znasllc.memql-worker" || cap_fail 5 "could not activate worker bundle; previous plist is in backups"
        cap_changed
    else
        rm -f "$stage"
        if ! launchctl print "gui/$uid_value/com.znasllc.memql-worker" >/dev/null 2>&1; then cap_changed; fi
        start_launchagent "gui/$uid_value" "$plist" "gui/$uid_value/com.znasllc.memql-worker" || cap_fail 5 "could not start worker service"
    fi
    for identifier in com.znasllc.memql-cockpit-worker com.visionarys.memql-cockpit-worker; do
        legacy="$HOME/Library/LaunchAgents/$identifier.plist"
        if [[ -f "$legacy" && ! -L "$legacy" && -O "$legacy" ]]; then
            backup="$(mktemp -d "$HOME/.memql/backups/legacy-agent.XXXXXX")"
            stop_launchagent "gui/$uid_value/$identifier" || cap_fail 5 "could not stop previous service; retry after it exits"
            mv "$legacy" "$backup/$identifier.plist"
            cap_changed
        fi
    done
    if [[ "$menu" == true ]]; then
        "$REPO_ROOT/scripts/macos/install-menubar.sh" --app="$helper" --destination="$helper" >&2 || cap_fail 5 "could not activate embedded menu"
    else
        stop_launchagent "gui/$uid_value/com.znasllc.memql-cockpit-menubar" || cap_fail 5 "could not stop previous service; retry after it exits"
        rm -f "$HOME/Library/LaunchAgents/com.znasllc.memql-cockpit-menubar.plist"
    fi
    mkdir -p "$(dirname "$marker")"
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
