package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func controlFixture(t *testing.T) (*Fleet, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "workers.yaml")
	body := `version: 1
worker_name: fixture
capabilities: [HEADLESS]
custom_setting: preserve-me
homes:
  - id: a
    cluster_url: https://a.example
    token: mql_wkr_aaaaaaaaaaaaaaaaaaaaaaa
  - id: b
    cluster_url: https://b.example
    token: mql_wkr_bbbbbbbbbbbbbbbbbbbbbbb
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	workers, err := LoadWorkers(path, "")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := NewFleet(FleetOptions{Workers: workers, WorkersPath: path})
	if err != nil {
		t.Fatal(err)
	}
	return fleet, path
}

func eventuallyControl(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestLocalPausePersistsAndIsolatesOtherHomes(t *testing.T) {
	fleet, path := controlFixture(t)
	var mu sync.Mutex
	starts, stops := map[string]int{}, map[string]int{}
	fleet.runHome = func(ctx context.Context, h *managedHome) error {
		mu.Lock()
		starts[h.home.ID]++
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		stops[h.home.ID]++
		mu.Unlock()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { fleet.Run(ctx); close(done) }()
	eventuallyControl(t, func() bool { mu.Lock(); defer mu.Unlock(); return starts["a"] == 1 && starts["b"] == 1 })
	if err := fleet.SetHomeEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	eventuallyControl(t, func() bool { mu.Lock(); defer mu.Unlock(); return stops["a"] == 1 })
	mu.Lock()
	if stops["b"] != 0 || starts["a"] != 1 {
		t.Error("pause interrupted sibling or restarted paused home")
	}
	mu.Unlock()
	data, _ := os.ReadFile(path)
	for _, want := range []string{"custom_setting: preserve-me", "mql_wkr_aaaaaaaaaaaaaaaaaaaaaaa", "mql_wkr_bbbbbbbbbbbbbbbbbbbbbbb"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("pause lost %s", want)
		}
	}
	workers, err := LoadWorkers(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if workers.Homes[0].IsEnabled() || !workers.Homes[1].IsEnabled() {
		t.Fatal("persisted state is incorrect")
	}
	if err := fleet.SetHomeEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	workers, err = LoadWorkers(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = workers.ValidateRun(); err != nil {
		t.Fatalf("all paused must remain controllable after relaunch: %v", err)
	}
	cancel()
	<-done
	// Relaunch using the persisted registry: no home may connect until resumed.
	relaunched, err := NewFleet(FleetOptions{Workers: workers, WorkersPath: path})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	relaunched.runHome = func(ctx context.Context, h *managedHome) error { started <- h.home.ID; <-ctx.Done(); return ctx.Err() }
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := make(chan struct{})
	go func() { relaunched.Run(ctx2); close(done2) }()
	eventuallyControl(t, func() bool { relaunched.mu.Lock(); defer relaunched.mu.Unlock(); return len(relaunched.managed) == 2 })
	select {
	case id := <-started:
		t.Fatalf("paused %s connected on relaunch", id)
	case <-time.After(50 * time.Millisecond):
	}
	if err := relaunched.SetHomeEnabled("a", true); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		if id != "a" {
			t.Fatalf("resumed wrong home %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("resume did not start home")
	}
	cancel2()
	<-done2
}

func TestLocalControlSocketOwnershipAndNoDuplicateWorker(t *testing.T) {
	fleet, _ := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A pipeline's owned TMPDIR can exceed the Unix socket path limit.
	// MkdirTemp still creates a private directory; cleanup owns only this one.
	dir, err := os.MkdirTemp("/tmp", "mq-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "private", "worker.sock")
	listener, err := listenControl(ctx, path, fleet)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("socket must be mode0600")
	}
	response, err := callControl(path, controlRequest{Action: "status"})
	if err != nil || !response.OK {
		t.Fatalf("%+v %v", response, err)
	}
	body, _ := json.Marshal(response)
	if strings.Contains(string(body), "mql_wkr_") {
		t.Fatal("status exposed worker token")
	}
	if _, err := listenControl(ctx, path, fleet); err == nil {
		t.Fatal("second worker must not take over socket")
	}
	if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := callControl(path, controlRequest{Action: "status"}); err == nil {
		t.Fatal("insecure directory accepted")
	}
}

func TestLocalControlRejectsInvalidRequests(t *testing.T) {
	fleet, _ := controlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Keep the socket path short even when the native runner's TMPDIR is long.
	dir, err := os.MkdirTemp("/tmp", "mq-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "private", "worker.sock")
	l, err := listenControl(ctx, path, fleet)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, request := range []string{`{"action":"status","file":"/etc/passwd"}`, `{"action":"home","home":"a"}`, `{"action":"delete"}`, `{"action":"request-permission","permission":"screen_recording"}`, `{"action":"request-permission","permission":"all","pid":1}`} {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write([]byte(request + "\n"))
		var response controlResponse
		err = json.NewDecoder(conn).Decode(&response)
		conn.Close()
		if err != nil || response.OK {
			t.Fatalf("request %s: %+v %v", request, response, err)
		}
	}
}

func TestLocalLogsAreBoundedRedactedAndFixedToOwnedFile(t *testing.T) {
	dir := t.TempDir()
	line := `{"time":"2026-09-15T12:00:00Z","level":"WARN","home":"a","msg":"connect failed","error":"Bearer abc-secret https://example.test/magic/private-token?token=abc mql_wkr_abcdefghijklmnopqrstuvwxyz","token":"must-not-appear","payload":"private prompt"}`
	if err := os.WriteFile(filepath.Join(dir, "worker.log"), []byte(strings.Repeat(line+"\n", 1000)), 0600); err != nil {
		t.Fatal(err)
	}
	logs, err := readLocalWorkerLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) > 300 || len(logs) == 0 {
		t.Fatalf("entry count %d", len(logs))
	}
	encoded, _ := json.Marshal(logs)
	for _, secret := range []string{"abc-secret", "private-token", "abcdefghijklmnopqrstuvwxyz", "must-not-appear", "private prompt"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("log leaked %s", secret)
		}
	}
	if !strings.Contains(string(encoded), "connect failed") {
		t.Fatal("lost useful diagnostics")
	}
	os.Remove(filepath.Join(dir, "worker.log"))
	os.Symlink("/etc/passwd", filepath.Join(dir, "worker.log"))
	if _, err := readLocalWorkerLogs(dir); err == nil {
		t.Fatal("log symlink must be refused")
	}
}

func TestSaveHomeEnabledRefusesUnknownHomeWithoutChangingFile(t *testing.T) {
	_, path := controlFixture(t)
	before, _ := os.ReadFile(path)
	if err := saveHomeEnabled(path, "not-enrolled", false); err == nil {
		t.Fatal("unknown home accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed update changed configuration")
	}
}

func TestHomeOSURLRequiresExplicitOrKnownLocalAddress(t *testing.T) {
	for _, tc := range []struct {
		home Home
		want string
	}{
		{Home{ClusterURL: "https://api.memql.localhost"}, "https://os.memql.localhost/"},
		{Home{ClusterURL: "https://api.example.test"}, ""},
		{Home{OSURL: "https://workspace.example.test/"}, "https://workspace.example.test/"},
		{Home{OSURL: "https://user:secret@example.test/"}, ""},
		{Home{OSURL: "https://example.test/?token=secret"}, ""},
		{Home{OSURL: "file:///etc/passwd"}, ""},
	} {
		if got := homeOSURL(tc.home); got != tc.want {
			t.Fatalf("OS URL %q want %q", got, tc.want)
		}
	}
}

// Older registries may contain duplicate enrollments. A menu pause must
// never make a previously suppressed enrollment reconnect after relaunch.
func TestLocalControlRefusesDuplicateEnrollmentPolicyChanges(t *testing.T) {
	f, path := controlFixture(t)
	f.workers.Homes[1].ClusterURL = "https://a.example:443"
	f.workers.Homes[1].ID = "newer"
	f.runHome = func(ctx context.Context, h *managedHome) error { <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	eventuallyControl(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.managed) == 2 })
	before, _ := os.ReadFile(path)
	for _, id := range []string{"a", "newer"} {
		if err := f.SetHomeEnabled(id, false); err == nil {
			t.Fatalf("ambiguous pause %s accepted", id)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("refused pause changed disk")
	}
	status := f.LocalStatus()
	if status.Homes[0].State != "Duplicate of newer" {
		t.Fatalf("duplicate status: %+v", status.Homes)
	}
}

func TestLocalSupervisorRecoversAnExitedHomeWithoutRestartingSibling(t *testing.T) {
	f, _ := controlFixture(t)
	var mu sync.Mutex
	starts := map[string]int{}
	f.runHome = func(ctx context.Context, h *managedHome) error {
		mu.Lock()
		starts[h.home.ID]++
		n := starts[h.home.ID]
		mu.Unlock()
		if h.home.ID == "a" && n == 1 {
			return errors.New("runner ended")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	eventuallyControl(t, func() bool { return f.LocalStatus().Homes[0].State == "Disconnected / retrying" })
	eventuallyControl(t, func() bool { mu.Lock(); defer mu.Unlock(); return starts["a"] == 2 })
	mu.Lock()
	defer mu.Unlock()
	if starts["b"] != 1 {
		t.Fatalf("sibling restarted %d times", starts["b"])
	}
}
