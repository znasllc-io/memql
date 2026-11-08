package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStableReleaseSelectionReadsAllPages(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"tag_name":"cockpit/v1.9.0","draft":false,"prerelease":false},{"tag_name":"cockpit/v2.0.0","draft":false,"prerelease":false}]`)
			return
		}
		w.Header().Set("Link", `<`+server.URL+`/releases?page=2>; rel="next"`)
		fmt.Fprint(w, `[{"tag_name":"v999.0.0","draft":false,"prerelease":false},{"tag_name":"cockpit/v3.0.0","draft":true,"prerelease":false},{"tag_name":"cockpit/v4.0.0","draft":false,"prerelease":true},{"tag_name":"cockpit/v7.0.0"},{"tag_name":"cockpit/v1.10.0","draft":false,"prerelease":false}]`)
	}))
	defer server.Close()
	c := &Client{Version: "1.0.0", catalog: server.URL + "/releases", http: server.Client()}
	r, err := c.latest(context.Background())
	if err != nil || r.Tag != "cockpit/v2.0.0" {
		t.Fatalf("got %s, %v", r.Tag, err)
	}
}

func TestPaginationCannotLeaveCatalogOrLoop(t *testing.T) {
	initial := "https://api.github.com/repos/znasllc-io/memql/releases?per_page=100"
	for _, target := range []string{"https://evil.example/releases", "http://api.github.com/repos/znasllc-io/memql/releases?page=2", "https://api.github.com/repos/other/project/releases", "https://user@api.github.com/repos/znasllc-io/memql/releases"} {
		if _, err := nextPage("<"+target+">; rel=\"next\"", initial, initial); err == nil {
			t.Errorf("accepted %s", target)
		}
	}
	valid := "https://api.github.com/repositories/123/releases?page=2"
	if got, err := nextPage("<"+valid+">; rel=\"next\"", initial, initial); err != nil || got != valid {
		t.Fatalf("numeric catalog: %s %v", got, err)
	}
	if _, err := nextPage(`<https://api.github.com/repositories/456/releases?page=3>; rel="next"`, initial, valid); err == nil {
		t.Error("changed numeric repository")
	}
}

func TestVersionsNeverDowngradeAndIgnoreBuildMetadata(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want int
	}{{"1.9.0", "1.10.0", -1}, {"v2.0.0", "1.99.0", 1}, {"1.0.0+gabc.dirty", "1.0.0", 0}, {"999999999999999999999.0.0", "2.0.0", 1}} {
		got, err := compareVersions(test.a, test.b)
		if err != nil || got != test.want {
			t.Errorf("%s vs %s: %d %v", test.a, test.b, got, err)
		}
	}
	for _, bad := range []string{"dev", "1.01.0", "1.0", "1.0.0-rc1", "1.0.0\n"} {
		if _, err := compareVersions(bad, "1.0.0"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

type fixture struct {
	c                           *Client
	worker                      workerStatus
	payload                     []byte
	manifest                    string
	assets                      bool
	restarts, prepares, cancels int
	restartFail                 bool
	prepareFail                 bool
	wrongReady                  bool
	progressChecked             bool
	running                     bool
	config                      string
}

func setupFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "memql")
	if err := os.WriteFile(executable, []byte("memql 1.0.0 (computeruse)\n"), 0755); err != nil {
		t.Fatal(err)
	}
	c, err := New("1.0.0+gabc", "computeruse", executable)
	if err != nil {
		t.Fatal(err)
	}
	busy := false
	f := &fixture{c: c, worker: workerStatus{Version: "1.0.0+gabc", PID: 100, Executable: executable, Busy: &busy, Homes: []homeStatus{{ID: "on", Enabled: true, State: "Connected"}, {ID: "off", Enabled: false, State: "Paused"}}}, payload: []byte("memql 1.1.0+grelease (computeruse)\n"), assets: true, config: filepath.Join(root, "worker.yaml")}
	if err := os.WriteFile(f.config, []byte("keep identity, credentials, homes and paused policy exactly"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(f.payload)
	f.manifest = hex.EncodeToString(digest[:]) + "  memql-computeruse-linux-amd64\n"
	c.OS = "linux"
	c.Arch = "amd64"
	c.bundle = ""
	c.state = filepath.Join(root, "updates")
	c.home = root
	c.readyTimeout = 10 * time.Millisecond
	c.reconnectTimeout = 10 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			assets := []map[string]string{}
			if f.assets {
				assets = []map[string]string{{"name": "memql-computeruse-linux-amd64"}, {"name": "memql-computeruse-linux-amd64.sha256"}}
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"tag_name": "cockpit/v1.1.0", "draft": false, "prerelease": false, "assets": assets}})
		case "/download/cockpit/v1.1.0/memql-computeruse-linux-amd64.sha256":
			fmt.Fprint(w, f.manifest)
		case "/download/cockpit/v1.1.0/memql-computeruse-linux-amd64":
			status := c.Status()
			f.progressChecked = status.OK && status.Phase == "downloading" && status.OperationID != ""
			_, _ = w.Write(f.payload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c.http = server.Client()
	c.catalog = server.URL + "/releases"
	c.downloads = server.URL + "/download/"
	c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" {
			return []byte("{ path=" + executable + " ; argv[]=memql worker run ; }"), nil
		}
		if len(args) == 1 && args[0] == "--version" {
			return os.ReadFile(name)
		}
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	f.running = true
	c.status = func(context.Context) (workerStatus, error) {
		if !f.running {
			return workerStatus{}, errors.New("worker stopped")
		}
		return f.worker, nil
	}
	c.prepare = func(context.Context, int) error {
		f.prepares++
		if f.prepareFail {
			return errors.New("The worker became busy")
		}
		f.worker.UpdatePending = true
		return nil
	}
	c.cancel = func(context.Context, int) error { f.cancels++; return nil }
	c.start = func(context.Context) error {
		f.restarts++
		if f.restartFail && f.restarts == 1 {
			return errors.New("restart refused")
		}
		f.running = true
		body, err := os.ReadFile(executable)
		if err != nil {
			return err
		}
		fields := strings.Fields(string(body))
		f.worker.Version = fields[1]
		f.worker.PID++
		f.worker.UpdatePending = false
		if f.wrongReady && f.restarts == 1 {
			f.worker.Homes = append(f.worker.Homes, homeStatus{ID: "unexpected"})
		} else if f.restarts > 1 && f.wrongReady {
			f.worker.Homes = f.worker.Homes[:2]
		}
		return nil
	}
	c.alive = func(pid int) bool { return f.running && pid == f.worker.PID }
	c.finish = func(_ context.Context, pid int) error {
		if pid != f.worker.PID || !f.worker.UpdatePending {
			return errors.New("worker changed or unreserved")
		}
		f.running = false
		return nil
	}
	return f
}

func TestInstallVerifiesReplacesRestartsAndPreservesHomes(t *testing.T) {
	f := setupFixture(t)
	before, _ := os.ReadFile(f.config)
	r := f.c.Install(context.Background(), "1.1.0")
	if !r.OK || !r.Updated || !r.WorkerReconnected || r.CurrentVersion != "1.1.0" || r.Connected != 1 || r.Total != 1 || r.Phase != "complete" {
		t.Fatalf("%+v", r)
	}
	if f.prepares != 1 || f.restarts != 1 || f.cancels != 0 || !f.progressChecked {
		t.Fatalf("prepare/restart/cancel/progress %d %d %d %v", f.prepares, f.restarts, f.cancels, f.progressChecked)
	}
	after, _ := os.ReadFile(f.config)
	if !bytes.Equal(before, after) {
		t.Error("enrollment/policy changed")
	}
	if f.worker.Homes[1].Enabled || f.worker.Homes[1].State != "Paused" {
		t.Error("paused home resumed")
	}
	status := f.c.Status()
	if !status.OK || status.OperationID != r.OperationID || !status.Updated {
		t.Fatalf("receipt lost: %+v", status)
	}
	_ = f.c.Check(context.Background())
	if status2 := f.c.Status(); status2.OperationID != status.OperationID {
		t.Error("background check overwrote install receipt")
	}
}

func TestInstallReportsOfflineSeparately(t *testing.T) {
	f := setupFixture(t)
	f.worker.Homes[0].State = "Connecting / retrying"
	r := f.c.Install(context.Background(), "1.1.0")
	if !r.OK || !r.Updated || r.WorkerReconnected || r.Connected != 0 || r.Total != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestInstallFailuresLeaveOriginalInPlace(t *testing.T) {
	for _, kind := range []string{"checksum", "variant", "wrong-version", "missing-assets", "busy", "busy-at-reservation", "target-changed", "unsupported-worker"} {
		t.Run(kind, func(t *testing.T) {
			f := setupFixture(t)
			requested := "1.1.0"
			switch kind {
			case "checksum":
				f.payload = []byte("tampered")
			case "variant", "wrong-version":
				if kind == "variant" {
					f.payload = []byte("memql 1.1.0 (headless)\n")
				} else {
					f.payload = []byte("memql 2.0.0 (computeruse)\n")
				}
				sum := sha256.Sum256(f.payload)
				f.manifest = hex.EncodeToString(sum[:]) + "  memql-computeruse-linux-amd64\n"
			case "missing-assets":
				f.assets = false
			case "busy":
				*f.worker.Busy = true
			case "busy-at-reservation":
				f.prepareFail = true
			case "target-changed":
				requested = "1.0.5"
			case "unsupported-worker":
				f.worker.Busy = nil
			}
			r := f.c.Install(context.Background(), requested)
			if r.OK || r.Updated || r.Error == "" {
				t.Fatalf("%+v", r)
			}
			body, _ := os.ReadFile(f.c.Executable)
			if string(body) != "memql 1.0.0 (computeruse)\n" || f.restarts != 0 {
				t.Fatalf("modified old worker: %q restarts %d", body, f.restarts)
			}
		})
	}
}

func TestInstallRetainsTargetAndRecoveryWhenRestartOrHomesCannotBeVerified(t *testing.T) {
	for _, kind := range []string{"restart", "homes"} {
		t.Run(kind, func(t *testing.T) {
			f := setupFixture(t)
			f.restartFail = kind == "restart"
			f.wrongReady = kind == "homes"
			r := f.c.Install(context.Background(), "1.1.0")
			if r.OK || !r.Updated || !r.RestartRequired || !strings.Contains(r.Error, "previous copy is retained") {
				t.Fatalf("%+v", r)
			}
			body, _ := os.ReadFile(f.c.Executable)
			if string(body) != string(f.payload) || f.restarts != 1 {
				t.Fatalf("replacement was rolled back or restarted again: %s restarts %d", body, f.restarts)
			}
			if pending, ok := f.c.pendingRecord(); !ok || pending.Version != "1.1.0" {
				t.Fatalf("recovery journal lost: %+v %v", pending, ok)
			}
		})
	}
}

func TestUpdateLockRejectsConcurrentInstallAndDetectsInterruption(t *testing.T) {
	f := setupFixture(t)
	unlock, err := lockUpdate(f.c.state)
	if err != nil {
		t.Fatal(err)
	}
	r := f.c.Install(context.Background(), "1.1.0")
	if r.OK || !strings.Contains(r.Error, "already running") {
		t.Fatalf("%+v", r)
	}
	progress := f.c.result()
	progress.OK = true
	progress.Phase = "verifying"
	_ = f.c.save(progress)
	if s := f.c.Status(); !s.OK || s.Phase != "verifying" {
		t.Fatalf("live receipt: %+v", s)
	}
	unlock()
	if s := f.c.Status(); s.OK || !strings.Contains(s.Error, "interrupted") {
		t.Fatalf("stale receipt: %+v", s)
	}
}

func TestCheckDoesNotInstallOrWriteState(t *testing.T) {
	f := setupFixture(t)
	r := f.c.Check(context.Background())
	if !r.OK || !r.Available || !r.CanInstall || r.Updated {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(f.c.state); !os.IsNotExist(err) {
		t.Fatal("background check wrote state")
	}
	if f.prepares != 0 || f.restarts != 0 {
		t.Fatal("background check changed worker")
	}
	f.c.Version = "2.0.0"
	r = f.c.Check(context.Background())
	if r.Available || r.CanInstall {
		t.Fatal("offered downgrade")
	}
}

func TestChecksumRequiresOneExactAsset(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, manifest := range []string{sum + "  other\n", sum + "  asset\n" + sum + "  asset\n", "bad  asset\n"} {
		if _, err := expectedChecksum([]byte(manifest), "asset"); err == nil {
			t.Error("accepted invalid manifest")
		}
	}
	if got, err := expectedChecksum([]byte(sum+" *asset\n"), "asset"); err != nil || got != sum {
		t.Fatal(got, err)
	}
}

func TestArchiveRejectsEscapeLinksAndDuplicateFiles(t *testing.T) {
	for _, test := range []struct {
		name      string
		kind      byte
		duplicate bool
	}{{"../outside", tar.TypeReg, false}, {"/outside", tar.TypeReg, false}, {"memql", tar.TypeSymlink, false}, {"memql", tar.TypeLink, false}, {"memql", tar.TypeReg, true}} {
		t.Run(fmt.Sprintf("%s-%d-%v", test.name, test.kind, test.duplicate), func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "asset.tar.gz")
			file, _ := os.Create(archive)
			gz := gzip.NewWriter(file)
			tw := tar.NewWriter(gz)
			header := &tar.Header{Name: test.name, Typeflag: test.kind, Mode: 0755}
			_ = tw.WriteHeader(header)
			if test.duplicate {
				_ = tw.WriteHeader(header)
			}
			_ = tw.Close()
			_ = gz.Close()
			_ = file.Close()
			if err := extract(archive, filepath.Join(root, "out")); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestAtomicSwapAndRestore(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "memql")
	incoming := filepath.Join(root, "new")
	old := filepath.Join(root, "old")
	_ = os.WriteFile(target, []byte("old"), 0755)
	_ = os.WriteFile(incoming, []byte("new"), 0755)
	if err := swap(target, incoming, old); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(target); string(body) != "new" {
		t.Fatal(string(body))
	}
	if err := restore(target, old); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(target); string(body) != "old" {
		t.Fatal(string(body))
	}
}

func TestServiceExecutableRequiresExactMatch(t *testing.T) {
	if !systemdProgram("{ path=/opt/memql ; argv[]=x ; }", "/opt/memql") || systemdProgram("{ path=/opt/memql-old ; }", "/opt/memql") {
		t.Fatal("systemd path match")
	}
	if !launchdProgram("\tprogram = /Applications/MemQL.app/Contents/MacOS/MemQL\n", "/Applications/MemQL.app/Contents/MacOS/MemQL") || launchdProgram("program = /tmp/other/memql", "/tmp/memql") {
		t.Fatal("launchd path match")
	}
}
