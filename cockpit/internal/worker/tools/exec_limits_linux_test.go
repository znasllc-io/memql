//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestShellMemoryLimitOnlyAffectsTheChild(t *testing.T) {
	if os.Getenv("MEMQL_TEST_CHILD_MEMORY") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^TestShellMemoryLimitOnlyAffectsTheChild$")
		cmd.Env = append(os.Environ(), "MEMQL_TEST_CHILD_MEMORY=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("memory limit regression: %v\n%s", err, out)
		}
		return
	}
	var before, after syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &before); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy()
	p.shell = ShellPolicy{Allow: []string{"ulimit"}, MaxMemoryMB: 64}
	result, fail := runExec(context.Background(), map[string]any{"cmd": "ulimit -v", "cwd": t.TempDir()}, p)
	if fail != nil || result.GetExitCode() != 0 {
		t.Fatalf("memory-limited command: %+v %+v", result, fail)
	}
	var output struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal(result.ResultJson, &output); err != nil {
		t.Fatal(err)
	}
	if output.Stdout != "65536\n" {
		t.Fatalf("child memory limit=%q", output.Stdout)
	}
	if err := syscall.Getrlimit(syscall.RLIMIT_AS, &after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("worker memory limit changed: %v -> %v", before, after)
	}
}
