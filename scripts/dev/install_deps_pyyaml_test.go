package dev

// install_deps_pyyaml_test.go -- install-deps.sh's PyYAML step
// (install_pyyaml), driven against stub tools standing in for each kind of
// python3 the step tells apart.
//
// The step exists because Go tests exec scripts/ci's PyYAML-importing Python
// and the connection gate's (scripts/ci/pyyaml). Its route depends on who
// owns the interpreter, so each route is pinned here to the exact install
// command it must run, along with the ways the step must refuse to report
// success. The stubs log every install command they receive; "ok" is printed
// only after the stub python3 really imports yaml.

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pyyamlStubPython3 imports yaml once $STUB_DIR/yaml-installed exists, answers
// the step's environment probe (read on stdin) with $STUB_KIND, and logs pip
// installs.
const pyyamlStubPython3 = `#!/bin/sh
case "$1" in
-c)
	case "$2" in
	*"import yaml"*)
		if [ -f "$STUB_DIR/yaml-installed" ]; then
			case "$2" in *__version__*) echo 6.0.3 ;; esac
			exit 0
		fi
		echo "ModuleNotFoundError: No module named 'yaml'" >&2
		exit 1 ;;
	esac ;;
-)
	cat >/dev/null
	echo "$STUB_KIND"
	exit 0 ;;
-m)
	if [ "$2 $3" = "pip --version" ]; then
		if [ "$STUB_PIP" = missing ]; then
			echo "No module named pip" >&2
			exit 1
		fi
		echo "pip 26.0"
		exit 0
	fi
	if [ "$2 $3" = "pip install" ]; then
		echo "python3 $*" >> "$STUB_LOG"
		if [ "$STUB_INSTALL" = takes ]; then : > "$STUB_DIR/yaml-installed"; fi
		exit 0
	fi ;;
esac
echo "stub python3: unexpected arguments: $*" >&2
exit 97
`

// pyyamlStubPackageManager logs its invocation and, when asked to install,
// leaves PyYAML importable -- as the distribution's package would.
const pyyamlStubPackageManager = `#!/bin/sh
echo "${0##*/} $*" >> "$STUB_LOG"
case " $* " in *" install "*|*" -S "*) : > "$STUB_DIR/yaml-installed" ;; esac
exit 0
`

// pyyamlStubSudo answers the non-interactive probe (sudo -n true) with
// $STUB_SUDO and otherwise logs and runs the command.
const pyyamlStubSudo = `#!/bin/sh
if [ "$1" = -n ]; then
	if [ "$STUB_SUDO" = passwordless ]; then exit 0; fi
	exit 1
fi
echo "sudo $*" >> "$STUB_LOG"
exec "$@"
`

const pyyamlStubID = `#!/bin/sh
if [ "$1" = -u ]; then echo "$STUB_UID"; exit 0; fi
exec /usr/bin/id "$@"
`

type pyyamlCase struct {
	name    string
	present bool   // python3 imports yaml before the step runs
	kind    string // the environment probe's answer: venv | externally-managed | base
	distro  string // package manager when python3 is the distribution's own, else ""
	uid     string
	sudo    string // passwordless | password
	pip     string // ok | missing
	install string // takes | does-not-take
	// PIP_BREAK_SYSTEM_PACKAGES, pip's own PEP 668 override, as the user set it.
	override string

	wantOK    bool
	wantCalls []string
	wantOut   []string
}

func TestInstallPyYAMLChoosesTheRouteTheInterpreterAllows(t *testing.T) {
	cases := []pyyamlCase{
		{name: "already importable", present: true, wantOK: true,
			wantOut: []string{"[ok] PyYAML 6.0.3 for "}},
		{name: "base interpreter (Command Line Tools)", kind: "base", wantOK: true,
			wantCalls: []string{"python3 -m pip install --disable-pip-version-check --user pyyaml"},
			wantOut:   []string{"[ok] PyYAML 6.0.3 installed for "}},
		// The PEP 668 override is the user's to give, so without it the step
		// installs nothing and offers both ways forward.
		{name: "externally managed (Homebrew)", kind: "externally-managed",
			wantOut: []string{"EXTERNALLY-MANAGED", "PIP_BREAK_SYSTEM_PACKAGES=1 make install-deps", "python3 -m venv"}},
		{name: "externally managed, override declined", kind: "externally-managed", override: "0",
			wantOut: []string{"EXTERNALLY-MANAGED"}},
		{name: "externally managed, override given", kind: "externally-managed", override: "1", wantOK: true,
			wantCalls: []string{"python3 -m pip install --disable-pip-version-check --user --break-system-packages pyyaml"},
			wantOut:   []string{"[ok] PyYAML 6.0.3 installed for "}},
		{name: "externally managed, override spelled as pip reads it", kind: "externally-managed", override: "Yes", wantOK: true,
			wantCalls: []string{"python3 -m pip install --disable-pip-version-check --user --break-system-packages pyyaml"}},
		{name: "virtualenv", kind: "venv", wantOK: true,
			wantCalls: []string{"python3 -m pip install --disable-pip-version-check pyyaml"},
			wantOut:   []string{"[ok] PyYAML 6.0.3 installed for "}},
		{name: "debian system interpreter", distro: "apt", kind: "externally-managed", wantOK: true,
			wantCalls: []string{"sudo apt-get update -qq", "apt-get update -qq", "sudo apt-get install -y python3-yaml", "apt-get install -y python3-yaml"},
			wantOut:   []string{"[ok] PyYAML 6.0.3 installed for "}},
		{name: "debian system interpreter as root", distro: "apt", kind: "externally-managed", uid: "0", wantOK: true,
			wantCalls: []string{"apt-get update -qq", "apt-get install -y python3-yaml"}},
		{name: "fedora system interpreter", distro: "dnf", kind: "base", wantOK: true,
			wantCalls: []string{"sudo dnf install -y python3-pyyaml", "dnf install -y python3-pyyaml"}},
		{name: "arch system interpreter", distro: "pacman", kind: "externally-managed", wantOK: true,
			wantCalls: []string{"sudo pacman -S --noconfirm python-yaml", "pacman -S --noconfirm python-yaml"}},
		{name: "system interpreter, sudo would prompt", distro: "apt", kind: "externally-managed", sudo: "password",
			wantOut: []string{"needs sudo", "sudo apt-get install -y python3-yaml"}},
		{name: "unknown distribution falls back to pip", distro: "zypper", kind: "base", wantOK: true,
			wantCalls: []string{"python3 -m pip install --disable-pip-version-check --user pyyaml"}},
		{name: "install that does not take", kind: "base", install: "does-not-take",
			wantCalls: []string{"python3 -m pip install --disable-pip-version-check --user pyyaml"},
			wantOut:   []string{"still cannot import yaml", "ModuleNotFoundError: No module named 'yaml'"}},
		{name: "no pip", kind: "base", pip: "missing",
			wantOut: []string{"has no pip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, calls, ok := runInstallPyYAML(t, tc)
			if ok != tc.wantOK {
				t.Errorf("succeeded=%v, want %v", ok, tc.wantOK)
			}
			if !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("install commands:\n  got  %q\n  want %q", calls, tc.wantCalls)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q", want)
				}
			}
			if t.Failed() {
				t.Logf("output:\n%s", out)
			}
		})
	}
}

// runInstallPyYAML sources install-deps.sh and runs install_pyyaml with the
// stubs first on PATH. Only the platform facts are replaced -- which package
// manager is present, and whether python3 is the distribution's own -- because
// the machine running the test is one platform and these cases need all of them.
func runInstallPyYAML(t *testing.T, tc pyyamlCase) (out string, calls []string, ok bool) {
	t.Helper()
	dir := t.TempDir()
	stubs := map[string]string{"python3": pyyamlStubPython3, "sudo": pyyamlStubSudo, "id": pyyamlStubID}
	for _, pm := range []string{"apt-get", "dnf", "pacman"} {
		stubs[pm] = pyyamlStubPackageManager
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if tc.present {
		write(t, filepath.Join(dir, "yaml-installed"), "")
	}
	pkg, distro := tc.distro, "no"
	if pkg == "" {
		pkg = "brew"
	} else {
		distro = "yes"
	}
	harness := filepath.Join(dir, "harness.sh")
	write(t, harness, "set -euo pipefail\n"+
		"source "+shQuote(filepath.Join(repoRoot(t), "scripts", "dev", "install-deps.sh"))+"\n"+
		"detect_pkg_manager() { echo \"$STUB_PKG\"; }\n"+
		"is_distro_python3() { [[ \"$STUB_DISTRO\" == yes ]]; }\n"+
		"install_pyyaml\n")

	logPath := filepath.Join(dir, "calls.log")
	cmd := exec.Command("bash", harness)
	cmd.Env = hermeticEnv(
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_DIR="+dir,
		"STUB_LOG="+logPath,
		"STUB_KIND="+tc.kind,
		"STUB_PKG="+pkg,
		"STUB_DISTRO="+distro,
		"STUB_UID="+orDefault(tc.uid, "1000"),
		"STUB_SUDO="+orDefault(tc.sudo, "passwordless"),
		"STUB_PIP="+orDefault(tc.pip, "ok"),
		"STUB_INSTALL="+orDefault(tc.install, "takes"),
		"PIP_BREAK_SYSTEM_PACKAGES="+tc.override,
	)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		if _, exited := err.(*exec.ExitError); !exited {
			t.Fatalf("harness did not run: %v\n%s", err, combined)
		}
	}
	if log, rerr := os.ReadFile(logPath); rerr == nil {
		calls = strings.Split(strings.TrimSpace(string(log)), "\n")
	}
	return string(combined), calls, err == nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
