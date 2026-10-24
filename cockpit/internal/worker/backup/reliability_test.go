package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionIsDurableBeforeFirstChunkAndSourceCannotChangeUploadedSnapshot(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	path := filepath.Join(root, "video.bin")
	original := []byte("abcdefgh")
	os.WriteFile(path, original, 0600)
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/memql/query":
			io.WriteString(w, `{"result":{"data":[]}}`)
		case r.URL.Path == "/artifacts/uploads":
			json.NewEncoder(w).Encode(initResponse{UploadID: "durable", ChunkSize: 4})
		case strings.Contains(r.URL.Path, "/chunks/"):
			ledger := LoadLedger(state, "watch")
			rec, ok := ledger.Get(path)
			if !ok || rec.UploadID != "durable" || rec.UploadSHA256 == "" {
				t.Error("session was not persisted before bytes were sent")
			}
			data, _ := io.ReadAll(r.Body)
			received = append(received, data...)
			// Mutate the ORIGINAL between chunks. The second must still come from
			// the same immutable snapshot as the first.
			os.WriteFile(path, []byte("XXXXXXXX"), 0600)
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			json.NewEncoder(w).Encode(PushResult{FileID: "file", VersionNumber: 1})
		}
	}))
	defer server.Close()
	m := New(Options{StateDir: state, BaseURL: server.URL, Bearer: func(context.Context) (string, error) { return "token", nil }, CheckPath: allow})
	m.workerID = "worker"
	m.library.oneShotLimit = 1
	stamp, _ := statOf(path)
	changed, err := m.pushIfChanged(context.Background(), Watch{ID: "watch"}, m.ledgerFor("watch"), Entry{Path: path, Stamp: stamp})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(received, original) {
		t.Fatalf("uploaded mixed bytes: %q", received)
	}
	rec, _ := LoadLedger(state, "watch").Get(path)
	if rec.UploadID != "" || rec.VersionNumber != 1 {
		t.Fatalf("completion not persisted: %+v", rec)
	}
}

func TestChangedSameSizeSourceDoesNotResumeOldChunksAfterRestart(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	path := filepath.Join(root, "file")
	os.WriteFile(path, []byte("new-data"), 0600)
	ledger := LoadLedger(state, "watch")
	ledger.Put(path, Record{UploadID: "old", UploadSize: 8, UploadSHA256: "old-digest"})
	ledger.Save()
	inventory, init := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/memql/query" {
			io.WriteString(w, `{"result":{"data":[]}}`)
			return
		}
		if r.Method == "GET" {
			inventory++
			http.Error(w, "wrong resume", 500)
			return
		}
		if r.URL.Path == "/artifacts/uploads" {
			init++
			json.NewEncoder(w).Encode(initResponse{UploadID: "new", ChunkSize: 8})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/complete") {
			json.NewEncoder(w).Encode(PushResult{FileID: "f", VersionNumber: 1})
			return
		}
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(204)
	}))
	defer server.Close()
	m := New(Options{StateDir: state, BaseURL: server.URL, Bearer: func(context.Context) (string, error) { return "t", nil }, CheckPath: allow})
	m.workerID = "w"
	m.library.oneShotLimit = 1
	stamp, _ := statOf(path)
	if _, err := m.pushIfChanged(context.Background(), Watch{ID: "watch"}, m.ledgerFor("watch"), Entry{Path: path, Stamp: stamp}); err != nil {
		t.Fatal(err)
	}
	if inventory != 0 || init != 1 {
		t.Fatalf("inventory=%d init=%d", inventory, init)
	}
}

func TestCompletedSessionRecoversLostResponseWithoutNewVersion(t *testing.T) {
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(inventoryResponse{Status: "completed", Size: 8})
			return
		}
		json.NewEncoder(w).Encode(PushResult{FileID: "same", VersionNumber: 3})
	}))
	defer server.Close()
	l := NewLibrary(server.URL, server.Client(), func(context.Context) (string, error) { return "t", nil })
	out, err := l.pushSession(context.Background(), "w", "watch", "source", "snapshot", "", 8, "saved", 2, nil)
	if err != nil || out.VersionNumber != 3 || len(calls) != 2 || calls[1] != "POST /artifacts/uploads/saved/complete" {
		t.Fatalf("result=%+v err=%v calls=%v", out, err, calls)
	}
}

func TestInventoryOutageDoesNotAbandonExistingUpload(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "temporarily unavailable", 503) }))
	defer server.Close()
	l := NewLibrary(server.URL, server.Client(), func(context.Context) (string, error) { return "t", nil })
	_, err := l.pushSession(context.Background(), "w", "watch", "source", "snapshot", "", 8, "saved", 0, nil)
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestMissingRootDoesNotClaimAllOriginalsWereDeleted(t *testing.T) {
	f := newFakeEngine(t)
	root := filepath.Join(t.TempDir(), "unmounted")
	m := managerFor(t, f, root, allow)
	ledger := m.ledgerFor("watch")
	ledger.Put(filepath.Join(root, "file"), Record{FileID: "f", LinkState: "synced"})
	m.sweepWatch(context.Background(), Watch{ID: "watch", LocalPath: root})
	calls := f.graphCalls("setLibraryFileLinkState")
	if len(calls) != 1 || !strings.Contains(calls[0], `"unavailable"`) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestUnreadableAndExcludedEntriesAreNotReportedDeleted(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	path := filepath.Join(root, "private")
	os.WriteFile(path, []byte("x"), 0600)
	m := managerFor(t, f, root, func(string) error { return fmt.Errorf("not authorized") })
	ledger := m.ledgerFor("watch")
	ledger.Put(path, Record{FileID: "f", LinkState: "synced"})
	m.verify(context.Background(), ledger, ScanResult{Skipped: 1})
	calls := f.graphCalls("setLibraryFileLinkState")
	if len(calls) != 1 || !strings.Contains(calls[0], `"unavailable"`) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestRenameWithinAuthorizedWatchKeepsRemoteFileAndHistory(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	old := filepath.Join(root, "old.txt")
	next := filepath.Join(root, "new.txt")
	writeFile(t, old, 12)
	f.watches = []map[string]any{watchRow("watch", root, nil)}
	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")
	before, _ := m.ledgerFor("watch").Get(old)
	if err := os.Rename(old, next); err != nil {
		t.Fatal(err)
	}
	m.SweepOnce(context.Background(), "wkr-1")
	after, ok := m.ledgerFor("watch").Get(next)
	if !ok || after.FileID != before.FileID {
		t.Fatalf("identity lost: before=%+v after=%+v", before, after)
	}
	if len(f.graphCalls("relinkLibraryFileOrigin")) != 1 {
		t.Fatalf("relink calls=%v", f.graphCalls("relinkLibraryFileOrigin"))
	}
	if _, uploaded := f.uploadedPaths()[next]; uploaded {
		t.Fatal("rename unnecessarily uploaded another copy")
	}
}

func TestCopyWithSameContentIsNotMistakenForRename(t *testing.T) {
	f := newFakeEngine(t)
	root := t.TempDir()
	old := filepath.Join(root, "old.txt")
	next := filepath.Join(root, "copy.txt")
	writeFile(t, old, 12)
	f.watches = []map[string]any{watchRow("watch", root, nil)}
	m := managerFor(t, f, root, allow)
	m.SweepOnce(context.Background(), "wkr-1")
	writeFile(t, next, 12)
	os.Remove(old)
	m.SweepOnce(context.Background(), "wkr-1")
	if len(f.graphCalls("relinkLibraryFileOrigin")) != 0 {
		t.Fatal("copy incorrectly treated as native rename")
	}
	if _, ok := f.uploadedPaths()[next]; !ok {
		t.Fatal("new copy not uploaded")
	}
}
