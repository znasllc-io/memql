# scripts/lib/elevate.sh
# ======================
#
# Getting root from a process that has no terminal.
#
# THE PROBLEM. Three things an install has to do need root: writing the marked
# block in /etc/hosts, putting the local CA in the system trust store, and
# installing the NSS tools browsers read their certificates from. A capability
# script is non-interactive by contract and, when the wizard runs it, has no
# terminal attached at all -- so `sudo` cannot ask for a password and dies with
#
#     sudo: a terminal is required to read the password; either use the -S
#     option to read from standard input or configure an askpass helper
#
# That is the whole reason the installer used to stop and hand the operator a
# command to run themselves (memql#3560).
#
# THE MECHANISM, which is sudo's own and not a trick. When sudo needs a password
# and finds NO TERMINAL, it runs the program named by $SUDO_ASKPASS and reads
# the password from that program's stdout. `-A` forces the same path when a
# terminal does exist. So the fix is not to give sudo a terminal; it is to give
# it a helper that asks graphically -- a native password dialog on the desktop
# the operator is already sitting in front of.
#
# WHY THIS BEATS pkexec AND `osascript ... with administrator privileges`.
# Both of those run the whole command AS ROOT. That is fine for a hosts-file
# write and actively wrong for mkcert, which resolves CAROOT from the running
# user's home: as root it would build a SECOND certificate authority under
# /root, trusted for a user who is not the one running a browser. With an
# askpass helper mkcert keeps running as the operator and only the trust-store
# write it shells out to becomes root -- which is exactly the split mkcert
# already implements. It also means memQL never re-implements any tool's
# privileged half; it hands the tool a way to ask.
#
# The helper is a small script this file writes at call time. It carries NO
# secret -- it is a dialog invoker, and the password it prints goes down a pipe
# to sudo and nowhere else. It is created 0700, used, and removed.
#
# WHEN THERE IS NO WAY TO ASK -- over SSH, on a headless box, in CI, on a
# desktop with no dialog program -- this reports so, and the caller falls back
# to what it did before: refuse with exit 4 and hand back the exact command to
# run in a terminal. Automatic where it can be, honest where it cannot.
#
# Refs: #3562 #3560 #2221

# Guard against double-sourcing: these are function definitions, and a second
# source would be harmless but the state below would be reset mid-run.
[[ -n "${_MEMQL_ELEVATE_SOURCED:-}" ]] && return 0
_MEMQL_ELEVATE_SOURCED=1

# The helper this run created, so it can be removed again. Empty when none.
_ELEVATE_HELPER=""

#=============================================================================
# CAPABILITY PROBES
#=============================================================================

# elevate_is_root -- already root, so nothing to ask.
function elevate_is_root() {
    [[ "${EUID:-$(id -u)}" -eq 0 ]]
}

# elevate_sudo_is_free -- sudo will run without asking, because this user is
# NOPASSWD or a recent authentication is still cached. Checked with -n so the
# probe itself can never block.
function elevate_sudo_is_free() {
    command -v sudo &>/dev/null && sudo -n true 2>/dev/null
}

# elevate_has_display -- there is a graphical session to put a dialog on.
#
# A dialog program being INSTALLED is not the same as there being a screen to
# show it on: zenity exists on plenty of servers nobody is sitting at. Both
# have to be true.
function elevate_has_display() {
    [[ -n "${DISPLAY:-}" || -n "${WAYLAND_DISPLAY:-}" ]]
}

# elevate_dialog_program -- prints the password-dialog program for this
# machine, or nothing.
#
# macOS always has osascript, and its dialog does not need DISPLAY. On Linux the
# desktop's own prompt is preferred over anything memQL might draw, in the order
# a desktop is likely to have installed one.
function elevate_dialog_program() {
    if [[ "$(uname -s 2>/dev/null || true)" == "Darwin" ]]; then
        command -v osascript 2>/dev/null
        return
    fi
    elevate_has_display || return 0
    local candidate
    for candidate in zenity kdialog; do
        if command -v "$candidate" &>/dev/null; then
            command -v "$candidate"
            return
        fi
    done
}

# elevate_method -- how this machine can reach root without a terminal:
#
#   root    already there
#   free    sudo runs without asking (NOPASSWD, or a cached authentication)
#   dialog  sudo can ask through a graphical password prompt
#   none    it cannot; the caller must fall back to a terminal handoff
function elevate_method() {
    if elevate_is_root; then printf 'root\n'; return; fi
    if ! command -v sudo &>/dev/null; then printf 'none\n'; return; fi
    if elevate_sudo_is_free; then printf 'free\n'; return; fi
    if [[ -n "$(elevate_dialog_program)" ]]; then printf 'dialog\n'; return; fi
    printf 'none\n'
}

# elevate_available -- true when something above will work.
function elevate_available() {
    [[ "$(elevate_method)" != "none" ]]
}

#=============================================================================
# THE HELPER
#=============================================================================

# elevate_begin <purpose> -- prepare elevation, exporting SUDO_ASKPASS when a
# dialog is needed. Returns non-zero when this machine cannot elevate at all.
#
# <purpose> is one clause completing "memQL needs your password to ___". It
# reaches the operator inside a dialog box that has interrupted them, so it has
# to say which of several privileged things is being asked for.
#
# EXPORTED, NOT PASSED. sudo reads $SUDO_ASKPASS from the environment, and the
# programs that need it most (mkcert) invoke sudo themselves with arguments
# memQL does not control. Setting the variable is the entire integration.
function elevate_begin() {
    local purpose="$1" program prompt
    case "$(elevate_method)" in
        none) return 1 ;;
        root|free) return 0 ;;
    esac

    program="$(elevate_dialog_program)"
    prompt="memQL needs your password to ${purpose}."
    _ELEVATE_HELPER="$(mktemp "${TMPDIR:-/tmp}/memql-askpass.XXXXXX")"
    chmod 700 "$_ELEVATE_HELPER"

    if [[ "$(basename "$program")" == "osascript" ]]; then
        # One script, two statements: the dialog, then the field out of its
        # result. `with hidden answer` is what makes it a password field.
        cat >"$_ELEVATE_HELPER" <<EOF
#!/usr/bin/env bash
exec $(printf '%q' "$program") \\
  -e $(printf '%q' "display dialog \"${prompt}\" default answer \"\" with title \"memQL installer\" with hidden answer") \\
  -e 'text returned of result'
EOF
    elif [[ "$(basename "$program")" == "kdialog" ]]; then
        cat >"$_ELEVATE_HELPER" <<EOF
#!/usr/bin/env bash
exec $(printf '%q' "$program") --password $(printf '%q' "$prompt") --title 'memQL installer'
EOF
    else
        cat >"$_ELEVATE_HELPER" <<EOF
#!/usr/bin/env bash
exec $(printf '%q' "$program") --password --title=$(printf '%q' "memQL installer -- ${prompt}")
EOF
    fi

    chmod 700 "$_ELEVATE_HELPER"
    export SUDO_ASKPASS="$_ELEVATE_HELPER"
    return 0
}

# elevate_end -- remove the helper. Safe to call when there is none.
function elevate_end() {
    if [[ -n "$_ELEVATE_HELPER" && -f "$_ELEVATE_HELPER" ]]; then
        rm -f "$_ELEVATE_HELPER"
    fi
    _ELEVATE_HELPER=""
    unset SUDO_ASKPASS
}

#=============================================================================
# RUNNING SOMETHING
#=============================================================================

# elevate_run <argv...> -- run a command as root, or return non-zero if this
# machine cannot. The command's own exit status is returned when it ran.
#
# NO SHELL, ANYWHERE. Each element stays its own argv element the whole way
# down, so a path containing a space or a `;` is an inert string rather than
# something a shell re-parses -- the same promise the TypeScript and Go runners
# make about capability-script params.
function elevate_run() {
    local method
    method="$(elevate_method)"
    case "$method" in
        root)  "$@"; return $? ;;
        free)  sudo "$@"; return $? ;;
        none)  return 127 ;;
    esac
    # -A: there may or may not be a terminal (the wizard has none, a developer
    # running this by hand does). Forcing the askpass path makes the behaviour
    # identical either way, which is what stops "it worked when I ran it".
    sudo -A "$@"
}

# elevate_explain -- one sentence about how this machine will be asked, for the
# log above whatever is about to happen.
function elevate_explain() {
    case "$(elevate_method)" in
        root)   printf 'already running as root\n' ;;
        free)   printf 'sudo runs without a password here\n' ;;
        dialog) printf 'a password dialog will open on your desktop\n' ;;
        *)      printf 'this machine has no way to ask for a password without a terminal\n' ;;
    esac
}

#=============================================================================
# THE TEST SEAM
#=============================================================================
#
# A test suite that can pop a real password dialog on the runner's desktop is
# not one anybody can run. `go test ./scripts/...` on a developer's own machine
# would otherwise interrupt them with a prompt for their own password, and a CI
# runner with NOPASSWD sudo would silently perform the privileged write for
# real -- the two worst outcomes available.
#
# Tests therefore build their environment explicitly, and this is what they aim
# the probes at: a PATH whose `sudo` is a stub, and no DISPLAY. Nothing here is
# a production knob; the functions above read only the environment sudo itself
# reads.
