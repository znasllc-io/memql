#!/usr/bin/env bash
#
# scripts/install/install-mac.sh
# ===============================
#
# Install memql-worker on macOS. Downloads the appropriate
# binary, drops a LaunchAgent at ~/Library/LaunchAgents/, and
# (optionally) generates a worker.yaml from a token supplied on the
# command line.
#
# Usage:
#   ./install-mac.sh --token mql_wkr_<...> --cluster https://app.copresent.ai
#
# Flags:
#   --token <token>     Worker token (mql_wkr_<...>). Required.
#   --cluster <url>     Cluster URL. Required.
#   --name <name>       Worker name (default: hostname).
#   --computeruse               Install the computer-use variant (memql-computeruse).
#   --inference         Also set this machine up to serve local models.
#   --version <X.Y.Z>   Pin the release to install (0.16.0 or v0.16.0); default: latest.
#   --download-base <u> Base URL for binary downloads (wins over --version).
#   --force             Remap a home id onto a different cluster_url (not binary overwrite).
#   --no-service        Skip LaunchAgent installation.

set -euo pipefail

# Where lib.sh can be fetched from when this script travels alone.
# Piped execution (`curl ... | bash`) makes $0 `bash`, so the sibling
# source below would look for lib.sh in the operator's cwd and die.
# Pinned to main -- the same raw base the one-liner serves this
# installer from -- and overridable so lib_test.sh can point the fetch
# at a local file:// fixture and stay offline.
readonly RAW_BASE="${MEMQL_INSTALL_RAW_BASE:-https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install}"

# The lib.sh functions this script calls (memql-cockpit#525). Every piped
# fetch comes from raw.githubusercontent.com, which caches each file on its
# own, so a fresh script can arrive with a lib.sh from before a function it
# calls existed -- and stopped with exit 127 halfway through. source_lib
# checks these before anything runs, refetches past the cache once, and
# otherwise refuses in a sentence. lib_test.sh keeps the list complete.
readonly REQUIRED_LIB_FUNCTIONS="allow_installed_apps binary_name_for compare_semver detect_arch fetch_macos_app fetch_macos_menu install_binary_with_mode install_check_release install_sudo install_ui_binary_result install_ui_finish install_ui_init install_ui_stage macos_bundle_requirement macos_signing_transition ollama_env_names ollama_plist_env parse_version_flag read_binary_version read_binary_version_exact release_download_base repair_changed_macos_permissions report_setup_finished report_setup_plan resolve_target_version setup_inference setup_workbench write_worker_yaml"

# missing_lib_functions names the required functions a lib.sh file does not
# define. It reads the TEXT, before anything is sourced: lib.sh declares
# readonly variables, so a second copy could not be sourced over a first.
function missing_lib_functions() {
    local file="$1" fn missing="" text
    # Bash alone: the uninstallers run with PATH cut to a few tools.
    text=$'\n'"$(<"$file")"
    for fn in $REQUIRED_LIB_FUNCTIONS; do
        [[ "$text" == *$'\n'"function ${fn}()"* ]] || missing="${missing} ${fn}"
    done
    printf '%s' "$missing"
}

# fetch_lib fetches lib.sh into a temp file and prints its path.
function fetch_lib() {
    local url="$1" tmp
    tmp="$(mktemp)"
    if ! curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 15 --max-time 60 "$url" -o "$tmp" || [[ ! -s "$tmp" ]]; then
        rm -f "$tmp"
        echo "ERROR: failed to fetch $url" >&2
        echo "       Piped execution needs it; check network access or run from a repo clone." >&2
        exit 4
    fi
    printf '%s\n' "$tmp"
}

# Source the shared helper library. A sibling lib.sh (the cloned-repo
# case) always wins, so a checkout never gains a network dependency.
# Only the piped case fetches -- into a temp file, checked non-empty
# BEFORE sourcing, because under `set -e` a bare `source <(curl ...)`
# turns a failed fetch into a cryptic bash error naming no URL.
function source_lib() {
    local lib missing
    lib="$(dirname "$0")/lib.sh"
    if [[ ! -f "$lib" ]]; then
        lib="$(fetch_lib "${RAW_BASE}/lib.sh")"
        missing="$(missing_lib_functions "$lib")"
        if [[ -n "$missing" && "$RAW_BASE" == http* ]]; then
            # A cached copy from before this script: once more, past the cache.
            rm -f "$lib"
            lib="$(fetch_lib "${RAW_BASE}/lib.sh?refresh=$(date +%s)")"
        fi
    fi
    missing="$(missing_lib_functions "$lib")"
    if [[ -n "$missing" ]]; then
        [[ "$lib" == "$(dirname "$0")/lib.sh" ]] || rm -f "$lib"
        echo "ERROR: this script and its lib.sh come from different versions (lib.sh lacks:${missing})." >&2
        echo "       A cached copy is the usual cause. Nothing was changed; wait a minute and run the same command again." >&2
        exit 4
    fi
    # shellcheck source=lib.sh
    source "$lib"
    [[ "$lib" == "$(dirname "$0")/lib.sh" ]] || rm -f "$lib"
}
source_lib

# The latest release, unless --version pins one (see parse_args).
readonly DEFAULT_DOWNLOAD_BASE="${RELEASE_BASE}/latest/download"

function show_help() {
    cat << EOF
Usage: $(basename "$0") --token <token> --cluster <url> [options]

Required:
    --token <token>           Worker token (mql_wkr_<...>)
    --cluster <url>           Cluster URL (e.g. https://app.copresent.ai)

Options:
    --no-apps                Keep app permissions unchanged. By default, installed
                              Claude Code and Codex are allowed for this cluster.
    --name <name>             Worker name (default: hostname)
    --computeruse                     Install the computer-use variant (memql-computeruse)
    --workbench               Enable isolated Fleet workbenches (requires Docker).
    --inference               Also set this machine up to serve local models:
                              install a model runtime, pull the default models,
                              and write models.allow. At a terminal it asks once
                              before installing the runtime; unattended, it
                              reports the command for you to run. Either way
                              the install still succeeds.
    --user-local              Install under \$HOME/.memql/bin instead of
                              /usr/local/bin. Use when sudo isn't
                              available (CI, restricted environments).
                              Provides weaker isolation -- a compromised
                              user account can swap the binary without
                              privilege escalation.
    --version <X.Y.Z>         Install this release instead of the latest:
                              downloads from releases/download/vX.Y.Z and
                              checks the binary answers to it. "0.16.0" and
                              "v0.16.0" both work. An older release than the
                              installed one is refused (no downgrade).
    --download-base <url>     Override binary download base URL (wins over
                              --version for where to download from)
    --force                   Remap a home id onto a different cluster_url
                              (siblings kept). Not required to refresh the
                              same cluster_url or re-run install; never
                              means "binary exists".
    --no-menu                 Skip the native menu companion
    --no-service              Skip worker and menu LaunchAgent installation
    --verbose                 Show diagnostic output as it happens
    --plain                   Disable color, logo artwork, and animation
    --help                    Print this help
EOF
}

function parse_args() {
    TOKEN=""
    CLUSTER_URL=""
    NAME="$(hostname -s)"
    FLAVOUR="headless"
    DOWNLOAD_BASE="$DEFAULT_DOWNLOAD_BASE"
    FORCE="no"
    INSTALL_SERVICE="yes"
    INSTALL_MENU="yes"
    INSTALL_MODE="system"  # default: sudo-gated /usr/local/bin (#66)
    INFERENCE="no"
    WORKBENCH="no"
    INSTALL_APPS="yes"
    PIN_VERSION=""
    DOWNLOAD_BASE_SET="no"
    INSTALL_VERBOSE="no"
    INSTALL_PLAIN="no"

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --token)         TOKEN="$2"; shift 2 ;;
            --cluster)       CLUSTER_URL="$2"; shift 2 ;;
            --name)          NAME="$2"; shift 2 ;;
            --computeruse)           FLAVOUR="computeruse"; shift ;;
            --workbench)     WORKBENCH="yes"; shift ;;
            --inference)     INFERENCE="yes"; shift ;;
            --no-apps)       INSTALL_APPS="no"; shift ;;
            --user-local)    INSTALL_MODE="user-local"; shift ;;
            --download-base) DOWNLOAD_BASE="$2"; DOWNLOAD_BASE_SET="yes"; shift 2 ;;
            --download-base=*) DOWNLOAD_BASE="${1#*=}"; DOWNLOAD_BASE_SET="yes"; shift ;;
            # Both spellings, because the portal composes one and people
            # type the other. A malformed value is a bad parameter (2)
            # here, not a 404 later.
            --version=*)
                PIN_VERSION="$(parse_version_flag "${1#*=}")" || exit 2
                shift ;;
            --version)
                [[ $# -gt 1 ]] || { echo "ERROR: --version needs a value, e.g. --version 0.16.0" >&2; exit 2; }
                PIN_VERSION="$(parse_version_flag "$2")" || exit 2
                shift 2 ;;
            --force)         FORCE="yes"; shift ;;
            --no-menu)       INSTALL_MENU="no"; shift ;;
            --no-service)    INSTALL_SERVICE="no"; shift ;;
            --verbose)       INSTALL_VERBOSE="yes"; shift ;;
            --plain)         INSTALL_PLAIN="yes"; shift ;;
            --help|-h)       show_help; exit 0 ;;
            *)
                echo "ERROR: unknown flag $1" >&2
                show_help
                exit 1
                ;;
        esac
    done

    if [[ -z "$TOKEN" || -z "$CLUSTER_URL" ]]; then
        echo "ERROR: --token and --cluster are required" >&2
        show_help
        exit 1
    fi
    if [[ ! "$TOKEN" =~ ^mql_wkr_ ]]; then
        echo "ERROR: token must start with mql_wkr_" >&2
        exit 1
    fi
    # A pin names the release to download from -- unless --download-base
    # said where, which wins for the location. Either way the pin is the
    # version the downloaded binary must answer to: MEMQL_INSTALL_VERSION
    # is what resolve_target_version and install_binary_with_mode's
    # exact-build check already read, so the flag feeds that one path
    # rather than a second one.
    if [[ -n "$PIN_VERSION" ]]; then
        MEMQL_INSTALL_VERSION="$PIN_VERSION"
        if [[ "$DOWNLOAD_BASE_SET" != yes ]]; then
            DOWNLOAD_BASE="$(release_download_base "$PIN_VERSION")"
        fi
    fi
}

function install_binary() {
    local binary
    binary="$(binary_name_for "$FLAVOUR")"
    local url="${DOWNLOAD_BASE}/${binary}"
    # ONE installed command for BOTH variants (design D4). The DOWNLOAD
    # artifact carries the variant (memql-computeruse-<os>-<arch>); the
    # installed file never does. Installing the computer-use build as
    # `memql-computeruse` would reinstate the second command name D4
    # retires -- and it is the name the docs, the service unit's
    # ExecStart, and `memql --version`'s variant line all assume.
    local target_ver
    target_ver="$(resolve_target_version "$DOWNLOAD_BASE")"
    install_binary_with_mode "$INSTALL_MODE" "$url" "$binary" "$INSTALLED_COMMAND" "$target_ver" "${MEMQL_INSTALL_VERSION:-}"
    INSTALLED_BINARY="$INSTALL_BINARY_FRIENDLY"
    install_ui_binary_result
}

function write_config() {
    local path="${HOME}/.memql/worker.yaml"
    # The computer-use variant additionally advertises COMPUTERUSE.
    # macOS has no Wayland gate, so the flavour alone decides the set.
    local capabilities="HEADLESS"
    if [[ "$FLAVOUR" == "computeruse" ]]; then
        capabilities="HEADLESS,COMPUTERUSE"
    fi
    write_worker_yaml "$path" "$CLUSTER_URL" "$TOKEN" "$NAME" "$FORCE" "$capabilities"
    INSTALL_UI_RESULT="Machine configured"
}

# Computer-use releases from 0.15 carry the actual permission-bearing app.
# Older releases retain their original raw-worker + standalone-menu layout.
function install_native_app() {
    NATIVE_APP=""; NATIVE_STAGE=""
    [[ "$FLAVOUR" == computeruse ]] || return 0
    local version release_version base source_app app_version app_worker_version old_requirement new_requirement transition alternate
    version="$(read_binary_version_exact "$INSTALLED_BINARY")"
    release_version="${version%%+*}"
    if [[ -z "$version" || "$(compare_semver "$version" 0.15.0)" == -1 ]]; then
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Native app requires Cockpit 0.15.0 or later"
        return 0
    fi
    base="$DOWNLOAD_BASE"
    [[ "$base" != "$DEFAULT_DOWNLOAD_BASE" ]] || base="$(release_download_base "$release_version")"
    NATIVE_STAGE="$(mktemp -d)"
    fetch_macos_app "$base" "$(detect_arch)" "$NATIVE_STAGE" || return $?
    source_app="$NATIVE_STAGE/unpacked/MemQL.app"
    app_version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$source_app/Contents/Info.plist")"
    app_worker_version="$(read_binary_version_exact "$source_app/Contents/MacOS/MemQL")"
    if [[ "$app_version" != "$release_version" || "$app_worker_version" != "$version" ]]; then
        echo "ERROR: app and worker versions differ (app: ${app_version}, bundled worker: ${app_worker_version:-unreadable}, installed worker: ${version})." >&2
        echo "       Re-run the installer to fetch the selected release; if this persists, include these versions when reporting the failure." >&2
        return 3
    fi
    case "$INSTALL_MODE" in system) NATIVE_APP="/Applications/MemQL.app" ;; *) NATIVE_APP="$HOME/Applications/MemQL.app" ;; esac
    if [[ -d "$NATIVE_APP" ]]; then
        old_requirement="$(macos_bundle_requirement "$NATIVE_APP" || true)"
        new_requirement="$(macos_bundle_requirement "$source_app" || true)"
        transition="$(macos_signing_transition "$old_requirement" "$new_requirement")"
        if [[ "$transition" != unchanged ]]; then
            echo "NOTICE: MemQL signing identity $transition; existing enabled privacy entries may not authorize this build."
            if [[ "$transition" == changed ]]; then
                alternate="/Applications/MemQL.app"
                [[ "$INSTALL_MODE" != system ]] || alternate="$HOME/Applications/MemQL.app"
                repair_changed_macos_permissions "$NATIVE_APP" "$source_app" "$alternate" || return $?
            else
                echo "       Signing identity could not be measured; no permission reset performed."
            fi
        else
            echo "INFO: MemQL signing requirement unchanged; preserving existing authorization decisions."
        fi
    fi
    case "$INSTALL_MODE" in
        system)
            NATIVE_APP="/Applications/MemQL.app"
            install_sudo bash "$NATIVE_STAGE/unpacked/scripts/macos/install-app-files.sh" --app="$source_app" --destination="$NATIVE_APP" --cli-path="$INSTALLED_BINARY"
            ;;
        *)
            NATIVE_APP="$HOME/Applications/MemQL.app"
            bash "$NATIVE_STAGE/unpacked/scripts/macos/install-app-files.sh" --app="$source_app" --destination="$NATIVE_APP" --cli-path="$INSTALLED_BINARY"
            ;;
    esac
    if [[ "$INSTALL_SERVICE" == yes ]]; then
        "$NATIVE_APP/Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit" --register-bundles
    fi
    INSTALL_UI_RESULT="MemQL app ready"
}

function cleanup_native_stage() {
    [[ -z "${NATIVE_STAGE:-}" ]] || rm -rf "$NATIVE_STAGE"
}

function install_launch_agent() {
    if [[ "$INSTALL_SERVICE" != "yes" ]]; then
        echo "INFO: --no-service set; skipping LaunchAgent installation"
        return 0
    fi
    if [[ -n "${NATIVE_APP:-}" ]]; then
        local menu=true activate name
        local -a env_args=()
        [[ "$INSTALL_MENU" == yes ]] || menu=false
        activate="$NATIVE_STAGE/unpacked/scripts/macos/activate-app.sh"
        # The Ollama variables reach the app's worker service too, when the
        # release's activation script knows the parameters (memql-cockpit#524).
        while IFS= read -r name; do
            [[ -n "$name" ]] || continue
            if grep -qF "cap_spec_param \"ollama-env\"" "$activate"; then
                env_args+=("--ollama-env=${name}=${!name}")
            else
                echo "WARN: ${name} is set in this shell and this Cockpit release cannot pass it to the worker service; the worker uses Ollama's default." >&2
            fi
        done < <(ollama_env_names)
        bash "$activate" --app="$NATIVE_APP" --menu="$menu" "${env_args[@]+"${env_args[@]}"}"
        # Activation is idempotent when the app/plist are unchanged. Enrollment
        # is not: worker run reads workers.yaml only at startup, so an existing
        # process would keep the previous token (or never discover a new home).
        # Match the raw-worker and Linux installers: reload saved enrollment
        # by restarting only the worker, preserving the menu and all home data.
        launchctl kickstart -k "gui/$(id -u)/com.znasllc.memql-worker"
        INSTALL_SERVICE_STATE=started
        INSTALL_UI_RESULT="Worker started"
        return
    fi
    local plist_dir="${HOME}/Library/LaunchAgents"
    local plist="${plist_dir}/com.znasllc.memql-worker.plist"
    mkdir -p "$plist_dir" "${HOME}/.memql/state"
    # Migrate: retire the pre-rename LaunchAgent so an upgraded machine
    # never runs two workers (znasllc-io/memql#4553).
    local legacy="${plist_dir}/com.znasllc.memql-cockpit-worker.plist"
    if [[ -f "$legacy" ]]; then
        launchctl unload "$legacy" >/dev/null 2>&1 || true
        rm -f "$legacy"
        echo "INFO: removed legacy com.znasllc.memql-cockpit-worker LaunchAgent"
    fi
    # PATH is launchd's four system directories, then where the app
    # installers put claude and codex -- appended, so nothing the user can
    # write to shadows launchctl. Without it the worker finds no app on
    # this machine. internal/worker/servicepath.go spells the same string.
    cat > "$plist" << PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.znasllc.memql-worker</string>
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALLED_BINARY}</string>
        <string>worker</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${HOME}/.memql/state/worker.log</string>
    <key>StandardErrorPath</key>
    <string>${HOME}/.memql/state/worker.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>${HOME}</string>
        <key>PATH</key>
        <string>/usr/bin:/bin:/usr/sbin:/sbin:${HOME}/.local/bin:${HOME}/.claude/local:/opt/homebrew/bin:/usr/local/bin</string>
$(ollama_plist_env)    </dict>
</dict>
</plist>
PLIST
    chmod 644 "$plist"
    echo "INFO: wrote $plist"
    launchctl unload "$plist" >/dev/null 2>&1 || true
    launchctl load "$plist"
    echo "INFO: launched memql-worker LaunchAgent"
    INSTALL_SERVICE_STATE=started
    INSTALL_UI_RESULT="Worker started"
}

function install_menu_companion() {
    [[ -z "${NATIVE_APP:-}" ]] || return 0
    if [[ "$INSTALL_SERVICE" != yes || "$INSTALL_MENU" != yes ]]; then
        echo "INFO: menu companion skipped (--no-service or --no-menu)"
        return 0
    fi
    local version base stage menu_version rc
    version="$(read_binary_version "$INSTALLED_BINARY")"
    if [[ -z "$version" ]]; then
        echo "ERROR: cannot determine installed worker version for menu companion" >&2
        return 5
    fi
    if [[ "$(compare_semver "$version" 0.14.0)" == -1 ]]; then
        echo "INFO: native menu companion requires Cockpit 0.14.0 or later; selected $version"
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Menu companion requires Cockpit 0.14.0 or later"
        return 0
    fi
    base="$DOWNLOAD_BASE"
    # Match the actual installed worker, even if latest moves during install.
    if [[ "$base" == "$DEFAULT_DOWNLOAD_BASE" ]]; then
        base="$(release_download_base "$version")"
    fi
    stage="$(mktemp -d)"
    rc=0
    if fetch_macos_menu "$base" "$(detect_arch)" "$stage"; then
        menu_version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$stage/unpacked/MemQL Cockpit.app/Contents/Info.plist")"
        if [[ "$menu_version" != "$version" ]]; then
            echo "ERROR: menu version $menu_version differs from installed worker $version" >&2
            INSTALL_MENU_REASON="the menu release ($menu_version) does not match the worker ($version)"
            rc=3
        else
            bash "$stage/unpacked/scripts/macos/install-menubar.sh" --app="$stage/unpacked/MemQL Cockpit.app" || rc=$?
            [[ "$rc" == 0 ]] || INSTALL_MENU_REASON="its installer exited ${rc}"
        fi
    else
        rc=$?
        INSTALL_MENU_REASON="it could not be downloaded from $base"
        echo "ERROR: menu companion could not be prepared from $base; worker installation already completed. Retry this installer to finish the menu installation." >&2
    fi
    rm -rf "$stage"
    # The worker is installed and running by now; a menu that did not
    # install is reported in the summary, never a failed install
    # (memql-cockpit#524).
    if [[ "$rc" != 0 ]]; then
        INSTALL_MENU_STATE=failed
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Menu companion not installed"
        return 0
    fi
    INSTALL_UI_RESULT="Menu companion ready"
}

function main() {
    parse_args "$@"
    install_ui_init install
    # Preflight must run before privileges, worker configuration, or services.
    install_ui_stage "Checking the release" install_check_release
    install_ui_stage "Installing Cockpit" install_binary
    if [[ "$FLAVOUR" == computeruse ]]; then
        install_ui_stage "Preparing the MemQL app" install_native_app
    fi
    install_ui_stage "Configuring this machine" write_config
    install_ui_stage "Preparing apps" allow_installed_apps "$INSTALLED_BINARY"
    if [[ "$INSTALL_SERVICE" == yes ]]; then
        install_ui_stage "Starting the worker" install_launch_agent
    fi
    # Everything below runs after the worker has connected, so the worker
    # carries it to the cluster for MemQL OS to show (memql-cockpit#541).
    if [[ "$INSTALL_SERVICE" == yes ]]; then
        report_setup_plan "$INSTALLED_BINARY" "$INFERENCE" "$WORKBENCH"
    fi
    # Setup signals the running worker to reload its policy, so service first.
    # Runtime approval or model download failures do not undo enrollment.
    # Local models before workbenches: models never wait on Docker, and
    # neither stage can abort the install (memql-cockpit#524).
    if [[ "$INFERENCE" == "yes" ]]; then
        install_ui_stage "Preparing local models" setup_inference "$INSTALLED_BINARY"
    fi
    if [[ "$WORKBENCH" == "yes" ]]; then
        install_ui_stage "Preparing Fleet workbenches" setup_workbench "$INSTALLED_BINARY"
    fi
    if [[ -z "${NATIVE_APP:-}" && "$INSTALL_SERVICE" == yes && "$INSTALL_MENU" == yes ]]; then
        install_ui_stage "Preparing the menu companion" install_menu_companion
    fi
    if [[ "$INSTALL_SERVICE" == yes ]]; then
        report_setup_finished "$INSTALLED_BINARY" "$INFERENCE" "$WORKBENCH"
    fi
    install_ui_finish
}

main "$@"
