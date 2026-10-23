//go:build linux || darwin

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The watched-folder sweeper (memql#4841).
//
// The fake below answers the engine's REAL routes -- /memql/query for the
// graph half and the /artifacts family for the bytes -- because everything
// worth testing here is a contract with those routes. A double that stubbed
// this package's own methods would test that the methods were called and
// nothing about the calls the engine has to accept.

type fakeEngine struct {
	mu sync.Mutex

	watches []map[string]any
	// calls records the MemQL construct text of every graph call, so a test
	// can assert what was SENT rather than what a mock remembered.
	calls []string
	// uploads records every file that arrived, by uploadedFromPath.
	uploads map[string]int64
	// chunks records the chunk numbers a session received.
	chunks map[string][]int
	// refuseUpload makes /artifacts answer a status.
	refuseUpload int
	// oneShotOnly rejects the session route, so a test can prove which route
	// a size took.
	sessionSize int64
	// holdAfter, when set, makes every one-shot upload after the first
	// holdAfter hang until the client gives up -- a sweep interrupted in
	// the middle of a push, with the uploads before it fully answered.
	// holding is signalled when one starts hanging.
	holdAfter int
	holding   chan struct{}
	served    int

	server *httptest.Server
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	f := &fakeEngine{uploads: map[string]int64{}, chunks: map[string][]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/memql/query", f.handleQuery)
	mux.HandleFunc("/artifacts", f.handleOneShot)
	mux.HandleFunc("/artifacts/uploads", f.handleInit)
	mux.HandleFunc("/artifacts/uploads/", f.handleSession)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeEngine) handleQuery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls = append(f.calls, body.Query)
	watches := f.watches
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(body.Query, "query libraryWatchedFolders"):
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"data": watches}})
	default:
		// Mutations answer an empty result, which is what the gateway does.
		_, _ = w.Write([]byte(`{"result":{}}`))
	}
}

func (f *fakeEngine) handleOneShot(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hold := f.holdAfter > 0 && f.served >= f.holdAfter
	f.served++
	f.mu.Unlock()
	if hold {
		// The body is read FIRST: Go's server only notices a client that
		// hung up once the request body is consumed, and a handler parked
		// on an unread body would outlive the test and hold
		// httptest.Server.Close forever.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case f.holding <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return
	}
	if f.refuseUpload != 0 {
		http.Error(w, "the worker registration is not one of your machines", f.refuseUpload)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := r.FormValue("uploadedFromPath")
	if r.FormValue("backupWatchId") == "" {
		http.Error(w, "backup scope required", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	n, _ := io.Copy(io.Discard, file)
	f.mu.Lock()
	f.uploads[path] = n
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(PushResult{ArtifactID: "a-1", FileID: "f-" + filepath.Base(path)})
}

func (f *fakeEngine) handleInit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		Path    string `json:"uploadedFromPath"`
		WatchID string `json:"backupWatchId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	if body.WatchID == "" {
		f.mu.Unlock()
		http.Error(w, "backup scope required", http.StatusBadRequest)
		return
	}
	f.sessionSize = body.Size
	f.uploads[body.Path] = 0
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	// A DIFFERENT chunk size from the client's own constant, deliberately:
	// the session's size is the server's to name, and a client that used its
	// own would commit a block list the server counts differently.
	_ = json.NewEncoder(w).Encode(initResponse{UploadID: "u-1", ChunkSize: 1024})
}

func (f *fakeEngine) handleSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/artifacts/uploads/")
	parts := strings.Split(rest, "/")
	uploadID := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		f.mu.Lock()
		staged := []stagedChunk{}
		for _, n := range f.chunks[uploadID] {
			staged = append(staged, stagedChunk{N: n, Size: 1024})
		}
		size := f.sessionSize
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(inventoryResponse{UploadID: uploadID, Status: "open", Size: size, ChunkSize: 1024, Staged: staged})
	case len(parts) == 3 && parts[1] == "chunks" && r.Method == http.MethodPut:
		n, _ := strconv.Atoi(parts[2])
		written, _ := io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.chunks[uploadID] = append(f.chunks[uploadID], n)
		for path := range f.uploads {
			f.uploads[path] += written
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[1] == "complete" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(PushResult{ArtifactID: "a-1", FileID: "f-session"})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeEngine) graphCalls(construct string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, call := range f.calls {
		if strings.Contains(call, construct+"(") {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeEngine) uploadedPaths() map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for k, v := range f.uploads {
		out[k] = v
	}
	return out
}

func managerFor(t *testing.T, f *fakeEngine, root string, check func(string) error) *Manager {
	t.Helper()
	m := New(Options{
		StateDir:   t.TempDir(),
		BaseURL:    f.server.URL,
		Bearer:     func(context.Context) (string, error) { return "token", nil },
		CheckPath:  check,
		HTTPClient: f.server.Client(),
	})
	if m == nil {
		t.Fatal("New returned nil with a base URL and a bearer -- the manager disabled itself")
	}
	_ = root
	return m
}

func watchRow(id, path string, over map[string]any) map[string]any {
	row := map[string]any{
		"id": id, "workerId": "wkr-1", "localPath": path,
		"folderId": "", "status": "active", "includeHidden": false,
	}
	for k, v := range over {
		row[k] = v
	}
	return row
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, size)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func allow(string) error { return nil }

// ---------------------------------------------------------------------------

// THE VETO IS THE LOAD-BEARING RULE. A watch row is written from a browser, so
// its path is one the cluster names on somebody else's machine; without this
// check, anybody who could write a watch could have ~/.ssh uploaded.
func TestAPathThisMachineHasNotAllowedIsRefusedAndReported(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "secret.txt"), 16)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, func(string) error { return fmt.Errorf("not in backup.roots") })
	m.SweepOnce(context.Background(), "wkr-1")

	if got := f.uploadedPaths(); len(got) != 0 {
		t.Fatalf("a refused folder was uploaded anyway: %v", got)
	}
	reports := f.graphCalls("reportLibraryWatchedFolderSweep")
	if len(reports) != 1 {
		t.Fatalf("want exactly one sweep report, got %d: %v", len(reports), reports)
	}
	// REPORTED, not silent. A machine that quietly ignored a watch would be
	// indistinguishable from one that was offline, and the person would have
	// nothing to act on.
	if !strings.Contains(reports[0], `"refused_by_policy"`) {
		t.Errorf("the refusal was not reported as refused_by_policy: %s", reports[0])
	}
	if !strings.Contains(reports[0], "not in backup.roots") {
		t.Errorf("the machine's own reason was not carried to the cluster: %s", reports[0])
	}
}

// A nil check is a wiring mistake, and it must fail CLOSED.
func TestNoPolicyAtAllBacksUpNothing(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 8)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, nil)
	m.SweepOnce(context.Background(), "wkr-1")

	if got := f.uploadedPaths(); len(got) != 0 {
		t.Fatalf("a manager with no policy uploaded files: %v", got)
	}
}

func TestASweepPushesEveryFileOnceAndThenStopsPushing(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 32)
	writeFile(t, filepath.Join(root, "nested", "b.txt"), 64)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")

	uploaded := f.uploadedPaths()
	if len(uploaded) != 2 {
		t.Fatalf("want both files uploaded, got %v", uploaded)
	}

	// THE SECOND SWEEP IS THE POINT. Every sweep after the first must cost
	// stat calls and no bytes, or a folder of client video would be re-sent
	// every five minutes forever.
	before := len(f.uploadedPaths())
	m.SweepOnce(context.Background(), "wkr-1")
	if after := len(f.uploadedPaths()); after != before {
		t.Errorf("an unchanged folder was re-uploaded: %d files before, %d after", before, after)
	}
}

func TestAChangedFileIsPushedAgainAndATouchedOneIsNot(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, 32)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")
	if f.uploadedPaths()[path] != 32 {
		t.Fatalf("first push did not land: %v", f.uploadedPaths())
	}

	// A TOUCH with identical bytes: the cheap check moves, the digest does
	// not, and nothing is sent. This is the case that would otherwise re-push
	// a whole folder because a sync tool rewrote timestamps.
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	m.SweepOnce(context.Background(), "wkr-1")
	if got := f.uploadedPaths()[path]; got != 32 {
		t.Errorf("a touched file with unchanged bytes was re-uploaded (%d bytes recorded)", got)
	}

	// Real new bytes DO go.
	writeFile(t, path, 48)
	m.SweepOnce(context.Background(), "wkr-1")
	if got := f.uploadedPaths()[path]; got != 48 {
		t.Errorf("changed bytes were not re-uploaded: recorded %d, want 48", got)
	}
}

// A big file takes the SESSION route, and the chunking uses the SERVER's size.
func TestAFileOverTheThresholdTakesTheSessionRouteWithTheServersChunkSize(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "big.mov")
	writeFile(t, path, 4096)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	// Lower the threshold rather than writing 32 MiB, so this exercises the
	// ROUTE CHOICE in Push and not just the session code beneath it.
	m.library.oneShotLimit = 1024
	m.SweepOnce(context.Background(), "wkr-1")

	f.mu.Lock()
	got := append([]int(nil), f.chunks["u-1"]...)
	f.mu.Unlock()
	// 4096 bytes at the SERVER's 1024 = four chunks, 1-based. A client that
	// used its own 16 MiB constant would have sent exactly one.
	if len(got) != 4 {
		t.Fatalf("want 4 chunks at the server's chunk size, got %d (%v)", len(got), got)
	}
	if got[0] != 1 {
		t.Errorf("chunks are 1-based on the wire; first was %d", got[0])
	}
}

// Every push names its machine and its path, which is what makes a re-push a
// new VERSION rather than a duplicate.
func TestEveryPushNamesItsMachineAndPath(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, 8)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")

	if _, ok := f.uploadedPaths()[path]; !ok {
		t.Fatalf("the upload did not carry uploadedFromPath; recorded: %v", f.uploadedPaths())
	}
}

// A vanished file is FLAGGED, never removed -- the epic's invariant.
func TestAFileDeletedAtTheOriginIsFlaggedAndNothingIsDeleted(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, 8)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m.SweepOnce(context.Background(), "wkr-1")

	states := f.graphCalls("setLibraryFileLinkState")
	if len(states) == 0 {
		t.Fatal("nothing reported the file as gone from the machine")
	}
	last := states[len(states)-1]
	if !strings.Contains(last, `"origin_gone"`) {
		t.Errorf("want origin_gone, got: %s", last)
	}
	// NOTHING removes the copy. There is no archive, no delete, no supersede
	// anywhere in this package, and this is the assertion that says so.
	for _, call := range f.calls {
		for _, forbidden := range []string{"archive", "delete", "Delete", "Archive"} {
			if strings.Contains(constructOf(call), forbidden) {
				t.Errorf("the sweeper sent a destructive call, which the one-way invariant forbids: %s", call)
			}
		}
	}
}

// A paused watch is read and left entirely alone.
func TestAPausedWatchIsNotSweptAndIsNotReported(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 8)
	f.watches = []map[string]any{watchRow("w-1", root, map[string]any{"status": "paused"})}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")

	if got := f.uploadedPaths(); len(got) != 0 {
		t.Errorf("a paused watch uploaded files: %v", got)
	}
	if reports := f.graphCalls("reportLibraryWatchedFolderSweep"); len(reports) != 0 {
		t.Errorf("a paused watch reported a sweep it did not run: %v", reports)
	}
}

// A missing folder is `missing`, and it is told apart from unreadable.
func TestAMissingFolderIsReportedAsMissing(t *testing.T) {
	f := newFakeEngine(t)
	f.watches = []map[string]any{watchRow("w-1", filepath.Join(t.TempDir(), "gone"), nil)}

	m := managerFor(t, f, "", allow)
	m.SweepOnce(context.Background(), "wkr-1")

	reports := f.graphCalls("reportLibraryWatchedFolderSweep")
	if len(reports) != 1 || !strings.Contains(reports[0], `"missing"`) {
		t.Fatalf("want one report of missing, got: %v", reports)
	}
}

// A refusal arrives as HTTP 200 with an `errors` array, and a client that only
// read the status code would treat "you may not see these" as "you are
// watching nothing" -- a backup with nothing to do looks exactly like one that
// is up to date.
func TestA200WithErrorsIsARefusalNotAnEmptyList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/memql/query", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"message":"actor rank below the floor"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := NewGraph(srv.URL, srv.Client(), func(context.Context) (string, error) { return "t", nil })
	_, err := g.Watches(context.Background(), "wkr-1")
	if err == nil {
		t.Fatal("a 200 carrying an errors array was read as success")
	}
	if !strings.Contains(err.Error(), "actor rank below the floor") {
		t.Errorf("the server's own sentence was not carried: %v", err)
	}
}

// A 401 names the repair, because it is the single most common reason this
// feature does nothing and "unauthorized" alone is a diagnosis with no
// treatment.
func TestA401SaysWhatToDoAboutIt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/memql/query", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := NewGraph(srv.URL, srv.Client(), func(context.Context) (string, error) { return "t", nil })
	_, err := g.Watches(context.Background(), "wkr-1")
	if err == nil {
		t.Fatal("a 401 was not an error")
	}
	if !strings.Contains(err.Error(), "memql login") {
		t.Errorf("the 401 does not name the repair: %v", err)
	}
}

// The sweep must never claim a check time: `lastSweepAt` is server-stamped, so
// a caller cannot say it looked when it did not.
func TestTheSweepReportNeverSendsItsOwnCheckTime(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	f.watches = []map[string]any{watchRow("w-1", root, nil)}
	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")

	for _, call := range f.graphCalls("reportLibraryWatchedFolderSweep") {
		if strings.Contains(call, "lastSweepAt") {
			t.Errorf("the sweep named its own check time, which the mutation stamps: %s", call)
		}
	}
}

// ---------------------------------------------------------------------------
// Regressions from the review pass. Each of these shipped in the first draft
// and each was silent -- which is why they are named for the wrong OUTCOME
// rather than for the code.
// ---------------------------------------------------------------------------

// A sweep with no registration id used to read every row as "nothing to do"
// and report success. Now it refuses, loudly, and touches nothing.
func TestASweepWithNoRegistrationIdRefusesInsteadOfReportingSuccess(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 8)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "")

	if len(f.calls) != 0 {
		t.Errorf("a sweep with no id talked to the cluster: %v", f.calls)
	}
	if got := f.uploadedPaths(); len(got) != 0 {
		t.Errorf("a sweep with no id uploaded files: %v", got)
	}
}

// Asking for EVERY watch has to omit the argument. The engine's `when()` guard
// drops an ABSENT argument, not an empty string -- `workerId: ""` is a present
// value that matches no row, which read exactly like a person having set no
// backups up.
func TestAskingForEveryWatchOmitsTheArgumentRatherThanSendingAnEmptyOne(t *testing.T) {
	f := newFakeEngine(t)
	g := NewGraph(f.server.URL, f.server.Client(), func(context.Context) (string, error) { return "t", nil })

	if _, err := g.Watches(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Watches(context.Background(), "wkr-1"); err != nil {
		t.Fatal(err)
	}
	calls := f.graphCalls("libraryWatchedFolders")
	if len(calls) != 2 {
		t.Fatalf("want two reads, got %v", calls)
	}
	if calls[0] != "query libraryWatchedFolders()" {
		t.Errorf("a blank id must omit the argument entirely, got: %s", calls[0])
	}
	// The reachable positive: a real id still narrows, so the assertion above
	// is about the blank case rather than about the argument having been
	// dropped altogether.
	if !strings.Contains(calls[1], `workerId: "wkr-1"`) {
		t.Errorf("a named machine must still narrow the read, got: %s", calls[1])
	}
}

// A file that GREW between the walk and the push must not be declared at its
// old size: the session route would send only that many bytes, the engine's
// commit check would pass (staged == declared), and the copy would be
// truncated -- then never repaired, because the digest already matched.
func TestAFileThatGrewAfterTheWalkIsPushedWhole(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "render.mov")
	writeFile(t, path, 512)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.library.oneShotLimit = 256 // force the session route at this size

	// The walk's view: 512 bytes. Then the file finishes being written.
	scan, err := Scan(root, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Entries) != 1 {
		t.Fatalf("want one entry, got %d", len(scan.Entries))
	}
	writeFile(t, path, 4096)

	ledger := LoadLedger(t.TempDir(), "w-1")
	m.workerID = "wkr-1"
	if _, err := m.pushIfChanged(context.Background(), Watch{ID: "w-1"}, ledger, scan.Entries[0]); err != nil {
		t.Fatalf("push: %v", err)
	}

	f.mu.Lock()
	declared := f.sessionSize
	f.mu.Unlock()
	if declared != 4096 {
		t.Errorf("declared size = %d, want 4096 -- the stale walk-time size would truncate the copy", declared)
	}
	// And the recorded stamp is the one that was actually sent, so the next
	// sweep compares like with like.
	rec, ok := ledger.Get(path)
	if !ok || rec.Stamp.Size != 4096 {
		t.Errorf("ledger recorded size %d, want 4096", rec.Stamp.Size)
	}
}

// `stale` had no writer at all: a file whose push failed kept reporting
// `synced`, so the Library went on claiming a copy was current when this
// machine knew it was not.
func TestAFileWhosePushFailedIsReportedStaleRatherThanSynced(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, 32)
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")
	if _, ok := f.uploadedPaths()[path]; !ok {
		t.Fatal("the first push did not land, so this test measures nothing")
	}

	// The file changes and every upload from here on is refused.
	writeFile(t, path, 64)
	f.mu.Lock()
	f.refuseUpload = http.StatusInsufficientStorage
	f.mu.Unlock()

	m.SweepOnce(context.Background(), "wkr-1")

	states := f.graphCalls("setLibraryFileLinkState")
	if len(states) == 0 {
		t.Fatal("nothing reported a state after a failed push")
	}
	last := states[len(states)-1]
	if !strings.Contains(last, `"stale"`) {
		t.Errorf("want stale after a failed push, got: %s", last)
	}
}

// An interrupted sweep must keep the record of what it already sent, or the
// unit of loss is a whole sweep and the next run re-pushes everything.
func TestAnInterruptedSweepStillSavesWhatItAlreadySent(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(t, filepath.Join(root, name), 8)
	}
	f.watches = []map[string]any{watchRow("w-1", root, nil)}

	stateDir := t.TempDir()
	m := New(Options{
		StateDir:   stateDir,
		BaseURL:    f.server.URL,
		Bearer:     func(context.Context) (string, error) { return "token", nil },
		CheckPath:  allow,
		HTTPClient: f.server.Client(),
	})
	// The first upload is answered in full; the second hangs, and the sweep
	// is cancelled while it does. Cancelling on "the server has recorded an
	// upload" instead raced the CLIENT still reading that upload's answer:
	// cancelled there, it rightly never recorded the file, and the test
	// failed about one run in thirty under -race.
	f.holdAfter = 1
	f.holding = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-f.holding:
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()
	m.SweepOnce(ctx, "wkr-1")

	if got := len(f.uploadedPaths()); got != 1 {
		t.Fatalf("%d uploads arrived, want exactly the first before the sweep was cut short", got)
	}
	// What it managed to send is on disk. The deferred save is what makes
	// that true; without it the file would not exist at all.
	back := LoadLedger(stateDir, "w-1")
	if len(back.Paths()) != 1 {
		t.Errorf("the ledger holds %d paths after an interrupted sweep, want the 1 it sent: a sweep uploaded files and then discarded its record of them", len(back.Paths()))
	}
}

// The registration id round-trips so `--once` can act as the same machine the
// worker does.
func TestTheRegistrationIdRoundTripsAndIsAbsentBeforeAnyRegistration(t *testing.T) {
	dir := t.TempDir()
	if got := LoadRegistrationID(dir); got != "" {
		t.Errorf("a machine that never registered reported id %q", got)
	}
	if err := SaveRegistrationID(dir, "wkr-7"); err != nil {
		t.Fatal(err)
	}
	if got := LoadRegistrationID(dir); got != "wkr-7" {
		t.Errorf("id = %q, want wkr-7", got)
	}
}
