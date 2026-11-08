#!/usr/bin/env bash
set -euo pipefail

# One-time bridge for an existing native installation. The verified release
# helper owns signing, replacement, rollback and worker recovery. This driver
# never enrolls, writes worker policy, resets TCC or creates a service.
UPGRADE_SCRATCH=""
UPGRADE_SOURCE=""

function bootstrap_failure() {
    printf '%s\n' "$2" >&2
    printf '{"ok":false,"capability":"cockpit.upgrade.mac","changed":false,"result":{},"error":{"code":%s,"message":"Could not initialize the pinned upgrade. See diagnostics."}}\n' "$1"
    exit "$1"
}

function show_bootstrap_help() {
    printf '%s\n' 'Usage: upgrade-mac.sh --version=X.Y.Z [--confirm=restart-worker]' \
        'Upgrade the existing user-writable MemQL.app without changing enrollment.' \
        'For an older worker, finish active work first, then explicitly confirm its restart.'
}

function load_runtime() {
    local version="" argument entry="${BASH_SOURCE[0]:-}"
    for argument in "$@"; do
        case "$argument" in
            --version=*) version="${argument#*=}" ;;
            --help|-h) show_bootstrap_help; exit 0 ;;
        esac
    done
    if [[ -f "$entry" && "${entry##*/}" == upgrade-mac.sh \
        && -f "$(dirname "$entry")/../../../scripts/lib/capability.sh" && -f "$(dirname "$entry")/lib.sh" ]]; then
        UPGRADE_SOURCE="$(cd "$(dirname "$entry")" && pwd)"
        # shellcheck source=../../../scripts/lib/capability.sh
        source "$UPGRADE_SOURCE/../../../scripts/lib/capability.sh"
        return
    fi
    [[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
        || bootstrap_failure 2 'Pass --version=X.Y.Z for the published Cockpit release.'
    UPGRADE_SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/memql-upgrade-mac.XXXXXX")" \
        || bootstrap_failure 5 'Could not create a private upgrade directory.'
    if ! curl -fsSL --proto '=https' --proto-redir '=https' --max-filesize 2097152 --retry 3 --connect-timeout 15 --max-time 60 \
        "https://raw.githubusercontent.com/znasllc-io/memql/cockpit/v${version}/scripts/lib/capability.sh" \
        -o "$UPGRADE_SCRATCH/capability.sh"; then
        rm -rf -- "$UPGRADE_SCRATCH"
        bootstrap_failure 4 'Could not download the pinned upgrade runtime. Nothing was changed.'
    fi
    source "$UPGRADE_SCRATCH/capability.sh"
}

function cleanup_upgrade() {
    local status=$?
    trap - EXIT
    [[ -z "$UPGRADE_SCRATCH" ]] || rm -rf -- "$UPGRADE_SCRATCH"
    set +e
    (exit "$status")
    _cap_on_exit
    exit "$status"
}

function selected_worker() {
    python3 - "$HOME/Library/LaunchAgents/com.znasllc.memql-worker.plist" <<'PY'
import os, pathlib, plistlib, stat, sys
path = pathlib.Path(sys.argv[1])
try:
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
        raise ValueError()
    data = plistlib.loads(path.read_bytes())
    args = data.get('ProgramArguments', [])
    if data.get('Label') != 'com.znasllc.memql-worker' or len(args) < 3 or args[1:3] != ['worker', 'run']:
        raise ValueError()
    executable = os.path.realpath(args[0])
    if not os.path.isabs(args[0]) or not executable.endswith('/MemQL.app/Contents/MacOS/MemQL'):
        raise ValueError()
    print(executable)
except (OSError, ValueError, TypeError, IndexError, plistlib.InvalidFileException):
    print('The existing native worker LaunchAgent could not be verified. No files were changed.', file=sys.stderr)
    sys.exit(3)
PY
}

function restart_existing_menu() {
    local app="$1" target="gui/$(id -u)/com.znasllc.memql-cockpit-menubar" description
    cap_result_set_raw menu_restarted false
    # A disabled or absent companion stays disabled/absent. The running menu
    # must belong to the same app before its existing job can be restarted.
    description="$(launchctl print "$target" 2>/dev/null)" || return 0
    local menu_state
    menu_state="$(printf '%s\n' "$description" | python3 -c '
import os, sys
lines = [line.strip() for line in sys.stdin]
if not any(line.startswith("pid = ") and line[6:].isdigit() and int(line[6:]) > 0 for line in lines):
    print("stopped"); sys.exit(0)
program = next((line[10:] for line in lines if line.startswith("program = ")), "")
if not os.path.isabs(program) or os.path.realpath(program) != os.path.realpath(sys.argv[1]): sys.exit(1)
print("running")
' "$app/Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit")" || {
        cap_fail 3 'Cockpit was upgraded, but its running menu belongs to another installation. Open the updated app to finish.'
    }
    [[ "$menu_state" == running ]] || return 0
    launchctl kickstart -k "$target" >&2 || cap_fail 5 'Cockpit was upgraded, but its menu could not restart. Open the updated app to finish.'
    cap_changed
    cap_result_set_raw menu_restarted true
}

function main() {
    load_runtime "$@"
    cap_init "cockpit.upgrade.mac" "Upgrade an existing native Cockpit without changing enrollment, policy or permissions."
    cap_spec_param_required "version" "Exact stable Cockpit release, X.Y.Z"
    cap_spec_param "confirm" "restart-worker confirms that legacy work is finished and the worker may restart"
    trap cleanup_upgrade EXIT
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local version confirmation executable installed app arch lib helper result status
    version="$(cap_param version)"; confirmation="$(cap_param confirm)"
    [[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || cap_fail 2 'version must be X.Y.Z'
    [[ -z "$confirmation" || "$confirmation" == restart-worker ]] || cap_fail 2 'confirm must be restart-worker'
    [[ "$(uname -s)" == Darwin ]] || cap_fail 4 'This upgrade command is for macOS'
    [[ "$(id -u)" != 0 ]] || cap_fail 3 'Run this command as the signed-in user, without sudo'
    for helper in python3 curl shasum tar codesign; do command -v "$helper" >/dev/null || cap_fail 4 "$helper is required"; done
    executable="$(selected_worker)" || cap_fail 3 'The existing native installation is not supported by this upgrade command'
    app="${executable%/Contents/MacOS/MemQL}"
    [[ -w "$(dirname "$app")" ]] || cap_fail 3 'This app is protected by administrator ownership. Use its administrator-managed upgrade; this command does not weaken its permissions.'
    installed="$("$executable" --version)" || cap_fail 4 'The installed worker did not report its version'
    [[ "$installed" =~ ^memql\ ([^[:space:]]+)\ \(computeruse\)$ ]] || cap_fail 3 'This command requires the existing computer-use MemQL.app; its variant will not be changed'
    installed="${BASH_REMATCH[1]}"
    case "$(uname -m)" in arm64) arch=arm64 ;; x86_64) arch=amd64 ;; *) cap_fail 4 'Unsupported Mac processor' ;; esac
    [[ -n "$UPGRADE_SCRATCH" ]] || UPGRADE_SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/memql-upgrade-mac.XXXXXX")"
    lib="$UPGRADE_SOURCE/lib.sh"
    if [[ -z "$UPGRADE_SOURCE" ]]; then
        lib="$UPGRADE_SCRATCH/lib.sh"
        curl -fsSL --proto '=https' --proto-redir '=https' --max-filesize 2097152 --retry 3 --connect-timeout 15 --max-time 60 \
            "https://raw.githubusercontent.com/znasllc-io/memql/cockpit/v${version}/cockpit/scripts/install/lib.sh" -o "$lib" \
            || cap_fail 4 'Could not download the pinned release verifier'
    fi
    source "$lib"
    CURL_NET+=(--proto '=https' --proto-redir '=https')
    fetch_macos_app "https://github.com/znasllc-io/memql/releases/download/cockpit/v${version}" "$arch" "$UPGRADE_SCRATCH" >&2 \
        || cap_fail 3 'The selected Mac release could not be downloaded and verified; nothing was changed'
    helper="$UPGRADE_SCRATCH/unpacked/MemQL.app/Contents/MacOS/MemQL"
    codesign --verify --deep --strict "$UPGRADE_SCRATCH/unpacked/MemQL.app" >&2 || cap_fail 3 'Release signature invalid; nothing was changed'
    cap_info 'Upgrading the existing Cockpit. Saved servers, paused connections and policy remain unchanged.'
    [[ "$confirmation" != restart-worker ]] || cap_info 'Legacy restart confirmed. Existing work must be finished before this command is run.'
    status=0
    result="$("$helper" update --bootstrap --installed-executable="$executable" --installed-version="$installed" \
        --version="$version" --confirm="$confirmation" --json)" || status=$?
    printf '%s' "$result" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert isinstance(r,dict) and isinstance(r.get("ok"),bool)' \
        || cap_fail 5 'The release helper returned no valid upgrade result'
    cap_result_set_raw update "$result"
    if printf '%s' "$result" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("updated") is True else 1)'; then cap_changed; fi
    if [[ "$status" != 0 ]] || ! printf '%s' "$result" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["ok"] is True else 1)'; then
        cap_fail 5 'The upgrade did not finish. The update result names the reason and recovery step.'
    fi
    restart_existing_menu "$app"
    cap_result_set app "$app"
    cap_ok
}

main "$@"
