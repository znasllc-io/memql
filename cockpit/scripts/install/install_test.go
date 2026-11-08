package install_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Native Go test lanes must exercise the shipped shell entry points, not only Go
// installer helpers. These suites use temporary homes, fixture binaries and
// mocked services; they never enroll a machine or install a real release.
// Missing declared Bash/Python prerequisites are failures, not silent skips.
func TestInstallerScripts(t *testing.T) {
	for _, suite := range []struct {
		name string
		tool string
		file string
	}{
		{"fixture-process", "python3", "fixture_process_test.py"},
		{"release-discovery", "python3", "release_discovery_test.py"},
		{"upgrade-linux", "python3", "upgrade_linux_test.py"},
		{"upgrade-mac", "python3", "upgrade_mac_test.py"},
		{"presentation", "python3", "ui_test.py"},
		{"library", "bash", "lib_test.sh"},
		{"menubar", "bash", "menubar_test.sh"},
	} {
		t.Run(suite.name, func(t *testing.T) {
			tool, err := exec.LookPath(suite.tool)
			if err != nil {
				t.Fatalf("installer checks require %s: %v", suite.tool, err)
			}
			ctx, cancel := suiteContext(t)
			defer cancel()
			output, err := suiteCommand(ctx, tool, suite.file).CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed (%v):\n%s", suite.file, err, output)
			}
		})
	}
}

// Every fixture shares this deadline rather than imposing a shorter per-child
// wall-clock limit. Shell suites use the same process-group runner as Python
// fixtures: killing just Bash leaves loggers holding CombinedOutput's pipe,
// hiding the suite's diagnostics until the Go binary itself times out.
func suiteCommand(ctx context.Context, tool, file string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, tool, file)
	if filepath.Base(tool) == "bash" {
		cmd = exec.CommandContext(ctx, "python3", "fixture_process.py", "--program", tool, "--script", file)
	}
	// The fixture runner owns descendant cleanup. Also bound pipe draining if
	// that runner fails, without extending or shortening a running suite's budget.
	cmd.WaitDelay = time.Second
	if deadline, ok := ctx.Deadline(); ok {
		cmd.Env = append(os.Environ(), "MEMQL_INSTALLER_TEST_DEADLINE_UNIX_MS="+strconv.FormatInt(deadline.UnixMilli(), 10))
	}
	return cmd
}

// suiteContext stops a suite shortly before the test binary's own deadline,
// so a suite that hangs still fails with its output; the binary's timeout
// panic would print none of it. How long a healthy suite takes is the
// machine's business. The fixed three-minute bound this replaces killed
// lib_test.sh on CyberPC on 2026-11-03 while every check it had run was
// passing (cockpit 0.22.1's push run), though the whole package needs about
// 96 s on the MSI Raider. The cost: a hung suite is noticed near the lane's
// -timeout (600 s in memql-package.yaml) rather than at three minutes.
func suiteContext(t *testing.T) (context.Context, context.CancelFunc) {
	deadline, ok := t.Deadline()
	if !ok {
		// -timeout=0: nothing else ends the run, so keep a bound of our own.
		return context.WithTimeout(t.Context(), 10*time.Minute)
	}
	// Time to print the output: a tenth of what is left, at most 30 s.
	return context.WithDeadline(t.Context(), deadline.Add(-min(time.Until(deadline)/10, 30*time.Second)))
}

// A suite may run until shortly before the test binary's own deadline: a
// slower machine is held to the -timeout its lane set, not to a bound of ours.
func TestASuiteRunsUntilShortlyBeforeTheBinaryDeadline(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Skip("run without -timeout=0: the bound under test is the binary's deadline")
	}
	ctx, cancel := suiteContext(t)
	defer cancel()
	got, ok := ctx.Deadline()
	if !ok {
		t.Fatal("a suite must have a deadline, so a hung suite still fails with its output")
	}
	if !got.Before(deadline) {
		t.Fatalf("suite deadline %v is not before the binary's %v: a timeout panic would print none of the suite's output", got, deadline)
	}
	if margin := deadline.Sub(got); margin > 30*time.Second {
		t.Fatalf("suite deadline ends %v before the binary's: a healthy suite on a slower machine is killed early", margin)
	}
}

func TestFixturesInheritTheSuiteDeadline(t *testing.T) {
	ctx, cancel := suiteContext(t)
	defer cancel()
	deadline, _ := ctx.Deadline()
	want := "MEMQL_INSTALLER_TEST_DEADLINE_UNIX_MS=" + strconv.FormatInt(deadline.UnixMilli(), 10)
	for _, tool := range []string{"python3", "/bin/bash"} {
		cmd := suiteCommand(ctx, tool, "fixture")
		if cmd.Env[len(cmd.Env)-1] != want {
			t.Fatalf("%s fixture must inherit the suite's deadline: got %v, want %s", tool, cmd.Env[len(cmd.Env)-1], want)
		}
	}
}
