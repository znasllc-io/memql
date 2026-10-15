package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// artifact_blob_key_db_test.go -- a Library file row's blobUrl is a storage
// path, and GET /artifacts/{id}/content reads whatever path the row names
// with the NODE's storage credential. So the path has to be one the server
// wrote for the row's owner. If a client could write it, row admission would
// guard the row and nothing would guard the bytes: the caller owns the row
// that points at somebody else's object.
//
// Against a real engine and Postgres, because both halves are DSL facts a
// fake store would only restate: whether createLibraryFile is reachable from
// a client, and what libraryFileById hands the content route.

// wireCallerCtx is a signed-in person as the gRPC / WebSocket interceptor
// admits them: an access context and a token, and NO origin stamp, so the
// engine treats every call made under it as a client's.
func wireCallerCtx(userId string) context.Context {
	ctx := auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: userId})
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userId, Role: auth.RoleWriter})
}

// libraryDBEngine is a real engine over the CI database, skipped (or failed,
// under MEMQL_REQUIRE_DB=1) when none answers.
func libraryDBEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "library blob keys", dbtest.DSN(), nil)
		return nil, nil
	}
	if _, err = memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = quietLogger()
	if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return e, db
}

// promoteFile writes the index row indexFileOnCreate writes for a new file,
// with the arguments it passes, and returns its id. The automation is not
// running here; this is the one write it would make, owned by the file's
// owner exactly as the automation stamps it.
func promoteFile(t *testing.T, store *EngineLibraryStore, ctx context.Context, ownerUserId, fileId, name string) string {
	t.Helper()
	if _, err := store.exec(ctx, "createArtifact", map[string]any{
		"sourceConceptRef": LibraryFileConceptRef(fileId),
		"ownerUserId":      ownerUserId,
		"lens":             "artifact",
		"kind":             libraryKindFile,
		"source":           "uploaded",
		"title":            name,
		"format":           "text",
		"mimeType":         "text/csv",
	}); err != nil {
		t.Fatalf("promote %s: %v", fileId, err)
	}
	artifactId, err := store.ArtifactForFile(memql.ContextWithFreshRead(ctx), fileId)
	if err != nil || artifactId == "" {
		t.Fatalf("promoted artifact for %s not readable: %q %v", fileId, artifactId, err)
	}
	return artifactId
}

// libraryGatePool opens the write-gate pool of one replica, shaped as its database
// opens its own (database.NewGatePool, memql#6022): the file-version gate holds its
// lock there, never on the request pool, whose sessions PostgreSQL ends after a
// minute idle in a transaction -- the lock with them -- and which a gate refuses.
func libraryGatePool(t *testing.T) func() *sql.DB {
	t.Helper()
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	return func() *sql.DB { return gates }
}

func getArtifactContent(h *ArtifactHandler, ctx context.Context, artifactId string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/artifacts/"+artifactId+"/content", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAClientCannotPointAFileRowAtAnotherUsersBlob is the reported exposure,
// end to end: A uploads through POST /artifacts; B, a signed-in client with
// no relationship to A, sends the createLibraryFile statement the SDK builds
// naming A's blob key; B then exports the file through the content route.
//
// The key is the ONLY thing B has to learn, and a key is not a credential.
// The write must be refused because blobUrl is not a value a client may
// author at all, not because the key was hard to guess.
func TestAClientCannotPointAFileRowAtAnotherUsersBlob(t *testing.T) {
	eng, _ := libraryDBEngine(t)
	store := NewEngineLibraryStore(replicaFileExecutor{eng}, libraryGatePool(t))
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "memql-library", Uploader: blob, Downloader: blob,
		Store: store, UserQuotaBytes: -1, PromotionWait: 50 * time.Millisecond,
	})

	marker := fmt.Sprintf("blobkey-%d", time.Now().UnixNano())
	userA, userB := marker+"-a", marker+"-b"
	ctxA, ctxB := wireCallerCtx(userA), wireCallerCtx(userB)
	secret := []byte("employee,salary\nalex,182000\n")

	// --- A uploads, through the real route ---
	up := postUpload(t, h, userA, "payroll.csv", "text/csv", secret, nil)
	if up.Code != http.StatusCreated {
		t.Fatalf("A's upload answered %d: %s", up.Code, up.Body.String())
	}
	var uploaded ArtifactUploadResponse
	if err := json.Unmarshal(up.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	aFile, err := store.File(ctxA, LibraryFileConceptRef(uploaded.FileId))
	if err != nil || aFile == nil || aFile.BlobUrl == "" {
		t.Fatalf("A's own file row is not readable: %#v %v", aFile, err)
	}
	aKey := aFile.BlobUrl

	// --- B, as a client, names A's key ---
	forgedId := marker + "-forged"
	call, err := dslCall("createLibraryFile", map[string]any{
		"fileId":   forgedId,
		"name":     "mine.csv",
		"mimeType": "text/csv",
		"size":     len(secret),
		"blobUrl":  aKey,
		"source":   "uploaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, forgeErr := eng.Execute(ctxB, "mutation "+call)
	if forgeErr == nil {
		// Follow it to the end, so the failure says what it costs.
		artifactId := promoteFile(t, store, ctxB, userB, forgedId, "mine.csv")
		got := getArtifactContent(h, ctxB, artifactId)
		if got.Code == http.StatusOK && bytes.Equal(got.Body.Bytes(), secret) {
			t.Fatalf("EXPOSURE: B wrote a Library file row naming A's blob key %q through a client "+
				"createLibraryFile, then GET /artifacts/%s/content streamed A's bytes to B:\n%s",
				aKey, artifactId, got.Body.String())
		}
		t.Fatalf("a client-origin createLibraryFile naming another user's blob key was accepted "+
			"(the export then answered %d); blobUrl must never be a client's to write", got.Code)
	}
	if !strings.Contains(forgeErr.Error(), "server-only") {
		t.Fatalf("the forged write was refused, but not by the server-only gate: %v", forgeErr)
	}
	if forged, _ := store.File(memql.ContextWithFreshRead(ctxB), LibraryFileConceptRef(forgedId)); forged != nil {
		t.Fatalf("the refused write left a row behind: %#v", forged)
	}
}

// TestTheContentRouteRefusesAForgedRowAlreadyInTheGraph is the read-side half,
// against the real store: a row that ALREADY names somebody else's key -- one
// a client wrote before createLibraryFile was @serverOnly -- is not streamed,
// while the owner's own upload, whose stored ownerUserId is canonical and whose
// path carries the bare id, still is.
func TestTheContentRouteRefusesAForgedRowAlreadyInTheGraph(t *testing.T) {
	eng, _ := libraryDBEngine(t)
	store := NewEngineLibraryStore(replicaFileExecutor{eng}, libraryGatePool(t))
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "memql-library", Uploader: blob, Downloader: blob,
		Store: store, UserQuotaBytes: -1, PromotionWait: 50 * time.Millisecond,
	})

	marker := fmt.Sprintf("blobkey-read-%d", time.Now().UnixNano())
	userA, userB := marker+"-a", marker+"-b"
	ctxA, ctxB := wireCallerCtx(userA), wireCallerCtx(userB)
	secret := []byte("employee,salary\nalex,182000\n")

	up := postUpload(t, h, userA, "payroll.csv", "text/csv", secret, nil)
	if up.Code != http.StatusCreated {
		t.Fatalf("A's upload answered %d: %s", up.Code, up.Body.String())
	}
	var uploaded ArtifactUploadResponse
	if err := json.Unmarshal(up.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	aFile, err := store.File(ctxA, LibraryFileConceptRef(uploaded.FileId))
	if err != nil || aFile == nil {
		t.Fatalf("A's own file row is not readable: %#v %v", aFile, err)
	}

	// The owner's own download, through the real projection: canonical owner,
	// bare path segment.
	aArtifact := promoteFile(t, store, ctxA, userA, uploaded.FileId, "payroll.csv")
	if got := getArtifactContent(h, ctxA, aArtifact); got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), secret) {
		t.Fatalf("A's own download answered %d: %q (owner %q, key %q)", got.Code, got.Body.String(), aFile.OwnerUserId, aFile.BlobUrl)
	}

	// The forged row, planted with internal origin on the ENGINE call itself:
	// the shape every pre-fix client write left behind, and what a server
	// writer forwarding a caller's key would leave. It skips
	// component/server/libraryfile's own key check on purpose.
	forgedId := marker + "-forged"
	call, err := dslCall("createLibraryFile", map[string]any{
		"fileId": forgedId, "name": "mine.csv", "mimeType": "text/csv",
		"size": len(secret), "blobUrl": aFile.BlobUrl, "source": "uploaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(ctxB), "mutation "+call); err != nil {
		t.Fatalf("plant the forged row: %v", err)
	}
	bArtifact := promoteFile(t, store, ctxB, userB, forgedId, "mine.csv")
	got := getArtifactContent(h, ctxB, bArtifact)
	if got.Code != http.StatusNotFound || bytes.Contains(got.Body.Bytes(), secret) {
		t.Fatalf("B's row naming A's key %q answered %d with %q -- the content route served "+
			"another owner's bytes", aFile.BlobUrl, got.Code, got.Body.String())
	}
}
