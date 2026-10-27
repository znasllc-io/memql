// Package pyyaml is the preflight for Go tests that execute this repository's
// PyYAML-importing Python: scripts/ci's pipeline-db-tests.py and codeql-scan.py,
// the unittest suites beside them, and the Python inside
// scripts/deploy/conn-headroom-check.sh, the deploy path's connection gate.
//
// WHY A PREFLIGHT. PyYAML is not in Python's standard library. CI's toolchain
// image ships it as python3-yaml (deploy/toolchain-image/Dockerfile, asserted by
// its smoke-test.sh); a developer machine gets it from `make install-deps`
// (scripts/dev/install-deps.sh). Without it the tests of scripts/ci's Python
// failed with a raw ModuleNotFoundError, or, where a test read only stdout
// (scripts/cidb/planner_coverage_test.go), with "exit status 1" and nothing
// else. Neither says which interpreter answered or what to run. The connection
// gate's tests (scripts/deploy, deploy/k8s/overlays/local) skipped instead.
//
// WHY IT FAILS AND DOES NOT SKIP. A skip reads as green (CLAUDE.md, Testing): a
// machine without PyYAML would report the database lane's budget runner, the
// CodeQL and Scorecard protocols and the connection gate as passing having run
// none of them. CI always has the module, so a skip would only ever hide a gap
// on a developer machine. There is deliberately no opt-out.
package pyyaml

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// probe prints the interpreter and its version BEFORE importing yaml, so a
// failed import still says which python3 answered. It runs the way the tests
// run Python -- `python3 -c`, no -I, the test's own environment -- so
// PYTHONPATH and the user site count here exactly as they do there.
const probe = `import sys
print(sys.executable)
print(sys.version.split()[0])
import yaml`

// Require fails t, naming the module, the interpreter and the fix, when the
// python3 first on PATH cannot import yaml. Call it before running the Python.
func Require(t testing.TB) {
	t.Helper()
	if p := problem(); p != "" {
		t.Fatal(p)
	}
}

// problem is Require's verdict: empty when python3 imports yaml, otherwise the
// message.
func problem() string {
	// The same lookup exec.Command("python3", ...) makes in the tests.
	path, err := exec.LookPath("python3")
	if err != nil {
		return "python3 is not on PATH, and this test runs Python that needs it with PyYAML.\n" +
			"Install python3 (macOS: xcode-select --install), then run `make install-deps`,\n" +
			"which installs PyYAML for it."
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(path, "-c", probe)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err == nil {
		return ""
	}
	interpreter := path + " (Python version unknown)"
	if lines := strings.Split(strings.TrimSpace(stdout.String()), "\n"); len(lines) == 2 {
		interpreter = fmt.Sprintf("%s (Python %s)", lines[0], lines[1])
	}
	// The exception is the last line of a traceback. An interpreter that never
	// started has none, and then the error is the exec failure itself.
	reason := strings.TrimSpace(stderr.String())
	if i := strings.LastIndex(reason, "\n"); i >= 0 {
		reason = reason[i+1:]
	}
	if reason == "" {
		reason = err.Error()
	}
	return fmt.Sprintf("python3 cannot import yaml (the PyYAML package), and this test runs Python that imports it.\n"+
		"  interpreter: %s, found on PATH as %s\n"+
		"  error:       %s\n"+
		"Run `make install-deps`. It installs PyYAML for the python3 first on its own PATH,\n"+
		"so run it where `command -v python3` prints %s.\n"+
		"CI's toolchain image ships the same module as python3-yaml.",
		interpreter, path, reason, path)
}
