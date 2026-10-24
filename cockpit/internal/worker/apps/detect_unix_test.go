//go:build linux || darwin

package apps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDetector_VersionDoesNotWaitOnAChildHoldingItsOutput: an app behind a
// version-manager shim can leave a child holding stdout open after the
// probe has answered. The probe returns with what it printed once its own
// process is done, instead of waiting on the pipe -- the session's
// fingerprint asks for this version in front of the app's start.
func TestDetector_VersionDoesNotWaitOnAChildHoldingItsOutput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 &\nprintf '%s' \"$!\" > \"$0.pid\"\necho '2.1.270 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Detector{LookPath: func(string) (string, error) { return bin, nil }}
	got := d.Version(context.Background(), IDClaudeCode)
	child := probeChildPID(t, bin)
	if got != "2.1.270 (Claude Code)" {
		t.Errorf("version = %q, want what the probe printed", got)
	}
	assertProbeChildStopped(t, child)
}

func TestDetector_CancelStopsProbeChildren(t *testing.T) {
	for _, mode := range []string{"version", "probe"} {
		t.Run(mode, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "probe")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 &\nprintf '%s' \"$!\" > \"$0.pid\"\nwait\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				d := &Detector{}
				if mode == "probe" {
					done <- d.runProbe(ctx, bin, nil)
					return
				}
				_, err := d.runVersion(ctx, bin, nil)
				done <- err
			}()
			child := probeChildPID(t, bin)
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled probe succeeded")
				}
			case <-time.After(probeTimeout + time.Second):
				t.Fatal("cancelled probe did not finish")
			}
			assertProbeChildStopped(t, child)
		})
	}
}

func probeChildPID(t *testing.T, bin string) int {
	t.Helper()
	deadline := time.Now().Add(probeTimeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(bin + ".pid")
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("probe did not record its child")
	return 0
}

func assertProbeChildStopped(t *testing.T, pid int) {
	t.Helper()
	// Inspect the child itself instead of comparing total wall time on a
	// loaded test host. A zombie has exited and awaits its new parent's reap.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if err != nil {
			if exited, ok := err.(*exec.ExitError); ok && exited.ExitCode() == 1 {
				return
			}
			t.Fatalf("inspect child process: %v", err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("probe left child %d running after it finished", pid)
}
