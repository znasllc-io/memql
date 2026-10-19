#!/usr/bin/env bash
#
# scripts/install/lib.sh
# ======================
#
# Shared function library for the worker install scripts --
# function-based helpers sourced by the OS-specific drivers
# install-mac.sh and install-linux.sh, and by their inverses
# uninstall-mac.sh and uninstall-linux.sh. write_worker_yaml (below) is
# the single renderer for ~/.memql/workers.yaml (+ legacy mirror), so the config layout
# can never drift between the two platforms; the uninstall helpers at
# the bottom are the single statement of what an uninstall removes,
# for the same reason. The pure-logic helpers are exercised by
# lib_test.sh, wired into CI via
# .github/workflows/install-scripts-lint.yml.

set -uo pipefail

# Install modes:
#   system     -- /usr/local/bin (immutable; root-owned; sudo-gated).
#                 Default. A compromised user account can no longer
#                 swap the cockpit binary without privilege escalation
#                 (Wave 6 #45 hardening; #66 follow-up).
#   user-local -- ${HOME}/.memql/bin (user-writable). Opt-in via
#                 --user-local. Weaker isolation; kept for CI /
#                 restricted environments where sudo isn't available.
readonly INSTALL_PREFIX_SYSTEM="/usr/local/bin"
readonly INSTALL_PREFIX_USER="${HOME}/.memql/bin"

readonly STATE_DIR_DEFAULT="${HOME}/.memql/state"

# Where releases live. Both download bases the installers compose hang
# off this one root: `latest/download` when no version is named, and
# `download/vX.Y.Z` for a --version pin (and for the app / menu
# archives, which must match the worker that was just installed).
# Overridable ONLY so lib_test.sh can point the composition at a
# file:// fixture and prove the pinned URL offline; the published
# default is GitHub, and download_binary still enforces https on it.
readonly RELEASE_BASE="${MEMQL_INSTALL_RELEASE_BASE:-https://github.com/znasllc-io/memql-cockpit/releases}"

# Network discipline for every fetch (memql-cockpit#525): a transient
# failure is retried, a connection that never opens is given up on, and
# every transfer is bounded -- one dropped connection used to fail an
# install and a stalled one to hang it. A large download also gives up when
# it crawls below 1 KB/s for a minute, which a half-hour --max-time alone
# would let run on. Plain arrays, not readonly ones: macOS bash 3.2.
CURL_NET=(--retry 3 --retry-delay 2 --connect-timeout 15)
CURL_SMALL=(--max-time 60)
CURL_LARGE=(--max-time 1800 --speed-limit 1024 --speed-time 60)

# The installed command. ONE name for both build variants (design D4,
# znasllc-io/memql-cockpit#352): the download artifact carries the variant,
# the installed file never does. `memql --version` is what answers "which
# build is this", which only works because the path cannot.
readonly INSTALLED_COMMAND="memql"

# The pre-rename binary names the installers retire in place and the
# uninstallers remove (znasllc-io/memql#4553). The root install.sh
# carries its own copy under the same variable name -- it is fetched
# alone by `curl | sh` and has no lib.sh to source -- and lib_test.sh
# reads both files to hold the two copies together.
readonly LEGACY_BINARIES="memql-cockpit memql-cockpit-computeruse"

# The service names, current and pre-rename, on each platform. The
# installers write the current one and retire the legacy one; the
# uninstallers stop and remove both. install.sh restates these four
# too, under these names, for the reason above. Nothing in this file
# reads them -- the drivers that source it do -- hence the directive.
# shellcheck disable=SC2034  # read by the uninstallers, which source this file
readonly SERVICE_LABEL_DARWIN="com.znasllc.memql-worker" \
         LEGACY_LABEL_DARWIN="com.znasllc.memql-cockpit-worker" \
         SERVICE_LABEL_LINUX="memql-worker" \
         LEGACY_LABEL_LINUX="memql-cockpit-worker"

# The model runtime's own user unit on Linux, written by `memql worker
# setup --inference` (internal/worker/inference/stage.go, OllamaUnitName)
# rather than by an installer, and removed by the Linux uninstaller so a
# machine is not left serving models from a runtime nobody can see in
# Fleet. It has no macOS twin: there the runtime is Homebrew's service.
# shellcheck disable=SC2034  # read by uninstall-linux.sh, which sources this file
readonly OLLAMA_LABEL_LINUX="memql-ollama"

# ---------------------------------------------------------------
# Version-aware install + shared terminal UX
# ---------------------------------------------------------------
#
# VERSION is the cockpit tag (see VERSION / VERSIONING.md). Installed
# binaries answer `memql --version` as `memql X.Y.Z (headless|computeruse)`.
# --force never means "binary exists"; it only remaps workers.yaml homes
# (see write_worker_yaml). Same version → no-op skip; newer installed →
# refuse downgrade; older/missing → install/upgrade.

# Terminal artwork is generated from native/macos/mark.svg by render-mark.py.
function memql_terminal_mark() {
# BEGIN GENERATED MEMQL MARK
    cat <<'MEMQL_MARK'
⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣤⣤⠀⠀⠀⠀⠀⠀⠀⠀
⠀⠀⢀⣤⣀⣀⣤⠤⠖⣺⠿⠿⣄⠀⠀⠀⠀⠀⠀⠀
⠀⠀⠸⡿⠟⠶⢦⣶⣾⣁⣀⠀⠈⠓⢄⠀⠀⠀⠀⠀
⠀⠀⢠⡇⠀⢀⡼⠛⢯⠉⠉⠙⠛⠒⠲⣿⣿⡆⠀⠀
⠀⠀⣸⢀⡴⠋⠀⠀⠈⢧⠀⠀⠀⠀⣰⢫⡏⠁⠀⠀
⠀⣴⣿⡋⠀⠀⠀⠀⠀⠈⣇⠀⣠⠞⠁⣸⠀⠀⠀⠀
⠀⠛⠛⢯⡉⠙⠓⠒⠲⠦⣾⣿⣇⠀⢀⡏⠀⠀⠀⠀
⠀⠀⠀⠀⠙⠦⡀⠀⢀⡴⠋⢉⣉⡽⣿⣿⡀⠀⠀⠀
⠀⠀⠀⠀⠀⠀⢸⣿⣿⠒⠋⠉⠀⠀⠀⠀⢙⣶⣦⠀
⠀⠀⠀⠀⠀⠀⠀⠉⠁⠀⠀⠀⠀⠀⠀⠀⠈⠛⠛⠀
MEMQL_MARK
# END GENERATED MEMQL MARK
}

# Keep the existing entry point for uninstallers, which share this library.
function memql_ascii_banner() {
    printf '\n  MemQL Cockpit%s\n\n' "${1:+  v${1#v}}"
}

function install_step() {
    printf '\n  %s\n' "$2"
}

# Installer presentation stays here so `curl | bash` needs just this library.
# fd 3 is the terminal; fd 4 carries diagnostics through a redactor to disk.
# Stage callbacks MUST be simple commands, never the condition of if/||/!: a
# conditional call disables errexit throughout the callback on Bash 3.2 too.
function install_ui_log_directory() {
    if [[ -n "${MEMQL_INSTALL_LOG_DIR:-}" ]]; then
        printf '%s\n' "$MEMQL_INSTALL_LOG_DIR"
        return
    fi
    if [[ "${INSTALL_UI_OPERATION:-install}" == uninstall && "${DRY_RUN:-no}" == yes ]]; then
        mktemp -d "${TMPDIR:-/tmp}/memql-uninstall-preview.XXXXXX"
        return
    fi
    case "$(uname -s)" in
        Darwin) printf '%s\n' "$HOME/Library/Logs/MemQL" ;;
        *) printf '%s\n' "${XDG_STATE_HOME:-$HOME/.local/state}/memql/${INSTALL_UI_OPERATION:-install}" ;;
    esac
}

function install_ui_init() {
    local log_dir
    exec 3>&1
    INSTALL_UI_OPERATION="${1:-install}"
    INSTALL_UI_CAPTION='Machine setup'
    [[ "$INSTALL_UI_OPERATION" != uninstall ]] || INSTALL_UI_CAPTION='Remove from this machine'
    INSTALL_UI_ACTIVE=yes
    INSTALL_UI_TTY=no; INSTALL_UI_UNICODE=no
    INSTALL_UI_ACCENT=""; INSTALL_UI_DIM=""; INSTALL_UI_RESET=""
    INSTALL_UI_SPINNER=""; INSTALL_UI_LOGGER=""; INSTALL_UI_STAGE=""
    INSTALL_UI_CURSOR_HIDDEN=no
    INSTALL_MODELS_STATE=off; INSTALL_SERVICE_STATE=skipped
    INSTALL_MODELS_REASON=""; INSTALL_WORKBENCH_STATE=off; INSTALL_WORKBENCH_REASON=""
    INSTALL_MENU_STATE=off; INSTALL_MENU_REASON=""
    INSTALL_PERMISSIONS_STATE=off; INSTALL_PERMISSIONS_REASON=""
    if [[ -t 3 && "${TERM:-dumb}" != dumb && "${INSTALL_PLAIN:-no}" != yes ]]; then
        INSTALL_UI_TTY=yes
        case "${LC_ALL:-${LC_CTYPE:-${LANG:-}}}" in
            *UTF-8*|*utf-8*|*UTF8*|*utf8*) INSTALL_UI_UNICODE=yes ;;
        esac
        if [[ -z "${NO_COLOR+x}" ]]; then
            INSTALL_UI_ACCENT=$'\033[32m'; INSTALL_UI_DIM=$'\033[2m'; INSTALL_UI_RESET=$'\033[0m'
        fi
    fi
    log_dir="$(install_ui_log_directory)"
    # Only diagnostic storage exists before preflight. Never touch worker
    # configuration or ask for privileges just to prepare a log.
    if ! (umask 077; mkdir -p "$log_dir"); then
        printf '\n  Could not create the installer log directory: %s\n' "$log_dir" >&3
        return 5
    fi
    INSTALL_LOG="$(umask 077; mktemp "$log_dir/${INSTALL_UI_OPERATION}-$(date +%Y%m%d-%H%M%S).XXXXXX")"
    trap 'install_ui_exit "$?"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    install_ui_header
}

function install_ui_header() {
    local line row=0 caption
    printf '\n' >&3
    if [[ "$INSTALL_UI_UNICODE" == yes && "${COLUMNS:-80}" -ge 64 ]]; then
        while IFS= read -r line; do
            caption=""
            case "$row" in
                3) caption="MemQL Cockpit" ;;
                5) caption="$INSTALL_UI_CAPTION" ;;
            esac
            printf '  %s%s%s    %s\n' "$INSTALL_UI_ACCENT" "$line" "$INSTALL_UI_RESET" "$caption" >&3
            row=$((row + 1))
        done < <(memql_terminal_mark)
    else
        printf '  MemQL Cockpit\n  %s\n' "$INSTALL_UI_CAPTION" >&3
    fi
    printf '\n' >&3
}

function install_log_filter() {
    # No raw log is written, even temporarily. Strip terminal controls and
    # common credentials before either persisting or showing verbose output.
    LC_ALL=C awk -v verbose="${INSTALL_VERBOSE:-no}" '
        {
            gsub(/\033\[[0-?]*[ -\/]*[@-~]/, "")
            gsub(/[[:cntrl:]]/, "")
            while (match($0, /(^|[^[:alnum:]_])mql_[[:alnum:]_-]+/)) {
                prefix = substr($0, RSTART, 1)
                if (prefix == "m") prefix = ""
                $0 = substr($0, 1, RSTART - 1) prefix "[redacted]" substr($0, RSTART + RLENGTH)
            }
            gsub(/[Bb][Ee][Aa][Rr][Ee][Rr][ ]+[^ ,;]+/, "Bearer [redacted]")
            print; fflush()
            if (verbose == "yes") { print "      " $0 > "/dev/fd/3"; fflush("/dev/fd/3") }
        }
    '
}

function install_ui_start_progress() {
    [[ "$INSTALL_UI_TTY" == yes && "${INSTALL_VERBOSE:-no}" != yes ]] || return 0
    INSTALL_UI_CURSOR_HIDDEN=yes
    printf '\033[?25l' >&3
    (
        local frame=0 dots
        while :; do
            case "$frame" in
                0) dots='o . .' ;;
                1|3) dots='. o .' ;;
                2) dots='. . o' ;;
            esac
            if [[ "$INSTALL_UI_UNICODE" == yes ]]; then
                dots="${dots//o/●}"; dots="${dots//./·}"
            fi
            printf '\r\033[2K  %s%s%s  %s' "$INSTALL_UI_ACCENT" "$dots" "$INSTALL_UI_RESET" "$INSTALL_UI_STAGE" >&3
            frame=$(((frame + 1) % 4))
            sleep 0.18
        done
    ) &
    INSTALL_UI_SPINNER=$!
}

function install_ui_stop_progress() {
    if [[ -n "$INSTALL_UI_SPINNER" ]]; then
        kill "$INSTALL_UI_SPINNER" 2>/dev/null || true
        wait "$INSTALL_UI_SPINNER" 2>/dev/null || true
        INSTALL_UI_SPINNER=""
    fi
    if [[ "$INSTALL_UI_CURSOR_HIDDEN" == yes ]]; then
        printf '\r\033[2K\033[?25h' >&3
        INSTALL_UI_CURSOR_HIDDEN=no
    fi
}

# Change the active operation without adding per-file notices to the screen.
function install_ui_update_stage() {
    install_ui_stop_progress
    INSTALL_UI_STAGE="$1"
    printf '\n[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$INSTALL_UI_STAGE"
    if [[ "$INSTALL_UI_TTY" != yes || "${INSTALL_VERBOSE:-no}" == yes ]]; then
        printf '  ...  %s\n' "$INSTALL_UI_STAGE" >&3
    fi
    install_ui_start_progress
}

function install_ui_close_log() {
    if [[ -n "$INSTALL_UI_LOGGER" ]]; then
        exec 4>&-
        wait "$INSTALL_UI_LOGGER"
        INSTALL_UI_LOGGER=""
    fi
}

function install_ui_stage() {
    INSTALL_UI_STAGE="$1"; shift
    INSTALL_UI_RESULT="$INSTALL_UI_STAGE"
    INSTALL_UI_STAGE_STATE='done'
    if [[ "$INSTALL_UI_TTY" != yes || "${INSTALL_VERBOSE:-no}" == yes ]]; then
        printf '  ...  %s\n' "$INSTALL_UI_STAGE" >&3
    fi
    install_ui_start_progress
    # An explicit reader is waitable on macOS Bash 3.2; a process
    # substitution is not, and could lose the tail of a diagnostic log.
    mkfifo -m 600 "$INSTALL_LOG.pipe"
    install_log_filter < "$INSTALL_LOG.pipe" >> "$INSTALL_LOG" &
    INSTALL_UI_LOGGER=$!
    exec 4> "$INSTALL_LOG.pipe"
    rm -f "$INSTALL_LOG.pipe"
    printf '\n[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$INSTALL_UI_STAGE" >&4
    "$@" >&4 2>&1
    install_ui_stop_progress
    install_ui_close_log
    local symbol='+' color="$INSTALL_UI_ACCENT"
    [[ "$INSTALL_UI_UNICODE" != yes ]] || symbol='✓'
    if [[ "$INSTALL_UI_STAGE_STATE" == pending ]]; then
        symbol='-'; color="$INSTALL_UI_DIM"
        [[ "$INSTALL_UI_UNICODE" != yes ]] || symbol='○'
    fi
    printf '  %s%s%s  %s\n' "$color" "$symbol" "$INSTALL_UI_RESET" "$INSTALL_UI_RESULT" >&3
    INSTALL_UI_STAGE=""
}

function install_ui_exit() {
    local rc="$1"
    trap - EXIT INT TERM
    # An exit inside the callback still has stdout/stderr pointing at the
    # diagnostic pipe. Release those writers too before waiting for EOF.
    exec 1>&3 2>&3
    # Cleanup must not replace the original operation's exit status.
    install_ui_stop_progress
    install_ui_close_log || true
    rm -f "$INSTALL_LOG.pipe"
    if declare -F cleanup_native_stage >/dev/null; then
        cleanup_native_stage 2>&1 | install_log_filter >> "$INSTALL_LOG" || true
    fi
    if [[ "$INSTALL_UI_OPERATION" == uninstall ]]; then
        uninstall_ui_finish "$rc"
        exit "$rc"
    fi
    if [[ "$rc" != 0 ]]; then
        if [[ "$rc" == 130 || "$rc" == 143 ]]; then
            printf '\n  Installation interrupted.\n' >&3
        else
            printf '\n  Could not finish: %s.\n' "${INSTALL_UI_STAGE:-installation}" >&3
        fi
        printf '  Details: %s\n  Re-run the same command to retry; add --verbose for live details.\n\n' "$INSTALL_LOG" >&3
    fi
    exit "$rc"
}

function install_check_release() {
    local asset_name
    # shellcheck disable=SC2153 # assigned by both platform drivers
    asset_name="$(binary_name_for "$FLAVOUR")"
    echo "INFO: os=$(detect_os) arch=$(detect_arch) flavour=${FLAVOUR}"
    preflight_registry
    # shellcheck disable=SC2153 # assigned by both platform drivers
    preflight_asset "${DOWNLOAD_BASE}/${asset_name}" "$FLAVOUR"
    INSTALL_UI_RESULT='Release available'
}

function install_ui_binary_result() {
    case "$INSTALL_BINARY_ACTION" in
        skip) INSTALL_UI_RESULT="Cockpit v${INSTALL_BINARY_AFTER} is up to date" ;;
        upgrade) INSTALL_UI_RESULT="Cockpit updated to v${INSTALL_BINARY_AFTER}" ;;
        *) INSTALL_UI_RESULT="Cockpit v${INSTALL_BINARY_AFTER} installed" ;;
    esac
}

function install_ui_finish() {
    local version command
    version="$(read_binary_version "$INSTALLED_BINARY")"
    printf '\n  %sCockpit v%s is installed.%s\n' "$INSTALL_UI_ACCENT" "$version" "$INSTALL_UI_RESET" >&3
    if [[ "$INSTALL_SERVICE_STATE" == started ]]; then
        printf '  Worker started. Return to Fleet to check the connection.\n' >&3
    else
        printf '  Worker not started. Run:\n\n    %q worker run\n' "$INSTALLED_BINARY" >&3
    fi
    # One honest line per optional stage that did not finish, with its
    # retry command when a retry can help (memql-cockpit#524). A machine
    # that cannot run local models is told so once and never sent to run
    # the same command again.
    case "$INSTALL_MODELS_STATE" in
        pending)
            printf '\n  Local models need your approval to install their runtime. When ready, run:\n\n    %q worker setup --inference\n' "$INSTALLED_BINARY" >&3
            ;;
        unavailable)
            printf '\n  This machine can'"'"'t run local models: %s\n' "${INSTALL_MODELS_REASON:-see the log for the reason.}" >&3
            ;;
        failed)
            printf '\n  Local models could not be set up: %s\n  Retry with:\n\n    %q worker setup --inference\n' \
                "${INSTALL_MODELS_REASON:-see the log for details.}" "$INSTALLED_BINARY" >&3
            ;;
    esac
    case "${INSTALL_PERMISSIONS_STATE:-off}" in
        skipped|failed)
            printf '\n  Computer use needs Accessibility and Screen Recording: %s\n  Turn them on from MemQL in the menu bar → macOS permissions → Set Up Permissions…\n' \
                "${INSTALL_PERMISSIONS_REASON:-they are not allowed yet.}" >&3
            ;;
    esac
    case "$INSTALL_WORKBENCH_STATE" in
        unsupported)
            printf '\n  Fleet workbenches are not in Cockpit v%s. Run this installer again once a newer Cockpit is released.\n' "$version" >&3
            ;;
        failed)
            printf '\n  Fleet workbenches need attention: %s\n  Retry with:\n\n    %q worker setup --workbench\n' \
                "${INSTALL_WORKBENCH_REASON:-see the log for details.}" "$INSTALLED_BINARY" >&3
            ;;
    esac
    if [[ "${INSTALL_MENU_STATE:-off}" == failed ]]; then
        printf '\n  The menu companion was not installed: %s. Run this installer again to finish it.\n' "${INSTALL_MENU_REASON:-see the log}" >&3
    fi
    if [[ -n "${INSTALL_APPS_RECOVERY:-}" ]]; then
        printf '\n  App permissions need attention (Cockpit 0.17.0 or later).\n  See the log, then run:\n\n    %s\n' "$INSTALL_APPS_RECOVERY" >&3
    fi
    printf '\n  %sInstall log: %s%s\n\n' "$INSTALL_UI_DIM" "$INSTALL_LOG" "$INSTALL_UI_RESET" >&3
}

# Privileged commands in a quiet stage must never wait on a hidden prompt.
# Revalidate immediately before each write in case a long download expired it.
function install_sudo() {
    require_sudo
    sudo -n "$@"
}

function uninstall_ui_finish() {
    local rc="$1" other error_line heading color="$INSTALL_UI_ACCENT"
    if [[ "$rc" != 0 ]]; then
        color="$INSTALL_UI_RESET"
        heading='Removal did not finish.'
        [[ -z "$UNINSTALL_REMOVED" ]] || heading='Removal incomplete. Some items need attention.'
        [[ "$rc" != 130 && "$rc" != 143 ]] || heading='Removal interrupted.'
        printf '\n  %s\n' "$heading" >&3
        # One reason on screen; the full removed/kept ledger and remedies stay
        # in the redacted log. Never claim nothing changed after a failed step.
        error_line="$(awk '/ERROR:/{sub(/^.*ERROR: */, ""); print; exit}' "$INSTALL_LOG")"
        [[ -z "$error_line" ]] || printf '  %s\n' "$error_line" >&3
        if [[ -n "${UNINSTALL_UI_CHOICES:-}" ]]; then
            printf '  Choose one with --cluster=URL, or use --all-homes:\n' >&3
            while IFS= read -r other; do
                [[ -z "$other" ]] || printf '    %s\n' "$other" >&3
            done <<< "$UNINSTALL_UI_CHOICES"
        fi
        printf '  Review the log before retrying; add --verbose for live details.\n' >&3
    elif [[ "${DRY_RUN:-no}" == yes ]]; then
        printf '\n  Preview only. Nothing was changed.\n' >&3
        if [[ "${OTHER_HOMES:-0}" -gt 0 ]]; then
            printf '  Would disconnect: %s\n' "$CLUSTER_URL" >&3
            printf '  Cockpit and shared data would stay for %s other enrollment(s).\n' "$OTHER_HOMES" >&3
        else
            printf '  Would remove the selected Cockpit runtime and enrollments.\n' >&3
            if [[ "$PURGE" == yes ]]; then
                printf '  Would also remove eligible local worker data.\n' >&3
            else
                printf '  Local worker data would be kept.\n' >&3
            fi
        fi
        printf '  The full plan is in the log; --verbose shows it here.\n' >&3
    elif [[ "${OTHER_HOMES:-0}" -gt 0 ]]; then
        printf '\n  %sCluster disconnected.%s\n  %s\n' "$color" "$INSTALL_UI_RESET" "$CLUSTER_URL" >&3
        if [[ "$OTHER_HOMES" == 1 ]]; then
            printf '  Cockpit and shared data kept for 1 other enrollment.\n' >&3
        else
            printf '  Cockpit and shared data kept for %s other enrollments.\n' "$OTHER_HOMES" >&3
        fi
    else
        heading='Cockpit removed.'
        [[ -n "$UNINSTALL_REMOVED" ]] || heading='Nothing to remove.'
        printf '\n  %s%s%s\n' "$color" "$heading" "$INSTALL_UI_RESET" >&3
        if [[ -n "$UNINSTALL_KEPT" ]]; then
            printf '  Retained items are listed in the log.\n' >&3
            [[ "$PURGE" != no ]] || printf '  Local worker data is kept unless you use --purge.\n' >&3
        fi
        if other="$(command -v "$INSTALLED_COMMAND" 2>/dev/null)" && [[ -n "$other" ]]; then
            printf '  Another memql command is still installed: %s\n' "$other" >&3
        fi
    fi
    if [[ "$rc" == 0 && "${DRY_RUN:-no}" != yes && ( -n "$UNINSTALL_REMOVED" || "${OTHER_HOMES:-0}" -gt 0 ) ]]; then
        printf '  Remove its cluster registration in Fleet > Machines if needed.\n' >&3
    fi
    printf '\n  %sUninstall log: %s%s\n\n' "$INSTALL_UI_DIM" "$INSTALL_LOG" "$INSTALL_UI_RESET" >&3
}

# normalize_semver strips a leading v and any build metadata / variant
# suffix so "v0.12.1", "0.12.1", and "0.12.1 (headless)" compare equal.
function normalize_semver() {
    local raw="$1"
    raw="${raw#v}"
    raw="${raw%%[[:space:]]*}"
    raw="${raw%%+*}"
    raw="${raw%%-*}"
    echo "$raw"
}

# parse_memql_version_line extracts X.Y.Z from `memql --version` output
# (`memql 0.12.1 (headless)`). Empty on failure.
function parse_memql_version_line() {
    local line="$1"
    # Prefer the token after "memql ", else first semver-looking token.
    local ver
    ver="$(printf '%s\n' "$line" | sed -n -E 's/^[[:space:]]*memql[[:space:]]+([0-9]+(\.[0-9]+){1,3}).*/\1/p' | head -1)"
    if [[ -z "$ver" ]]; then
        ver="$(printf '%s\n' "$line" | sed -n -E 's/.*(^|[[:space:]])v?([0-9]+(\.[0-9]+){1,3}).*/\2/p' | head -1)"
    fi
    normalize_semver "$ver"
}

# read_binary_version runs `$1 --version` and parses it. Empty if the
# binary is missing or does not answer.
function read_binary_version() {
    local bin="$1"
    if [[ -z "$bin" || ! -x "$bin" ]]; then
        echo ""
        return 0
    fi
    local out
    out="$("$bin" --version 2>/dev/null | head -1)" || out=""
    parse_memql_version_line "$out"
}

# Keep prerelease/build suffixes when matching a worker with its app archive.
# Numeric normalization is only appropriate for upgrade ordering.
function read_binary_version_exact() {
    local bin="$1" exact
    [[ -x "$bin" ]] || { echo ""; return 0; }
    exact="$("$bin" --version 2>/dev/null | awk '$1 == "memql" {print $2; exit}')" || exact=""
    if [[ "$exact" =~ ^[0-9]+(\.[0-9]+){1,3}([-+][[:alnum:].-]+)*$ ]]; then printf '%s\n' "$exact"; else echo ""; fi
}

# A release selector retains its prerelease suffix. Revision metadata only
# participates in equality when the caller explicitly selected that metadata.
function binary_version_matches_selection() {
    local actual="$1" selected="${2#v}"
    [[ -n "$actual" && -n "$selected" ]] || return 1
    if [[ "$selected" == *+* ]]; then
        [[ "$actual" == "$selected" ]]
    else
        [[ "${actual%%+*}" == "$selected" ]]
    fi
}

# compare_semver prints -1 / 0 / 1 for a<b / a==b / a>b (numeric dotted).
# Non-numeric segments compare as 0. Empty either side → treat as 0.0.0.
function compare_semver() {
    local a b
    a="$(normalize_semver "${1:-0}")"
    b="$(normalize_semver "${2:-0}")"
    [[ -z "$a" ]] && a="0"
    [[ -z "$b" ]] && b="0"
    local IFS=.
    # shellcheck disable=SC2086
    set -- $a
    local a1="${1:-0}" a2="${2:-0}" a3="${3:-0}"
    # shellcheck disable=SC2086
    set -- $b
    local b1="${1:-0}" b2="${2:-0}" b3="${3:-0}"
    a1="${a1:-0}"; a2="${a2:-0}"; a3="${a3:-0}"
    b1="${b1:-0}"; b2="${b2:-0}"; b3="${b3:-0}"
    # Strip non-digits for bash arithmetic safety.
    a1="${a1%%[!0-9]*}"; a2="${a2%%[!0-9]*}"; a3="${a3%%[!0-9]*}"
    b1="${b1%%[!0-9]*}"; b2="${b2%%[!0-9]*}"; b3="${b3%%[!0-9]*}"
    a1="${a1:-0}"; a2="${a2:-0}"; a3="${a3:-0}"
    b1="${b1:-0}"; b2="${b2:-0}"; b3="${b3:-0}"
    if (( a1 < b1 )); then echo -1; return; fi
    if (( a1 > b1 )); then echo 1; return; fi
    if (( a2 < b2 )); then echo -1; return; fi
    if (( a2 > b2 )); then echo 1; return; fi
    if (( a3 < b3 )); then echo -1; return; fi
    if (( a3 > b3 )); then echo 1; return; fi
    echo 0
}

# resolve_target_version picks the version about to be installed.
# Order: MEMQL_INSTALL_VERSION → sibling/repo VERSION file → version
# embedded in --download-base (.../releases/download/vX.Y.Z/...) → empty
# (caller downloads and reads the binary).
function resolve_target_version() {
    local download_base="${1:-}"
    local ver=""
    if [[ -n "${MEMQL_INSTALL_VERSION:-}" ]]; then
        normalize_semver "$MEMQL_INSTALL_VERSION"
        return 0
    fi
    local here sibling_version
    here="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd)" || here=""
    for sibling_version in \
        "${here}/../../VERSION" \
        "${here}/../VERSION" \
        "${MEMQL_INSTALL_VERSION_FILE:-}"
    do
        [[ -n "$sibling_version" && -f "$sibling_version" ]] || continue
        ver="$(tr -d '[:space:]' < "$sibling_version")"
        if [[ -n "$ver" ]]; then
            normalize_semver "$ver"
            return 0
        fi
    done
    # sed keeps this portable on macOS bash 3.2 (no BASH_REMATCH nests).
    ver="$(printf '%s' "$download_base" | sed -n -E 's#.*/download/v?([0-9]+(\.[0-9]+){1,3})(/.*)?$#\1#p')"
    if [[ -n "$ver" ]]; then
        normalize_semver "$ver"
        return 0
    fi
    echo ""
}

# release_download_base prints the download base for ONE release tag,
# from a version with or without its leading v. This is the URL a
# --version pin downloads from, and the one the app / menu archives
# are fetched from once the worker's version is known.
function release_download_base() {
    local ver="$1"
    printf '%s/download/v%s\n' "$RELEASE_BASE" "${ver#v}"
}

# parse_version_flag validates a --version value and prints it
# normalised (leading v dropped, prerelease / build suffix kept, so an
# explicit 0.16.0-rc1 is downloaded and checked as exactly that). "v"
# is accepted because that is how the tag is spelled on the releases
# page and how people copy it. Anything else is a bad parameter (2):
# a typo here must fail before a download, not surface as a 404 whose
# URL the person then has to read backwards.
function parse_version_flag() {
    local raw="$1" ver
    ver="${raw#v}"
    if [[ ! "$ver" =~ ^[0-9]+(\.[0-9]+){1,3}([-+][[:alnum:].-]+)*$ ]]; then
        echo "ERROR: --version wants a release version such as 0.16.0 or v0.16.0 (got '${raw}')" >&2
        return 2
    fi
    printf '%s\n' "$ver"
}

# yaml_top_scalar prints the value of ONE top-level scalar key in a
# worker YAML file (`state_dir: ...`), the way YAML reads it: a `#`
# starts a comment only after whitespace (or right after the key), so
# `~/.memql/state#1` is a directory name and `~/.memql/state # note` is
# not; surrounding quotes come off. Empty when the key is absent. The
# earlier sed cut the value at any `#`, and a purge then aimed at a
# sibling of the directory the worker used (a review finding).
function yaml_top_scalar() {
    local file="$1" key="$2" value
    value="$(awk -v key="$key" '
        index($0, key ":") == 1 && substr($0, length(key) + 2) ~ /^([[:space:]]|$)/ {
            v = substr($0, length(key) + 2)
            sub(/^[[:space:]]+/, "", v)
            if (v ~ /^#/) v = ""
            sub(/[[:space:]]+#.*$/, "", v)
            sub(/[[:space:]]+$/, "", v)
            print v
            exit
        }' "$file")"
    case "$value" in
        \"*\") value="${value#\"}"; value="${value%\"}" ;;
        \'*\') value="${value#\'}"; value="${value%\'}" ;;
    esac
    printf '%s\n' "$value"
}

# home_id_from_cluster_url mirrors Go HomeIDFromURL: host only, lowercased.
function home_id_from_cluster_url() {
    local cluster_url="$1"
    local home_id
    home_id="$(echo "$cluster_url" | sed -E 's#^[a-zA-Z][a-zA-Z0-9+.-]*://##' | sed -E 's#/.*##' | sed -E 's#:[0-9]+$##' | tr '[:upper:]' '[:lower:]')"
    if [[ -z "$home_id" ]]; then
        home_id="default"
    fi
    echo "$home_id"
}

# same_cluster_url mirrors Go sameClusterURL (trim, case-fold, trailing /).
function same_cluster_url() {
    local a b
    a="$(echo "$1" | sed -E 's/[[:space:]]+$//; s/^[[:space:]]+//; s#/+$##')"
    b="$(echo "$2" | sed -E 's/[[:space:]]+$//; s/^[[:space:]]+//; s#/+$##')"
    local al bl
    al="$(printf '%s' "$a" | tr '[:upper:]' '[:lower:]')"
    bl="$(printf '%s' "$b" | tr '[:upper:]' '[:lower:]')"
    if [[ "$al" == "$bl" ]]; then
        return 0
    fi
    local ha hb
    ha="$(home_id_from_cluster_url "$a")"
    hb="$(home_id_from_cluster_url "$b")"
    [[ "$ha" != "default" && "$ha" == "$hb" ]]
}

# require_cluster_url_flag refuses (2) a --cluster value the binary's
# `worker unpair --cluster-url` is certain to reject
# (internal/worker/unpair_url.go): an http(s) URL with a host and no
# userinfo, query or fragment. same_cluster_url matches a bare host the
# way Go's sameClusterURL does, which is right for the registry and
# wrong as an admission test for the flag: without this a bare
# `api.example.com` matched the enrollment in the shell and then reached
# the binary, which answered 5 blaming the enrollment files.
function require_cluster_url_flag() {
    local url re
    url="$(printf '%s' "$1" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
    re='^[Hh][Tt][Tt][Pp][Ss]?://[^/?#@[:space:]]+(/[^?#[:space:]]*)?$'
    if [[ ! "$url" =~ $re ]]; then
        echo "ERROR: --cluster wants the cluster's URL, such as https://api.example.com (got '${1}'); nothing was changed" >&2
        return 2
    fi
}

# workers_registry_readable answers whether the text readers in this file
# see every home the binary's decoder (gopkg.in/yaml.v3, into
# internal/worker/homes.go's WorkersFile) sees in the workers.yaml at $1.
# They read ONE shape: a mapping rooted at column 0, in LF or CRLF lines,
# whose `homes:` key is a block list of block mappings (the spellings
# find_home_cluster_url_by_id lists) or Go's `homes: []`. yaml.v3 decodes
# a great deal more -- a flow or JSON root, a quoted, spaced, tabbed,
# explicit (`? homes`), anchored, tagged or escaped ("hom\x65s") key, a
# merge key, an indented root, a byte-order mark or UTF-16, a CR, NEL,
# LS or PS line break -- and every one of those read here as NO homes: a
# no-flag or --cluster uninstall took the --all-homes path over every
# token, and the next install rewrote the registry holding only its own
# home (a verification finding). So the answer is yes only for that one
# shape, or for a file that provably holds no homes key at all: a root
# of plain column-0 keys, none of them homes, and the word `homes`
# nowhere else in it. Anything else is a no, and every caller turns a no
# into a refusal (5, refuse_unreadable_registry), never into "zero
# enrollments". Inside the block, only a block-mapping item (`- key:`),
# a continuation (`key:`), a bare `-`, a blank or a comment may appear:
# a sequence of flow mappings (`- {id: ..., cluster_url: ...}`) is valid
# YAML the binary decodes and the block readers cannot see into; nor may
# a value there open with a tag, anchor, alias, block scalar or flow
# collection, which the readers would take literally. Bytes,
# not characters (LC_ALL=C), so the line-break and BOM tests are exact
# in every locale; awk alone, because the uninstallers run under a PATH
# that holds little else.
function workers_registry_readable() {
    LC_ALL=C awk '
        function plain_key(s) { return s ~ /^[A-Za-z_][A-Za-z0-9_-]*[ \t]*:([[:space:]]|$)/ }
        BEGIN { ok = 1; headers = 0; in_homes = 0; content = 0; docstart = 0; word = 0 }
        {
            line = $0
            if (NR == 1 && (substr(line, 1, 3) == "\357\273\277" || substr(line, 1, 2) == "\376\377" || substr(line, 1, 2) == "\377\376")) ok = 0
            if (substr(line, length(line), 1) == "\r") line = substr(line, 1, length(line) - 1)
            if (index(line, "\r") || index(line, "\302\205") || index(line, "\342\200\250") || index(line, "\342\200\251")) ok = 0
            if (line ~ /^[[:space:]]*(#.*)?$/) { if (index(line, "homes")) word = 1; next }
            # One leading document marker is harmless; any other marker
            # starts a document yaml.v3 never reads.
            if (!content && !docstart && line ~ /^---[[:space:]]*(#.*)?$/) { docstart = 1; next }
            if (!content) { content = 1; if (!plain_key(line)) ok = 0 }
            if (line ~ /^[^[:space:]]/ && line !~ /^-([[:space:]]|$)/) {
                if (!plain_key(line)) { ok = 0; next }
                key = line
                sub(/[ \t]*:.*$/, "", key)
                in_homes = 0
                if (key == "homes") {
                    if (line ~ /^homes:[[:space:]]*(#.*)?$/) { headers++; in_homes = 1; next }
                    if (line ~ /^homes:[[:space:]]*\[[[:space:]]*\][[:space:]]*(#.*)?$/) { headers++; next }
                    ok = 0
                    next
                }
            }
            if (index(line, "homes")) word = 1
            if (!in_homes) next
            item = line
            sub(/^[[:space:]]+/, "", item)
            sub(/^-([[:space:]]+|$)/, "", item)
            if (item != "" && item !~ /^[A-Za-z_][A-Za-z0-9_]*:([[:space:]]|$)/) ok = 0
            # A value opening with a node property (tag, anchor, alias), a
            # block scalar or a flow collection is its literal text to the
            # readers and something else to yaml.v3: two `!!str https://...`
            # URLs both reduced to the host `!!str https:` and read as ONE
            # enrollment (a review finding). (The empty test comes first:
            # BWK awk answers index(s, "") with 1.)
            value = item
            sub(/^[A-Za-z_][A-Za-z0-9_]*:[[:space:]]*/, "", value)
            first = substr(value, 1, 1)
            if (first != "" && index("!&*|>{[", first)) ok = 0
        }
        END { exit ((ok && headers <= 1 && (headers == 1 || !word)) ? 0 : 1) }
    ' "$1"
}

# refuse_unreadable_registry prints the refusal for the workers.yaml at
# $1 that workers_registry_readable said no to, then $2 -- the way past
# it for this script -- and returns 5, the binary's own code for
# enrollment files it cannot use.
function refuse_unreadable_registry() {
    echo "ERROR: $1 is not an enrollment registry this script can read; nothing was changed." >&2
    echo "       $2" >&2
    return 5
}

# The installer's way past an unreadable registry: it never writes one
# (write_worker_yaml would keep only the homes it can see).
INSTALL_REGISTRY_REMEDY="This installer will not rewrite it: the homes it holds would be dropped. Restore it to a block list (a homes: line, then one '- id:' item per cluster), then re-run."

# preflight_registry refuses (5) before an install changes anything when
# ~/.memql/workers.yaml exists and is not a registry the installer's
# readers can read -- step 3 would refuse the same file after the
# binary, the app and the state directory were already in place.
function preflight_registry() {
    local workers_path="${HOME}/.memql/workers.yaml"
    [[ -e "$workers_path" ]] || return 0
    workers_registry_readable "$workers_path" || refuse_unreadable_registry "$workers_path" "$INSTALL_REGISTRY_REMEDY"
}

# find_home_id_by_cluster_url prints the id of a home whose cluster_url
# matches, or empty. Used so install (URL-host id) and pair (--home-id
# local) refresh the same enrollment without --force.
#
# MATCHED THE WAY GO'S sameClusterURL MATCHES (memql-cockpit#433): the same
# URL (trimmed, case-folded, no trailing slash), or failing that the same
# HOST -- so https://api.example.com and https://api.example.com:443 are
# one cluster. An exact match wins over a host match. Matching on the
# exact string alone appended a second home for the same cluster whenever
# the URL was spelled differently, and one machine then held two streams
# to one cluster with two tokens. A registry it cannot see into is a
# refusal (5), never "no home matches" (workers_registry_readable).
function find_home_id_by_cluster_url() {
    local workers_path="$1"
    local cluster_url="$2"
    [[ -f "$workers_path" ]] || { echo ""; return 0; }
    if ! workers_registry_readable "$workers_path"; then
        refuse_unreadable_registry "$workers_path" "$INSTALL_REGISTRY_REMEDY"
        return 5
    fi
    awk -v want="$cluster_url" '
        function norm(s) {
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", s)
            sub(/\/+$/, "", s)
            return tolower(s)
        }
        function host(s) {
            s = norm(s)
            sub(/^[a-z][a-z0-9+.-]*:\/\//, "", s)
            sub(/\/.*$/, "", s)
            sub(/:[0-9]+$/, "", s)
            return s
        }
        function consider(id, url) {
            if (id == "" || url == "") return
            if (exact == "" && norm(url) == wantn) exact = id
            if (byhost == "" && wanth != "" && host(url) == wanth) byhost = id
        }
        BEGIN { wantn = norm(want); wanth = host(want); in_homes=0; cur=""; curl=""; exact=""; byhost="" }
        /^homes:[[:space:]]*(#.*)?$/ { in_homes=1; next }
        !in_homes { next }
        /^[^[:space:]#-]/ { consider(cur, curl); cur=""; curl=""; in_homes=0; next }
        /^[[:space:]]*-[[:space:]]*/ { consider(cur, curl); cur=""; curl=""; sub(/^[[:space:]]*-[[:space:]]*/, "") }
        /^[[:space:]]*id:[[:space:]]*/ {
            cur=$0; sub(/^[[:space:]]*id:[[:space:]]*/, "", cur); sub(/[[:space:]]+(#.*)?$/, "", cur)
            gsub(/^["'"'"']|["'"'"']$/, "", cur)
            next
        }
        /^[[:space:]]*cluster_url:[[:space:]]*/ {
            curl=$0; sub(/^[[:space:]]*cluster_url:[[:space:]]*/, "", curl); sub(/[[:space:]]+(#.*)?$/, "", curl)
            gsub(/^["'"'"']|["'"'"']$/, "", curl)
            next
        }
        END {
            consider(cur, curl)
            if (exact != "") print exact
            else if (byhost != "") print byhost
        }
    ' "$workers_path" | head -1
}

# find_home_cluster_url_by_id prints cluster_url for a given home id.
#
# Every registry reader in this file (this one, find_home_id_by_cluster_url,
# the sibling walk in write_worker_yaml, list_enrolled_cluster_urls) reads
# the block-list spellings yaml.v3 accepts the same way: the `homes:` line
# may carry a comment, items may sit indented or at column 0, any `- ` opens
# an item whatever key follows the dash, `id:` / `cluster_url:` are read on
# the dash line or an inner line, and the next column-0 key closes the list.
# A review found the installer's readers behind the uninstaller's: a
# commented header or a url-first item made the next install DROP every
# sibling home it could not see. A quoted id or cluster_url reads as
# what it says, as in list_enrolled_cluster_urls: one that kept its
# quotes matched nothing, and the next install appended a second home
# for the same cluster (a review finding). Every one of them asks
# workers_registry_readable first and refuses (5) a file it says no to:
# any other spelling of the registry read as no homes at all.
function find_home_cluster_url_by_id() {
    local workers_path="$1"
    local home_id="$2"
    [[ -f "$workers_path" ]] || { echo ""; return 0; }
    if ! workers_registry_readable "$workers_path"; then
        refuse_unreadable_registry "$workers_path" "$INSTALL_REGISTRY_REMEDY"
        return 5
    fi
    awk -v want="$home_id" '
        function settle() { if (cur == want && curl != "") { print curl; found=1; exit } cur=""; curl="" }
        BEGIN { in_homes=0; cur=""; curl=""; found=0 }
        /^homes:[[:space:]]*(#.*)?$/ { in_homes=1; next }
        !in_homes { next }
        /^[^[:space:]#-]/ { settle(); in_homes=0; next }
        /^[[:space:]]*-[[:space:]]*/ { settle(); sub(/^[[:space:]]*-[[:space:]]*/, "") }
        /^[[:space:]]*id:[[:space:]]*/ {
            cur=$0; sub(/^[[:space:]]*id:[[:space:]]*/, "", cur); sub(/[[:space:]]+(#.*)?$/, "", cur)
            gsub(/^["'"'"']|["'"'"']$/, "", cur)
            next
        }
        /^[[:space:]]*cluster_url:[[:space:]]*/ {
            curl=$0; sub(/^[[:space:]]*cluster_url:[[:space:]]*/, "", curl); sub(/[[:space:]]+(#.*)?$/, "", curl)
            gsub(/^["'"'"']|["'"'"']$/, "", curl)
            next
        }
        END { if (!found && cur == want && curl != "") print curl }
    ' "$workers_path" | head -1
}

# Detect host os ("darwin" or "linux") and arch ("amd64" or "arm64").
function detect_os() {
    local raw
    raw="$(uname -s)"
    case "$raw" in
        Darwin) echo "darwin" ;;
        Linux)  echo "linux" ;;
        *)
            echo "ERROR: unsupported os $raw" >&2
            return 1
            ;;
    esac
}

function detect_arch() {
    local raw
    raw="$(uname -m)"
    case "$raw" in
        x86_64|amd64) echo "amd64" ;;
        arm64|aarch64) echo "arm64" ;;
        *)
            echo "ERROR: unsupported arch $raw" >&2
            return 1
            ;;
    esac
}

# Pick the DOWNLOAD artifact name for the requested headless /
# computeruse flavour and the detected platform.
#
# The artifact carries the variant AND the platform; the installed file is
# always plain $INSTALLED_COMMAND. Both names derive from that one
# constant, so a rename cannot move the download without moving the
# install. These strings must also agree with the Makefile's dist targets
# and release.yml's upload list -- TestReleaseArtifactNamesAgree holds
# those three together.
function binary_name_for() {
    local flavour="$1"  # "headless" or "computeruse"
    local os arch
    os="$(detect_os)"
    arch="$(detect_arch)"
    case "$flavour" in
        headless)
            echo "${INSTALLED_COMMAND}-${os}-${arch}"
            ;;
        computeruse)
            echo "${INSTALLED_COMMAND}-computeruse-${os}-${arch}"
            ;;
        *)
            echo "ERROR: unknown flavour $flavour" >&2
            return 1
            ;;
    esac
}

# preflight_asset probes the resolved release-asset URL BEFORE anything
# is mutated -- before the sudo prompt, the binary install, the
# worker.yaml write and the LaunchAgent / systemd load -- so a flavour
# whose asset the release never published (the --computeruse case,
# znasllc-io/memql-cockpit#374) is a clean refusal up front instead of a
# bare curl 404 after the operator has already typed their password.
#
# The probe is a HEAD after redirects (`curl -fsIL`; GitHub release
# download URLs answer HEAD). When HEAD fails, a one-byte ranged GET
# retries the same question -- some curl builds mishandle HEAD on
# non-HTTP protocols (file:// among them, which is how lib_test.sh
# exercises this offline), and GitHub's missing-asset answer to HEAD is
# a connection drop rather than a clean 404, so the second opinion costs
# one byte and removes a false refusal either way. Deliberately NO
# --proto '=https' here: the probe writes nothing (-o /dev/null), so the
# https-only enforcement stays on download_binary, the call that
# produces the bytes that get executed.
#
# On refusal the message names the exact URL and the flavour/platform
# pair, and returns 4 -- prerequisite missing, the same capability-script
# exit convention cut-release.sh uses (2 bad param, 3 refused, 4
# prerequisite missing). The installers run under `set -e`, so the
# return surfaces as the process exit code.
function preflight_asset() {
    local url="$1"
    local flavour="$2"  # "headless" or "computeruse", for the message
    if ! command -v curl >/dev/null 2>&1; then
        echo "ERROR: curl required" >&2
        return 4
    fi
    echo "INFO: checking release asset $url"
    if curl -fsIL "${CURL_NET[@]}" "${CURL_SMALL[@]}" -o /dev/null "$url"; then
        return 0
    fi
    if curl -fsL "${CURL_NET[@]}" "${CURL_SMALL[@]}" -r 0-0 -o /dev/null "$url"; then
        return 0
    fi
    local os arch
    os="$(detect_os)"
    arch="$(detect_arch)"
    echo "ERROR: release asset not found (or unreachable): $url" >&2
    echo "       (flavour: ${flavour}, platform: ${os}/${arch})" >&2
    echo "       The default download base is the LATEST release" >&2
    echo "       (releases/latest/download), which may not publish an asset for" >&2
    echo "       this flavour. Check the release's published assets, pass" >&2
    echo "       --version=X.Y.Z to pin a release that ships it, or pass" >&2
    echo "       --download-base to point at any other location." >&2
    echo "       Nothing was installed or modified." >&2
    return 4
}

# Download the supplied URL to the supplied path, with a basic
# integrity check (HTTP 200, non-empty file).
function download_binary() {
    local url="$1"
    local dest="$2"
    if ! command -v curl >/dev/null 2>&1; then
        echo "ERROR: curl required for the download step." >&2
        echo "       Install curl, or pass --download-base file:///... for an offline asset." >&2
        return 1
    fi
    echo "INFO: downloading $url"
    # Progress bar when stdout is a TTY; silent -sS for CI / pipes.
    local curl_flags
    read -r -a curl_flags <<< "$(release_fetch_protocol "$url")"
    if [[ -t 1 ]]; then
        curl_flags+=(--progress-bar)
    else
        curl_flags+=(-sS)
    fi
    if ! curl "${curl_flags[@]}" "${CURL_NET[@]}" "${CURL_LARGE[@]}" "$url" -o "$dest.partial"; then
        echo "ERROR: download failed for $url" >&2
        echo "       Check network access, or pass --download-base to a release that" >&2
        echo "       publishes this asset. Nothing was installed from this URL." >&2
        rm -f "$dest.partial"
        return 1
    fi
    if [[ ! -s "$dest.partial" ]]; then
        echo "ERROR: downloaded file is empty ($url)" >&2
        rm -f "$dest.partial"
        return 1
    fi
    mv "$dest.partial" "$dest"
    chmod +x "$dest"
}

# release_fetch_protocol prints the curl flags that pin a release fetch's
# protocol: https, always, for anything published; plain http only for an
# explicit local test against a literal loopback address, with redirects
# refused there.
function release_fetch_protocol() {
    local url="$1"
    if [[ "${MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP:-}" == 1 && "$url" =~ ^http://(127\.0\.0\.1|\[::1\])(:[0-9]+)?/ ]]; then
        printf '%s\n' "-fL --proto =http --max-redirs 0"
        return 0
    fi
    printf '%s\n' "-fL --proto =https"
}

# fetch_release_file fetches a small release file (a checksum manifest)
# under the same protocol rule as the binary it vouches for. A function so
# the installer tests can serve one from disk.
function fetch_release_file() {
    local url="$1" dest="$2" flags
    read -r -a flags <<< "$(release_fetch_protocol "$url")"
    curl "${flags[@]}" -sS "${CURL_NET[@]}" "${CURL_SMALL[@]}" --max-filesize 65536 "$url" -o "$dest"
}

# sha256_of prints a file's SHA-256: shasum on macOS, sha256sum on Linux.
function sha256_of() {
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        sha256sum "$1" | awk '{print $1}'
    fi
}

# manifest_checksum prints the hex a SHA256SUMS file lists for an asset,
# or nothing. Both spellings: `<hex>  name` and `<hex> *name`.
function manifest_checksum() {
    awk -v asset="$2" 'NF >= 2 { name = $2; sub(/^\*/, "", name); sub(/^\.\//, "", name); if (name == asset) { print $1; exit } }' "$1"
}

# release_checksum_manifest names the manifest that lists a raw binary:
# memql-computeruse-SHA256SUMS for the computer-use build, memql-SHA256SUMS
# for the headless one (published from the release after 0.19.0).
function release_checksum_manifest() {
    case "$1" in
        memql-computeruse-*) printf 'memql-computeruse-SHA256SUMS\n' ;;
        *) printf 'memql-SHA256SUMS\n' ;;
    esac
}

# resolve_latest_version prints the version `releases/latest` points at
# now, read from the redirect (no API call, no rate limit), or nothing.
function resolve_latest_version() {
    local effective
    effective="$(curl -fsSLI "${CURL_NET[@]}" "${CURL_SMALL[@]}" -o /dev/null -w '%{url_effective}' "${RELEASE_BASE}/latest" 2>/dev/null)" || return 0
    printf '%s' "$effective" | sed -n -E 's#.*/tag/v?([0-9]+(\.[0-9]+){2}([-+][0-9A-Za-z.-]+)?)$#\1#p'
}

# verify_release_binary checks a downloaded binary against its release's
# checksum manifest BEFORE anything executes it (memql-cockpit#525) --
# the version probe that follows runs the file, so an unverified download
# was code run from the network. A mismatch, or a manifest that cannot
# vouch for the file, refuses with exit 5 and nothing installed.
#
# A headless binary from a release published before memql-SHA256SUMS
# existed is vouched for by its release archive instead: the archive is
# listed in memql-<version>-SHA256SUMS, and its `memql` must be the same
# bytes as the raw download.
function verify_release_binary() {
    local file="$1" asset="$2" base="$3" manifest list expected="" actual
    manifest="$(release_checksum_manifest "$asset")"
    list="$(mktemp)"
    if fetch_release_file "${base}/${manifest}" "$list" 2>/dev/null; then
        expected="$(manifest_checksum "$list" "$asset")"
    fi
    rm -f "$list"
    if [[ -z "$expected" && "$asset" != memql-computeruse-* ]]; then
        verify_headless_via_archive "$file" "$asset" "$base"
        return $?
    fi
    if [[ ! "$expected" =~ ^[[:xdigit:]]{64}$ ]]; then
        echo "ERROR: ${base}/${manifest} does not vouch for ${asset}, so the download cannot be verified." >&2
        echo "       Nothing was run or installed. Retry in a few minutes if the release is still being" >&2
        echo "       published, or pin one with --version." >&2
        return 5
    fi
    actual="$(sha256_of "$file")"
    if [[ "$actual" != "$expected" ]]; then
        echo "ERROR: ${asset} does not match its release checksum (downloaded ${actual}, ${manifest} lists ${expected})." >&2
        echo "       Nothing was run or installed." >&2
        return 5
    fi
    echo "INFO: verified ${asset} against ${manifest}"
}

function verify_headless_via_archive() {
    local file="$1" asset="$2" base="$3" version os_arch archive manifest list expected work actual
    version="$(printf '%s' "$base" | sed -n -E 's#.*/download/v?([0-9]+(\.[0-9]+){2}([-+][0-9A-Za-z.-]+)?)$#\1#p')"
    [[ -n "$version" ]] || version="${MEMQL_INSTALL_VERSION:-}"
    version="${version#v}"
    [[ -n "$version" ]] || version="$(resolve_latest_version)"
    os_arch="${asset#memql-}"
    if [[ -z "$version" || "$os_arch" == "$asset" ]]; then
        echo "ERROR: ${asset} cannot be verified: its release publishes no memql-SHA256SUMS and its version is unknown." >&2
        echo "       Nothing was run or installed. Pin a release with --version and run this again." >&2
        return 5
    fi
    [[ "$base" == */download/v* ]] || base="$(release_download_base "$version")"
    archive="memql-${version}-${os_arch}.tar.gz"
    manifest="memql-${version}-SHA256SUMS"
    list="$(mktemp)"
    expected=""
    if fetch_release_file "${base}/${manifest}" "$list" 2>/dev/null; then
        expected="$(manifest_checksum "$list" "$archive")"
    fi
    rm -f "$list"
    if [[ ! "$expected" =~ ^[[:xdigit:]]{64}$ ]]; then
        echo "ERROR: neither memql-SHA256SUMS nor ${manifest} vouches for ${asset}; nothing was run or installed." >&2
        return 5
    fi
    work="$(mktemp -d)"
    if ! download_binary "${base}/${archive}" "${work}/${archive}" >/dev/null; then
        rm -rf "$work"
        echo "ERROR: ${archive} could not be downloaded to verify ${asset}; nothing was run or installed." >&2
        return 5
    fi
    if [[ "$(sha256_of "${work}/${archive}")" != "$expected" ]]; then
        rm -rf "$work"
        echo "ERROR: ${archive} does not match ${manifest}; nothing was run or installed." >&2
        return 5
    fi
    if ! tar -xzf "${work}/${archive}" -C "$work" ./memql 2>/dev/null && ! tar -xzf "${work}/${archive}" -C "$work" memql 2>/dev/null; then
        rm -rf "$work"
        echo "ERROR: ${archive} carries no memql to compare ${asset} with; nothing was run or installed." >&2
        return 5
    fi
    actual="$(sha256_of "${work}/memql")"
    rm -rf "$work"
    if [[ "$actual" != "$(sha256_of "$file")" ]]; then
        echo "ERROR: ${asset} is not the memql in its verified release archive ${archive}; nothing was run or installed." >&2
        return 5
    fi
    echo "INFO: verified ${asset} against ${archive} (${manifest})"
}

# install_mode_dir prints the install destination directory for
# the given mode. Used by install_binary_with_mode + by the OS-
# specific driver scripts when they build the LaunchAgent / systemd
# unit ExecStart path.
function install_mode_dir() {
    local mode="$1"
    case "$mode" in
        system)
            echo "$INSTALL_PREFIX_SYSTEM"
            ;;
        user-local)
            echo "$INSTALL_PREFIX_USER"
            ;;
        *)
            echo "ERROR: unknown install mode '$mode' (expected: system, user-local)" >&2
            return 1
            ;;
    esac
}

# require_sudo ensures we can call `sudo` non-interactively, or
# prompts the user. Returns 0 on success, 1 if sudo isn't available
# or auth fails. Already-root processes succeed immediately. Used by
# install_binary_with_mode for the system install path so a
# /usr/local/bin write attempt fails fast with a clear message
# instead of failing in the middle of the download, and by
# remove_binaries_with_mode for the same directory on the way out.
#
# $1 is the action being gated, "install" (the default) or
# "uninstall", and it changes the WORDING only: the way out of a
# missing sudo is --user-local in both cases, but telling a person who
# is removing a worker to "install under ~/.memql/bin instead" sends
# them the wrong way.
function require_sudo() {
    local action="${1:-install}"
    local need="the immutable install at $INSTALL_PREFIX_SYSTEM"
    local way_out="install under $INSTALL_PREFIX_USER"
    if [[ "$action" == "uninstall" ]]; then
        need="removing the immutable install at $INSTALL_PREFIX_SYSTEM"
        way_out="remove a --user-local install from $INSTALL_PREFIX_USER"
    fi
    if [[ $EUID -eq 0 ]]; then
        return 0
    fi
    if ! command -v sudo >/dev/null 2>&1; then
        echo "ERROR: sudo required for ${need} but is not available." >&2
        echo "       Pass --user-local to ${way_out} instead." >&2
        return 1
    fi
    if sudo -n true 2>/dev/null; then
        return 0
    fi
    if [[ "${INSTALL_UI_ACTIVE:-no}" == yes ]]; then
        install_ui_stop_progress
        printf '       Administrator access is needed to %s Cockpit.\n' "$action" >&3
        if ! sudo -v >&3 2>&3; then
            echo "ERROR: sudo authentication failed. Pass --user-local for a passwordless ${action}." >&2
            return 1
        fi
        install_ui_start_progress
        return 0
    fi
    echo "INFO: $INSTALL_PREFIX_SYSTEM ${action} requires sudo; you'll be prompted for your password..."
    if ! sudo -v; then
        echo "ERROR: sudo authentication failed. Pass --user-local for a passwordless ${action}." >&2
        return 1
    fi
    return 0
}

# install_binary_with_mode downloads $url into the mode-appropriate
# directory under the binary name $binary, makes it executable, and
# symlinks $friendly_name to the downloaded file. Sets two globals
# the driver scripts read after the call:
#
#   INSTALL_BINARY_DEST     -- full path to the downloaded binary
#   INSTALL_BINARY_FRIENDLY -- full path to the friendly symlink
#                              (memql / memql-computeruse).
#
# In system mode the install path is created via `sudo install
# -m 0755 ...` so the destination is root-owned and the worker
# user can read+exec but not write -- closes the
# swap-without-privesc gap from Wave 6 #45.
function install_binary_with_mode() {
    local mode="$1"
    local url="$2"
    local binary="$3"
    local friendly_name="$4"
    # Optional 5th arg: target version (empty → resolve / read after download).
    local target_ver="${5:-}"
    local target_exact="${6:-}" installed_exact=""
    target_exact="${target_exact#v}"

    local dest_dir
    dest_dir="$(install_mode_dir "$mode")"
    INSTALL_BINARY_DEST="$dest_dir/$binary"
    INSTALL_BINARY_FRIENDLY="$dest_dir/$friendly_name"
    INSTALL_BINARY_ACTION=""   # skip | upgrade | fresh | refuse
    INSTALL_BINARY_BEFORE=""
    INSTALL_BINARY_AFTER=""

    local installed_ver=""
    if [[ -x "$INSTALL_BINARY_FRIENDLY" ]]; then
        installed_ver="$(read_binary_version "$INSTALL_BINARY_FRIENDLY")"
    elif [[ -x "$INSTALL_BINARY_DEST" ]]; then
        installed_ver="$(read_binary_version "$INSTALL_BINARY_DEST")"
    fi
    installed_exact="$(read_binary_version_exact "$INSTALL_BINARY_FRIENDLY")"
    [[ -n "$installed_exact" ]] || installed_exact="$(read_binary_version_exact "$INSTALL_BINARY_DEST")"
    INSTALL_BINARY_BEFORE="$installed_ver"

    if [[ -z "$target_ver" ]]; then
        target_ver="$(resolve_target_version "")"
    fi

    # The numeric version orders releases, but cannot identify a build. Even a
    # pinned release must be downloaded before deciding whether to keep a local
    # executable with the same version (which may carry another commit).
    if [[ -n "$installed_ver" && -n "$target_ver" ]]; then
        local cmp
        cmp="$(compare_semver "$installed_ver" "$target_ver")"
        if [[ "$cmp" == "1" ]]; then
            echo "ERROR: installed memql v${installed_ver} is newer than v${target_ver} about to install." >&2
            echo "       Refusing to downgrade. Install a newer release, or remove the" >&2
            echo "       existing binary first (see uninstall-mac.sh / uninstall-linux.sh)." >&2
            echo "       --force does not override this (it only remaps workers.yaml homes)." >&2
            INSTALL_BINARY_ACTION="refuse"
            return 3
        fi
        if [[ "$cmp" == "0" ]]; then
            echo "INFO: installed memql v${installed_ver}; checking downloaded binary version"
        else
            echo "INFO: upgrading memql v${installed_ver} → v${target_ver}"
        fi
        INSTALL_BINARY_ACTION="upgrade"
    elif [[ -n "$installed_ver" && -z "$target_ver" ]]; then
        # Unknown target: download to temp, compare, then decide.
        :
        INSTALL_BINARY_ACTION="upgrade"
        echo "INFO: installed memql v${installed_ver}; checking downloaded binary version"
    else
        INSTALL_BINARY_ACTION="fresh"
        if [[ -n "$target_ver" ]]; then
            echo "INFO: fresh install → v${target_ver}"
        else
            echo "INFO: fresh install (version resolved after download)"
        fi
    fi

    case "$mode" in
        system)
            require_sudo
            local tmp
            tmp="$(mktemp)"
            if ! download_binary "$url" "$tmp"; then
                rm -f "$tmp"
                return 1
            fi
            if ! verify_release_binary "$tmp" "$binary" "${url%/*}"; then
                rm -f "$tmp"
                return 5
            fi
            local dl_ver
            if [[ -n "$target_exact" ]] && ! binary_version_matches_selection "$(read_binary_version_exact "$tmp")" "$target_exact"; then
                echo "ERROR: downloaded binary does not match the explicitly selected build" >&2
                rm -f "$tmp"; return 3
            fi
            dl_ver="$(read_binary_version "$tmp")"
            if [[ -z "$target_ver" && -n "$dl_ver" ]]; then
                target_ver="$dl_ver"
            fi
            if [[ -n "$installed_ver" && -n "$dl_ver" ]]; then
                local cmp2
                cmp2="$(compare_semver "$installed_ver" "$dl_ver")"
                if [[ -n "$installed_exact" && "$installed_exact" == "$(read_binary_version_exact "$tmp")" ]]; then
                    echo "INFO: already at v${installed_ver}; downloaded asset matches — leaving binary in place"
                    rm -f "$tmp"
                    INSTALL_BINARY_ACTION="skip"
                    INSTALL_BINARY_AFTER="$installed_ver"
                    return 0
                fi
                if [[ "$cmp2" == "1" ]]; then
                    echo "ERROR: installed memql v${installed_ver} is newer than downloaded v${dl_ver}." >&2
                    echo "       Refusing to downgrade. --force does not override this." >&2
                    rm -f "$tmp"
                    INSTALL_BINARY_ACTION="refuse"
                    return 3
                fi
                echo "INFO: upgrading memql v${installed_ver} → v${dl_ver}"
                INSTALL_BINARY_ACTION="upgrade"
            elif [[ -z "$installed_ver" && -n "$dl_ver" ]]; then
                echo "INFO: fresh install → v${dl_ver}"
                INSTALL_BINARY_ACTION="fresh"
            fi
            install_sudo mkdir -p "$dest_dir"
            install_sudo install -m 0755 "$tmp" "$INSTALL_BINARY_DEST"
            rm -f "$tmp"
            install_sudo ln -sf "$INSTALL_BINARY_DEST" "$INSTALL_BINARY_FRIENDLY"
            ;;
        user-local)
            mkdir -p "$dest_dir"
            local tmp
            tmp="$(mktemp)"
            if ! download_binary "$url" "$tmp"; then
                rm -f "$tmp"
                return 1
            fi
            if ! verify_release_binary "$tmp" "$binary" "${url%/*}"; then
                rm -f "$tmp"
                return 5
            fi
            local dl_ver
            if [[ -n "$target_exact" ]] && ! binary_version_matches_selection "$(read_binary_version_exact "$tmp")" "$target_exact"; then
                echo "ERROR: downloaded binary does not match the explicitly selected build" >&2
                rm -f "$tmp"; return 3
            fi
            dl_ver="$(read_binary_version "$tmp")"
            if [[ -z "$target_ver" && -n "$dl_ver" ]]; then
                target_ver="$dl_ver"
            fi
            if [[ -n "$installed_ver" && -n "$dl_ver" ]]; then
                local cmp2
                cmp2="$(compare_semver "$installed_ver" "$dl_ver")"
                if [[ -n "$installed_exact" && "$installed_exact" == "$(read_binary_version_exact "$tmp")" ]]; then
                    echo "INFO: already at v${installed_ver}; downloaded asset matches — leaving binary in place"
                    rm -f "$tmp"
                    INSTALL_BINARY_ACTION="skip"
                    INSTALL_BINARY_AFTER="$installed_ver"
                    return 0
                fi
                if [[ "$cmp2" == "1" ]]; then
                    echo "ERROR: installed memql v${installed_ver} is newer than downloaded v${dl_ver}." >&2
                    echo "       Refusing to downgrade. --force does not override this." >&2
                    rm -f "$tmp"
                    INSTALL_BINARY_ACTION="refuse"
                    return 3
                fi
                echo "INFO: upgrading memql v${installed_ver} → v${dl_ver}"
                INSTALL_BINARY_ACTION="upgrade"
            elif [[ -z "$installed_ver" && -n "$dl_ver" ]]; then
                echo "INFO: fresh install → v${dl_ver}"
                INSTALL_BINARY_ACTION="fresh"
            fi
            mv "$tmp" "$INSTALL_BINARY_DEST"
            chmod +x "$INSTALL_BINARY_DEST"
            ln -sf "$INSTALL_BINARY_DEST" "$INSTALL_BINARY_FRIENDLY"
            echo "WARN: --user-local install at $dest_dir provides weaker isolation than"
            echo "      $INSTALL_PREFIX_SYSTEM. A compromised user account can swap the"
            echo "      binary without privilege escalation. Use the default install path"
            echo "      on shared / production machines."
            ;;
        *)
            echo "ERROR: unknown install mode '$mode'" >&2
            return 1
            ;;
    esac

    INSTALL_BINARY_AFTER="$(read_binary_version "$INSTALL_BINARY_FRIENDLY")"
    if [[ -z "$INSTALL_BINARY_AFTER" ]]; then
        INSTALL_BINARY_AFTER="$target_ver"
    fi
    if [[ -n "$INSTALL_BINARY_BEFORE" && -n "$INSTALL_BINARY_AFTER" && "$INSTALL_BINARY_ACTION" != "skip" ]]; then
        echo "INFO: binary ${INSTALL_BINARY_BEFORE} → ${INSTALL_BINARY_AFTER}"
    elif [[ -z "$INSTALL_BINARY_BEFORE" && -n "$INSTALL_BINARY_AFTER" ]]; then
        echo "INFO: binary fresh install → v${INSTALL_BINARY_AFTER}"
    fi

    # Migrate: drop the pre-rename binaries/symlinks beside the new one
    # (znasllc-io/memql#4553). Harmless when absent. The names come from
    # LEGACY_BINARIES so the uninstaller removes exactly the set this
    # retires.
    local legacy
    for legacy in $LEGACY_BINARIES; do
        case "$mode" in
            system)
                install_sudo rm -f "$dest_dir/$legacy" 2>/dev/null || true
                ;;
            *)
                rm -f "$dest_dir/$legacy" 2>/dev/null || true
                ;;
        esac
    done
}

function linux_worker_capabilities() {
    local flavour="$1" wayland_display="$2" session_type="$3" display="$4"
    session_type="${session_type#"${session_type%%[![:space:]]*}"}"
    session_type="${session_type%"${session_type##*[![:space:]]}"}"
    if [[ "$flavour" != computeruse || -n "$wayland_display" || -z "$display" ]]; then
        echo HEADLESS
        return
    fi
    case "$session_type" in
        [Ww][Aa][Yy][Ll][Aa][Nn][Dd]) echo HEADLESS ;;
        *) echo HEADLESS,COMPUTERUSE ;;
    esac
}

# write_worker_yaml upserts one home into ~/.memql/workers.yaml and
# mirrors that home into the legacy worker.yaml path. ADDITIVE: a
# second install against a different cluster keeps the first home.
#
# --force (force == "yes") is ONLY for remapping a home id onto a
# different cluster_url (cluster identity change). It is NOT required
# for: same cluster_url refresh (even when the suggested id differs
# from the enrolled id, e.g. install host-id vs `pair --home-id local`),
# same-id token rotation, or "binary already exists".
#
# Args:
#   $1 path          -- legacy worker.yaml path (usually ~/.memql/worker.yaml)
#   $2 cluster_url   -- cluster edge URL
#   $3 token         -- worker token (mql_wkr_...)
#   $4 name          -- worker name
#   $5 force         -- "yes" to remap matched home id → new URL (default "no")
#   $6 capabilities  -- comma-separated capability list to advertise
#                       (default "HEADLESS"; pass "HEADLESS,COMPUTERUSE"
#                       for the computer-use variant). The Wayland
#                       downgrade decision stays in install-linux.sh --
#                       this helper only renders what it is handed.
#
# The concurrency block (HEADLESS: 8, COMPUTERUSE: 1) is fixed and
# platform-agnostic; os/arch labels come from detect_os/detect_arch.
# shellcheck disable=SC2034  # WORKER_YAML_ACTION / _HOME_ID are read by the installers' closing block
function write_worker_yaml() {
    local path="$1"
    local cluster_url="$2"
    local token="$3"
    local name="$4"
    local force="${5:-no}"
    local capabilities="${6:-HEADLESS}"
    local dir workers_path home_id match_id existing_url
    dir="$(dirname "$path")"
    workers_path="${dir}/workers.yaml"
    # Default id from URL host (Go HomeIDFromURL). May be remapped below
    # when an existing home already holds this cluster_url under a
    # different id (e.g. pair --home-id local vs install host id).
    home_id="$(home_id_from_cluster_url "$cluster_url")"
    match_id="$home_id"
    # What this write did to the enrollment, for the installer's closing
    # block: "created" a home, "refreshed" the one already holding this
    # cluster (the same token and cluster re-run is a refresh that
    # changes nothing), or "remapped" one under --force.
    WORKER_YAML_ACTION="created"

    # A registry the readers below cannot see into would be rewritten
    # holding only this home, every sibling dropped: refuse it before
    # anything is written. preflight_registry makes the same call before
    # an install changes anything; this is the writer's own guard.
    if [[ -e "$workers_path" ]] && ! workers_registry_readable "$workers_path"; then
        refuse_unreadable_registry "$workers_path" "$INSTALL_REGISTRY_REMEDY"
        return 5
    fi

    mkdir -p "$dir"

    # Match semantics (aligned with Go UpsertHome):
    #   1. Same cluster_url → refresh that home in place; preserve its id.
    #      No --force. (Fixes install host-id vs pair --home-id local.)
    #   2. Same home id + same/empty URL → refresh token; no --force.
    #   3. Same home id + DIFFERENT cluster_url → --force required
    #      (cluster identity remapping). Siblings are always kept.
    # --force never means "binary exists" and never wipes sibling homes.
    if [[ -e "$workers_path" ]]; then
        local by_url
        by_url="$(find_home_id_by_cluster_url "$workers_path" "$cluster_url")" || return $?
        by_url="$(printf '%s' "$by_url" | tr -d '\r' | head -1)"
        if [[ -n "$by_url" ]]; then
            match_id="$by_url"
            WORKER_YAML_ACTION="refreshed"
            echo "INFO: refreshing existing home ${match_id} for ${cluster_url} (token upsert; --force not required)"
        else
            existing_url="$(find_home_cluster_url_by_id "$workers_path" "$home_id")" || return $?
            if [[ -n "$existing_url" ]]; then
                if same_cluster_url "$existing_url" "$cluster_url"; then
                    match_id="$home_id"
                    WORKER_YAML_ACTION="refreshed"
                elif [[ "$force" != "yes" ]]; then
                    echo "ERROR: $workers_path already has home id '${home_id}' for ${existing_url}." >&2
                    echo "       Pass --force to remap that home to ${cluster_url} (siblings are preserved)." >&2
                    echo "       Same cluster_url under another id refreshes without --force." >&2
                    return 1
                else
                    match_id="$home_id"
                    WORKER_YAML_ACTION="remapped"
                    echo "INFO: --force remapping home ${match_id}: ${existing_url} → ${cluster_url}"
                fi
            fi
        fi
    elif [[ -e "$path" && "$force" != "yes" ]]; then
        # Legacy-only machine: promote via rewrite rather than refuse —
        # the Go loader migrates on first run too. Still require --force
        # only when we would destroy the legacy token for a *different*
        # URL; same URL refresh is fine.
        local legacy_url
        legacy_url="$(grep -E '^cluster_url:' "$path" | head -1 | sed -E 's/^cluster_url:[[:space:]]*//')"
        if [[ -n "$legacy_url" ]] && ! same_cluster_url "$legacy_url" "$cluster_url"; then
            echo "ERROR: $path already exists for ${legacy_url}; pass --force to replace that home (siblings are preserved in workers.yaml)" >&2
            return 1
        fi
        [[ -z "$legacy_url" ]] || WORKER_YAML_ACTION="refreshed"
    fi
    WORKER_YAML_HOME_ID="$match_id"

    # Rebuild workers.yaml: keep sibling homes, upsert this one.
    # Preserve existing registry HEADER fields (worker_name, labels,
    # concurrency, state_dir, log_level) when present so a second
    # install/pair does not clobber Go-tuned shared knobs. Capabilities
    # still come from this install (computer-use may widen them), matching
    # Go UpsertHome when Capabilities is non-empty. Fresh files get defaults.
    local tmp
    tmp="$(mktemp)"
    local out_name out_state out_log
    out_name="$name"
    out_state="$STATE_DIR_DEFAULT"
    out_log="info"
    local labels_block concurrency_block
    labels_block="$(printf 'labels:\n  os: %s\n  arch: %s\n' "$(detect_os)" "$(detect_arch)")"
    concurrency_block="$(printf 'concurrency:\n  HEADLESS: 8\n  COMPUTERUSE: 1\n')"
    if [[ -e "$workers_path" ]]; then
        local existing
        existing="$(sed -n -E 's/^worker_name:[[:space:]]*//p' "$workers_path" | head -1)"
        [[ -n "$existing" ]] && out_name="$existing"
        existing="$(yaml_top_scalar "$workers_path" state_dir)"
        [[ -n "$existing" ]] && out_state="$existing"
        existing="$(sed -n -E 's/^log_level:[[:space:]]*//p' "$workers_path" | head -1)"
        [[ -n "$existing" ]] && out_log="$existing"
        existing="$(awk '
            /^labels:[[:space:]]*$/ {grab=1; print; next}
            grab && /^[^[:space:]#]/ {exit}
            grab {print}
        ' "$workers_path")"
        [[ -n "$existing" ]] && labels_block="$existing"
        existing="$(awk '
            /^concurrency:[[:space:]]*$/ {grab=1; print; next}
            grab && /^[^[:space:]#]/ {exit}
            grab {print}
        ' "$workers_path")"
        [[ -n "$existing" ]] && concurrency_block="$existing"
    fi
    {
        echo "version: 1"
        echo "worker_name: ${out_name}"
        printf '%s\n' "$labels_block"
        printf '%s\n' "$concurrency_block"
        echo "state_dir: ${out_state}"
        echo "log_level: ${out_log}"
        echo "capabilities:"
        echo "$capabilities" | tr ',' '\n' | sed 's/^/  - /'
        echo "homes:"
        if [[ -e "$workers_path" ]]; then
            # Emit sibling home blocks at the same indentation as the new home.
            # Go yaml.Marshal uses four spaces; shell installs use two. Keeping
            # siblings verbatim makes the next additive install invalid YAML.
            # The guard at the top of this function already refused a file
            # workers_registry_readable says this walk cannot see into.
            awk -v keep_id="$match_id" '
                function emit() { if (buf != "" && !skip) printf "%s", buf; buf = ""; skip = 0 }
                function note_id(line,    id) {
                    if (line !~ /^id:[[:space:]]*/) return
                    id = line; sub(/^id:[[:space:]]*/, "", id); sub(/[[:space:]]+(#.*)?$/, "", id)
                    gsub(/^["'"'"']|["'"'"']$/, "", id)
                    skip = (id == keep_id) ? 1 : 0
                }
                BEGIN { in_homes=0; skip=0; buf=""; indent=0 }
                /^homes:[[:space:]]*(#.*)?$/ { in_homes=1; next }
                !in_homes { next }
                /^[^[:space:]#-]/ { emit(); in_homes=0; next }
                /^[[:space:]]*-[[:space:]]*/ {
                    emit()
                    match($0, /[^[:space:]]/)
                    indent = RSTART - 1
                    buf = "  " substr($0, indent + 1) "\n"
                    rest = $0; sub(/^[[:space:]]*-[[:space:]]*/, "", rest)
                    note_id(rest)
                    next
                }
                {
                    if (buf == "") next
                    # Preserve relative indentation inside each home.
                    buf = buf "  " substr($0, indent + 1) "\n"
                    rest = $0; sub(/^[[:space:]]+/, "", rest)
                    note_id(rest)
                }
                END { emit() }
            ' "$workers_path"
        elif [[ -e "$path" ]]; then
            # Promote legacy single-home when present and URL differs.
            local leg_url leg_token leg_id
            leg_url="$(grep -E '^cluster_url:' "$path" | head -1 | sed -E 's/^cluster_url:[[:space:]]*//')"
            leg_token="$(grep -E '^token:' "$path" | head -1 | sed -E 's/^token:[[:space:]]*//')"
            if [[ -n "$leg_url" ]] && ! same_cluster_url "$leg_url" "$cluster_url" && [[ -n "$leg_token" ]]; then
                leg_id="$(home_id_from_cluster_url "$leg_url")"
                echo "  - id: ${leg_id}"
                echo "    cluster_url: ${leg_url}"
                echo "    token: ${leg_token}"
                echo "    enabled: true"
            fi
        fi
        echo "  - id: ${match_id}"
        echo "    cluster_url: ${cluster_url}"
        echo "    token: ${token}"
        echo "    enabled: true"
    } > "$tmp"
    mv "$tmp" "$workers_path"
    chmod 600 "$workers_path"
    echo "INFO: upserted home ${match_id} in $workers_path (capabilities: ${capabilities})"

    # Legacy mirror for older tooling / mid-transition LaunchAgents.
    # state_dir is namespaced per home (homes/<id>) to match ConfigForHome
    # so a single-file reader does not share registration with siblings.
    cat > "$path" << YAML
cluster_url: ${cluster_url}
token: ${token}
name: ${out_name}
labels:
  os: $(detect_os)
  arch: $(detect_arch)
concurrency:
  HEADLESS: 8
  COMPUTERUSE: 1
state_dir: ${out_state}/homes/${match_id}
log_level: ${out_log}
capabilities:
$(echo "$capabilities" | tr ',' '\n' | sed 's/^/  - /')
YAML
    chmod 600 "$path"
    echo "INFO: mirrored legacy $path"
}

# Match the directories appended by internal/worker/servicepath.go. Resolve
# executable files only: a shell alias/function is not an installed worker app.
function installed_worker_app() {
    local PATH="${PATH:-/usr/bin:/bin:/usr/sbin:/sbin}:${HOME}/.local/bin:${HOME}/.claude/local:/opt/homebrew/bin:/usr/local/bin"
    type -P "$1" >/dev/null 2>&1
}

# Running this enrollment installer grants this cluster the supported apps
# present now. The worker never grants apps on a heartbeat or on discovery.
# --no-apps lets an operator install/repair without changing app permissions.
# Use the installed CLI's per-home policy editor, never edit policy.yaml here.
function allow_installed_apps() {
    local binary="$1" labels="" command arg
    local -a args=()
    INSTALL_APPS_RECOVERY=""
    if [[ "${INSTALL_APPS:-yes}" == no ]]; then
        INSTALL_UI_RESULT="App permissions unchanged"
        return 0
    fi
    if installed_worker_app claude; then
        args+=(--allow claude-code)
        labels="Claude Code"
    fi
    if installed_worker_app codex; then
        args+=(--allow codex)
        labels="${labels:+${labels}, }Codex"
    fi
    if [[ ${#args[@]} == 0 ]]; then
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Claude Code and Codex are not installed"
        return 0
    fi
    printf -v command '%q worker apps' "$binary"
    for arg in "${args[@]}" --home "$CLUSTER_URL"; do
        printf -v command '%s %q' "$command" "$arg"
    done
    echo "INFO: allowing ${labels} for ${CLUSTER_URL}"
    if "$binary" worker apps "${args[@]}" --home "$CLUSTER_URL"; then
        INSTALL_UI_RESULT="${labels} allowed for this cluster"
    else
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="App permissions need attention"
        INSTALL_APPS_RECOVERY="$command"
        echo "ERROR: app permissions could not be configured; use Cockpit 0.17.0 or later and retry: ${command}" >&2
    fi
}

# The Ollama variables the worker service must share with the installing
# shell (memql-cockpit#524). A LaunchAgent or user unit inherits nothing
# from a shell, so an OLLAMA_HOST or OLLAMA_MODELS set here would reach the
# setup (which pulls) and not the worker (which serves and advertises): the
# models land where the worker never looks. The installers copy whichever
# are set into the service's own environment.
function ollama_env_names() {
    local name
    for name in OLLAMA_HOST OLLAMA_MODELS; do
        [[ -z "${!name:-}" ]] || printf '%s\n' "$name"
    done
}

# xml_escape makes a value safe as plist <string> content.
function xml_escape() {
    local v="$1"
    v="${v//&/&amp;}"; v="${v//</&lt;}"; v="${v//>/&gt;}"
    printf '%s' "$v"
}

# ollama_plist_env prints the EnvironmentVariables entries for the Ollama
# variables set in this shell, indented for the worker plist.
function ollama_plist_env() {
    local name
    while IFS= read -r name; do
        [[ -n "$name" ]] || continue
        printf '        <key>%s</key>\n        <string>%s</string>\n' "$name" "$(xml_escape "${!name}")"
    done < <(ollama_env_names)
}

# upsert_env_file_var sets KEY=VALUE in a systemd EnvironmentFile, keeping
# every other line. The value is double-quoted with its quotes and
# backslashes escaped, which is the file's own syntax.
function upsert_env_file_var() {
    local file="$1" key="$2" value="$3" tmp quoted
    quoted="${value//\\/\\\\}"; quoted="${quoted//\"/\\\"}"
    tmp="$(mktemp "${file}.XXXXXX")"
    if [[ -f "$file" ]]; then
        grep -v "^${key}=" "$file" > "$tmp" || true
    fi
    printf '%s="%s"\n' "$key" "$quoted" >> "$tmp"
    chmod 0600 "$tmp"
    mv "$tmp" "$file"
}

# install_terminal prints the terminal a stage may ask a question on, or
# nothing. The installer arrives through `curl | bash`, so stdin is the
# script and the person is on /dev/tty -- and only when the screen (fd 3)
# is a terminal too is somebody there to read the question. A scripted run
# (CI, a pipe to a file) gets no question and keeps the exit-3 path.
# MEMQL_INSTALL_TTY names another device; the installer tests use it.
function install_terminal() {
    if [[ -n "${MEMQL_INSTALL_TTY:-}" ]]; then
        printf '%s\n' "$MEMQL_INSTALL_TTY"
        return 0
    fi
    [[ -t 3 ]] || return 0
    if ( exec </dev/tty ) 2>/dev/null; then
        printf '%s\n' /dev/tty
    fi
}

# read_setup_reason prints the one-line outcome `worker setup` recorded
# (MEMQL_SETUP_REASON_FILE), or nothing for a release that records none.
function read_setup_reason() {
    local file="$1" line=""
    [[ -s "$file" ]] && IFS= read -r line < "$file"
    printf '%s' "$line"
}

# setup_inference runs `worker setup --inference` and records how it ended.
# It never fails the install: a machine that paired and has no local models
# is still a working worker.
#
# With a person at a terminal (install_terminal), the setup runs in that
# terminal: it prints the runtime commands, asks once, installs, and pulls
# -- one session instead of a second command to copy (memql-cockpit#524).
# Its output reaches the log too. Without one it runs --non-interactive, and
# a runtime install it may not approve is exit 3, with the command printed.
#
# The exit codes are the setup's own (internal/worker/setup_exit.go):
#   3  approval needed       -> pending, with the command
#   4  this machine cannot   -> unavailable, with the reason, never "retry"
#   2  usage error           -> failed, with the reason
#   5  pull/allow/readiness  -> failed, with the reason and the command
function setup_inference() {
    local binary="$1" rc=0 tty reason_file reason
    local -a wrap=()
    INSTALL_MODELS_REASON=""
    echo ""
    echo "INFO: setting this machine up to serve local models"
    reason_file="$(mktemp)"
    # Models can take many minutes to pull; keep a Mac awake for them.
    if command -v caffeinate >/dev/null 2>&1; then
        wrap=(caffeinate -i)
    fi
    tty="$(install_terminal)"
    if [[ -n "$tty" ]]; then
        install_ui_stop_progress
        printf '\n' >&3
        if MEMQL_SETUP_REASON_FILE="$reason_file" "${wrap[@]+"${wrap[@]}"}" \
            "$binary" worker setup --inference < "$tty" 2>&1 | tee -a /dev/fd/3; then
            rc=0
        else
            rc=$?
        fi
        printf '\n' >&3
    else
        MEMQL_SETUP_REASON_FILE="$reason_file" "${wrap[@]+"${wrap[@]}"}" \
            "$binary" worker setup --inference --non-interactive || rc=$?
    fi
    reason="$(read_setup_reason "$reason_file")"
    rm -f "$reason_file"
    case "$rc" in
        0)
            INSTALL_MODELS_STATE=ready
            INSTALL_UI_RESULT="Local models ready"
            return 0
            ;;
        3)
            INSTALL_MODELS_STATE=pending
            INSTALL_UI_STAGE_STATE=pending
            INSTALL_UI_RESULT="Local models need your approval"
            echo ""
            echo "INFO: the model runtime is not installed or not running here, and"
            echo "      this run could not ask you to approve it. Run this yourself,"
            echo "      in a terminal:"
            echo ""
            echo "  ${binary} worker setup --inference"
            echo ""
            ;;
        4)
            INSTALL_MODELS_STATE=unavailable
            INSTALL_UI_STAGE_STATE=pending
            INSTALL_MODELS_REASON="$reason"
            INSTALL_UI_RESULT="This machine can't run local models"
            echo "INFO: '${binary} worker setup --inference' exited 4: this machine cannot serve local models${reason:+: ${reason}}." >&2
            echo "      It is paired and working; everything else is unaffected." >&2
            ;;
        *)
            INSTALL_MODELS_STATE=failed
            INSTALL_UI_STAGE_STATE=pending
            if [[ -z "$reason" ]]; then
                case "$rc" in
                    2) reason="the setup command did not accept its arguments (exit 2). See the log: ${INSTALL_LOG:-}" ;;
                    *) reason="the setup exited ${rc}. See the log: ${INSTALL_LOG:-}" ;;
                esac
            fi
            INSTALL_MODELS_REASON="$reason"
            INSTALL_UI_RESULT="Local model setup needs attention"
            echo "WARN: '${binary} worker setup --inference' exited ${rc}: ${reason}" >&2
            echo "      This machine is paired and working; it is not offering local" >&2
            echo "      models yet." >&2
            ;;
    esac
    return 0
}

# setup_permissions is the first stage on a computer-use Mac
# (memql-cockpit#548): `worker setup --permissions` opens the Permissions
# window and waits until Accessibility and Screen Recording are allowed, or
# until the person chooses to go on without them -- in the window, or by
# pressing Return in this terminal. Nothing downloads before then. Like the
# other stages it never fails the install.
#
# The exit codes are the setup's own (internal/worker/setup_permissions.go):
#   0  both allowed
#   3  nobody to ask (no terminal)   -> failed, with the reason
#   4  the person went on without    -> skipped, with what was left off
#   5  the worker could not be read  -> failed, with the reason
function setup_permissions() {
    local binary="$1" usage rc=0 tty reason_file reason
    local -a args=(worker setup --permissions)
    INSTALL_PERMISSIONS_REASON=""
    usage="$("$binary" worker setup --help 2>&1 || true)"
    if [[ "$usage" != *"-permissions"* ]]; then
        INSTALL_PERMISSIONS_STATE=failed
        INSTALL_PERMISSIONS_REASON="this Cockpit cannot ask for them during setup."
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Permissions not set up"
        return 0
    fi
    [[ "${INSTALL_MENU:-yes}" == yes ]] || args+=(--no-window)
    reason_file="$(mktemp)"
    tty="$(install_terminal)"
    if [[ -n "$tty" ]]; then
        install_ui_stop_progress
        printf '\n' >&3
        if MEMQL_SETUP_REASON_FILE="$reason_file" "$binary" "${args[@]}" < "$tty" 2>&1 | tee -a /dev/fd/3; then
            rc=0
        else
            rc=$?
        fi
        printf '\n' >&3
    else
        MEMQL_SETUP_REASON_FILE="$reason_file" "$binary" "${args[@]}" --non-interactive || rc=$?
    fi
    reason="$(read_setup_reason "$reason_file")"
    rm -f "$reason_file"
    case "$rc" in
        0)
            INSTALL_PERMISSIONS_STATE=ready
            INSTALL_UI_RESULT="Accessibility and Screen Recording allowed"
            ;;
        4)
            INSTALL_PERMISSIONS_STATE=skipped
            INSTALL_PERMISSIONS_REASON="${reason:-skipped for now.}"
            INSTALL_UI_STAGE_STATE=pending
            INSTALL_UI_RESULT="Permissions: ${reason:-skipped for now.}"
            ;;
        *)
            INSTALL_PERMISSIONS_STATE=failed
            INSTALL_PERMISSIONS_REASON="${reason:-the setup exited ${rc}. See the log: ${INSTALL_LOG:-}}"
            INSTALL_UI_STAGE_STATE=pending
            INSTALL_UI_RESULT="Permissions not set up"
            ;;
    esac
    return 0
}

# setup_workbench enables Fleet workbenches and, like setup_inference, never
# fails the install (memql-cockpit#524). The installer comes from main and
# installs the latest release, so the binary may predate --workbench: that
# is detected and reported, not run into. Docker missing, stopped, too
# small, or an image pull that times out is one line and a retry command
# in the summary -- and the stage runs after local models, so they never
# wait on Docker.
function setup_workbench() {
    local binary="$1" usage rc=0 reason_file reason version
    INSTALL_WORKBENCH_REASON=""
    usage="$("$binary" worker setup --help 2>&1 || true)"
    if [[ "$usage" != *"-workbench"* ]]; then
        version="$(read_binary_version "$binary")"
        INSTALL_WORKBENCH_STATE=unsupported
        INSTALL_UI_STAGE_STATE=pending
        INSTALL_UI_RESULT="Fleet workbenches need a newer Cockpit"
        echo "INFO: Cockpit ${version:-(unknown version)} has no 'worker setup --workbench'; skipped."
        return 0
    fi
    reason_file="$(mktemp)"
    MEMQL_SETUP_REASON_FILE="$reason_file" "$binary" worker setup --workbench || rc=$?
    reason="$(read_setup_reason "$reason_file")"
    rm -f "$reason_file"
    if [[ "$rc" == 0 ]]; then
        INSTALL_WORKBENCH_STATE=ready
        INSTALL_UI_RESULT="Fleet workbenches ready"
        return 0
    fi
    INSTALL_WORKBENCH_STATE=failed
    INSTALL_WORKBENCH_REASON="${reason:-the setup exited ${rc}. See the log: ${INSTALL_LOG:-}}"
    INSTALL_UI_STAGE_STATE=pending
    INSTALL_UI_RESULT="Fleet workbenches need attention"
    echo "WARN: '${binary} worker setup --workbench' exited ${rc}: ${INSTALL_WORKBENCH_REASON}" >&2
    return 0
}

# ---------------------------------------------------------------
# The setup report (memql-cockpit#541)
# ---------------------------------------------------------------
#
# The local-model and workbench stages run AFTER the worker has connected,
# so MemQL OS can show them only if the worker carries them to the cluster.
# These two tell the worker which stages are coming and when the installer
# is done; the setup commands report their own progress in between. Both are
# best-effort: a worker that is not up, or too old to know the action,
# changes nothing about the install, and neither prints anything.

# setup_report_plan names the stages this install will run, in its order:
# the person's turn first (a computer-use Mac's permissions), then the
# downloads, which need nobody.
function setup_report_plan() {
    local inference="$1" workbench="$2" permissions="${3:-no}" plan=""
    if [[ "$permissions" == yes ]]; then
        plan="permissions"
    fi
    if [[ "$inference" == yes ]]; then
        plan="${plan:+${plan},}runtime,models"
    fi
    if [[ "$workbench" == yes ]]; then
        plan="${plan:+${plan},}workbench"
    fi
    printf '%s' "$plan"
}

# report_setup_plan tells the worker what is coming. It waits for the
# worker the installer has just started to open its control socket. An
# install with no stage still says so: the worker keeps its report across
# restarts, and an earlier install's would otherwise stand for this one.
function report_setup_plan() {
    local binary="$1" plan
    plan="$(setup_report_plan "$2" "$3" "${4:-no}")"
    if [[ -z "$plan" ]]; then
        "$binary" worker control --action=setup --reset --wait=20s >/dev/null 2>&1 || true
        return 0
    fi
    "$binary" worker control --action=setup --plan="$plan" --wait=20s >/dev/null 2>&1 || true
}

# report_setup_finished says the installer is done: a stage it never
# reached is skipped rather than shown as still to come.
function report_setup_finished() {
    local binary="$1"
    [[ -n "$(setup_report_plan "$2" "$3" "${4:-no}")" ]] || return 0
    "$binary" worker control --action=setup --finished --wait=5s >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------
# Uninstall helpers -- shared by uninstall-mac.sh and uninstall-linux.sh
# ---------------------------------------------------------------
#
# The uninstallers are the installers run backwards, and everything
# that is the same on both platforms lives here so the two cannot
# disagree about what "uninstalled" means: which binaries go, which
# files under ~/.memql go always, which go only under --purge, and
# the one fence every recursive delete is checked against. The
# service half (launchctl against systemctl) stays in the drivers, as
# the install half does.
#
# Two ledgers, appended to by every helper that removes or keeps
# something and printed by print_uninstall_summary. Newline-joined
# strings rather than arrays: macOS ships bash 3.2, where expanding an
# EMPTY array under `set -u` is an unbound-variable error, and an
# empty ledger is the common case for half of these helpers.
UNINSTALL_REMOVED=""
UNINSTALL_KEPT=""

# The third ledger: the exit code a leftover earns. A file or tree that
# could not be removed is recorded as Kept by the helper that tried and
# noted here, so the step that found it can carry on (every remover is
# called `|| true` from the drivers) and the run still ends non-zero
# with the summary printed. Without it a failing rm -rf under `set -e`
# aborted the script mid-purge, with no summary and the later steps
# never attempted (a review finding). First failure wins.
UNINSTALL_LEFTOVER_RC=0

function note_leftover() {
    [[ "$UNINSTALL_LEFTOVER_RC" -ne 0 ]] || UNINSTALL_LEFTOVER_RC="$1"
}

function record_removed() {
    UNINSTALL_REMOVED="${UNINSTALL_REMOVED}  $1"$'\n'
}

function record_kept() {
    UNINSTALL_KEPT="${UNINSTALL_KEPT}  $1"$'\n'
}

# record_scoped_unpair_failure is for the one failure that happens AFTER
# a write: the real `worker unpair` ran and failed, or answered
# something unreadable, once the worker had been stopped. That run is
# not "nothing was changed" -- the service is down until the person
# looks, and Go writes workers.yaml before the worker.yaml mirror, so
# the files may be half-applied. $1 is the command that starts the
# worker again, empty when this run did not stop it. With something
# kept and nothing removed the summary reads FAILED, not REFUSED.
function record_scoped_unpair_failure() {
    local restart="${1:-}"
    record_kept "${HOME}/.memql/workers.yaml and worker.yaml (removal of ${CLUSTER_URL} did not complete and may be half-applied; repair them before restarting the worker)"
    [[ -z "$restart" ]] || record_kept "the worker service (stopped by this run and NOT restarted; once repaired, start it with:  ${restart})"
}

# record_scoped_restart_failure is for the failures AFTER the real
# unpair SUCCEEDED with siblings remaining: the worker for them would
# not start again, or a legacy agent would not stop. The enrollment is
# gone (Removed) and the worker is down (Kept, with the command that
# starts it), so the summary reads PARTIAL -- never REFUSED over a
# rewritten registry and a stopped service (a review finding). $2 is an
# extra Kept line for what else the person has to do.
function record_scoped_restart_failure() {
    local restart="${1:-}" extra="${2:-}"
    record_removed "the ${CLUSTER_URL} enrollment from ${HOME}/.memql/workers.yaml (${OTHER_HOMES} other enrollment(s) remain, with the shared runtime)"
    [[ -z "$restart" ]] || record_kept "the worker service for the remaining enrollment(s) (stopped by this run and NOT restarted; start it with:  ${restart})"
    [[ -z "$extra" ]] || record_kept "$extra"
}

# record_scoped_leftovers settles a sibling-remaining removal that
# SUCCEEDED but left a file behind (a legacy service file that would not
# unlink, recorded Kept by remove_path_if_present): the enrollment is
# gone (Removed), and the leftover code is returned so the driver's
# finish prints the PARTIAL summary instead of the SUCCESS line. 0 when
# nothing was left over.
function record_scoped_leftovers() {
    [[ "$UNINSTALL_LEFTOVER_RC" -ne 0 ]] || return 0
    record_removed "the ${CLUSTER_URL} enrollment from ${HOME}/.memql/workers.yaml (${OTHER_HOMES} other enrollment(s) remain, with the shared runtime)"
    return "$UNINSTALL_LEFTOVER_RC"
}

# under_memql_home answers whether $1 lies inside ${HOME}/.memql, the
# ONLY tree the uninstallers delete recursively. It is strict about
# shape on purpose -- absolute, no `..` segment -- because a state_dir
# read out of worker config is operator-authored text, and the one thing
# an uninstaller must never do is `rm -rf` wherever a file it did not
# write points.
function under_memql_home() {
    local path="$1" fence
    # HOME itself may carry a trailing slash; the fence is compared as a
    # string, so it is spelled the one way the paths are.
    fence="$(normalize_tree_path "${HOME}/.memql")"
    [[ "$path" == /* ]] || return 1
    [[ "$path" != *"/../"* && "$path" != *"/.." ]] || return 1
    [[ "$path" == "$fence" || "$path" == "$fence"/* ]]
}

# remove_path_if_present deletes ONE file or symlink and says so, or
# says there was nothing there. Never recursive -- a directory handed
# to it is reported and left -- so the drivers can name paths outside
# the ~/.memql fence (the LaunchAgent plist, the systemd unit) without
# the fence being all that stands between them and a tree.
function remove_path_if_present() {
    local path="$1"
    if [[ -d "$path" && ! -L "$path" ]]; then
        echo "WARN: $path is a directory; not removed by this step"
        return 0
    fi
    if [[ ! -e "$path" && ! -L "$path" ]]; then
        echo "INFO: $path not present; nothing to remove"
        return 0
    fi
    if rm -f "$path"; then
        echo "INFO: removed $path"
        record_removed "$path"
    else
        echo "WARN: could not remove $path; delete it by hand"
        record_kept "$path (could not be removed; delete it by hand)"
        note_leftover 5
        return 5
    fi
}

# remove_worker_config deletes the worker token files on a full
# uninstall: the multi-home registry (~/.memql/workers.yaml) and the
# legacy single-home mirror (~/.memql/worker.yaml). Both hold cluster
# tokens, so both always go -- not only under --purge. clusters.yaml
# and credentials/ stay: they belong to `memql cluster`, not the worker.
function remove_worker_config() {
    remove_path_if_present "${HOME}/.memql/workers.yaml" || true
    remove_path_if_present "${HOME}/.memql/worker.yaml" || true
}

# normalize_tree_path prints $1 with every `//` collapsed and every
# trailing slash dropped ("/" stays "/"). The fence below compares
# STRINGS, and a state_dir is operator-authored text: `~/.memql/` IS
# ~/.memql and must hit the same guard (a review found it did not, and
# --purge then rm -rf'd the whole tree, credentials and backups
# included). The trailing slash matters for rm as well: `rm -rf link/`
# FOLLOWS the link (POSIX trailing-slash resolution; BSD rm removes the
# target directory, GNU rm empties it, both exit 0), so nothing here may
# hand rm a path ending in `/`. Spelled with prefix/suffix expansions on
# purpose: bash 3.2 renders `${p//\/\//\/}` with a literal backslash.
function normalize_tree_path() {
    local p="$1"
    while [[ "$p" == *//* ]]; do p="${p%%//*}/${p#*//}"; done
    while [[ "$p" == */ && "$p" != / ]]; do p="${p%/}"; done
    printf '%s\n' "$p"
}

# tree_removal_verdict is the ONE statement of what a recursive delete
# under --purge may touch. It prints "absent", "remove", or "keep:<why>"
# for a directory and changes nothing, so the remover and the --dry-run
# plan cannot disagree about the fence: inside ~/.memql only; never
# through a symlinked ancestor (an alias that would sweep something
# else); never ~/.memql itself or a `.` spelling of it; never the
# credential / certificate / backup trees, whatever a state_dir says.
# Every guard judges ONE normalized spelling (normalize_tree_path).
function tree_removal_verdict() {
    local dir fence
    dir="$(normalize_tree_path "$1")"
    fence="$(normalize_tree_path "${HOME}/.memql")"
    if [[ ! -e "$dir" && ! -L "$dir" ]]; then
        echo "absent"
        return 0
    fi
    if ! under_memql_home "$dir"; then
        echo "keep:outside ${fence}; not touched"
        return 0
    fi
    # A state_dir is a DIRECTORY. One that names a regular file inside
    # the fence (clusters.yaml, say) is a file this script must not
    # rm -rf on the strength of a config line. A symlink is judged as a
    # link (removed as one, below), whatever it points at.
    if [[ ! -L "$dir" && ! -d "$dir" ]]; then
        echo "keep:not a directory"
        return 0
    fi
    local cursor="$dir" protected
    while [[ "$cursor" != "$fence" && "$cursor" != / ]]; do
        if [[ -L "$cursor" && "$cursor" != "$dir" ]]; then
            echo "keep:symlinked ancestor"
            return 0
        fi
        cursor="$(dirname "$cursor")"
    done
    case "$dir" in
        "$fence"|*/./*|*/.) echo "keep:unsafe purge target"; return 0 ;;
    esac
    # Case-folded: macOS's default APFS is case-insensitive, so
    # ~/.memql/Credentials IS ~/.memql/credentials there. On a
    # case-sensitive filesystem this over-keeps a genuinely distinct
    # directory of that name, which is the safe direction and is said
    # in the summary. bash 3.2 has no ${var,,}; tr does the folding.
    local dir_folded fence_folded
    dir_folded="$(printf '%s' "$dir" | tr '[:upper:]' '[:lower:]')"
    fence_folded="$(printf '%s' "$fence" | tr '[:upper:]' '[:lower:]')"
    for protected in backups credentials certs certificates; do
        case "$dir_folded" in
            "$fence_folded/$protected"|"$fence_folded/$protected/"*) echo "keep:protected data"; return 0 ;;
        esac
    done
    echo "remove"
}

# remove_tree_if_present deletes a directory recursively, inside the
# ~/.memql fence and nowhere else. Outside it the directory is KEPT
# and reported with its path, so the person can decide. That is not
# an error: the uninstall still did everything it was allowed to. A
# symlink AT the path is removed as a link and nothing else: what it
# points at (a state dir someone moved to another disk) is theirs.
function remove_tree_if_present() {
    local dir verdict
    dir="$(normalize_tree_path "$1")"
    verdict="$(tree_removal_verdict "$dir")"
    case "$verdict" in
        absent)
            echo "INFO: $dir not present; nothing to remove"
            ;;
        remove)
            if [[ -L "$dir" ]]; then
                if rm -f "$dir"; then
                    echo "INFO: removed the symlink $dir (what it pointed at was not touched)"
                    record_removed "$dir (the symlink only)"
                else
                    echo "WARN: could not remove the symlink $dir; delete it by hand"
                    record_kept "$dir (could not be removed; delete it by hand)"
                    note_leftover 5
                    return 5
                fi
            elif rm -rf "$dir"; then
                echo "INFO: removed $dir"
                record_removed "$dir"
            else
                echo "WARN: could not fully remove $dir; delete what is left by hand"
                record_kept "$dir (could not be fully removed; delete what is left by hand)"
                note_leftover 5
                return 5
            fi
            ;;
        keep:outside*)
            echo "WARN: $dir is outside ${HOME}/.memql; not touched. Delete it by hand if you want it gone."
            record_kept "$dir (${verdict#keep:})"
            ;;
        keep:*)
            echo "WARN: kept $dir (${verdict#keep:})"
            record_kept "$dir (${verdict#keep:})"
            ;;
    esac
}

# worker_state_dir_from_yaml prints the machine's state ROOT, the
# directory --purge removes, or the default when no file names one.
# It reads the multi-home registry (workers.yaml beside the path handed
# in) FIRST -- its state_dir is the machine root -- and the legacy
# mirror second: the installer writes the mirror's state_dir PER HOME
# (<root>/homes/<id>, mirroring ConfigForHome), and a purge that read
# the mirror first removed one home's subdirectory, left worker.log and
# the root behind, and said SUCCESS (a review finding). A per-home
# directory answers its root exactly as Go's machineStateRoot does
# (internal/worker/machineid.go). The drivers call it BEFORE the token
# files are removed. A leading `~/` is expanded the way the shell would
# have; the spelling is normalized (normalize_tree_path); anything else
# reaches the fence as written.
function worker_state_dir_from_yaml() {
    local path="$1"
    local dir="" try parent
    for try in "$(dirname "$path")/workers.yaml" "$path"; do
        if [[ -f "$try" ]]; then
            dir="$(yaml_top_scalar "$try" state_dir)"
            [[ -n "$dir" ]] && break
        fi
    done
    case "$dir" in
        "")   dir="$STATE_DIR_DEFAULT" ;;
        \~)   dir="$HOME" ;;
        \~/*) dir="${HOME}/${dir#\~/}" ;;
    esac
    dir="$(normalize_tree_path "$dir")"
    parent="${dir%/*}"
    if [[ "$dir" == */* && "$parent" == */* && "${parent##*/}" == homes && -n "${parent%/*}" ]]; then
        dir="${parent%/*}"
    fi
    echo "$dir"
}

# mode_binary_names prints every file name an install can leave in a
# mode's bin directory: the installed command (first, so it goes first
# on the way out), the two download-named binaries for this platform
# (headless and computer-use) and the pre-rename names. One list, read
# by the remover, the presence probe and the retained-runtime ledger,
# so a rename cannot leave one of them looking for the old name.
function mode_binary_names() {
    local headless computeruse
    headless="$(binary_name_for headless)" || return 1
    computeruse="$(binary_name_for computeruse)" || return 1
    printf '%s\n' "${INSTALLED_COMMAND} ${headless} ${computeruse} ${LEGACY_BINARIES}"
}

# install_mode_has_files answers whether ANYTHING an install leaves in
# a mode's bin directory is there -- a dangling `memql` symlink counts,
# because it is ours to remove. It is how the uninstallers detect the
# shapes on a machine instead of asking the caller which one was used.
function install_mode_has_files() {
    local mode="$1" dest_dir names name path
    dest_dir="$(install_mode_dir "$mode")" || return 1
    names="$(mode_binary_names)" || return 1
    for name in $names; do
        path="${dest_dir}/${name}"
        if [[ -e "$path" || -L "$path" ]]; then
            return 0
        fi
    done
    return 1
}

# first_executable prints the first argument that is an executable
# regular file (through a symlink), or returns 1 when none is. The
# uninstallers use it to find A memql that can parse the enrollment
# files -- any installed shape will do, and a dangling symlink will not.
function first_executable() {
    local candidate
    for candidate in "$@"; do
        if [[ -n "$candidate" && -f "$candidate" && -x "$candidate" ]]; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done
    return 1
}

# remove_binaries_with_mode is install_binary_with_mode's inverse: from
# the mode's directory it deletes the installed command, the two
# download-named binaries beside it (headless and computer-use, for
# this platform) and the pre-rename names. Every name derives from the
# constants the install used, so a rename moves both or neither.
#
# Scoped to that ONE directory on purpose, as install.sh's
# remove_legacy_binaries is: a PATH-wide sweep would delete a memql
# this installer never placed. Sudo is asked for only once something
# is actually there to remove -- a machine with nothing at
# /usr/local/bin must not raise a password prompt to find that out.
# When sudo is needed and cannot be had, the paths are printed for the
# person to delete by hand and the function returns 4 (prerequisite
# missing, the capability-script convention preflight_asset uses); the
# drivers carry on to the token, which matters more than the binary,
# and exit with that code at the end.
function remove_binaries_with_mode() {
    local mode="$1"
    local dest_dir names
    dest_dir="$(install_mode_dir "$mode")" || return 1
    names="$(mode_binary_names)" || return 1

    local name path
    if ! install_mode_has_files "$mode"; then
        echo "INFO: no ${INSTALLED_COMMAND} binary at ${dest_dir}; nothing to remove"
        return 0
    fi

    local privileged="no"
    if [[ "$mode" == "system" && $EUID -ne 0 ]]; then
        if ! require_sudo uninstall; then
            echo "ERROR: cannot remove from ${dest_dir} without sudo. Delete these by hand:" >&2
            for name in $names; do
                path="${dest_dir}/${name}"
                if [[ -e "$path" || -L "$path" ]]; then
                    echo "       $path" >&2
                    record_kept "$path (needs sudo; delete it by hand)"
                fi
            done
            return 4
        fi
        privileged="yes"
    fi

    # The command's symlink goes first (it is first in the list), so no
    # moment leaves a `memql` pointing at a file already gone.
    for name in $names; do
        path="${dest_dir}/${name}"
        [[ -e "$path" || -L "$path" ]] || continue
        if [[ "$privileged" == "yes" ]]; then
            sudo rm -f "$path"
        else
            rm -f "$path"
        fi
        echo "INFO: removed $path"
        record_removed "$path"
    done
    # The user-local bin dir was the installer's to create, so it is
    # taken away again when nothing is left in it. /usr/local/bin is
    # nobody's to remove, and rmdir refuses a non-empty directory.
    if [[ "$mode" == "user-local" ]]; then
        rmdir "$dest_dir" 2>/dev/null || true
    fi
}

# purge_worker_state is what --purge adds: policy.yaml (the owner's
# apps.homes / models.allow / backup.roots -- kept by default because
# it is authored, not generated), the state dir (logs, ledgers, the
# recorded registration id), the consent socket, and then ~/.memql
# itself once nothing is left in it. The CLI's clusters.yaml and
# credentials/ are never on this list: they belong to `memql cluster`,
# not to the worker, and an uninstall of the worker that signed the
# person out of every cluster would be a second thing nobody asked
# for. When they are there ~/.memql stays, and the summary says what
# kept it.
function purge_worker_state() {
    local state_dir="$1"
    # Every step runs whatever the one before it left: a leftover is in
    # the ledger and UNINSTALL_LEFTOVER_RC, never a reason to stop.
    remove_path_if_present "${HOME}/.memql/policy.yaml" || true
    remove_tree_if_present "$state_dir" || true
    remove_path_if_present "${HOME}/.memql/worker.sock" || true
    # The native model runtime and its models (Linux, and a Mac without
    # Homebrew): gigabytes under the fence, kept without --purge because
    # the models were pulled on purpose and cost hours to pull again.
    remove_tree_if_present "${HOME}/.memql/ollama" || true
    remove_memql_home_if_empty
}

# report_kept_state is the no-purge counterpart: it names what stays
# and the flag that removes it, so a token-less ~/.memql is never left
# behind unexplained.
function report_kept_state() {
    local state_dir="$1"
    local path
    for path in "${HOME}/.memql/policy.yaml" "$state_dir" "${HOME}/.memql/ollama"; do
        if [[ -e "$path" ]]; then
            echo "INFO: kept $path (re-run with --purge to remove it)"
            record_kept "$path (--purge removes it)"
        fi
    done
}

# remove_memql_home_if_empty takes ~/.memql away only when it is
# empty -- rmdir, never rm -rf, so whatever is still in there decides.
# What kept it is named, because "kept ~/.memql" alone reads as a
# purge that did not work.
function remove_memql_home_if_empty() {
    local dir="${HOME}/.memql"
    [[ -d "$dir" ]] || return 0
    if rmdir "$dir" 2>/dev/null; then
        echo "INFO: removed $dir (empty)"
        record_removed "$dir"
        return 0
    fi
    # A glob walk rather than `ls`: no external tool, and a name with
    # a space or a newline in it is listed rather than split. The three
    # patterns are the visible entries, the dotfiles, and the `..x`
    # names the second pattern cannot reach; an unmatched pattern stays
    # literal and fails the existence test.
    local left="" entry
    for entry in "$dir"/* "$dir"/.[!.]* "$dir"/..?*; do
        if [[ -e "$entry" || -L "$entry" ]]; then
            left="${left}${entry##*/} "
        fi
    done
    echo "INFO: kept $dir; it still holds: ${left}"
    record_kept "$dir (still holds: ${left})"
}

# ---------------------------------------------------------------
# Enrollment discovery + the no-flag decision -- shared by both uninstallers
# ---------------------------------------------------------------
#
# MemQL OS composes the uninstall one-liner without --cluster=URL or
# --all-homes, and the first thing a person copying it saw was
# "ERROR: choose --cluster=URL or --all-homes". The person has no
# better information than the enrollment files already hold, so the
# script reads them and decides; the flags remain for saying it
# explicitly. The reading here is a TEXT view of workers.yaml (one URL
# per cluster identity, matched the way Go's sameClusterURL matches),
# used only to count and to name clusters -- the removal itself still
# goes through the installed binary's `worker unpair --cluster-url`,
# whose YAML decoder is the one that decides what a home is.

# enrollment_files_regular refuses (3) to decide anything from an
# enrollment file that is a symlink: the binary refuses to unpair
# through one ("enrollment files must be regular files"), and a text
# reader that followed the link would count tokens that live somewhere
# this uninstall has no business touching.
function enrollment_files_regular() {
    local path
    for path in "${HOME}/.memql/workers.yaml" "${HOME}/.memql/worker.yaml"; do
        if [[ -L "$path" ]]; then
            echo "ERROR: $path is a symlink; not deciding from it. Restore the file, or pass --all-homes to remove everything." >&2
            return 3
        fi
    done
}

# count_lines prints how many non-empty lines $1 holds. Pure bash: the
# uninstallers run this under a PATH that may hold no wc or grep.
function count_lines() {
    local text="$1" n=0 line
    while IFS= read -r line || [[ -n "$line" ]]; do
        if [[ -n "$line" ]]; then
            n=$((n + 1))
        fi
    done <<< "$text"
    echo "$n"
}

# list_enrolled_cluster_urls prints one cluster URL per enrolled
# cluster: from the `homes:` list of workers.yaml when it is present
# (the registry is the whole truth once it exists -- see
# decideRunMode), else from the top-level cluster_url of the legacy
# worker.yaml. Two homes for the same cluster identity -- the same
# host, however the URL is spelled -- print once, because unpairing by
# URL removes them together and they are one enrollment to the person.
# Disabled homes count: they are enrollments a scoped removal keeps.
# Neither file, `homes: []`, or a registry with provably no homes key
# prints nothing. The reader accepts every block-list spelling yaml.v3
# does (a review found three it missed, each read as ZERO
# enrollments): items indented under the key or at column 0 (a
# mapping-rooted document has no other column-0 dash, so one cannot
# start a new top-level key), a comment on the `homes:` line, and an
# item whose first key is cluster_url rather than id -- any `- ` starts
# an item, whatever key follows it. Every OTHER spelling of the
# registry is a refusal (5), never zero: see workers_registry_readable.
function list_enrolled_cluster_urls() {
    local workers_path="$1" legacy_path="$2"
    if [[ -f "$workers_path" && ! -L "$workers_path" ]]; then
        # The registry comes in the shapes workers_registry_readable
        # accepts, or this script does not decide from it: "no
        # enrollment" over a registry the block reader below cannot see
        # into took the --all-homes path and deleted every token, the
        # siblings' too, under exit 0 (a flow or JSON root, a quoted or
        # spaced key, a BOM, CR-only lines ... -- review and verification
        # findings). 5, the binary's own code for it; --all-homes is the
        # way past, because it reads nothing.
        if ! workers_registry_readable "$workers_path"; then
            refuse_unreadable_registry "$workers_path" "Restore it, or pass --all-homes to remove everything on this machine."
            return 5
        fi
        awk '
            function norm(s) {
                gsub(/^[[:space:]]+|[[:space:]]+$/, "", s)
                gsub(/^["'"'"']|["'"'"']$/, "", s)
                sub(/\/+$/, "", s)
                return tolower(s)
            }
            function host(s) {
                s = norm(s)
                sub(/^[a-z][a-z0-9+.-]*:\/\//, "", s)
                sub(/\/.*$/, "", s)
                sub(/:[0-9]+$/, "", s)
                return s
            }
            function flush(    key) {
                if (cur == "") return
                key = host(cur)
                if (key == "") key = norm(cur)
                if (!(key in seen)) { seen[key] = 1; print cur }
                cur = ""
            }
            BEGIN { in_homes = 0; cur = "" }
            /^homes:[[:space:]]*(#.*)?$/ { in_homes = 1; next }
            !in_homes { next }
            /^[^[:space:]#-]/ { flush(); in_homes = 0; next }
            /^[[:space:]]*-[[:space:]]*/ { flush(); sub(/^[[:space:]]*-[[:space:]]*/, "") }
            /^[[:space:]]*cluster_url:[[:space:]]*/ {
                cur = $0
                sub(/^[[:space:]]*cluster_url:[[:space:]]*/, "", cur)
                sub(/[[:space:]]+(#.*)?$/, "", cur)
                gsub(/^["'"'"']|["'"'"']$/, "", cur)
                next
            }
            END { flush() }
        ' "$workers_path"
    elif [[ -f "$legacy_path" && ! -L "$legacy_path" ]]; then
        awk '
            /^cluster_url:[[:space:]]*/ {
                sub(/^cluster_url:[[:space:]]*/, "")
                sub(/[[:space:]]+(#.*)?$/, "")
                gsub(/^["'"'"']|["'"'"']$/, "")
                if ($0 != "") print
                exit
            }
        ' "$legacy_path"
    fi
}

# unpair_json_remaining prints the `remaining` count out of the JSON
# `worker unpair --cluster-url ... --json` writes -- Go's
# UnpairURLResult, one object of ints and bools on one line (see
# internal/worker/unpair_url.go). Anything that is not that shape is a
# failure (5), never a guess: this number decides whether the shared
# runtime stays. sed rather than a platform JSON tool so the Linux
# driver and the ubuntu test lane read it the same way macOS does.
function unpair_json_remaining() {
    unpair_json_int "$1" remaining
}

# unpair_json_removed prints the `removed` count out of the same
# result: how many homes matched the URL. The --dry-run plan says it;
# the decision to keep the runtime is `remaining`'s alone.
function unpair_json_removed() {
    unpair_json_int "$1" removed
}

function unpair_json_int() {
    local file="$1" field="$2" n
    n="$(sed -n -E 's/^\{.*"'"$field"'":[[:space:]]*([0-9]+)[,}].*$/\1/p' "$file")"
    if [[ ! "$n" =~ ^[0-9]+$ ]]; then
        echo "ERROR: unexpected unpair result; enrollment files left as they were" >&2
        return 5
    fi
    printf '%s\n' "$n"
}

# uninstall_invocation prints how THIS run was started, so a printed
# remedy is the same command with the missing flag added: the script
# by path when it was run from a clone, or the `curl ... | bash -s --`
# one-liner from RAW_BASE (what MemQL OS shows) when it was piped, in
# which case $0 is `bash` and says nothing about where it came from.
# $1 is the driver's file name.
function uninstall_invocation() {
    local script_name="$1"
    if [[ "$(basename "$0")" == "$script_name" && -f "$0" ]]; then
        printf '%s' "$0"
    else
        printf 'curl -fsSL %s/%s | bash -s --' \
            "${RAW_BASE:-https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install}" "$script_name"
    fi
}

# print_enrollment_commands prints, for every enrolled cluster, the
# exact command that removes just it, then the one that removes them
# all -- so the person copies rather than guesses. $2 carries the flags
# that stay valid for a one-cluster removal (--user-local, and --dry-run
# when this run was one, so a copied remedy is still a dry run); --purge
# is only offered on the --all-homes line, because a scoped removal
# refuses it while another enrollment remains.
function print_enrollment_commands() {
    local script_name="$1" carried="$2" urls="$3"
    local prefix url all_flags="$carried"
    UNINSTALL_UI_CHOICES="$urls"
    prefix="$(uninstall_invocation "$script_name")"
    [[ "${PURGE:-no}" != yes ]] || all_flags="${all_flags} --purge"
    while IFS= read -r url; do
        [[ -n "$url" ]] || continue
        echo "         ${prefix}${carried} --cluster=${url}"
    done <<< "$urls"
    echo "       Or remove every enrollment and the shared worker runtime at once:"
    echo "         ${prefix}${all_flags} --all-homes"
}

# resolve_uninstall_scope turns a run with neither --cluster=URL nor
# --all-homes into one of them, from the machine:
#
#   one enrollment  -> that cluster, as if --cluster=<its url> were given
#   none            -> --all-homes: nothing to unpair, only runtime files
#   several         -> refuse (2) with one exact command per cluster
#
# Explicit flags are left exactly as given. Reads and sets the drivers'
# CLUSTER_URL / ALL_HOMES globals. An enrollment file that is a symlink
# is not decided from (3), and a registry the reader cannot make sense
# of is not either (5) -- --all-homes is the way past both. $1 is the
# driver's file name and $2 the flags to carry into printed commands.
# shellcheck disable=SC2034  # CLUSTER_URL / ALL_HOMES are the drivers' globals
function resolve_uninstall_scope() {
    local script_name="$1" carried="${2:-}"
    if [[ -n "$CLUSTER_URL" || "$ALL_HOMES" == yes ]]; then
        return 0
    fi
    enrollment_files_regular || return $?
    local urls n
    urls="$(list_enrolled_cluster_urls "${HOME}/.memql/workers.yaml" "${HOME}/.memql/worker.yaml")" || return $?
    n="$(count_lines "$urls")"
    case "$n" in
        0)
            ALL_HOMES="yes"
            echo "INFO: no worker enrollment on this machine; removing the worker runtime (as --all-homes)"
            ;;
        1)
            CLUSTER_URL="$urls"
            echo "INFO: one worker enrollment on this machine, ${CLUSTER_URL}; removing it (as --cluster=${CLUSTER_URL})"
            ;;
        *)
            {
                echo "ERROR: ${n} clusters are enrolled on this machine and this run did not say which one to remove."
                echo "       Nothing was changed. Re-run naming the cluster to remove:"
                print_enrollment_commands "$script_name" "$carried" "$urls"
            } >&2
            return 2
            ;;
    esac
}

# scoped_precheck runs before the binary is asked to unpair CLUSTER_URL
# and settles the cases where the binary is the wrong tool:
#
#   no enrollment at all   -> SCOPED_DECISION=full: nothing to unpair; the
#                             runtime goes as --all-homes would (a machine
#                             that never paired, or already unpaired, is
#                             still being uninstalled)
#   none matches the URL   -> refuse (3) with every enrolled cluster and
#                             the command for each: a wrong URL must never
#                             remove someone else's enrollment
#   one matches            -> SCOPED_DECISION=unpair, and ENROLLED_OTHER
#                             says whether any OTHER cluster is enrolled,
#                             which is what the no-binary path needs
#
# Matching is Go's sameClusterURL, via same_cluster_url: whitespace and
# a trailing slash trimmed, case-insensitive, and the same host counts,
# so https://api.memql.localhost/ and https://api.memql.localhost are
# one enrollment. $1 is the driver's file name, $2 the carried flags.
# shellcheck disable=SC2034  # SCOPED_DECISION / ENROLLED_OTHER are read by the drivers' scoped_unpair
function scoped_precheck() {
    local script_name="$1" carried="${2:-}" urls n url matched=no
    enrollment_files_regular || return $?
    urls="$(list_enrolled_cluster_urls "${HOME}/.memql/workers.yaml" "${HOME}/.memql/worker.yaml")" || return $?
    n="$(count_lines "$urls")"
    SCOPED_DECISION="unpair"
    ENROLLED_OTHER="no"
    if [[ "$n" -eq 0 ]]; then
        echo "INFO: no worker enrollment on this machine, so there is nothing to unpair for ${CLUSTER_URL}; removing the worker runtime (as --all-homes)"
        SCOPED_DECISION="full"
        return 0
    fi
    while IFS= read -r url; do
        [[ -n "$url" ]] || continue
        if same_cluster_url "$url" "$CLUSTER_URL"; then
            matched="yes"
        else
            ENROLLED_OTHER="yes"
        fi
    done <<< "$urls"
    if [[ "$matched" != yes ]]; then
        {
            echo "ERROR: no enrollment on this machine matches --cluster=${CLUSTER_URL}; nothing was changed."
            echo "       Enrolled clusters, each with the command that removes just it:"
            print_enrollment_commands "$script_name" "$carried" "$urls"
        } >&2
        return 3
    fi
}

# binary_speaks_scoped_unpair answers whether an installed memql can
# be asked `worker unpair --cluster-url ... --dry-run --json` at all:
# that contract shipped in 0.15.0, and an older build's flag parser
# exits 2 on the unknown flag (or 1 on an unknown verb) after printing
# its usage. $2 is the version read from it, possibly empty -- an
# unreadable version is not held against the binary; the call itself
# then decides (see the drivers' handling of exit 1 / 2).
function binary_speaks_scoped_unpair() {
    local binary="$1" ver="$2"
    [[ -n "$binary" ]] || return 1
    if [[ -n "$ver" && "$(compare_semver "$ver" 0.15.0)" == -1 ]]; then
        echo "INFO: the installed memql v${ver} predates URL-scoped unpair (0.15.0)"
        return 1
    fi
}

# scoped_without_binary is the scoped path when no installed memql can
# split the enrollment -- none is there, or the one there is too old.
# Fine when the requested cluster is the only one: the whole machine
# IS that enrollment, and the worker files go with the runtime exactly
# as --all-homes would. A refusal (4, prerequisite missing) when others
# exist: the shell's text view of the registry counts and names
# clusters, it never decides which of several homes to delete.
# shellcheck disable=SC2034  # OTHER_HOMES is the drivers' global
function scoped_without_binary() {
    if [[ "$ENROLLED_OTHER" == no ]]; then
        echo "INFO: ${CLUSTER_URL} is the only enrollment on this machine; removing it with the worker files (as --all-homes)"
        OTHER_HOMES=0
        return 0
    fi
    echo "ERROR: removing ${CLUSTER_URL} while keeping the other enrollment(s) needs an installed memql 0.15.0 or newer; nothing was changed." >&2
    echo "       Re-run the installer to upgrade it (enrollments are kept), or pass --all-homes to remove every enrollment." >&2
    return 4
}

# ---------------------------------------------------------------
# --dry-run: the plan, printed from read-only probes
# ---------------------------------------------------------------
#
# A dry run resolves everything the real run would -- the scope, the
# shapes, which files and services exist -- and prints what would go,
# what would stay and why, changing nothing. The refusals a real run
# makes BEFORE touching anything (several enrollments and none named,
# a URL nothing matches, an unreadable registry) come out identical,
# with the same exit code; otherwise the plan ends in exit 0. These
# helpers print the per-path lines; the drivers order them the way
# their main() runs.

# plan_path prints one "would remove" line for a present file or
# symlink, and nothing for an absent one.
function plan_path() {
    local path="$1"
    if [[ -e "$path" || -L "$path" ]]; then
        echo "  would remove:  $path"
    fi
}

# plan_binaries_with_mode is remove_binaries_with_mode's read-only twin.
function plan_binaries_with_mode() {
    local mode="$1" dest_dir names name
    dest_dir="$(install_mode_dir "$mode")" || return 1
    names="$(mode_binary_names)" || return 1
    if ! install_mode_has_files "$mode"; then
        echo "  nothing at:    ${dest_dir} (no ${INSTALLED_COMMAND} binary)"
        return 0
    fi
    for name in $names; do
        plan_path "${dest_dir}/${name}"
    done
}

# plan_worker_config is remove_worker_config's read-only twin.
function plan_worker_config() {
    plan_path "${HOME}/.memql/workers.yaml"
    plan_path "${HOME}/.memql/worker.yaml"
}

# plan_tree prints the fence's verdict for one directory, over the same
# normalized spelling the remover uses.
function plan_tree() {
    local dir verdict
    dir="$(normalize_tree_path "$1")"
    verdict="$(tree_removal_verdict "$dir")"
    case "$verdict" in
        absent) ;;
        remove)
            if [[ -L "$dir" ]]; then
                echo "  would remove:  $dir (the symlink only; what it points at is not touched)"
            else
                echo "  would remove:  $dir (recursively)"
            fi ;;
        keep:*) echo "  would keep:    $dir (${verdict#keep:})" ;;
    esac
}

# plan_purge_state is purge_worker_state's read-only twin;
# plan_kept_state is report_kept_state's.
function plan_purge_state() {
    local state_dir="$1"
    plan_path "${HOME}/.memql/policy.yaml"
    plan_tree "$state_dir"
    plan_path "${HOME}/.memql/worker.sock"
    plan_tree "${HOME}/.memql/ollama"
    echo "  would remove:  ${HOME}/.memql (only once nothing else is left in it)"
}

function plan_kept_state() {
    local state_dir="$1" path
    for path in "${HOME}/.memql/policy.yaml" "$state_dir" "${HOME}/.memql/ollama"; do
        if [[ -e "$path" ]]; then
            echo "  would keep:    $path (--purge removes it)"
        fi
    done
}

# print_uninstall_summary is the closing block, the uninstall's
# counterpart to the installers' SUCCESS block: what went, what stayed
# and why, and the one thing this script cannot do. The registration
# row lives on the cluster, and the token that could have spoken for
# this machine has just been deleted -- Fleet -> Machines in MemQL OS
# is where a machine is revoked, and a worker retrying with a dead
# token is the reason to revoke there first. $1 is the run's exit
# code: non-zero with something removed means something else is still
# on disk (PARTIAL); non-zero with nothing removed but something kept
# means a step ran and failed, and Kept says what it left (FAILED);
# non-zero with nothing recorded at all means the run refused before it
# touched anything (REFUSED, and the error above says why). No heading
# claims success over a leftover, and none claims "nothing was changed"
# over a stopped worker.
function print_uninstall_summary() {
    local rc="${1:-0}"
    local heading="SUCCESS: memql-worker uninstalled."
    if [[ "$rc" -ne 0 && -z "$UNINSTALL_REMOVED" && -z "$UNINSTALL_KEPT" ]]; then
        heading="REFUSED: memql-worker was not uninstalled; nothing was changed (see the error above)."
    elif [[ "$rc" -ne 0 && -z "$UNINSTALL_REMOVED" ]]; then
        heading="FAILED: memql-worker uninstall did not complete; nothing was removed, and Kept names what needs you (see the error above)."
    elif [[ "$rc" -ne 0 ]]; then
        heading="PARTIAL: memql-worker uninstalled, with leftovers (see Kept)."
    fi
    echo ""
    echo "================================================================"
    echo "$heading"
    echo ""
    echo "Removed:"
    if [[ -n "$UNINSTALL_REMOVED" ]]; then
        printf '%s' "$UNINSTALL_REMOVED"
    else
        echo "  (nothing)"
    fi
    echo ""
    echo "Kept:"
    if [[ -n "$UNINSTALL_KEPT" ]]; then
        printf '%s' "$UNINSTALL_KEPT"
    else
        echo "  (nothing)"
    fi
    # A memql still resolving on PATH after this is one this script did
    # not install -- the system copy after a --user-local run, or a
    # `go install` -- and saying so beats a person typing `memql` and
    # concluding the uninstall did nothing.
    local other
    if other="$(command -v "$INSTALLED_COMMAND" 2>/dev/null)" && [[ -n "$other" ]]; then
        echo ""
        echo "NOTE: a ${INSTALLED_COMMAND} is still on your PATH at ${other}."
        echo "      This script did not put it there and has not touched it."
    fi
    echo ""
    echo "This machine's registration on the cluster is revoked from MemQL OS"
    echo "(Fleet -> Machines), not from here."
    echo "================================================================"
}

# Fetch into a caller-owned staging directory. Verify the digest and closed
# archive layout before extraction or execution; an archive cannot choose a
# destination path, symlink, installer name, or extra executable.
function fetch_macos_menu() {
    local base="$1" arch="$2" stage="$3"
    local asset expected actual entry
    [[ "$arch" == arm64 || "$arch" == amd64 ]] || return 2
    asset="memql-menubar-darwin-${arch}.tar.gz"
    curl -fsSL "${CURL_NET[@]}" "${CURL_LARGE[@]}" --max-filesize 30000000 "${base}/${asset}" -o "$stage/$asset" || return 5
    curl -fsSL "${CURL_NET[@]}" "${CURL_SMALL[@]}" --max-filesize 1024 "${base}/${asset}.sha256" -o "$stage/checksum" || return 5
    expected="$(awk -v asset="$asset" 'NF == 2 && $2 == asset { print $1 }' "$stage/checksum")"
    [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || { echo "ERROR: invalid menu checksum manifest" >&2; return 3; }
    actual="$(shasum -a 256 "$stage/$asset" | awk '{print $1}')"
    [[ "$expected" == "$actual" ]] || { echo "ERROR: menu archive checksum mismatch" >&2; return 3; }
    tar -tzf "$stage/$asset" > "$stage/entries" || return 3
    tar -tvzf "$stage/$asset" > "$stage/types" || return 3
    if grep -qvE '^[-d]' "$stage/types"; then
        echo "ERROR: menu archive contains a link or special file" >&2
        return 3
    fi
    while IFS= read -r entry; do
        case "$entry" in
            'MemQL Cockpit.app/'|'MemQL Cockpit.app/Contents/'|'MemQL Cockpit.app/Contents/MacOS/'|\
            'MemQL Cockpit.app/Contents/Resources/'|'MemQL Cockpit.app/Contents/_CodeSignature/'|\
            'MemQL Cockpit.app/Contents/Info.plist'|'MemQL Cockpit.app/Contents/MacOS/MemQLCockpit'|\
            'MemQL Cockpit.app/Contents/Resources/mark.svg'|'MemQL Cockpit.app/Contents/Resources/MemQL.icns'|'MemQL Cockpit.app/Contents/_CodeSignature/CodeResources'|\
            scripts/|scripts/lib/|scripts/macos/|scripts/lib/capability.sh|scripts/macos/install-menubar.sh|scripts/macos/launchagent.sh) ;;
            *) echo "ERROR: unexpected menu archive path: $entry" >&2; return 3 ;;
        esac
    done < "$stage/entries"
    mkdir -p "$stage/unpacked"
    tar -xzf "$stage/$asset" -C "$stage/unpacked" || return 5
    [[ -x "$stage/unpacked/MemQL Cockpit.app/Contents/MacOS/MemQLCockpit" && -f "$stage/unpacked/scripts/macos/install-menubar.sh" && -f "$stage/unpacked/scripts/lib/capability.sh" ]] || return 3
}

# The full app carries the permission-bearing worker plus an embedded menu.
# Keep a closed file list; never extract links, extra tools, or traversal paths.
function fetch_macos_app() {
    local base="$1" arch="$2" stage="$3" asset expected actual entry normalized
    [[ "$arch" == arm64 || "$arch" == amd64 ]] || return 2
    asset="memql-app-darwin-${arch}.tar.gz"
    curl -fsSL "${CURL_NET[@]}" "${CURL_LARGE[@]}" --max-filesize 120000000 "${base}/${asset}" -o "$stage/$asset" || return 5
    curl -fsSL "${CURL_NET[@]}" "${CURL_SMALL[@]}" --max-filesize 1024 "${base}/${asset}.sha256" -o "$stage/checksum" || return 5
    expected="$(awk -v asset="$asset" 'NF == 2 && $2 == asset { print $1 }' "$stage/checksum")"
    [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || return 3
    actual="$(shasum -a 256 "$stage/$asset" | awk '{print $1}')"
    [[ "$expected" == "$actual" ]] || { echo "ERROR: MemQL app checksum mismatch" >&2; return 3; }
    tar -tzf "$stage/$asset" > "$stage/entries" || return 3
    tar -tvzf "$stage/$asset" > "$stage/types" || return 3
    if grep -qvE '^[-d]' "$stage/types"; then echo "ERROR: app archive contains a link or special file" >&2; return 3; fi
    while IFS= read -r entry; do
        normalized="$entry"
        case "$entry" in
            'MemQL.app/Contents/Library/LoginItems/MemQL Menu.app/'*) normalized="menu/${entry#MemQL.app/Contents/Library/LoginItems/MemQL Menu.app/}" ;;
            'MemQL.app/'*) normalized="app/${entry#MemQL.app/}" ;;
        esac
        case "$normalized" in
            app/|app/Contents/|app/Contents/MacOS/|app/Contents/Resources/|app/Contents/Library/|app/Contents/Library/LoginItems/|\
            app/Contents/Info.plist|app/Contents/MacOS/MemQL|app/Contents/Resources/MemQL.icns|app/Contents/_CodeSignature/|app/Contents/_CodeSignature/CodeResources|\
            menu/|menu/Contents/|menu/Contents/MacOS/|menu/Contents/Resources/|menu/Contents/Info.plist|menu/Contents/MacOS/MemQLCockpit|\
            menu/Contents/Resources/mark.svg|menu/Contents/Resources/MemQL.icns|menu/Contents/_CodeSignature/|menu/Contents/_CodeSignature/CodeResources|\
            scripts/|scripts/lib/|scripts/macos/|scripts/lib/capability.sh|scripts/macos/install-app-files.sh|scripts/macos/activate-app.sh|scripts/macos/install-menubar.sh|scripts/macos/launchagent.sh) ;;
            *) echo "ERROR: unexpected app archive path: $entry" >&2; return 3 ;;
        esac
    done < "$stage/entries"
    mkdir -p "$stage/unpacked"
    tar -xzf "$stage/$asset" -C "$stage/unpacked" || return 5
    [[ -x "$stage/unpacked/MemQL.app/Contents/MacOS/MemQL" && -f "$stage/unpacked/scripts/macos/install-app-files.sh" && -f "$stage/unpacked/scripts/macos/activate-app.sh" && -f "$stage/unpacked/scripts/macos/install-menubar.sh" && -f "$stage/unpacked/scripts/lib/capability.sh" ]] || return 3
}

# Compare designated requirements, not paths, display names or version strings.
# A stable signed update keeps the same requirement and needs no permission reset.
function macos_signing_transition() {
    local before="$1" after="$2"
    if [[ -z "$before" || -z "$after" ]]; then echo unknown
    elif [[ "$before" == "$after" ]]; then echo unchanged
    else echo changed
    fi
}

function macos_bundle_requirement() {
    codesign -d -r- "$1" 2>&1 | sed -n -E 's/^#? ?(designated => .*)$/\1/p'
}

# Privacy decisions are bundle-wide within this user account. An alternate
# standard installation sharing the identifier prevents scoped grant cleanup.
function macos_privacy_scope_unique() {
    local alternate="$1" identifier
    [[ -d "$alternate" ]] || return 0
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$alternate/Contents/Info.plist" 2>/dev/null || true)"
    case "$identifier" in
        com.znasllc.memql-worker|com.znasllc.memql-cockpit-menubar)
            echo "ERROR: another MemQL installation at $alternate shares app permissions; no reset performed. App files retained for explicit cleanup." >&2
            return 3 ;;
    esac
}

# Obsolete ad-hoc approvals cannot authorize a replacement signature. Reset only
# the current user's known worker bundle; never touch other apps or shared installs.
function repair_changed_macos_permissions() {
    local installed="$1" incoming="$2" alternate="$3" before after service
    [[ -d "$installed" ]] || return 0
    [[ ! -L "$installed" ]] || return 3
    [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$installed/Contents/Info.plist")" == com.znasllc.memql-worker ]] || return 3
    [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$incoming/Contents/Info.plist")" == com.znasllc.memql-worker ]] || return 3
    codesign --verify --deep --strict "$incoming" >&2 || return 3
    before="$(macos_bundle_requirement "$installed" || true)"
    after="$(macos_bundle_requirement "$incoming" || true)"
    [[ "$(macos_signing_transition "$before" "$after")" == changed ]] || return 0
    [[ "$EUID" -ne 0 ]] || { echo "ERROR: run without sudo to keep permission repair scoped to your account" >&2; return 3; }
    macos_privacy_scope_unique "$alternate" || return $?
    for service in Accessibility ScreenCapture; do
        tccutil reset "$service" com.znasllc.memql-worker >&2 || {
            echo "ERROR: could not clear obsolete MemQL permissions; retry installation before approving the new app" >&2
            return 5
        }
    done
    echo "NOTICE: Obsolete MemQL approvals cleared for this account. Approve the new MemQL app when prompted."
}
