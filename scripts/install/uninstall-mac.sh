#!/usr/bin/env bash
#
# scripts/install/uninstall-mac.sh
# ================================
#
# Remove memql-worker from a macOS machine: install-mac.sh run
# backwards, in one line, because an install that is one line and an
# uninstall that is a runbook leaves the token on machines nobody
# finishes cleaning up. Unloads and removes the LaunchAgent, removes
# the binary and its symlink, removes workers.yaml and worker.yaml
# (the tokens); --purge
# takes policy.yaml, the state dir and an emptied ~/.memql as well.
# Every step says what it did, or that there was nothing to do, and
# nothing here prompts except sudo.
#
# Usage:
#   ./uninstall-mac.sh [--cluster=URL|--all-homes] [--purge] [--user-local] [--dry-run]
#
# With neither --cluster nor --all-homes the script decides from the
# machine: one enrollment is removed as if --cluster=<its url> were
# given, none means the runtime files go (--all-homes), and several is
# a refusal that prints the exact command for each. Which install shape
# to remove is detected the same way: the per-user one (~/.memql/bin +
# ~/Applications/MemQL.app), the system one (/usr/local/bin +
# /Applications/MemQL.app, sudo), or both; --user-local narrows the run
# to the per-user shape. Both decisions exist because MemQL OS composes
# this as a one-liner and the person copying it should not have to know
# either answer.
#
# Scope: ~/.memql, the managed LaunchAgents, binary paths under the
# detected prefixes, and the standard MemQL app for each. Rollback
# copies remain. The machine's registration on the cluster is revoked
# from MemQL OS (Fleet -> Machines), not from here -- by the time this
# script could ask, the token that would have spoken for the machine is
# gone.

set -euo pipefail

# Where lib.sh can be fetched from when this script travels alone.
# Piped execution (`curl ... | bash`) makes $0 `bash`, so the sibling
# source below would look for lib.sh in the operator's cwd and die.
# Pinned to main -- the same raw base the one-liner serves this
# script from -- and overridable so lib_test.sh can point the fetch
# at a local file:// fixture and stay offline.
readonly RAW_BASE="${MEMQL_INSTALL_RAW_BASE:-https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install}"

# This file's own name: the remedy printed for an ambiguous run has to
# spell the command back, and under `bash -s --` $0 does not know it.
readonly SCRIPT_NAME="uninstall-mac.sh"

# The lib.sh functions this script calls (memql-cockpit#525). Every piped
# fetch comes from raw.githubusercontent.com, which caches each file on its
# own, so a fresh script can arrive with a lib.sh from before a function it
# calls existed -- and stopped with exit 127 halfway through. source_lib
# checks these before anything runs, refetches past the cache once, and
# otherwise refuses in a sentence. lib_test.sh keeps the list complete.
readonly REQUIRED_LIB_FUNCTIONS="binary_speaks_scoped_unpair first_executable install_mode_dir install_mode_has_files install_ui_init install_ui_stage install_ui_update_stage macos_privacy_scope_unique mode_binary_names note_leftover plan_binaries_with_mode plan_kept_state plan_path plan_purge_state plan_worker_config print_uninstall_summary purge_worker_state read_binary_version record_kept record_removed record_scoped_leftovers record_scoped_restart_failure record_scoped_unpair_failure remove_binaries_with_mode remove_path_if_present remove_worker_config report_kept_state require_cluster_url_flag require_sudo resolve_uninstall_scope scoped_precheck scoped_without_binary unpair_json_remaining unpair_json_removed worker_state_dir_from_yaml"

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

function show_help() {
    cat << EOF
Usage: $(basename "$0") [options]

Removes memql-worker from this machine: the LaunchAgent, the binary
and its symlink, and ~/.memql/workers.yaml plus the legacy
worker.yaml (the tokens). Also removes the standard MemQL app and menu helper;
custom app destinations and rollback copies are retained.

With no options the script decides from the machine: one enrolled
cluster is removed as if --cluster=<its URL> were given; no enrollment
removes the worker runtime as --all-homes would; several enrollments
are refused with the exact command to run for each. The install shape
is detected too: the per-user install (\$HOME/.memql/bin,
\$HOME/Applications/MemQL.app), the system install (/usr/local/bin,
/Applications/MemQL.app, needs sudo), or both when both exist.

Options:
    --cluster=URL             Remove this cluster enrollment. Keep the app and
                              services if any other enrollment remains.
    --all-homes               Remove every worker enrollment and shared runtime.
                              Last/full removal resets MemQL app approvals for
                              Accessibility and Screen Recording only.
    --user-local              Remove only the per-user install under
                              \$HOME/.memql/bin and \$HOME/Applications; never
                              asks for sudo. Without it every shape found is
                              removed, and sudo is asked for only when a
                              system install is actually present.
    --purge                   Also remove ~/.memql/policy.yaml, the state
                              dir (logs, ledgers) and, once it is empty,
                              ~/.memql itself. Without it they are kept,
                              and the script says so. Refused while another
                              enrollment remains.
    --dry-run                 Resolve everything -- the enrollment(s) that
                              match, the install shape(s) found, every path
                              and service that would go, whether sudo would
                              be needed -- and print that plan without
                              changing anything. Exit 0, or the refusal the
                              real run would give before touching anything.
    --verbose                 Show diagnostic output and the full removal ledger
    --plain                   Disable color, logo artwork, and animation
    --help                    Print this help

Exit codes: 0 everything that existed was removed; 2 bad parameter (or
several enrollments and none named); 3 refused; 4 a prerequisite is
missing (sudo, the binary a scoped removal needs); 5 a step failed. A
non-zero exit always names what remains and what to do about it.

The machine's registration on the cluster is revoked from MemQL OS
(Fleet -> Machines), not from here.
EOF
}

function parse_args() {
    REMOVE_SCOPE="auto"  # every install shape present; --user-local narrows to the per-user one
    PURGE="no"
    CLUSTER_URL=""
    ALL_HOMES="no"
    DRY_RUN="no"
    INSTALL_VERBOSE="no"
    INSTALL_PLAIN="no"

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --user-local) REMOVE_SCOPE="user-local"; shift ;;
            --purge)      PURGE="yes"; shift ;;
            --dry-run)    DRY_RUN="yes"; shift ;;
            --cluster=*)
                CLUSTER_URL="${1#*=}"
                [[ -n "$CLUSTER_URL" ]] || { echo "ERROR: --cluster needs a URL" >&2; exit 2; }
                shift ;;
            # The space form takes the NEXT WORD, so an omitted URL must
            # not swallow the flag after it: `--cluster --dry-run` once
            # read `--dry-run` as the cluster and ran for real.
            --cluster)    [[ $# -gt 1 && -n "$2" && "$2" != -* ]] || { echo "ERROR: --cluster needs a URL" >&2; exit 2; }; CLUSTER_URL="$2"; shift 2 ;;
            --all-homes)  ALL_HOMES="yes"; shift ;;
            --verbose)    INSTALL_VERBOSE="yes"; shift ;;
            --plain)      INSTALL_PLAIN="yes"; shift ;;
            --help|-h)    show_help; exit 0 ;;
            *)
                # 2 is "bad parameter" in the capability-script exit
                # convention this tree already uses (preflight_asset's
                # 4, cut-release.sh's table).
                echo "ERROR: unknown flag $1" >&2
                show_help
                exit 2
                ;;
        esac
    done
    # Neither flag is fine -- resolve_uninstall_scope decides from the
    # machine -- but both at once still contradict each other.
    if [[ -n "$CLUSTER_URL" && "$ALL_HOMES" == yes ]]; then
        echo "ERROR: --cluster=URL and --all-homes exclude each other; pass one" >&2
        exit 2
    fi
    # The shape the binary will accept, refused here as a bad parameter
    # rather than later as its "files may need repair" 5.
    [[ -z "$CLUSTER_URL" ]] || require_cluster_url_flag "$CLUSTER_URL" || exit 2
    if [[ -L "$HOME/.memql" ]]; then
        echo "ERROR: refusing an aliased ~/.memql directory" >&2
        exit 3
    fi
}

# worker_app_for_mode prints the standard MemQL app path for an
# install shape; the app follows the CLI's prefix.
function worker_app_for_mode() {
    case "$1" in
        system) echo "/Applications/MemQL.app" ;;
        *)      echo "$HOME/Applications/MemQL.app" ;;
    esac
}

# install_shape_present prints what is on disk for one shape -- the
# bin directory when any of the install's names is in it, the app
# when it is there -- one per line, or nothing.
function install_shape_present() {
    local mode="$1" app
    if install_mode_has_files "$mode"; then
        echo "$(install_mode_dir "$mode")/${INSTALLED_COMMAND} (and siblings)"
    fi
    app="$(worker_app_for_mode "$mode")"
    if [[ -e "$app" || -L "$app" ]]; then
        echo "$app"
    fi
}

# detect_install_shapes decides WHICH shapes go from what is actually
# installed, not from a flag the caller had to know: a machine may hold
# the per-user shape, the system one, both after a mode switch, or
# neither. --user-local narrows the run to the per-user shape and never
# asks for sudo; otherwise every shape found is removed, and sudo is
# named here, before it is asked for, only when something system-owned
# is present. Sets REMOVE_MODES (space-separated; the two mode names
# carry no spaces) and SYSTEM_SHAPE_PRESENT.
function detect_install_shapes() {
    local mode found
    REMOVE_MODES="system user-local"
    [[ "$REMOVE_SCOPE" != user-local ]] || REMOVE_MODES="user-local"
    SYSTEM_SHAPE_PRESENT="no"
    for mode in $REMOVE_MODES; do
        found="$(install_shape_present "$mode")"
        if [[ -n "$found" ]]; then
            echo "INFO: ${mode} install found: $(printf '%s' "$found" | tr '\n' ' ')"
            [[ "$mode" != system ]] || SYSTEM_SHAPE_PRESENT="yes"
        else
            echo "INFO: no ${mode} install under $(install_mode_dir "$mode") or at $(worker_app_for_mode "$mode")"
        fi
    done
    if [[ "$SYSTEM_SHAPE_PRESENT" == yes ]]; then
        echo "INFO: the system install is root-owned; removing it needs sudo, and you may be asked for your password."
    fi
}

# ensure_system_sudo asks for sudo ONCE, right after the lines above
# said why, and only when a system shape is actually present. A run
# that cannot get it stops before touching anything: resetting the
# app's privacy grants and deleting the per-user half around a system
# app that then stays would leave a worse machine than the one found.
function ensure_system_sudo() {
    [[ "$SYSTEM_SHAPE_PRESENT" == yes ]] || return 0
    if require_sudo uninstall; then
        return 0
    fi
    echo "ERROR: this machine has a system install (listed above) and this session cannot use sudo; nothing was changed." >&2
    echo "       Re-run from a terminal where sudo works, or pass --user-local to remove only the per-user install." >&2
    return 4
}

# Stop by service label even when a partially removed install lost its plist.
# A failed stop of a still-loaded agent is an error; never delete its executable.
function stop_agent() {
    local label="$1" target
    if ! command -v launchctl >/dev/null 2>&1; then
        echo "INFO: launchctl not found; removing service files only"
        return 0
    fi
    target="gui/$(id -u)/$1"
    if launchctl print "$target" >/dev/null 2>&1; then
        launchctl bootout "$target" >/dev/null 2>&1 || true
        # bootout can return before launchd removes the job. Allow up to ten
        # seconds for that transition, including a final check at the deadline.
        # A successful bootout alone never authorizes deleting runtime files.
        local attempt
        for ((attempt = 0; attempt <= 40; attempt++)); do
            if ! launchctl print "$target" >/dev/null 2>&1; then
                return 0
            fi
            [[ "$attempt" -lt 40 ]] || break
            sleep 0.25
        done
        echo "ERROR: could not stop $label (still loaded after 10s); runtime files retained" >&2
        echo "       Stop it yourself, then re-run this uninstaller:  launchctl bootout $target" >&2
        return 5
    fi
}

# The MemQL.app whose own executable registered the worker and menu
# services (memql-cockpit#545). Only that app can take them back out of
# Login Items, and only it can start them again: there is no plist to reload.
function app_service_executables() {
    local mode app identifier
    for mode in $REMOVE_MODES; do
        app="$(worker_app_for_mode "$mode")"
        [[ -d "$app" && ! -L "$app" && -f "$app/Contents/Library/LaunchAgents/$SERVICE_LABEL_DARWIN.plist" ]] || continue
        identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)"
        [[ "$identifier" == com.znasllc.memql-worker ]] || continue
        echo "$app/Contents/MacOS/MemQL"
    done
}

# Before anything is removed, the app unregisters what it registered; that
# stops both services. An app from before memql-cockpit#545 registered
# nothing, and remove_launch_agent stops its plists by label.
function unregister_app_services() {
    local executable
    while IFS= read -r executable; do
        [[ -n "$executable" ]] || continue
        if ! "$executable" worker service unregister >/dev/null; then
            echo "ERROR: MemQL's services did not stop; runtime files retained" >&2
            echo "       Run:  \"$executable\" worker service unregister   then re-run this uninstaller." >&2
            return 5
        fi
        echo "INFO: MemQL's worker and menu services removed from Login Items"
    done < <(app_service_executables)
}

# Stop each managed agent before removing its plist. Missing agents are safe
# to clean up; a still-loaded agent aborts removal so a retry can finish later.
function remove_launch_agent() {
    local plist_dir="${HOME}/Library/LaunchAgents"
    local label plist
    # com.znasllc.memql-ollama is the native model runtime `worker setup
    # --inference` installs on a Mac without Homebrew (memql-cockpit#524).
    # It is stopped before --purge takes ~/.memql/ollama out from under it.
    for label in "$SERVICE_LABEL_DARWIN" "$LEGACY_LABEL_DARWIN" "com.visionarys.memql-cockpit-worker" "com.znasllc.memql-cockpit-menubar" "com.znasllc.memql-ollama"; do
        plist="${plist_dir}/${label}.plist"
        stop_agent "$label" || return $?
        remove_path_if_present "$plist" || true
    done
}

# A helper opened directly can outlive its LaunchAgent. Match the current
# user's exact executable and its mapped text file before sending one TERM.
function menu_process_matches() {
    local pid="$1" expected="$2" owner="" state="" executable=""
    read -r owner state executable < <(/bin/ps -ww -p "$pid" -o uid=,stat=,comm=)
    [[ "$owner" == "$EUID" && "$state" != Z* && "$executable" == "$expected" ]]
}

function stop_menu_bundle() {
    local app="$1" expected_id="$2" relative="$3" helper identifier canonical
    [[ -d "$app" && ! -L "$app" ]] || return 0
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)"
    [[ "$identifier" == "$expected_id" ]] || return 0
    helper="$app/$relative"
    [[ -f "$helper" && ! -L "$helper" ]] || return 0
    canonical="$(cd -P "$(dirname "$helper")" && pwd)/$(basename "$helper")"
    local processes pid owner executable attempt
    processes="$(/bin/ps -awwxo pid=,uid=,comm=)" || return 4
    while read -r pid owner executable; do
        [[ "$owner" == "$EUID" && "$executable" == "$helper" ]] || continue
        menu_process_matches "$pid" "$helper" || continue
        if ! /usr/sbin/lsof -a -p "$pid" -d txt -Fn 2>/dev/null | grep -Fx -- "n$canonical" >/dev/null; then
            menu_process_matches "$pid" "$helper" || continue
            echo "ERROR: could not verify menu executable for PID $pid; app retained" >&2
            return 3
        fi
        kill -TERM "$pid" 2>/dev/null || true
        for ((attempt = 0; attempt <= 40; attempt++)); do
            menu_process_matches "$pid" "$helper" || break
            [[ "$attempt" -lt 40 ]] || break
            sleep 0.25
        done
        if menu_process_matches "$pid" "$helper"; then
            echo "ERROR: menu PID $pid remains running after 10s; app retained" >&2
            echo "       Quit MemQL from the menu bar (or kill $pid), then re-run this uninstaller." >&2
            return 5
        fi
        echo "INFO: stopped menu process $pid at $helper"
    done <<< "$processes"
}

# The embedded menu of every shape being removed, then the standalone
# pre-0.15 menu companion, which only ever had the per-user location.
function stop_remaining_menus() {
    local mode
    for mode in $REMOVE_MODES; do
        stop_menu_bundle "$(worker_app_for_mode "$mode")" com.znasllc.memql-worker 'Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit' || return $?
    done
    stop_menu_bundle "$HOME/Applications/MemQL Cockpit.app" com.znasllc.memql-cockpit-menubar 'Contents/MacOS/MemQLCockpit'
}

# Only the standard companion bundle bearing our identifier is removed.
# Custom destinations and rollback copies remain for the owner to inspect.
function remove_menu_companion() {
    local app="$HOME/Applications/MemQL Cockpit.app" identifier
    [[ -e "$app" || -L "$app" ]] || return 0
    if [[ -L "$app" || ! -d "$app" || ! -O "$app" ]]; then
        echo "WARN: leaving unexpected menu app path $app"
        record_kept "$app"
        return 0
    fi
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)"
    if [[ "$identifier" != com.znasllc.memql-cockpit-menubar ]]; then
        echo "WARN: leaving app with unexpected bundle identifier at $app"
        record_kept "$app"
        return 0
    fi
    if rm -rf "$app"; then
        record_removed "$app"
    else
        record_kept "$app (could not be removed; delete it by hand)"
        note_leftover 5
    fi
}

# One shape's app. Only a bundle carrying OUR identifier is removed;
# anything else at the standard path is named and left, with the
# documented codes (4 when sudo could not be had, 5 for a step that
# failed) so the summary is PARTIAL rather than SUCCESS over something
# the person should look at.
function remove_worker_app() {
    local mode="$1" app identifier
    app="$(worker_app_for_mode "$mode")"
    [[ -e "$app" || -L "$app" ]] || return 0
    if [[ -L "$app" || ! -d "$app" ]]; then record_kept "$app (not a regular app bundle; inspect and delete by hand)"; return 5; fi
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)"
    if [[ "$identifier" != com.znasllc.memql-worker ]]; then
        echo "WARN: leaving unrelated app at $app"; record_kept "$app (bundle identifier is not ours; inspect and delete by hand)"; return 5
    fi
    case "$mode" in
        system)
            require_sudo uninstall || { record_kept "$app (needs sudo; delete it by hand)"; return 4; }
            sudo rm -rf "$app" || { record_kept "$app (could not be removed; delete it by hand)"; return 5; }
            ;;
        *)
            if [[ ! -O "$app" ]]; then record_kept "$app (not owned by you; delete it by hand)"; return 5; fi
            rm -rf "$app" || { record_kept "$app (could not be fully removed; delete what is left by hand)"; return 5; }
            ;;
    esac
    record_removed "$app"
}

function remove_worker_apps() {
    local mode rc=0
    for mode in $REMOVE_MODES; do
        remove_worker_app "$mode" || rc=$?
    done
    return "$rc"
}

function remove_binaries_for_modes() {
    local mode rc=0 step_rc
    for mode in $REMOVE_MODES; do
        step_rc=0
        remove_binaries_with_mode "$mode" || step_rc=$?
        [[ "$rc" -ne 0 ]] || rc="$step_rc"
    done
    return "$rc"
}

# retain_runtime is what happens to the app and the CLI when a step
# between stopping the worker and deleting the bundle failed -- a menu
# that would not stop, a permission reset that did not go through. The
# retry needs a bundle Launch Services can still resolve, and a `memql`
# pointing into a deleted app helps nobody, so both stay and the
# summary says so; the error above names the remedy.
function retain_runtime() {
    local why="$1" mode app dest_dir name names
    names="$(mode_binary_names)"
    for mode in $REMOVE_MODES; do
        app="$(worker_app_for_mode "$mode")"
        [[ ! -d "$app" ]] || record_kept "$app (${why})"
        dest_dir="$(install_mode_dir "$mode")"
        for name in $names; do
            if [[ -e "$dest_dir/$name" || -L "$dest_dir/$name" ]]; then
                record_kept "$dest_dir/$name (${why})"
            fi
        done
    done
    app="$HOME/Applications/MemQL Cockpit.app"
    [[ ! -d "$app" ]] || record_kept "$app (${why})"
}

# installed_memql_binary prints a memql that can parse the enrollment
# files: the app's worker, else the CLI in the bin directory, of
# WHICHEVER shape holds one -- which shape is being removed does not
# narrow it, because the binary only reads the enrollment files in
# $HOME and needs no sudo (a --user-local scoped run on a machine with
# only a system install once refused for want of a binary it had). The
# per-user shape is probed first. Any of them will do -- they are the
# same build -- and a dangling symlink is skipped.
function installed_memql_binary() {
    local mode candidates=""
    for mode in user-local system; do
        candidates="$candidates
$(worker_app_for_mode "$mode")/Contents/MacOS/MemQL
$(install_mode_dir "$mode")/${INSTALLED_COMMAND}"
    done
    local IFS=$'\n'
    # shellcheck disable=SC2086  # split on newlines on purpose; paths may hold spaces
    first_executable $candidates
}

# Use the worker's YAML decoder and identity rules, never a text
# approximation of workers.yaml: the shell's view of the registry
# (scoped_precheck) only counts and names clusters -- which homes belong
# to one is the binary's call. A partially missing CLI may still have
# the real app worker. The cases where no binary can be asked (none
# installed, or one older than the 0.15.0 unpair contract) are settled
# by scoped_without_binary: full removal when this is the only
# enrollment, a refusal when others would be swept with it.
function scoped_unpair() {
    scoped_precheck "$SCRIPT_NAME" "$CARRIED_FLAGS" || return $?
    if [[ "$SCOPED_DECISION" == full ]]; then
        OTHER_HOMES=0
        return 0
    fi
    local binary="" ver=""
    binary="$(installed_memql_binary)" || binary=""
    [[ -z "$binary" ]] || ver="$(read_binary_version "$binary")"
    binary_speaks_scoped_unpair "$binary" "$ver" || binary=""
    if [[ -z "$binary" ]]; then
        scoped_without_binary
        return $?
    fi
    local result remaining plist target was_loaded=no rc=0 app_service=""
    result="$(mktemp)"
    "$binary" worker unpair --cluster-url "$CLUSTER_URL" --dry-run --json > "$result" 2> "$result.err" || rc=$?
    case "$rc" in
        0) ;;
        1|2)
            # flag.ExitOnError's "flag provided but not defined" (2) or an
            # unknown `worker` verb (1): a build that does not speak the
            # contract, whatever its version line said. Not a fault of
            # this machine's files, so the no-binary rule applies.
            rm -f "$result" "$result.err"
            echo "INFO: the installed memql${ver:+ v$ver} does not support URL-scoped unpair"
            scoped_without_binary
            return $?
            ;;
        *)
            cat "$result.err" >&2
            rm -f "$result" "$result.err"
            return 5
            ;;
    esac
    rm -f "$result.err"
    remaining="$(unpair_json_remaining "$result")" || { rm -f "$result"; return 5; }
    PREVIEW_REMOVED="$(unpair_json_removed "$result")"
    if [[ "$PURGE" == yes && "$remaining" -gt 0 ]]; then
        rm -f "$result"
        echo "ERROR: --purge would erase state shared with other enrollments; no enrollment was removed. Omit --purge or explicitly use --all-homes." >&2
        return 3
    fi
    # Read-only probe (launchctl print, -f / -L), and BEFORE the dry-run
    # return: a running worker with no regular plist to reload from is a
    # refusal the real run makes before touching anything, so the dry run
    # must make the same one, with the same code (a review finding).
    plist="$HOME/Library/LaunchAgents/$SERVICE_LABEL_DARWIN.plist"
    target="gui/$(id -u)/$SERVICE_LABEL_DARWIN"
    if command -v launchctl >/dev/null 2>&1 && launchctl print "$target" >/dev/null 2>&1; then
        # Reload only a service that was running and can start again: from
        # its retained plist, or by the app that registered it.
        [[ -f "$plist" && ! -L "$plist" ]] || app_service="$(app_service_executables | head -n 1)"
        if [[ "$remaining" -gt 0 && ( ! -f "$plist" || -L "$plist" ) && -z "$app_service" ]]; then
            rm -f "$result"
            echo "ERROR: running worker has no regular service plist to reload safely; nothing was changed." >&2
            echo "       Restore ${plist}, or pass --all-homes to remove every enrollment and the runtime." >&2
            return 3
        fi
        was_loaded=yes
    fi
    if [[ "$DRY_RUN" == yes ]]; then
        # The preview IS the dry run of this step: the binary matched the
        # enrollment(s) without writing anything.
        rm -f "$result"
        echo "  would unpair:  ${CLUSTER_URL} via ${binary} (${PREVIEW_REMOVED} home(s) match; ${remaining} other enrollment(s) would remain)"
        if [[ "$was_loaded" == yes && "$remaining" -gt 0 ]]; then
            echo "  would reload:  ${SERVICE_LABEL_DARWIN} (loaded) for the remaining enrollment(s)"
        fi
        OTHER_HOMES="$remaining"
        return 0
    fi
    # The last enrollment takes the runtime with it, and the runtime may
    # be the system shape: settle sudo BEFORE the worker is stopped and
    # the registry rewritten, or a refused sudo would leave the enrollment
    # gone and the agent booted out under a summary saying nothing was
    # changed (a review finding). A sibling-remaining removal touches
    # nothing system-owned and must keep working without sudo.
    if [[ "$remaining" -eq 0 ]]; then
        ensure_system_sudo || { rc=$?; rm -f "$result"; return "$rc"; }
    fi
    stop_agent "$SERVICE_LABEL_DARWIN" || { rm -f "$result"; return 5; }
    # From here a failure is not "nothing was changed": the worker is
    # down, and the files may be half-applied. The summary must say so,
    # with the command that starts the worker again once they are fixed.
    local restart=""
    if [[ "$was_loaded" == yes && -n "$app_service" ]]; then
        restart="\"$app_service\" worker service register"
    elif [[ "$was_loaded" == yes ]]; then
        restart="launchctl bootstrap gui/$(id -u) $plist"
    fi
    if ! "$binary" worker unpair --cluster-url "$CLUSTER_URL" --json > "$result"; then
        rm -f "$result"
        echo "ERROR: removal did not complete; worker remains stopped so a removed token cannot reconnect. Repair enrollment before restarting." >&2
        record_scoped_unpair_failure "$restart"
        return 5
    fi
    OTHER_HOMES="$(unpair_json_remaining "$result")" || { rm -f "$result"; record_scoped_unpair_failure "$restart"; return 5; }
    rm -f "$result"
    if [[ "$OTHER_HOMES" -gt 0 ]]; then
        # The enrollment IS gone from here on: every failure below is
        # recorded as such (Removed + the stopped worker), never as a
        # refusal that changed nothing (a review finding).
        # A legacy plist that will not unlink is recorded Kept, with its
        # remedy, by remove_path_if_present, and earns the leftover code;
        # the worker for the siblings still reloads, and the run then ends
        # in the PARTIAL summary rather than the SUCCESS line (a
        # verification finding: the `|| true` alone dropped it).
        local legacy
        for legacy in "$LEGACY_LABEL_DARWIN" "com.visionarys.memql-cockpit-worker"; do
            stop_agent "$legacy" || { rc=$?; record_scoped_restart_failure "$restart" "the legacy agent ${legacy} (still loaded; stop it by hand:  launchctl bootout gui/$(id -u)/${legacy})"; return "$rc"; }
            remove_path_if_present "$HOME/Library/LaunchAgents/$legacy.plist" || true
        done
        if [[ "$was_loaded" == yes && -n "$app_service" ]]; then
            "$app_service" worker service register >/dev/null || {
                echo "ERROR: enrollment removed but the worker for the remaining enrollment(s) could not restart; start it with:  ${restart}" >&2
                record_scoped_restart_failure "$restart"
                return 5
            }
        elif [[ "$was_loaded" == yes ]]; then
            launchctl bootstrap "gui/$(id -u)" "$plist" || {
                echo "ERROR: enrollment removed but the worker for the remaining enrollment(s) could not reload; start it with:  ${restart}" >&2
                record_scoped_restart_failure "$restart"
                return 5
            }
        fi
        record_scoped_leftovers || return $?
        echo "SUCCESS: selected cluster enrollment removed; $OTHER_HOMES other enrollment(s) and the shared app, CLI, menu, policy and state retained."
    fi
}

# tccutil is Apple's supported bundle-scoped reset. Reset before deleting the
# bundle so Launch Services can still resolve its identifier. Its success means
# decisions were reset, not that every cached Settings row has disappeared.
function reset_installed_memql_permissions() {
    local mode worker_app app identifier targets="" service failed=no
    for mode in $REMOVE_MODES; do
        worker_app="$(worker_app_for_mode "$mode")"
        if [[ -L "$worker_app" ]]; then
            echo "ERROR: refusing permission cleanup through an aliased MemQL app path" >&2
            return 3
        fi
        for app in "$worker_app" "$worker_app/Contents/Library/LoginItems/MemQL Menu.app" "$HOME/Applications/MemQL Cockpit.app"; do
            [[ -d "$app" && ! -L "$app" ]] || continue
            identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)"
            case "$identifier" in
                com.znasllc.memql-worker|com.znasllc.memql-cockpit-menubar)
                    case " $targets " in *" $identifier "*) ;; *) targets="$targets $identifier" ;; esac ;;
            esac
        done
    done
    if [[ -z "$targets" ]]; then
        echo "INFO: no installed matching MemQL bundle found; no permission reset attempted. If a MemQL row remains in Settings, remove that row manually."
        return 0
    fi
    if [[ "$EUID" -eq 0 ]]; then
        echo "ERROR: run this uninstaller without sudo so permission resets stay scoped to your user account; privileged file removal is handled separately" >&2
        return 3
    fi
    # Privacy decisions are per bundle identifier, so a standard install
    # that STAYS must keep its grants. Only a --user-local run leaves one
    # (the system app); a run removing every shape has no alternate.
    if [[ "$REMOVE_SCOPE" == user-local ]]; then
        macos_privacy_scope_unique "$(worker_app_for_mode system)" || return $?
    fi
    if ! command -v tccutil >/dev/null 2>&1; then
        echo "ERROR: tccutil is unavailable; app retained so its scoped permission cleanup can be retried" >&2
        return 4
    fi
    for identifier in $targets; do
        for service in Accessibility ScreenCapture; do
            if tccutil reset "$service" "$identifier"; then
                echo "INFO: reset $service authorization decisions for $identifier"
            else
                echo "ERROR: could not reset $service for $identifier; remove only its MemQL row in System Settings or retry the scoped reset" >&2
                failed=yes
            fi
        done
    done
    if [[ "$failed" == yes ]]; then
        echo "PARTIAL: app and remaining runtime files retained; macOS permission cleanup did not fully succeed" >&2
        return 5
    fi
    echo "INFO: MemQL authorization decisions reset. If Settings still displays a MemQL row, refresh Settings and remove that row manually; row disappearance is not guaranteed by tccutil."
}

# print_dry_run_plan is main()'s full-removal half as read-only probes,
# in main()'s order, so what it prints is what the real run would do.
# launchctl print and PlistBuddy read; nothing here writes or prompts.
function print_dry_run_plan() {
    local state_dir="$1" label plist target mode app identifier
    echo ""
    echo "DRY RUN: the plan for this machine. Nothing below has been done."
    if [[ "$SYSTEM_SHAPE_PRESENT" == yes ]]; then
        echo "  sudo:          needed (a system install is present); the real run asks once, before removing anything"
    else
        echo "  sudo:          not needed"
    fi
    for label in "$SERVICE_LABEL_DARWIN" "$LEGACY_LABEL_DARWIN" "com.visionarys.memql-cockpit-worker" "com.znasllc.memql-cockpit-menubar" "com.znasllc.memql-ollama"; do
        plist="${HOME}/Library/LaunchAgents/${label}.plist"
        target="gui/$(id -u)/${label}"
        if command -v launchctl >/dev/null 2>&1 && launchctl print "$target" >/dev/null 2>&1; then
            echo "  would stop:    ${label} (loaded)"
        fi
        plan_path "$plist"
    done
    for mode in $REMOVE_MODES; do
        app="$(worker_app_for_mode "$mode")"
        if [[ -d "$app" && ! -L "$app" ]]; then
            identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)"
            if [[ "$identifier" == com.znasllc.memql-worker ]]; then
                echo "  would reset:   Accessibility + ScreenCapture decisions for ${identifier} (and its embedded menu)"
                echo "  would remove:  $app"
            else
                echo "  would keep:    $app (bundle identifier is not ours)"
            fi
        elif [[ -e "$app" || -L "$app" ]]; then
            echo "  would keep:    $app (not a regular app bundle)"
        fi
        plan_binaries_with_mode "$mode"
    done
    if [[ "$REMOVE_SCOPE" == user-local && -d "$(worker_app_for_mode system)" ]]; then
        echo "  note:          $(worker_app_for_mode system) stays (--user-local); if it is a MemQL install the real run refuses the permission reset"
    fi
    app="$HOME/Applications/MemQL Cockpit.app"
    if [[ -d "$app" && ! -L "$app" ]]; then
        echo "  would remove:  $app (standalone menu companion)"
    fi
    plan_worker_config
    if [[ "$PURGE" == yes ]]; then
        plan_purge_state "$state_dir"
    else
        plan_kept_state "$state_dir"
    fi
    echo "  kept always:   CLI credentials, cluster settings, certificates, rollback backups"
    echo ""
    echo "DRY RUN: nothing was changed."
}

# finish is the ONE way out once anything may have changed: every path
# prints the summary -- what went, what stayed and why -- and exits
# with the code it was handed, so a refusal halfway through never
# leaves the person reading a stack of INFO lines with no verdict.
function finish() {
    local rc="$1"
    print_uninstall_summary "$rc"
    exit "$rc"
}

function run_uninstall() {
    # The flags a printed remedy must carry so it is the same run plus
    # the missing piece: --user-local and --dry-run, which stay valid
    # whatever scope is chosen (--purge is added only where it is
    # allowed). Dropping --dry-run made a copied remedy run for real.
    CARRIED_FLAGS=""
    [[ "$REMOVE_SCOPE" != user-local ]] || CARRIED_FLAGS="${CARRIED_FLAGS} --user-local"
    [[ "$DRY_RUN" != yes ]] || CARRIED_FLAGS="${CARRIED_FLAGS} --dry-run"
    resolve_uninstall_scope "$SCRIPT_NAME" "$CARRIED_FLAGS" || exit $?
    detect_install_shapes
    if [[ "$DRY_RUN" == yes ]]; then
        install_ui_update_stage "Planning removal"
    elif [[ -n "$CLUSTER_URL" ]]; then
        install_ui_update_stage "Disconnecting this cluster"
    else
        install_ui_update_stage "Removing Cockpit"
    fi
    # Read BEFORE the token files go: --purge deletes the directory the
    # worker actually used, and the default is only where that usually is.
    local state_dir rc=0
    state_dir="$(worker_state_dir_from_yaml "${HOME}/.memql/worker.yaml")"
    if [[ -n "$CLUSTER_URL" ]]; then
        # Refusals here (a worker that will not stop, a --purge with
        # siblings, a missing binary the person relied on) leave every
        # file as it was; the summary says REFUSED.
        scoped_unpair || finish $?
        # Siblings remain: the worker is still installed for them, so
        # the SUCCESS line scoped_unpair printed is the whole verdict.
        if [[ "$OTHER_HOMES" -gt 0 ]]; then
            [[ "$DRY_RUN" != yes ]] || echo "DRY RUN: the shared app, CLI, menu, policy and state would stay for the remaining enrollment(s). Nothing was changed."
            exit 0
        fi
    fi
    if [[ "$DRY_RUN" == yes ]]; then
        print_dry_run_plan "$state_dir"
        exit 0
    fi
    # From here on the last enrollment is gone or never existed, and the
    # runtime goes: services first, then the app's permission grants,
    # then the app and the CLI, then the token files, then (--purge) the
    # state. The order leaves the least behind if a step is interrupted.
    # A no-op on the scoped path, which settled sudo before its first
    # write; this is the gate for the no-enrollment and no-binary paths.
    ensure_system_sudo || finish $?
    unregister_app_services || finish $?
    remove_launch_agent || finish $?
    local runtime_rc=0
    stop_remaining_menus || runtime_rc=$?
    if [[ "$runtime_rc" -eq 0 ]]; then
        reset_installed_memql_permissions || runtime_rc=$?
    fi
    if [[ "$runtime_rc" -eq 0 ]]; then
        remove_menu_companion
        remove_binaries_for_modes || rc=$?
        remove_worker_apps || rc=$?
    else
        # The app stays resolvable for the retry; the tokens still go,
        # because a stopped worker's dead token is the one thing this
        # script must never leave behind.
        retain_runtime "retained after the error above; re-run this uninstaller once it is fixed"
        rc="$runtime_rc"
    fi
    # The removers record a file they could not remove and carry on; the
    # code such a leftover earns is folded in below, so the run still
    # ends with the summary rather than aborting mid-purge under set -e.
    remove_worker_config || true
    if [[ "$PURGE" == "yes" && "$runtime_rc" -eq 0 ]]; then
        purge_worker_state "$state_dir" || true
    else
        [[ "$PURGE" != yes ]] || echo "INFO: --purge skipped while runtime files are retained; re-run with --purge once they are gone"
        report_kept_state "$state_dir"
    fi
    echo "INFO: CLI credentials, cluster settings, certificates and rollback backups are retained."
    echo "INFO: shared credentials/backups were not purged, and no service-wide permission reset or direct privacy database edit was performed."
    [[ "$rc" -ne 0 ]] || rc="$UNINSTALL_LEFTOVER_RC"
    finish "$rc"
}

function main() {
    parse_args "$@"
    install_ui_init uninstall
    # The existing removal routine owns partial failures and exit codes.
    # Keep it a simple call so unexpected failures still reach the exit trap.
    install_ui_stage "Checking this machine" run_uninstall
}

main "$@"
