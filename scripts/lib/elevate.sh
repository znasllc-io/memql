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
# already implements. It also means MemQL never re-implements any tool's
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
# desktop's own prompt is preferred over anything MemQL might draw, in the order
# a desktop is likely to have installed one.
# elevate_inherited_helper -- an askpass helper our CALLER already provided.
#
# The wizard asks for the password ONCE and serves it to every step over a unix
# socket (editors/vscode/src/install/sudoAgent.ts), because sudo's own
# authentication cache is keyed by parent process and every step is its own
# process -- so without this the operator is asked once per privileged step.
#
# Honoured ahead of everything below: a caller that has already arranged how
# sudo will ask knows more than this file does, and a script that built its own
# dialog on top would put a second prompt in front of someone who has already
# answered.
function elevate_inherited_helper() {
    [[ -n "${SUDO_ASKPASS:-}" && -x "${SUDO_ASKPASS}" ]]
}

# elevate_caller_owns_the_asking -- the caller has told us it is the one asking.
#
# WHOEVER OWNS THE RUN OWNS THE ASKING (memql#3586). The install wizard has a
# password box of its own and asks ONCE; a script that also draws a desktop
# dialog puts a second, differently-shaped question in front of someone who has
# already answered. That is what an install did -- three zenity dialogs -- while
# the uninstall asked once in the editor.
#
# THIS IS THE INVARIANT, NOT THE SYMPTOM. The symptom was a probe in the
# extension that skipped the agent; fixing it alone would leave consistency
# resting on the agent always being created, and any future path that skipped it
# would quietly revert to an OS popup. With the marker set, this file offers
# nothing of its own: `elevate_method` answers `none` unless the caller supplied
# a helper, and the step refuses with the terminal remedy it already carries.
#
# A HUMAN RUNNING THE SCRIPT BY HAND SETS NOTHING and still gets the dialog.
function elevate_caller_owns_the_asking() {
    [[ "${MEMQL_ELEVATE_DIALOG:-}" == "never" ]]
}

function elevate_dialog_program() {
    # Checked before anything else: on macOS osascript always exists, so a later
    # check would never be reached there.
    elevate_caller_owns_the_asking && return 0
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
    # An inherited helper beats a dialog of our own, and works where no dialog
    # could -- a headless run whose caller has some other way to answer.
    if elevate_inherited_helper; then printf 'inherited\n'; return; fi
    if [[ -n "$(elevate_dialog_program)" ]]; then printf 'dialog\n'; return; fi
    printf 'none\n'
}

# elevate_available -- true when something above will work.
function elevate_available() {
    [[ "$(elevate_method)" != "none" ]]
}

# elevate_no_ask_reason -- why root cannot be reached, as the second half of a
# caller's refusal message.
#
# TWO DIFFERENT PROBLEMS WEAR THE SAME `none` (memql#3586), and they ask the
# operator for different things. A headless machine needs a terminal. A wizard
# run holds no password because the operator dismissed the box -- on a desktop
# that has a dialog program installed, where "this machine has no way to ask for
# a password without a terminal" is simply untrue and sends them to diagnose
# their display instead of re-running and typing their password.
function elevate_no_ask_reason() {
    if elevate_caller_owns_the_asking; then
        printf 'the MemQL installer is driving this run and holds no password for it -- re-run and enter your password when it asks, or run the command below in a terminal\n'
        return 0
    fi
    printf 'this machine has no way to ask for a password without a terminal\n'
}

#=============================================================================
# THE HELPER
#=============================================================================

# elevate_begin <purpose> -- prepare elevation, exporting SUDO_ASKPASS when a
# dialog is needed. Returns non-zero when this machine cannot elevate at all.
#
# <purpose> is one clause completing "MemQL needs your password to ___". It
# reaches the operator inside a dialog box that has interrupted them, so it has
# to say which of several privileged things is being asked for.
#
# EXPORTED, NOT PASSED. sudo reads $SUDO_ASKPASS from the environment, and the
# programs that need it most (mkcert) invoke sudo themselves with arguments
# MemQL does not control. Setting the variable is the entire integration.
function elevate_begin() {
    local purpose="$1" program prompt
    case "$(elevate_method)" in
        none) return 1 ;;
        # inherited: SUDO_ASKPASS is already set and already works. Nothing to
        # build and nothing to clean up -- the caller owns both.
        root|free|inherited) return 0 ;;
    esac

    program="$(elevate_dialog_program)"
    prompt="MemQL needs your password to ${purpose}."
    _ELEVATE_HELPER="$(mktemp "${TMPDIR:-/tmp}/memql-askpass.XXXXXX")"
    chmod 700 "$_ELEVATE_HELPER"

    if [[ "$(basename "$program")" == "osascript" ]]; then
        # One script, two statements: the dialog, then the field out of its
        # result. `with hidden answer` is what makes it a password field.
        cat >"$_ELEVATE_HELPER" <<EOF
#!/usr/bin/env bash
exec $(printf '%q' "$program") \\
  -e $(printf '%q' "display dialog \"${prompt}\" default answer \"\" with title \"MemQL installer\" with hidden answer") \\
  -e 'text returned of result'
EOF
    elif [[ "$(basename "$program")" == "kdialog" ]]; then
        cat >"$_ELEVATE_HELPER" <<EOF
#!/usr/bin/env bash
exec $(printf '%q' "$program") --password $(printf '%q' "$prompt") --title 'MemQL installer'
EOF
    else
        cat >"$_ELEVATE_HELPER" <<EOF
#!/usr/bin/env bash
exec $(printf '%q' "$program") --password --title=$(printf '%q' "MemQL installer -- ${prompt}")
EOF
    fi

    chmod 700 "$_ELEVATE_HELPER"
    export SUDO_ASKPASS="$_ELEVATE_HELPER"
    return 0
}

# elevate_end -- remove the helper. Safe to call when there is none.
function elevate_end() {
    # ONLY ever removes a helper THIS file created. An inherited one belongs to
    # the caller and is still needed by the steps after this one -- unsetting it
    # would put the second password prompt back.
    if [[ -z "$_ELEVATE_HELPER" ]]; then
        return 0
    fi
    [[ -f "$_ELEVATE_HELPER" ]] && rm -f "$_ELEVATE_HELPER"
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
        inherited) printf 'using the password you already gave the installer\n' ;;
        dialog) printf 'a password dialog will open on your desktop\n' ;;
        *)      elevate_no_ask_reason ;;
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
