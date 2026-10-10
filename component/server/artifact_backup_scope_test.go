package server

import (
	"context"
	"net/http"
	"testing"
)

func (f *fakeLibraryStore) BackupDirectory(ctx context.Context, watchID, workerID string) (string, error) {
	worker, err := f.OwnedWorker(ctx, workerID)
	if err != nil || worker == nil || watchID != "test-backup" {
		return "", err
	}
	return "/", nil
}

func TestOrdinaryUploadDoesNotTrackOrReplaceHostFile(t *testing.T) {
	store, blob := newFakeLibraryStore(), newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "Laptop")
	seedPushedFile(store, blob, "user-a", "old-art", "old-file", "wrk-1", watchedPath, []byte("old"))
	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "notes.md", "text/markdown", []byte("independent"), map[string]string{"uploadedFromWorkerId": "wrk-1", "uploadedFromPath": watchedPath})
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	created := store.snapshotCreated()
	if len(created) != 1 || created[0].UploadedFromPath != "" || len(store.snapshotSupersedes()) != 0 {
		t.Fatalf("ordinary upload retained host identity: %+v", created)
	}
	if store.linkStateOf(created[0].FileId) != "" {
		t.Fatal("ordinary upload acquired a backup state")
	}
}

func TestBackupUploadRequiresOwnedActiveDirectory(t *testing.T) {
	store, blob := newFakeLibraryStore(), newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "Laptop")
	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "notes.md", "text/markdown", []byte("no"), map[string]string{"uploadedFromWorkerId": "wrk-1", "uploadedFromPath": watchedPath, "backupWatchId": "foreign-or-paused"})
	if rec.Code != http.StatusForbidden || len(store.snapshotCreated()) != 0 {
		t.Fatalf("unowned backup accepted: %d", rec.Code)
	}
}

func TestBackupDirectoryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		root, file string
		want       bool
	}{
		{"/work", "/work/report.md", true}, {"/work", "/work2/report.md", false}, {"/work", "/work/../secret", false}, {"/work", "/work", false},
		{`C:\Work`, `C:\Work\report.md`, true}, {`C:\Work`, `C:\Work2\report.md`, false}, {`\\server\share`, `\\server\share\report.md`, true}, {"relative", "relative/file", false},
	} {
		if got := withinBackupDirectory(tc.root, tc.file); got != tc.want {
			t.Errorf("%q in %q: %v", tc.file, tc.root, got)
		}
	}
}
