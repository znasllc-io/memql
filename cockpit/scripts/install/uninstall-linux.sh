#!/usr/bin/env bash
#
# scripts/install/uninstall-linux.sh
# ===================================
#
# Remove memql-worker from a Linux machine: install-linux.sh run
# backwards, in one line, because an install that is one line and an
# uninstall that is a runbook leaves the token on machines nobody
# finishes cleaning up. Stops and removes the user-systemd units -- the
# worker's and, when `memql worker setup --inference` wrote one, the
# model runtime's (memql-ollama.service) -- removes the binary and its
# symlink, removes workers.yaml and worker.yaml (the tokens) and
# worker.env; --purge takes
# policy.yaml, the state dir, the native model runtime and its models
# under ~/.memql/ollama, and an emptied ~/.memql as well. Every step says
# what it did, or that there was nothing to do, and nothing here prompts
# except sudo.
#
# Usage:
#   ./uninstall-linux.sh [--cluster=URL|--all-homes] [--purge] [--user-local] [--dry-run]
#
# With neither --cluster nor --all-homes the script decides from the
# machine: one enrollment is removed as if --cluster=<its url> were
# given, none means the runtime files go (--all-homes), and several is
# a refusal that prints the exact command for each. Which install shape
# to remove is detected the same way: the per-user one (~/.memql/bin),
# the system one (/usr/local/bin, sudo), or both; --user-local narrows
# the run to the per-user shape. Both decisions exist because MemQL OS
# composes this as a one-liner and the person copying it should not
# have to know either answer.
#
# Removal is limited to ~/.memql, owned systemd units and desktop-menu
# entries, and the binary paths under the detected prefixes. The
# machine's registration on the cluster is revoked from MemQL OS
# (Fleet -> Machines), not from here -- by the time this script could
# ask, the token that would have spoken for the machine is gone.

set -euo pipefail

# Where lib.sh can be fetched from when this script travels alone.
# Piped execution (`curl ... | bash`) makes $0 `bash`, so the sibling
# source below would look for lib.sh in the operator's cwd and die.
# Pinned to main -- the same raw base the one-liner serves this
# script from -- and overridable so lib_test.sh can point the fetch
# at a local file:// fixture and stay offline.
readonly RAW_BASE="${MEMQL_INSTALL_RAW_BASE:-https://raw.githubusercontent.com/znasllc-io/memql/main/cockpit/scripts/install}"

# This file's own name: the remedy printed for an ambiguous run has to
# spell the command back, and under `bash -s --` $0 does not know it.
readonly SCRIPT_NAME="uninstall-linux.sh"

# The lib.sh functions this script calls (memql-cockpit#525). Every piped
# fetch comes from raw.githubusercontent.com, which caches each file on its
# own, so a fresh script can arrive with a lib.sh from before a function it
# calls existed -- and stopped with exit 127 halfway through. source_lib
# checks these before anything runs, refetches past the cache once, and
# otherwise refuses in a sentence. lib_test.sh keeps the list complete.
readonly REQUIRED_LIB_FUNCTIONS="binary_speaks_scoped_unpair first_executable install_mode_dir install_mode_has_files install_ui_init install_ui_stage install_ui_update_stage plan_binaries_with_mode plan_kept_state plan_path plan_purge_state plan_worker_config print_uninstall_summary purge_worker_state read_binary_version record_kept record_removed record_scoped_leftovers record_scoped_restart_failure record_scoped_unpair_failure remove_binaries_with_mode remove_path_if_present remove_worker_config report_kept_state require_cluster_url_flag resolve_uninstall_scope scoped_precheck scoped_without_binary unpair_json_remaining unpair_json_removed worker_state_dir_from_yaml"

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

Removes memql-worker from this machine: the user-systemd units (the
worker's, and the model runtime's memql-ollama.service when
\`memql worker setup --inference\` wrote one), the binary and its
symlink, and ~/.memql/workers.yaml plus the legacy worker.yaml (the
tokens) and worker.env. Full removal also removes the Cockpit tray service,
launcher, icon and autostart entry. Other desktop files are kept.

With no options the script decides from the machine: one enrolled
cluster is removed as if --cluster=<its URL> were given; no enrollment
removes the worker runtime as --all-homes would; several enrollments
are refused with the exact command to run for each. The install shape
is detected too: the per-user install (\$HOME/.memql/bin), the system
install (/usr/local/bin, needs sudo), or both when both exist.

Options:
    --cluster=URL             Remove this cluster enrollment. Keep the binary
                              and unit if any other enrollment remains.
    --all-homes               Remove every worker enrollment and the runtime.
    --user-local              Remove only the per-user install under
                              \$HOME/.memql/bin; never asks for sudo. Without
                              it every shape found is removed, and sudo is
                              asked for only when a system install is
                              actually present.
    --purge                   Also remove ~/.memql/policy.yaml, the state
                              dir (logs, ledgers), the native model
                              runtime and its models under ~/.memql/ollama
                              and, once it is empty, ~/.memql itself.
                              Without it they are kept, and the script
                              says so. Refused while another enrollment
                              remains.
    --dry-run                 Resolve everything -- the enrollment(s) that
                              match, the install shape(s) found, every path
                              and unit that would go, whether sudo would be
                              needed -- and print that plan without changing
                              anything. Exit 0, or the refusal the real run
                              would give before touching anything.
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

# detect_install_shapes decides WHICH shapes go from what is actually
# installed, not from a flag the caller had to know: a machine may hold
# the per-user binary, the system one, both after a mode switch, or
# neither. --user-local narrows the run to the per-user shape and never
# asks for sudo; otherwise every shape found is removed, and sudo is
# named here, before remove_binaries_with_mode asks for it, only when
# something system-owned is present. Sets REMOVE_MODES (space-separated;
# the two mode names carry no spaces) and SYSTEM_SHAPE_PRESENT.
function detect_install_shapes() {
    local mode
    REMOVE_MODES="system user-local"
    [[ "$REMOVE_SCOPE" != user-local ]] || REMOVE_MODES="user-local"
    SYSTEM_SHAPE_PRESENT="no"
    for mode in $REMOVE_MODES; do
        if install_mode_has_files "$mode"; then
            echo "INFO: ${mode} install found under $(install_mode_dir "$mode")"
            [[ "$mode" != system ]] || SYSTEM_SHAPE_PRESENT="yes"
        else
            echo "INFO: no ${mode} install under $(install_mode_dir "$mode")"
        fi
    done
    if [[ "$SYSTEM_SHAPE_PRESENT" == yes ]]; then
        echo "INFO: the system install is root-owned; removing it needs sudo, and you may be asked for your password."
    fi
}

# installed_memql_binary prints a memql that can parse the enrollment
# files: the CLI in the bin directory of WHICHEVER shape holds one --
# which shape is being removed does not narrow it, because the binary
# only reads the enrollment files in $HOME and needs no sudo (a
# --user-local scoped run on a machine with only a system install once
# refused for want of a binary it had). The per-user shape is probed
# first. Any will do -- they are the same build -- and a dangling
# symlink is skipped.
function installed_memql_binary() {
    local mode candidates=""
    for mode in user-local system; do
        candidates="$candidates
$(install_mode_dir "$mode")/${INSTALLED_COMMAND}"
    done
    local IFS=$'\n'
    # shellcheck disable=SC2086  # split on newlines on purpose; paths may hold spaces
    first_executable $candidates
}

# Use the worker's YAML decoder and identity rules, never a text
# approximation of workers.yaml: the shell's view of the registry
# (scoped_precheck) only counts and names clusters -- which homes belong
# to one is the binary's call. The unit is stopped across the change
# (worker run reads the registry only at start, so a running worker
# would keep the removed token until it happened to restart) and
# started again when other homes remain. The cases where no binary can
# be asked (none installed, or one older than the 0.15.0 unpair
# contract) are settled by scoped_without_binary: full removal when
# this is the only enrollment, a refusal when others would be swept
# with it. This is the macOS scoped_unpair with systemctl for launchctl.
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
    local result remaining unit="${SERVICE_LABEL_LINUX}.service" was_active=no rc=0
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
    if [[ "$DRY_RUN" == yes ]]; then
        # The preview IS the dry run of this step: the binary matched the
        # enrollment(s) without writing anything.
        rm -f "$result"
        echo "  would unpair:  ${CLUSTER_URL} via ${binary} (${PREVIEW_REMOVED} home(s) match; ${remaining} other enrollment(s) would remain)"
        OTHER_HOMES="$remaining"
        return 0
    fi
    if command -v systemctl >/dev/null 2>&1; then
        if systemctl --user is-active --quiet "$unit" 2>/dev/null; then
            was_active=yes
            if ! systemctl --user stop "$unit" >/dev/null 2>&1; then
                rm -f "$result"
                echo "ERROR: could not stop ${unit}; no enrollment was removed." >&2
                echo "       Stop it yourself, then re-run this uninstaller:  systemctl --user stop ${unit}" >&2
                return 5
            fi
            echo "INFO: stopped ${unit} for the enrollment change"
        fi
    else
        echo "INFO: systemctl not found; a running worker keeps its current stream until it is restarted"
    fi
    # From here a failure is not "nothing was changed": the worker is
    # down, and the files may be half-applied. The summary must say so,
    # with the command that starts the worker again once they are fixed.
    local restart=""
    [[ "$was_active" != yes ]] || restart="systemctl --user start ${unit}"
    if ! "$binary" worker unpair --cluster-url "$CLUSTER_URL" --json > "$result"; then
        rm -f "$result"
        echo "ERROR: removal did not complete; worker remains stopped so a removed token cannot reconnect. Repair enrollment before restarting." >&2
        record_scoped_unpair_failure "$restart"
        return 5
    fi
    OTHER_HOMES="$(unpair_json_remaining "$result")" || { rm -f "$result"; record_scoped_unpair_failure "$restart"; return 5; }
    rm -f "$result"
    if [[ "$OTHER_HOMES" -gt 0 ]]; then
        # The pre-rename unit is retired even when the worker stays: two
        # units would run two workers for the remaining homes. A unit file
        # that will not unlink is recorded Kept, with its remedy, by
        # remove_path_if_present, and earns the leftover code; the worker
        # for the siblings still starts, and the run then ends in the
        # PARTIAL summary rather than the SUCCESS line (a verification
        # finding: the `|| true` alone dropped it).
        local legacy_path="${HOME}/.config/systemd/user/${LEGACY_LABEL_LINUX}.service"
        if [[ -f "$legacy_path" ]]; then
            systemctl --user disable --now "${LEGACY_LABEL_LINUX}.service" >/dev/null 2>&1 || true
            remove_path_if_present "$legacy_path" || true
        fi
        if [[ "$was_active" == yes ]]; then
            # The enrollment IS gone from here on: a worker that will not
            # start again is recorded as such (Removed + the stopped
            # service), never as a refusal that changed nothing.
            if ! systemctl --user start "$unit" >/dev/null 2>&1; then
                echo "ERROR: enrollment removed but the worker for the remaining enrollment(s) could not start; start it with:  ${restart}" >&2
                record_scoped_restart_failure "$restart"
                return 5
            fi
            echo "INFO: started ${unit} for the remaining enrollment(s)"
        fi
        record_scoped_leftovers || return $?
        echo "SUCCESS: selected cluster enrollment removed; $OTHER_HOMES other enrollment(s) and the shared CLI, unit, policy and state retained."
    fi
}

# remove_systemd_unit stops, disables and removes the user units: the
# worker's current name, its pre-rename one, and the model runtime's
# (memql-ollama.service, written by `memql worker setup --inference`
# and by nothing else -- without this line a --purge would delete the
# runtime out from under a unit still trying to restart it). The
# runtime's unit goes BEFORE the runtime directory does, for that
# reason. A failed disable (for example an unavailable user manager)
# reports a partial uninstall (5, a failed step) and keeps the unit
# and runtime state for a safe retry while the worker token is still
# removed. A machine with no systemctl on PATH gets the same
# preservation: an absent command cannot establish that the service
# stopped.
# Full runtime removal only; scoped disconnection with sibling homes returns
# before reaching this function. The tray never owns worker enrollment data.
function remove_menu_companion() {
    local binary config_dir data_dir path
    config_dir="${XDG_CONFIG_HOME:-${HOME}/.config}"
    data_dir="${XDG_DATA_HOME:-${HOME}/.local/share}"
    [[ -f "$config_dir/systemd/user/memql-menu.service" || -f "$config_dir/autostart/memql-cockpit.desktop" || -f "$data_dir/applications/memql-cockpit.desktop" || -f "$data_dir/icons/hicolor/64x64/apps/memql-cockpit.png" ]] || return 0
    binary="$(installed_memql_binary)" || binary=""
    if [[ -n "$binary" ]] && "$binary" menu --help >/dev/null 2>&1; then
        "$binary" menu --uninstall || { record_kept "Cockpit menu (stop/removal failed; retry uninstall)"; return 5; }
        record_removed "Cockpit desktop menu and autostart"
        return 0
    fi
    # A removed binary must not strand its autostart entry. Refuse unfamiliar
    # files, and stop the unit before removing its launch files.
    if [[ -f "$config_dir/systemd/user/memql-menu.service" ]]; then
        grep -q '^# Managed by MemQL Cockpit desktop menu$' "$config_dir/systemd/user/memql-menu.service" || { record_kept "unrecognized Cockpit menu service"; return 5; }
        systemctl --user stop memql-menu.service || return 5
    fi
    for path in "$config_dir/systemd/user/memql-menu.service" "$config_dir/autostart/memql-cockpit.desktop" "$data_dir/applications/memql-cockpit.desktop"; do
        [[ -f "$path" ]] || continue
        if ! grep -q '^# Managed by MemQL Cockpit desktop menu$' "$path"; then
            record_kept "$path (unrecognized menu file)"
            return 5
        fi
        remove_path_if_present "$path" || return 5
    done
    remove_path_if_present "$data_dir/icons/hicolor/64x64/apps/memql-cockpit.png" || return 5
    systemctl --user daemon-reload || return 5
}

function remove_systemd_unit() {
    local unit_dir="${HOME}/.config/systemd/user"
    local have_systemctl="yes"
    if ! command -v systemctl >/dev/null 2>&1; then
        have_systemctl="no"
        echo "WARN: systemctl not found; installed units and runtime state will be kept because their services cannot be stopped"
    fi
    local label unit path
    local unit_rc=0
    for label in "$SERVICE_LABEL_LINUX" "$LEGACY_LABEL_LINUX" "$OLLAMA_LABEL_LINUX"; do
        unit="${label}.service"
        path="${unit_dir}/${unit}"
        if [[ ! -f "$path" ]]; then
            # The legacy unit is absent on every machine installed after
            # the rename; only the current ones are worth a line.
            if [[ "$label" != "$LEGACY_LABEL_LINUX" ]]; then
                echo "INFO: ${path} not present; no unit to stop"
            fi
            continue
        fi
        if [[ "$have_systemctl" == "no" ]]; then
            record_kept "$path (systemctl missing; retry uninstall with the user manager available)"
            unit_rc=5
            continue
        fi
        if [[ "$have_systemctl" == "yes" ]]; then
            if systemctl --user disable --now "$unit" >/dev/null 2>&1; then
                echo "INFO: stopped and disabled ${unit}"
            else
                echo "WARN: systemctl --user disable --now ${unit} failed; keeping the unit and runtime state so a running service is not purged"
                record_kept "$path (stop failed; retry uninstall when the user manager is available)"
                unit_rc=5
                continue
            fi
        fi
        if rm -f "$path"; then
            echo "INFO: removed ${path}"
            record_removed "$path"
        else
            echo "WARN: could not remove ${path}; delete it by hand"
            record_kept "$path (could not be removed; delete it by hand)"
            unit_rc=5
        fi
    done
    if [[ "$have_systemctl" == "yes" ]]; then
        systemctl --user daemon-reload >/dev/null 2>&1 || true
    fi
    return "$unit_rc"
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

# print_dry_run_plan is main()'s full-removal half as read-only probes,
# in main()'s order, so what it prints is what the real run would do.
# systemctl is-active reads; nothing here writes or prompts.
function print_dry_run_plan() {
    if [[ -f "${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user/memql-menu.service" ]]; then
        echo "DRY RUN: would stop the Cockpit desktop menu and remove its service, launcher and autostart entry."
    fi
    local state_dir="$1" label unit path mode
    echo ""
    echo "DRY RUN: the plan for this machine. Nothing below has been done."
    if [[ "$SYSTEM_SHAPE_PRESENT" == yes ]]; then
        echo "  sudo:          needed (a system install is present); the real run asks before removing it"
    else
        echo "  sudo:          not needed"
    fi
    for label in "$SERVICE_LABEL_LINUX" "$LEGACY_LABEL_LINUX" "$OLLAMA_LABEL_LINUX"; do
        unit="${label}.service"
        path="${HOME}/.config/systemd/user/${unit}"
        [[ -f "$path" ]] || continue
        if command -v systemctl >/dev/null 2>&1; then
            if systemctl --user is-active --quiet "$unit" 2>/dev/null; then
                echo "  would stop:    ${unit} (active) and disable it"
            else
                echo "  would disable: ${unit}"
            fi
        else
            echo "  would keep:    $path (systemctl not found; a unit that cannot be stopped is not removed)"
            continue
        fi
        plan_path "$path"
    done
    for mode in $REMOVE_MODES; do
        plan_binaries_with_mode "$mode"
    done
    plan_worker_config
    plan_path "${HOME}/.memql/worker.env"
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
    # worker actually used, and the default is only where that usually
    # is.
    local state_dir
    state_dir="$(worker_state_dir_from_yaml "${HOME}/.memql/worker.yaml")"
    if [[ -n "$CLUSTER_URL" ]]; then
        # Refusals here (a unit that will not stop, a --purge with
        # siblings, a missing binary the person relied on) leave every
        # file as it was; the summary says REFUSED.
        scoped_unpair || finish $?
        # Siblings remain: the worker is still installed for them, so
        # the SUCCESS line scoped_unpair printed is the whole verdict.
        if [[ "$OTHER_HOMES" -gt 0 ]]; then
            [[ "$DRY_RUN" != yes ]] || echo "DRY RUN: the shared CLI, unit, policy and state would stay for the remaining enrollment(s). Nothing was changed."
            exit 0
        fi
    fi
    if [[ "$DRY_RUN" == yes ]]; then
        print_dry_run_plan "$state_dir"
        exit 0
    fi
    # Service, binary, tokens: the install in reverse, and the order that
    # leaves the least behind if a step is interrupted -- a
    # Restart=on-failure unit still running would re-exec a binary that
    # is about to go, with a token that is about to go.
    local unit_rc=0
    remove_menu_companion || finish $?
    remove_systemd_unit || unit_rc=$?
    # A binary that needs sudo this run cannot get is reported and
    # left; the tokens still go, because they matter more, and the exit
    # code carries the leftover.
    local binary_rc=0
    remove_binaries_for_modes || binary_rc=$?
    # The removers record a file they could not remove and carry on; the
    # code such a leftover earns is folded in below, so the run still
    # ends with the summary rather than aborting mid-purge under set -e.
    remove_worker_config || true
    remove_path_if_present "${HOME}/.memql/worker.env" || true
    if [[ "$PURGE" == "yes" && "$unit_rc" -eq 0 ]]; then
        purge_worker_state "$state_dir" || true
    else
        [[ "$PURGE" != yes ]] || echo "INFO: --purge skipped while a unit is retained; re-run with --purge once it is gone"
        report_kept_state "$state_dir"
    fi
    if [[ "$unit_rc" -ne 0 ]]; then binary_rc="$unit_rc"; fi
    [[ "$binary_rc" -ne 0 ]] || binary_rc="$UNINSTALL_LEFTOVER_RC"
    finish "$binary_rc"
}

function main() {
    parse_args "$@"
    install_ui_init uninstall
    # The existing removal routine owns partial failures and exit codes.
    # Keep it a simple call so unexpected failures still reach the exit trap.
    install_ui_stage "Checking this machine" run_uninstall
}

main "$@"
