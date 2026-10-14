package consent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupTestServer(t *testing.T) (*Server, *Manager, *Client) {
	t.Helper()
	// macOS caps Unix socket paths at 104 chars; t.TempDir's
	// /var/folders/<...>/<TestName>/<seq> can blow past that.
	// Drop the socket in /tmp under a 6-char random name to stay
	// well within budget.
	dir, err := os.MkdirTemp("/tmp", "ws")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "w.sock")

	homes := NewHomes()
	mgr := homes.For("home-a")
	srv := NewServer(homes, path, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(srv.Close)

	client := &Client{Path: path, Timeout: time.Second}
	return srv, mgr, client
}

func TestSocket_StatusRoundTrip(t *testing.T) {
	_, _, client := setupTestServer(t)

	resp, err := client.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected OK; got %+v", resp)
	}
	if resp.Status.Granted {
		t.Errorf("fresh manager should report Granted=false; got %+v", resp.Status)
	}
}

func TestSocket_GrantThenStatus(t *testing.T) {
	_, _, client := setupTestServer(t)

	r, err := client.Grant(30*time.Minute, false, nil)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !r.OK || !r.Status.Granted {
		t.Errorf("Grant reply did not signal an open window: %+v", r)
	}
	r, err = client.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !r.Status.Granted {
		t.Errorf("post-Grant status reports Granted=false: %+v", r)
	}
}

func TestSocket_RevokeViaIPC(t *testing.T) {
	_, _, client := setupTestServer(t)

	if _, err := client.Grant(time.Hour, false, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	r, err := client.Revoke()
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !r.OK || r.Status.Granted {
		t.Errorf("Revoke reply did not close the window: %+v", r)
	}
}

func TestSocket_RevokeReportsStorageFailureAndStillStopsEveryHome(t *testing.T) {
	srv, a, client := setupTestServer(t)
	state := t.TempDir()
	a.loadDirectoryPause(state, a.Home())
	b := srv.homes.For("home-b")
	bad := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(bad, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	b.loadDirectoryPause(bad, b.Home())
	r, err := client.Revoke()
	if err != nil {
		t.Fatal(err)
	}
	if r.OK || !strings.Contains(r.Error, "could not be saved") {
		t.Fatalf("failed save acknowledged as durable: %+v", r)
	}
	for _, m := range []*Manager{a, b} {
		if m.AllowsDirectoryRead("fs_list").Allowed {
			t.Fatalf("failed revoke left %s open", m.Home())
		}
	}
	if NewPersistentHomes(state).For("home-a").AllowsDirectoryRead("fs_list").Allowed {
		t.Fatal("successful home stop was not retained after sibling failure")
	}
	client.Home = "home-a"
	r, err = client.Grant(time.Minute, false, nil)
	if err != nil || !r.OK || r.Status.DirectoryReadsPaused {
		t.Fatalf("local resume failed: %+v %v", r, err)
	}
	if b.AllowsDirectoryRead("fs_list").Allowed {
		t.Fatal("resume leaked into another cluster")
	}
}

func TestSocket_GrantNegativeRejected(t *testing.T) {
	_, _, client := setupTestServer(t)
	_, err := client.Grant(-time.Second, false, nil)
	if err == nil {
		t.Error("client must reject negative window pre-dial")
	}
}

func TestSocket_GrantThroughManagerVisibleToServer(t *testing.T) {
	_, mgr, client := setupTestServer(t)

	// Grant directly on the manager (simulating a TUI thread holding
	// the Manager pointer) and confirm the IPC reports it.
	if _, err := mgr.Grant(time.Hour, true, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	r, err := client.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !r.Status.Granted {
		t.Errorf("manager-side grant invisible via socket: %+v", r)
	}
	if !r.Status.Strict {
		t.Errorf("strict flag did not survive the socket: %+v", r)
	}
}

func TestSocket_WatchStreamsEvents(t *testing.T) {
	_, mgr, client := setupTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu       sync.Mutex
		received []map[string]any
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.Watch(ctx, func(line []byte) {
			var obj map[string]any
			if err := json.Unmarshal(line, &obj); err != nil {
				return
			}
			mu.Lock()
			received = append(received, obj)
			mu.Unlock()
		})
	}()

	// Give the WATCH connection time to register before we trigger
	// events; the server pushes an initial snapshot then events.
	time.Sleep(100 * time.Millisecond)

	if _, err := mgr.Grant(time.Hour, false, nil); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	mgr.Allows("workerHost", "exec")
	mgr.Revoke()

	// Wait briefly for events to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		// initial status + 3 events = 4 lines
		if n >= 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(received) < 4 {
		t.Fatalf("expected >= 4 WATCH lines (initial status + 3 events); got %d: %+v", len(received), received)
	}
	// First line is the initial Response with status; subsequent
	// lines are raw Event objects. Spot-check we saw a granted +
	// dispatch + revoked.
	var kinds []string
	for _, line := range received[1:] {
		if k, _ := line["kind"].(string); k != "" {
			kinds = append(kinds, k)
		}
	}
	want := []string{"granted", "dispatch", "revoked"}
	for _, k := range want {
		found := false
		for _, got := range kinds {
			if got == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing WATCH event kind %q (got %v)", k, kinds)
		}
	}
}

func TestSocket_RejectsUnknownOp(t *testing.T) {
	_, _, client := setupTestServer(t)
	// Manually craft a bad-op request via the exec path.
	r, err := client.exec(Request{Op: "bogus"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if r.OK {
		t.Fatalf("unknown op should yield OK=false; got %+v", r)
	}
	if !strings.Contains(r.Error, "unknown op") {
		t.Errorf("error message %q should name the op", r.Error)
	}
}

func TestSocket_PathPermissions(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	info, err := os.Stat(srv.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Socket should be 0600 -- only the running user can connect.
	if info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %o, want 0600", info.Mode().Perm())
	}
}
