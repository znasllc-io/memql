#!/usr/bin/env bash
#
# scripts/install/install-linux.sh
# =================================
#
# Install memql-worker on Linux. Downloads the appropriate
# binary, drops a user-systemd service, and (optionally) generates
# a worker.yaml from a token supplied on the command line.

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
readonly REQUIRED_LIB_FUNCTIONS="allow_installed_apps binary_name_for install_binary_with_mode install_check_release install_ui_binary_result install_ui_finish install_ui_init install_ui_stage linux_worker_capabilities parse_version_flag release_download_base resolve_target_version setup_inference write_worker_yaml"

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
    --name <name>             Worker name (default: hostname -s)
    --computeruse             Install the computer-use variant. Wayland and
                              displayless sessions register HEADLESS; X11 enables COMPUTERUSE.
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
    --no-menu                 Skip the desktop tray companion
    --no-service              Skip worker service and desktop tray installation
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
    INSTALL_UI_RESULT="Machine configured"
    # The computer-use variant advertises COMPUTERUSE, but only on X11:
    # RobotGo can't drive Wayland, so a Wayland session downgrades to
    # HEADLESS-only. This platform-specific decision stays here; the
    # shared renderer (write_worker_yaml) only emits what it is handed
    # and honors --force.
    local capabilities
    capabilities="$(linux_worker_capabilities "$FLAVOUR" "${WAYLAND_DISPLAY:-}" "${XDG_SESSION_TYPE:-}" "${DISPLAY:-}")"
    if [[ "$FLAVOUR" == computeruse && "$capabilities" == HEADLESS ]]; then
        echo "INFO: no supported X11 session detected; registering HEADLESS only (X11 required for COMPUTERUSE)"
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Machine configured without computer use (X11 required)"
    fi
    write_worker_yaml "$path" "$CLUSTER_URL" "$TOKEN" "$NAME" "$FORCE" "$capabilities"
}

function install_systemd_unit() {
    if [[ "$INSTALL_SERVICE" != "yes" ]]; then
        echo "INFO: --no-service set; skipping systemd installation"
        return 0
    fi
    if ! command -v systemctl >/dev/null 2>&1; then
        echo "WARN: systemctl not found; skipping service installation"
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Worker not started (systemd unavailable)"
        return 0
    fi
    mkdir -p "${HOME}/.memql/state"
    local unit_dir="${HOME}/.config/systemd/user"
    mkdir -p "$unit_dir"
    # Migrate: retire the pre-rename unit so an upgraded machine never
    # runs two workers (znasllc-io/memql#4553).
    if [[ -f "$unit_dir/memql-cockpit-worker.service" ]]; then
        systemctl --user disable --now memql-cockpit-worker.service >/dev/null 2>&1 || true
        rm -f "$unit_dir/memql-cockpit-worker.service"
        systemctl --user daemon-reload >/dev/null 2>&1 || true
        echo "INFO: removed legacy memql-cockpit-worker.service unit"
    fi
    local unit="${unit_dir}/memql-worker.service"
    local env_file="${HOME}/.memql/worker.env"
    # Create the env file as 0600 even when empty so an operator
    # who later wants to inject MEMQL_WORKER_TOKEN via the env-file
    # path drops it into a file that already has owner-only mode.
    # The unit references EnvironmentFile= with a leading `-` so
    # systemd ignores the file when it's missing.
    if [[ ! -f "$env_file" ]]; then
        umask 077 && touch "$env_file"
    fi
    chmod 0600 "$env_file" 2>/dev/null || true
    # The Ollama variables set in this shell reach the worker service, so
    # the worker looks for models where the setup puts them (memql-cockpit#524).
    local name
    while IFS= read -r name; do
        [[ -n "$name" ]] || continue
        upsert_env_file_var "$env_file" "$name" "${!name}"
        echo "INFO: passed ${name} to the worker service"
    done < <(ollama_env_names)
    cat > "$unit" << UNIT
[Unit]
Description=memQL Cockpit Worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALLED_BINARY} worker run
Restart=on-failure
RestartSec=5
StandardOutput=append:${HOME}/.memql/state/worker.log
StandardError=append:${HOME}/.memql/state/worker.log

# Environment file for the worker token + cluster URL. Marked with
# leading '-' so systemd doesn't fail if the file is missing; the
# worker also reads MEMQL_WORKER_TOKEN from ~/.memql/worker.yaml.
# Use this file (chmod 0600) instead of inline Environment= so
# the token is NOT visible in 'systemctl show' output.
EnvironmentFile=-${env_file}

# Hardening directives. The headless worker doesn't need write
# access to the system tree or other users' home dirs; tighten
# accordingly. The computer-use build (build tag computeruse) needs a
# more permissive ProtectHome to drive the user's desktop, but
# the headless install path here is the default.
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=${HOME}/.memql
NoNewPrivileges=yes
PrivateTmp=yes
RestrictSUIDSGID=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictRealtime=yes
RestrictNamespaces=yes
SystemCallArchitectures=native

[Install]
WantedBy=default.target
UNIT
    echo "INFO: wrote $unit"
    systemctl --user daemon-reload
    systemctl --user enable memql-worker.service
    systemctl --user restart memql-worker.service
    echo "INFO: enabled and started memql-worker.service"
    INSTALL_SERVICE_STATE=started
    INSTALL_UI_RESULT="Worker started"
}

# The companion is included in the same versioned Linux binary, so repair and
# upgrade cannot leave a tray talking a different worker-control protocol.
function install_menu_companion() {
    [[ "$INSTALL_SERVICE" == yes && "$INSTALL_MENU" == yes ]] || return 0
    if ! "$INSTALLED_BINARY" menu --help >/dev/null 2>&1; then
        echo "INFO: this Cockpit release does not include a Linux tray; upgrade to a release with desktop menu support"
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Desktop menu requires a newer Cockpit release"
        return 0
    fi
    "$INSTALLED_BINARY" menu --install
    if [[ -z "${DISPLAY:-}" && -z "${WAYLAND_DISPLAY:-}" ]]; then
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Desktop menu starts at your next desktop sign-in"
    else
        INSTALL_UI_RESULT="Desktop menu started"
    fi
}

function main() {
    parse_args "$@"
    install_ui_init install
    # Preflight must run before privileges, worker configuration, or services.
    install_ui_stage "Checking the release" install_check_release
    install_ui_stage "Installing Cockpit" install_binary
    install_ui_stage "Configuring this machine" write_config
    install_ui_stage "Preparing apps" allow_installed_apps "$INSTALLED_BINARY"
    if [[ "$INSTALL_SERVICE" == yes ]]; then
        install_ui_stage "Starting the worker" install_systemd_unit
        if [[ "$INSTALL_MENU" == yes ]]; then
            install_ui_stage "Preparing the desktop menu" install_menu_companion
        fi
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
    install_ui_finish
}

main "$@"
