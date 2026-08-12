package lib

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// elevate_test.go -- znasllc-io/memql#3562.
//
// scripts/lib/elevate.sh is how a capability script with NO TERMINAL gets root:
// it points $SUDO_ASKPASS at a small dialog program, which is the path sudo
// itself takes when it needs a password and finds no tty. That turns the three
// privileged install steps (the hosts block, the CA trust store, the NSS tools)
// from "stop and hand the operator a command" into a native password prompt.
//
// THE ASSERTION THAT MATTERS MOST IS ABOUT THESE TESTS, NOT ABOUT THE CODE.
// A suite that can reach the real sudo would, on a developer's own desktop,
// interrupt them with a password dialog -- and on a CI runner with NOPASSWD
// sudo it would silently perform privileged writes for real. So every case
// below builds its environment explicitly: PATH's `sudo` is a stub that records
// its argv, and DISPLAY is empty unless the case is about a dialog. Nothing here
// can touch the machine it runs on.

//=============================================================================
// HARNESS
//=============================================================================

func elevateRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// elevateWorld is one hermetic machine: a bin directory that comes first on
// PATH, and whatever display/dialog programs the case wants to exist.
type elevateWorld struct {
	bin string
	log string
	env []string
}

func newElevateWorld(t *testing.T) *elevateWorld {
	t.Helper()
	base := t.TempDir()
	w := &elevateWorld{
		bin: filepath.Join(base, "bin"),
		log: filepath.Join(base, "calls.log"),
	}
	if err := os.MkdirAll(w.bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return w
}

// stub writes an executable of the given name into the world's bin directory.
// $1 body runs with $STUB_LOG pointing at the call log.
func (w *elevateWorld) stub(t *testing.T, name, body string) {
	t.Helper()
	script := "#!/usr/bin/env bash\nprintf '%s %s\\n' " + name + " \"$*\" >> \"$STUB_LOG\"\n" + body
	if err := os.WriteFile(filepath.Join(w.bin, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// eval sources elevate.sh and runs a snippet against it. PATH is the world's
// bin directory FIRST, so a stub shadows the real program; DISPLAY and
// WAYLAND_DISPLAY are empty unless the case set them.
func (w *elevateWorld) eval(t *testing.T, snippet string) (string, int) {
	t.Helper()
	lib := filepath.Join(elevateRepoRoot(t), "scripts", "lib", "elevate.sh")
	cmd := exec.Command("bash", "-c", "set -uo pipefail; source "+lib+"\n"+snippet)
	cmd.Stdin = nil
	cmd.Env = append([]string{
		"PATH=" + w.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"STUB_LOG=" + w.log,
		"DISPLAY=",
		"WAYLAND_DISPLAY=",
	}, w.env...)

	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running snippet: %v\n%s", err, out)
	}
	return string(out), code
}

func (w *elevateWorld) calls(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.log)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read call log: %v", err)
	}
	return string(b)
}

// sudoNeedsAPassword is the ordinary desktop: sudo exists, refuses -n, and runs
// what it is given once a password has been supplied.
const sudoNeedsAPassword = `
if [ "$1" = "-n" ]; then exit 1; fi
if [ "$1" = "-A" ]; then shift; fi
exec "$@"
`

//=============================================================================
// WHICH METHOD THIS MACHINE HAS
//=============================================================================

func TestElevateMethodIsNoneWithoutADisplay(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("osascript needs no DISPLAY, so macOS always has a dialog")
	}
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	w.stub(t, "zenity", "exit 0")

	// The one that is easy to get wrong: a dialog program being INSTALLED is
	// not a screen to show it on. zenity is on plenty of servers nobody is
	// sitting at, and offering a prompt there hangs the install until it times
	// out instead of failing with something the operator can act on.
	out, _ := w.eval(t, `elevate_method`)
	if strings.TrimSpace(out) != "none" {
		t.Errorf("method = %q, want none -- there is no display to draw on", strings.TrimSpace(out))
	}
}

func TestElevateMethodIsDialogOnADesktop(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	w.stub(t, "zenity", "exit 0")
	w.env = append(w.env, "DISPLAY=:0")

	out, _ := w.eval(t, `elevate_method`)
	if strings.TrimSpace(out) != "dialog" {
		t.Errorf("method = %q, want dialog: %s", strings.TrimSpace(out), out)
	}
}

func TestElevateMethodIsFreeWhenSudoDoesNotAsk(t *testing.T) {
	w := newElevateWorld(t)
	// NOPASSWD, or a cached authentication: -n succeeds.
	w.stub(t, "sudo", "if [ \"$1\" = \"-n\" ]; then exit 0; fi\nif [ \"$1\" = \"-A\" ]; then shift; fi\nexec \"$@\"")

	out, _ := w.eval(t, `elevate_method`)
	if strings.TrimSpace(out) != "free" {
		t.Errorf("method = %q, want free: %s", strings.TrimSpace(out), out)
	}
}

func TestElevateMethodIsNoneWithoutSudo(t *testing.T) {
	w := newElevateWorld(t)
	w.env = append(w.env, "DISPLAY=:0")
	// A PATH with nothing on it at all: no sudo to ask with, dialog or not.
	w.env = append(w.env, "PATH="+w.bin)

	out, _ := w.eval(t, `elevate_method`)
	if strings.TrimSpace(out) != "none" {
		t.Errorf("method = %q, want none: %s", strings.TrimSpace(out), out)
	}
}

//=============================================================================
// WHOEVER OWNS THE RUN OWNS THE ASKING (memql#3586)
//=============================================================================
//
// The wizard has a password box of its own and asks once. A script that ALSO
// draws a desktop dialog puts a second, differently-shaped question in front of
// someone who has already answered -- which is what an install did, three times,
// while the uninstall asked once. So the wizard marks the child environment, and
// these cases pin what the mark means.

func TestElevateDrawsNoDialogWhenTheCallerOwnsTheAsking(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	w.stub(t, "zenity", "exit 0")
	// A machine that COULD draw a dialog -- a desktop with zenity installed --
	// which is the only configuration where the marker changes anything.
	w.env = append(w.env, "DISPLAY=:0", "MEMQL_ELEVATE_DIALOG=never")

	out, _ := w.eval(t, `elevate_method`)
	if strings.TrimSpace(out) != "none" {
		t.Errorf("method = %q, want none -- the caller said it owns the asking, so this file must not\n"+
			"offer a prompt of its own. The step refuses with the terminal remedy it already carries,\n"+
			"which is one answer rather than a second prompt.", strings.TrimSpace(out))
	}

	if _, code := w.eval(t, `elevate_begin "update the hosts file" && echo "begin succeeded"`); code == 0 {
		t.Errorf("elevate_begin built a helper anyway, so a dialog would open")
	}
	if strings.Contains(w.calls(t), "zenity") {
		t.Errorf("a dialog program was invoked despite the caller owning the asking:\n%s", w.calls(t))
	}
}

// THE ORDINARY CASE MUST NOT CHANGE. A human running this script by hand still
// gets the desktop dialog; the marker is the wizard saying it has already asked,
// not a decision that dialogs are wrong.
func TestElevateStillDrawsADialogForAHumanRunningItByHand(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	w.stub(t, "zenity", "exit 0")
	w.env = append(w.env, "DISPLAY=:0")

	out, _ := w.eval(t, `elevate_method`)
	if strings.TrimSpace(out) != "dialog" {
		t.Errorf("method = %q, want dialog with no marker set: %s", strings.TrimSpace(out), out)
	}
}

// An INHERITED helper still wins. The marker and SUDO_ASKPASS arrive together on
// the run where a password WAS collected, and answering through the agent is the
// whole point -- reading the marker as "never elevate" would break the case it
// was added to make consistent.
func TestElevateHonoursAnInheritedHelperEvenWhenDialogsAreOff(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	helper := filepath.Join(w.bin, "inherited-askpass")
	if err := os.WriteFile(helper, []byte("#!/usr/bin/env bash\nprintf 'from-the-wizard\\n'\n"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	w.env = append(w.env, "DISPLAY=:0", "MEMQL_ELEVATE_DIALOG=never", "SUDO_ASKPASS="+helper)

	out, _ := w.eval(t, `elevate_method`)
	if strings.TrimSpace(out) != "inherited" {
		t.Errorf("method = %q, want inherited -- the wizard collected a password and served it; the\n"+
			"marker turns off dialogs THIS file would draw, not the caller's own answer", strings.TrimSpace(out))
	}
}

// Being root, or having a sudo that never asks, is unaffected: there is nothing
// to ask for, so there is no asking to own.
func TestElevateMarkerDoesNotDisturbAFreeSudo(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", "if [ \"$1\" = \"-n\" ]; then exit 0; fi\nif [ \"$1\" = \"-A\" ]; then shift; fi\nexec \"$@\"")
	w.env = append(w.env, "DISPLAY=:0", "MEMQL_ELEVATE_DIALOG=never")

	out, _ := w.eval(t, `elevate_method`)
	if strings.TrimSpace(out) != "free" {
		t.Errorf("method = %q, want free: %s", strings.TrimSpace(out), out)
	}
}

// The refusal has to say the true thing. "This machine has no way to ask for a
// password without a terminal" is wrong about a desktop with zenity on it: the
// reason is that the installer is driving and holds no password. An operator
// sent to diagnose their display when the actual remedy is to re-run and type
// their password has been misdirected by the error message.
func TestElevateExplainsWhyItCannotAskWhenTheCallerOwnsTheAsking(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	w.stub(t, "zenity", "exit 0")
	w.env = append(w.env, "DISPLAY=:0", "MEMQL_ELEVATE_DIALOG=never")

	out, code := w.eval(t, `elevate_no_ask_reason`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, "no way to ask") {
		t.Errorf("the reason blames the machine on a desktop that has a dialog program installed: %q", out)
	}
	if !strings.Contains(out, "installer") {
		t.Errorf("the reason does not name what is actually withholding the prompt: %q", out)
	}
}

func TestElevateBlamesTheMachineWhenTheMachineIsTheReason(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("osascript needs no DISPLAY, so macOS always has a dialog")
	}
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)

	out, code := w.eval(t, `elevate_no_ask_reason`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "no way to ask") {
		t.Errorf("a headless machine with no dialog program is exactly the terminal-handoff case, and\n"+
			"the reason should still say so: %q", out)
	}
}

//=============================================================================
// THE HELPER
//=============================================================================

func TestElevateBeginExportsAnAskpassHelperThatAsksGraphically(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	w.stub(t, "zenity", "printf 'hunter2\\n'")
	w.env = append(w.env, "DISPLAY=:0")

	// The helper's OWN OUTPUT is what sudo reads as the password, so run it and
	// check the bytes rather than trusting that a file was written.
	out, code := w.eval(t, `
elevate_begin "write the hosts file" || { echo "begin refused"; exit 1; }
[ -n "${SUDO_ASKPASS:-}" ] || { echo "SUDO_ASKPASS not exported"; exit 1; }
[ -x "$SUDO_ASKPASS" ] || { echo "helper is not executable"; exit 1; }
"$SUDO_ASKPASS"
helper="$SUDO_ASKPASS"
elevate_end
[ -e "$helper" ] && { echo "helper survived elevate_end"; exit 1; }
[ -n "${SUDO_ASKPASS:-}" ] && { echo "SUDO_ASKPASS survived elevate_end"; exit 1; }
exit 0
`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "hunter2") {
		t.Errorf("the helper did not produce the dialog's answer on stdout, which is the only\n"+
			"thing sudo reads from it: %s", out)
	}
	if !strings.Contains(w.calls(t), "zenity") {
		t.Errorf("the dialog program was never invoked:\n%s", w.calls(t))
	}
}

// The purpose reaches the operator inside a box that has interrupted them. Three
// privileged things happen during one install, and "memQL needs your password"
// with no object leaves them guessing which.
func TestElevateHelperNamesWhatThePasswordIsFor(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	w.stub(t, "zenity", "exit 0")
	w.env = append(w.env, "DISPLAY=:0")

	// Asserted on what the DIALOG PROGRAM RECEIVES rather than on the helper's
	// source, because the source is shell-quoted -- and a test that reads the
	// quoted form is testing the quoting, not the sentence the operator sees.
	out, code := w.eval(t, `elevate_begin "trust the local certificate authority" && "$SUDO_ASKPASS"`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(w.calls(t), "trust the local certificate authority") {
		t.Errorf("the prompt does not say what it is for:\n%s", w.calls(t))
	}
}

func TestElevateBeginRefusesWhenThereIsNoWayToAsk(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("osascript needs no DISPLAY, so macOS always has a dialog")
	}
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)

	out, code := w.eval(t, `elevate_begin "do something" && echo "begin succeeded"`)
	if code == 0 && strings.Contains(out, "begin succeeded") {
		t.Errorf("elevate_begin claimed it could ask on a machine with no dialog and no display: %s", out)
	}
}

//=============================================================================
// RUNNING SOMETHING
//=============================================================================

func TestElevateRunGoesThroughSudoWithTheAskpassPath(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)
	w.stub(t, "zenity", "printf 'x\\n'")
	w.env = append(w.env, "DISPLAY=:0")

	out, code := w.eval(t, `elevate_begin "test" && elevate_run true; elevate_end`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	calls := w.calls(t)
	if !strings.Contains(calls, "sudo -A true") {
		t.Errorf("expected `sudo -A true` -- -A forces the askpass path whether or not a\n"+
			"terminal happens to exist, so a developer running this by hand and the wizard\n"+
			"running it get identical behaviour.\ncalls:\n%s", calls)
	}
}

// No shell, anywhere: an argument containing a space or a `;` stays one inert
// argv element rather than something a shell re-parses. The same promise the
// TypeScript and Go runners make about capability-script params.
func TestElevateRunPassesArgumentsWithoutAShell(t *testing.T) {
	w := newElevateWorld(t)
	w.stub(t, "sudo", "if [ \"$1\" = \"-n\" ]; then exit 0; fi\nexec \"$@\"")
	marker := filepath.Join(t.TempDir(), "should not exist")

	// If any shell re-parsed this, `touch` would run and the file would appear.
	out, _ := w.eval(t, `elevate_run printf '%s\n' 'a; touch `+marker+`'`)
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("an argument was re-parsed by a shell: %s got created", marker)
	}
	if !strings.Contains(out, "a; touch") {
		t.Errorf("the argument did not arrive intact: %s", out)
	}
}

func TestElevateRunReportsWhenItCannotElevate(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("osascript needs no DISPLAY, so macOS always has a dialog")
	}
	w := newElevateWorld(t)
	w.stub(t, "sudo", sudoNeedsAPassword)

	_, code := w.eval(t, `elevate_run true`)
	if code == 0 {
		t.Errorf("elevate_run reported success on a machine that cannot elevate")
	}
}

//=============================================================================
// THE SAFETY RAIL ON THE TESTS THEMSELVES
//=============================================================================

// Every case above runs through elevateWorld.eval, which pins PATH to a stub
// directory and blanks DISPLAY. A case that built its own exec.Command could
// reach the real sudo -- and on a desktop that means a password dialog in front
// of whoever ran `go test`, while on a NOPASSWD CI runner it means a real
// privileged write. Assert the harness is the only entry point.
func TestElevateTestsNeverReachTheRealSudo(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(elevateRepoRoot(t), "scripts", "lib", "elevate_test.go"))
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}
	needle := "exec." + "Command("
	if n := strings.Count(string(b), needle); n != 1 {
		t.Errorf("exec.Command appears %d times, want 1 (only elevateWorld.eval)", n)
	}
}
