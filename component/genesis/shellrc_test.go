package genesis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconcileShellRC_AppendsWhenAbsent(t *testing.T) {
	path := writeRCFixture(t, "rc-absent", "# my bashrc\nexport PATH=$HOME/bin:$PATH\n", 0o644)
	action, err := ReconcileShellRC(path, "abc123")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if action != RcAppended {
		t.Fatalf("action: got %v want RcAppended", action)
	}
	got := readFile(t, path)
	want := "# my bashrc\nexport PATH=$HOME/bin:$PATH\nexport MEMQL_MASTER_KEY=abc123\n"
	if got != want {
		t.Fatalf("content:\n got %q\nwant %q", got, want)
	}
}

func TestReconcileShellRC_AppendsAddsTrailingNewlineWhenMissing(t *testing.T) {
	path := writeRCFixture(t, "rc-no-nl", "export PATH=$HOME/bin:$PATH", 0o644)
	action, err := ReconcileShellRC(path, "abc123")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if action != RcAppended {
		t.Fatalf("action: got %v want RcAppended", action)
	}
	got := readFile(t, path)
	want := "export PATH=$HOME/bin:$PATH\nexport MEMQL_MASTER_KEY=abc123\n"
	if got != want {
		t.Fatalf("content:\n got %q\nwant %q", got, want)
	}
}

func TestReconcileShellRC_NoopWhenMatches(t *testing.T) {
	initial := "alias ll='ls -al'\nexport MEMQL_MASTER_KEY=abc123\n"
	path := writeRCFixture(t, "rc-match", initial, 0o644)
	action, err := ReconcileShellRC(path, "abc123")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if action != RcNoop {
		t.Fatalf("action: got %v want RcNoop", action)
	}
	if got := readFile(t, path); got != initial {
		t.Fatalf("file mutated on noop:\n got %q\nwant %q", got, initial)
	}
}

func TestReconcileShellRC_UpdatesMismatch(t *testing.T) {
	path := writeRCFixture(t, "rc-mismatch", "export MEMQL_MASTER_KEY=stale\nalias ll='ls -al'\n", 0o644)
	action, err := ReconcileShellRC(path, "fresh")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if action != RcUpdated {
		t.Fatalf("action: got %v want RcUpdated", action)
	}
	got := readFile(t, path)
	want := "export MEMQL_MASTER_KEY=fresh\nalias ll='ls -al'\n"
	if got != want {
		t.Fatalf("content:\n got %q\nwant %q", got, want)
	}
}

func TestReconcileShellRC_UpdatesBareForm(t *testing.T) {
	path := writeRCFixture(t, "rc-bare", "MEMQL_MASTER_KEY=stale\n", 0o644)
	action, err := ReconcileShellRC(path, "fresh")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if action != RcUpdated {
		t.Fatalf("action: got %v want RcUpdated", action)
	}
	got := readFile(t, path)
	want := "export MEMQL_MASTER_KEY=fresh\n"
	if got != want {
		t.Fatalf("content:\n got %q\nwant %q", got, want)
	}
}

func TestReconcileShellRC_IgnoresCommentedLine(t *testing.T) {
	initial := "# MEMQL_MASTER_KEY=oldsample\nalias ll='ls -al'\n"
	path := writeRCFixture(t, "rc-commented", initial, 0o644)
	action, err := ReconcileShellRC(path, "fresh")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if action != RcAppended {
		t.Fatalf("action: got %v want RcAppended (commented line should not match)", action)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "# MEMQL_MASTER_KEY=oldsample") {
		t.Fatalf("commented sample was removed: %q", got)
	}
	if !strings.Contains(got, "export MEMQL_MASTER_KEY=fresh\n") {
		t.Fatalf("new export not appended: %q", got)
	}
}

func TestReconcileShellRC_PreservesMode(t *testing.T) {
	path := writeRCFixture(t, "rc-mode", "export MEMQL_MASTER_KEY=stale\n", 0o644)
	if _, err := ReconcileShellRC(path, "fresh"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode: got %o want 0644", got)
	}
}

func writeRCFixture(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
