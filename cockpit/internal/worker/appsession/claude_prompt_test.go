//go:build linux || darwin

package appsession

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
)

// The leader exits while a child holds stdin open, without holding either
// output open. Joining the input writer before Cmd.Wait would hang forever:
// this supervisor's Wait closes StdinPipe, the failed writer terminates the
// group, and no external cancellation should be necessary to finish the turn.
func TestClaudePromptLeaderExitClosesInputAndStopsDescendant(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
(
  first=$(dd bs=1 count=1 2>/dev/null)
  [ "$first" = x ] || exit 91
  printf ready > %q
  exec sleep 60
) <&0 >/dev/null 2>&1 &
printf '%%s' "$!" > %q
i=0
while [ ! -f %q ] && [ "$i" -lt 500 ]; do
  i=$((i+1)); sleep 0.01
done
exit 0
`, binary+".ready", binary+".pid", binary+".ready")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &session{}
	h, err := harness.New(harness.HarnessClaudeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	stopChild := func() {
		s.mu.Lock()
		child := s.child
		s.mu.Unlock()
		child.terminate()
	}
	t.Cleanup(func() { _ = h.Close(); stopChild() })
	if err := h.Start(context.Background(), harness.Spec{Binary: binary, Workspace: dir, Launch: s.launcher()}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.Turn(ctx, strings.Repeat("x", 1<<20), nil)
		done <- err
	}()
	select {
	case err := <-done:
		if ctx.Err() != nil || err == nil || !strings.Contains(err.Error(), "complete Claude prompt") {
			t.Fatalf("early-exit delivery did not fail independently of cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		stopChild()
		<-done
		t.Fatal("turn waited for external cancellation after its leader exited")
	}
	if _, err := os.Stat(binary + ".ready"); err != nil {
		t.Fatalf("child did not read the first prompt byte from inherited stdin: %v", err)
	}
	data, err := os.ReadFile(binary + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid descendant pid %q: %v", data, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
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
			t.Fatalf("inspect descendant: %v", err)
		}
		// An orphan zombie has exited and only awaits its new parent's reap.
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("failed prompt delivery left descendant %d running", pid)
}
