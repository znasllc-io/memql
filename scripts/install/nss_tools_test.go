package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nss_tools_test.go -- znasllc-io/memql#3566.
//
// scripts/install/nss-tools.sh puts `certutil` on the machine, because without
// it mkcert cannot write the store Firefox and Chrome read and the install ends
// with a front door no browser trusts (memql#3560 turned that silent success
// into a refusal; this removes the refusal).
//
// NOTHING HERE MAY TOUCH THE MACHINE IT RUNS ON. Every package manager is a
// stub on a PATH prefix that records its argv, sudo is a stub, and DISPLAY is
// blank -- so a case can neither install a package nor raise a password prompt
// in front of whoever ran `go test`. TestNssToolsNeverReachesARealPackageManager
// keeps that true for future edits.

type nssEnvelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type nssWorld struct {
	bin string
	log string
	env []string
}

// nssEssentials are the real programs the script legitimately needs. They are
// SYMLINKED into the stub directory so PATH can be that directory and nothing
// else -- see newNssWorld.
var nssEssentials = []string{
	"bash", "env", "dirname", "basename", "uname", "id", "mktemp",
	"chmod", "rm", "cat", "sed", "tr", "wc", "cut", "head", "printf",
}

func newNssWorld(t *testing.T) *nssWorld {
	t.Helper()
	base := t.TempDir()
	w := &nssWorld{bin: filepath.Join(base, "bin"), log: filepath.Join(base, "calls.log")}
	if err := os.MkdirAll(w.bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// THE PATH IS THIS DIRECTORY AND NOTHING ELSE.
	//
	// The first version of this harness kept /usr/bin on PATH so bash had its
	// coreutils -- and a case that stubbed only `zypper` reached the RUNNER'S
	// OWN apt-get, which tried to take the dpkg lock. It failed on permissions
	// rather than installing anything, but on a machine with passwordless sudo
	// it would have installed a package during `go test`. Whether a real
	// package manager is reachable cannot be left to which one a case happened
	// to stub.
	for _, tool := range nssEssentials {
		real, err := exec.LookPath(tool)
		if err != nil {
			continue // not every system has every one; the script copes.
		}
		if err := os.Symlink(real, filepath.Join(w.bin, tool)); err != nil {
			t.Fatalf("symlink %s: %v", tool, err)
		}
	}
	return w
}

// stub writes an executable that records its argv and then runs `body`.
func (w *nssWorld) stub(t *testing.T, name, body string) {
	t.Helper()
	script := "#!/usr/bin/env bash\nprintf '" + name + " %s\\n' \"$*\" >> \"$STUB_LOG\"\n" + body
	if err := os.WriteFile(filepath.Join(w.bin, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// certutilAppearsAfterInstall makes a package manager stub behave like a real
// one: the binary it installs exists afterwards.
func (w *nssWorld) certutilAppearsAfterInstall(t *testing.T, manager string) {
	t.Helper()
	w.stub(t, manager, "printf '#!/usr/bin/env bash\\nexit 0\\n' > \""+w.bin+"/certutil\"\nchmod +x \""+w.bin+"/certutil\"\nexit 0\n")
}

func (w *nssWorld) run(t *testing.T, args ...string) (nssEnvelope, int, string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	script := filepath.Join(filepath.Clean(filepath.Join(wd, "..", "..")), "scripts", "install", "nss-tools.sh")

	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Stdin = nil
	// PATH is ONLY the stub directory plus the system essentials bash needs.
	// A real apt-get or brew must not be reachable from here.
	cmd.Env = append([]string{
		"PATH=" + w.bin,
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"STUB_LOG=" + w.log,
		"DISPLAY=",
		"WAYLAND_DISPLAY=",
	}, w.env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running nss-tools.sh: %v\n%s", err, stderr.String())
	}
	combined := "--- stdout ---\n" + stdout.String() + "--- stderr ---\n" + stderr.String()

	var env nssEnvelope
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			if jerr := json.Unmarshal([]byte(l), &env); jerr != nil {
				t.Fatalf("stdout is not a JSON envelope: %v\n%s", jerr, combined)
			}
		}
	}
	return env, code, combined
}

func (w *nssWorld) calls(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.log)
	if err != nil {
		if os.IsNotExist(err) {
			return "(nothing was invoked)"
		}
		t.Fatalf("read call log: %v", err)
	}
	return string(b)
}

const nssConfirm = "install-nss-tools"

// sudoThatWorks asks for a password, gets one, and runs the command.
const sudoThatWorks = `
if [ "$1" = "-n" ]; then exit 1; fi
if [ "$1" = "-A" ]; then shift; fi
exec "$@"
`

//=============================================================================
// THE ORDINARY PATHS
//=============================================================================

// Already there is the common case on a second run, and it must install
// nothing: a step that reinstalls a package every time it is retried is one an
// operator learns to dread.
func TestNssToolsAlreadyPresentInstallsNothing(t *testing.T) {
	w := newNssWorld(t)
	w.stub(t, "certutil", "exit 0")
	w.stub(t, "apt-get", "exit 0")

	env, code, out := w.run(t, "--confirm="+nssConfirm)
	if code != 0 || !env.OK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if env.Changed {
		t.Errorf("changed=true when certutil was already here: %s", out)
	}
	if env.Result["preExisting"] != true {
		t.Errorf("preExisting should be true: %s", out)
	}
	if strings.Contains(w.calls(t), "apt-get") {
		t.Errorf("a package manager ran with certutil already present:\n%s", w.calls(t))
	}
}

// Each distribution carries certutil in a differently-named package, and the
// manager's non-interactive flags differ with it. Getting the pair wrong is an
// install that stops on a machine MemQL claimed to support.
func TestNssToolsInstallsTheRightPackagePerManager(t *testing.T) {
	for _, tc := range []struct {
		manager string
		want    string
	}{
		{"apt-get", "install -y libnss3-tools"},
		{"dnf", "install -y nss-tools"},
		{"yum", "install -y nss-tools"},
		{"pacman", "-S --noconfirm nss"},
		{"zypper", "--non-interactive install mozilla-nss-tools"},
	} {
		tc := tc
		t.Run(tc.manager, func(t *testing.T) {
			w := newNssWorld(t)
			w.stub(t, "sudo", sudoThatWorks)
			w.certutilAppearsAfterInstall(t, tc.manager)
			w.env = append(w.env, "DISPLAY=:0")
			w.stub(t, "zenity", "printf 'pw\\n'")

			env, code, out := w.run(t, "--confirm="+nssConfirm, "--manager="+tc.manager)
			if code != 0 || !env.OK {
				t.Fatalf("exit %d: %s\ncalls:\n%s", code, out, w.calls(t))
			}
			if !env.Changed {
				t.Errorf("changed=false after installing a package: %s", out)
			}
			if !strings.Contains(w.calls(t), tc.manager+" "+tc.want) {
				t.Errorf("wrong command for %s -- want %q\ncalls:\n%s", tc.manager, tc.want, w.calls(t))
			}
		})
	}
}

// Homebrew installs into a prefix the operator owns and REFUSES to run as root.
// Elevating it would break the install on exactly the platform it serves.
func TestNssToolsDoesNotElevateHomebrew(t *testing.T) {
	w := newNssWorld(t)
	w.stub(t, "sudo", sudoThatWorks)
	w.certutilAppearsAfterInstall(t, "brew")
	w.env = append(w.env, "DISPLAY=:0")
	w.stub(t, "zenity", "printf 'pw\\n'")

	env, code, out := w.run(t, "--confirm="+nssConfirm, "--manager=brew")
	if code != 0 || !env.OK {
		t.Fatalf("exit %d: %s\ncalls:\n%s", code, out, w.calls(t))
	}
	calls := w.calls(t)
	if !strings.Contains(calls, "brew install nss") {
		t.Errorf("brew was not asked to install nss:\n%s", calls)
	}
	if strings.Contains(calls, "sudo") {
		t.Errorf("brew was run through sudo, which Homebrew refuses:\n%s", calls)
	}
}

//=============================================================================
// REFUSALS
//=============================================================================

// Installing a system package is a change to shared machine state, so it is
// gated on the phrase -- and the gate must come BEFORE the package manager
// runs, not after.
func TestNssToolsWithoutConfirmationIsRefused(t *testing.T) {
	w := newNssWorld(t)
	w.stub(t, "sudo", sudoThatWorks)
	w.certutilAppearsAfterInstall(t, "apt-get")

	env, code, out := w.run(t)
	if code != 3 {
		t.Errorf("exit %d, want 3 (refused): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 3 {
		t.Errorf("envelope should carry ok=false error.code=3: %s", out)
	}
	if strings.Contains(w.calls(t), "apt-get") {
		t.Errorf("a package was installed without confirmation:\n%s", w.calls(t))
	}
}

// No manager we know is a prerequisite failure with something to act on, not a
// crash and not a silent skip that leaves the next step to fail confusingly.
func TestNssToolsWithNoPackageManagerIsExitFour(t *testing.T) {
	w := newNssWorld(t)

	env, code, out := w.run(t, "--confirm="+nssConfirm)
	if code != 4 {
		t.Errorf("exit %d, want 4: %s", code, out)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "certutil") {
		t.Errorf("the message should name what is missing: %+v", env.Error)
	}
}

// No way to reach root: the install cannot proceed, and the operator gets the
// exact command rather than a dead end.
func TestNssToolsWithNoWayToElevateOffersTheCommand(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: nothing to elevate")
	}
	w := newNssWorld(t)
	w.stub(t, "sudo", "exit 1\n") // refuses -n and everything else
	w.certutilAppearsAfterInstall(t, "apt-get")

	env, code, out := w.run(t, "--confirm="+nssConfirm)
	if code != 4 {
		t.Errorf("exit %d, want 4: %s", code, out)
	}
	remedy, _ := env.Result["remedy"].(string)
	if !strings.Contains(remedy, "apt-get install -y libnss3-tools") {
		t.Errorf("no usable remedy: %q", remedy)
	}
}

// THE ONE THAT MATTERS MOST. The whole point of this step is that certutil is
// CALLABLE afterwards. A package manager that exits 0 having installed
// something with a different layout would otherwise hand mkcert the same silent
// failure this step exists to remove.
func TestNssToolsProvesCertutilIsCallableAfterwards(t *testing.T) {
	w := newNssWorld(t)
	w.stub(t, "sudo", sudoThatWorks)
	w.stub(t, "apt-get", "exit 0") // "succeeds" and installs nothing
	w.env = append(w.env, "DISPLAY=:0")
	w.stub(t, "zenity", "printf 'pw\\n'")

	env, code, out := w.run(t, "--confirm="+nssConfirm)
	if code != 5 {
		t.Errorf("exit %d, want 5 -- trusting the package manager's exit code is exactly "+
			"the silent success this step exists to remove: %s", code, out)
	}
	if env.OK {
		t.Errorf("reported success with no certutil on PATH: %s", out)
	}
}

//=============================================================================
// THE SAFETY RAIL ON THE TESTS THEMSELVES
//=============================================================================

func TestNssToolsNeverReachesARealPackageManager(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(wd, "nss_tools_test.go"))
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}
	needle := "exec." + "Command("
	if n := strings.Count(string(b), needle); n != 1 {
		t.Errorf("exec.Command appears %d times, want 1 (only nssWorld.run) -- every run must "+
			"go through the stubbed PATH, never a real apt-get or brew", n)
	}
}

// Detection, asserted separately from the per-manager table above (which names
// its manager explicitly). The SYSTEM manager wins: a Debian box can also carry
// linuxbrew, and a system tool belongs to the system's own package manager.
func TestNssToolsPrefersTheSystemPackageManager(t *testing.T) {
	w := newNssWorld(t)
	w.stub(t, "sudo", sudoThatWorks)
	w.stub(t, "brew", "exit 0")
	w.certutilAppearsAfterInstall(t, "apt-get")
	w.env = append(w.env, "DISPLAY=:0")
	w.stub(t, "zenity", "printf 'pw\\n'")

	if _, code, out := w.run(t, "--confirm="+nssConfirm); code != 0 {
		t.Fatalf("exit %d: %s\ncalls:\n%s", code, out, w.calls(t))
	}
	calls := w.calls(t)
	if !strings.Contains(calls, "apt-get install -y libnss3-tools") {
		t.Errorf("apt-get was not chosen:\n%s", calls)
	}
	if strings.Contains(calls, "brew install") {
		t.Errorf("brew was used on a machine that has apt-get:\n%s", calls)
	}
}
