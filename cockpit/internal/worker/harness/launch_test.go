package harness

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// launch_test.go gives every harness test a real LaunchFunc.
//
// It is a plain os/exec wrapper and lives in a _test.go file on purpose:
// production has exactly ONE process supervisor (the app-session one, in
// internal/worker/appsession/process.go), and a second one compiled into
// the binary would drift from it in the way that matters -- process
// groups, the SIGTERM-then-SIGKILL escalation -- without anyone noticing
// until a cancel failed to stop an agent. Tests do not need any of that;
// they need a child that starts, streams and exits.

// testProcess implements Process over os/exec.
type testProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader

	waitOnce sync.Once
	waitErr  error
}

func (p *testProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *testProcess) Stdout() io.Reader     { return p.stdout }
func (p *testProcess) Stderr() io.Reader     { return p.stderr }

func (p *testProcess) Terminate() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *testProcess) Wait() error {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	return p.waitErr
}

func (p *testProcess) ExitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

// testLaunch is the LaunchFunc every harness test injects into Spec.
func testLaunch(ctx context.Context, dir string, argv []string, env []string, stdin bool) (Process, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)

	p := &testProcess{cmd: cmd}
	if stdin {
		w, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		p.stdin = w
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p.stdout = out
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	p.stderr = errPipe
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return p, nil
}

// fakeBinary writes an executable shell script into a fresh directory and
// returns the directory, for a Spec.Binary of name resolved through PATH
// or an absolute path.
//
// Scripts rather than compiled Go helpers because what these tests assert
// is the PROTOCOL -- bytes in, bytes out -- and a script makes the
// recorded fixture visible in the test that uses it instead of hiding it
// behind another program's main.
func fakeBinary(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake harness binaries are shell scripts")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	body := script
	if !strings.HasPrefix(body, "#!") {
		body = "#!/bin/sh\n" + body
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}
