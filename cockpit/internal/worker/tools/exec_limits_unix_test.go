//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Keep the regression in a subprocess: the old implementation irreversibly
// lowered this process's hard limits, which must not contaminate other tests.
func TestShellLimitsDoNotConstrainTheWorkerOrItsNextPipeline(t *testing.T) {
	if os.Getenv("MEMQL_TEST_CHILD_LIMITS") == "1" {
		verifyChildLimits(t)
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestShellLimitsDoNotConstrainTheWorkerOrItsNextPipeline$", "-test.v")
	cmd.Env = append(os.Environ(), "MEMQL_TEST_CHILD_LIMITS=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child-limit regression: %v\n%s", err, out)
	}
}

func verifyChildLimits(t *testing.T) {
	const report = "ulimit -n; ulimit -t"
	var filesBefore, cpuBefore syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &filesBefore); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Getrlimit(syscall.RLIMIT_CPU, &cpuBefore); err != nil {
		t.Fatal(err)
	}
	baseline, err := exec.Command("/bin/sh", "-c", report).Output()
	if err != nil {
		t.Fatal(err)
	}
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	p, _ := pipelineTestPolicy(t, "")
	p.shell = ShellPolicy{Allow: []string{"ulimit"}, MaxOpenFiles: 64, MaxCPUSeconds: 3}
	run := func(want string) {
		t.Helper()
		result, fail := runExec(context.Background(), map[string]any{"cmd": report, "cwd": t.TempDir()}, p)
		if fail != nil || result.GetExitCode() != 0 {
			t.Fatalf("limited command: %+v %+v", result, fail)
		}
		var output struct {
			Stdout string `json:"stdout"`
		}
		if err := json.Unmarshal(result.ResultJson, &output); err != nil {
			t.Fatal(err)
		}
		if output.Stdout != want {
			t.Fatalf("child limits=%q, want %q", output.Stdout, want)
		}
	}
	run("64\n3\n")
	var filesAfter, cpuAfter syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &filesAfter)
	_ = syscall.Getrlimit(syscall.RLIMIT_CPU, &cpuAfter)
	if filesAfter != filesBefore || cpuAfter != cpuBefore {
		t.Fatalf("worker limits changed: files %v -> %v; CPU %v -> %v", filesBefore, filesAfter, cpuBefore, cpuAfter)
	}
	// Loosening a reloaded policy works; the earlier command changed no parent
	// hard limit that could prevent a later independent child from increasing it.
	p.shell.MaxOpenFiles = 128
	run("128\n3\n")
	out := &recordedOutput{}
	result, fail := runPipelineStep(context.Background(), "", stepArgs(t, fx, report, nil), p, out.emit, out.artifact)
	if got := decodeStep(t, result, fail); got.ExitCode != 0 {
		t.Fatalf("pipeline exit=%d", got.ExitCode)
	}
	if !strings.Contains(out.text(false), string(baseline)) {
		t.Fatalf("pipeline inherited shell limits: %q; original %q", out.text(false), baseline)
	}
}

func TestExecCannotReportSuccessWhenTheChildNeverStarted(t *testing.T) {
	p := DefaultPolicy()
	p.shell = ShellPolicy{Allow: []string{"echo"}}
	result, fail := runExec(context.Background(), map[string]any{"cmd": "echo should-not-run", "cwd": filepath.Join(t.TempDir(), "missing")}, p)
	if result != nil || fail.GetErrorCode() != "exec_failed" {
		t.Fatalf("start failure looked successful: %+v %+v", result, fail)
	}
}

func TestRefusedChildLimitDoesNotRunTheCommand(t *testing.T) {
	if os.Getenv("MEMQL_TEST_REFUSED_LIMIT") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^TestRefusedChildLimitDoesNotRunTheCommand$")
		cmd.Env = append(os.Environ(), "MEMQL_TEST_REFUSED_LIMIT=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("limit refusal: %v\n%s", err, out)
		}
		return
	}
	// This helper alone has a low hard ceiling; its requested command may not
	// run if applying the higher policy value is refused by the host.
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: 64, Max: 64}); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy()
	p.shell = ShellPolicy{Allow: []string{"echo"}, MaxOpenFiles: 128}
	marker := filepath.Join(t.TempDir(), "ran")
	result, fail := runExec(context.Background(), map[string]any{"cmd": `echo ran > "$MARKER"`, "env": map[string]any{"MARKER": marker}}, p)
	if fail != nil || result.GetExitCode() != 125 {
		t.Fatalf("refused limit: %+v %+v", result, fail)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran after a limit refusal: %v", err)
	}
}

func TestExecCancellationKillsItsChildProcessGroup(t *testing.T) {
	p := DefaultPolicy()
	p.shell = ShellPolicy{Allow: []string{"sleep"}}
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan string, 1)
	go func() {
		result, fail := runExec(ctx, map[string]any{"cmd": `sleep 30 & child=$!; echo "$child" > "$PID_FILE"; wait`, "env": map[string]any{"PID_FILE": pidFile}}, p)
		if result != nil {
			ended <- "unexpected success"
			return
		}
		ended <- fail.GetErrorCode()
	}()
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if pid > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("command never started its child")
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	cancel()
	select {
	case code := <-ended:
		if code != "timeout" {
			t.Fatalf("cancellation=%s", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exec did not stop after cancellation")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("child survived the cancelled exec")
}
