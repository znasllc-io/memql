//go:build linux || darwin

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/auth"
)

func TestMissingSignInWarnsOnceAndResumesPolicyReporting(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	f.watches = []map[string]any{watchRow("watch", root, nil)}
	writeFile(t, filepath.Join(root, "private.txt"), 12)
	var logs bytes.Buffer
	loginErr := fmt.Errorf("wrapped sign-in: %w", auth.ErrLoginRequired)
	var bearerErr error = loginErr
	lookups := 0
	m := New(Options{
		StateDir: t.TempDir(), BaseURL: f.server.URL, HTTPClient: f.server.Client(),
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
		Bearer:    func(context.Context) (string, error) { lookups++; return "test-token", bearerErr },
		CheckPath: func(string) error { return errors.New("not in backup.roots") },
	})
	for range 3 {
		m.sweep(context.Background(), "wkr-1", false)
	}
	if lookups != 3 || len(f.graphCalls("libraryWatchedFolders")) != 0 {
		t.Fatal("missing sign-in must retry locally without contacting the graph")
	}
	// An unrelated refresh/network failure remains visible and must not be
	// mistaken for successful sign-in or clear the outstanding episode.
	bearerErr = errors.New("identity temporarily unavailable")
	m.sweep(context.Background(), "wkr-1", false)
	bearerErr = loginErr
	m.sweep(context.Background(), "wkr-1", false)
	if got := signinLogCount(t, &logs, "WARN", "backup: sign-in required; worker connection is unaffected"); got != 1 {
		t.Fatalf("one warning per continuous missing sign-in, got %d", got)
	}
	if got := signinLogCount(t, &logs, "WARN", "backup: could not read this machine's watched folders"); got != 1 {
		t.Fatalf("unrelated failure was hidden: %d", got)
	}
	if got := signinLogCount(t, &logs, "INFO", "backup: sign-in available again; checking watched folders"); got != 0 {
		t.Fatal("reported recovery without usable watched-folder access")
	}
	// The next ordinary pass picks up a new sign-in. It must still tell the
	// cluster about a denied path, even when this machine allows no folders.
	bearerErr = nil
	m.sweep(context.Background(), "wkr-1", false)
	m.sweep(context.Background(), "wkr-1", false)
	if got := signinLogCount(t, &logs, "INFO", "backup: sign-in available again; checking watched folders"); got != 1 {
		t.Fatalf("expected one successful recovery, got %d", got)
	}
	if got := f.graphCalls("reportLibraryWatchedFolderSweep"); len(got) != 2 || !strings.Contains(got[0], `"refused_by_policy"`) {
		t.Fatalf("policy refusal no longer reported: %v", got)
	}
	if len(f.uploadedPaths()) != 0 {
		t.Fatal("sign-in recovery bypassed local path consent")
	}
	bearerErr = loginErr
	m.sweep(context.Background(), "wkr-1", false)
	if got := signinLogCount(t, &logs, "WARN", "backup: sign-in required; worker connection is unaffected"); got != 2 {
		t.Fatalf("a new missing sign-in episode must be visible: %d", got)
	}
}

func signinLogCount(t *testing.T, logs *bytes.Buffer, level, message string) int {
	t.Helper()
	count := 0
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var entry struct{ Level, Msg string }
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Level == level && entry.Msg == message {
			count++
		}
	}
	return count
}
