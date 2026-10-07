package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type pipelineStreamEngineFunc func(context.Context, string) (any, error)

func (f pipelineStreamEngineFunc) Execute(ctx context.Context, q string) (any, error) {
	return f(ctx, q)
}

type pipelineStreamFake struct {
	mu      sync.Mutex
	objects map[string]azureblob.VerifiedBlob
	writes  int
}

func (*pipelineStreamFake) Upload(context.Context, string, string, []byte, string) (string, error) {
	return "", errors.New("streamed artifact reached byte-slice uploader")
}

func (f *pipelineStreamFake) CreateVerifiedStream(ctx context.Context, bucket, object string, source io.Reader, size int64, digest, mime string) (azureblob.VerifiedBlob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects == nil {
		f.objects = map[string]azureblob.VerifiedBlob{}
	}
	if v, ok := f.objects[object]; ok {
		if v.Size != size || v.SHA256 != digest {
			return azureblob.VerifiedBlob{}, errors.New("object disagrees")
		}
		return v, nil
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(source, size+1))
	if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return azureblob.VerifiedBlob{}, errors.New("stream identity disagrees")
	}
	v := azureblob.VerifiedBlob{URL: "https://blob.example/" + bucket + "/" + object, ETag: "version-1", Size: size, SHA256: digest}
	f.objects[object] = v
	f.writes++
	return v, nil
}

func pipelineStreamDBStore(t *testing.T, uploader server.FileUploader) (*pipelinesLibraryStore, *memql.MemQLEngine, *sql.DB) {
	t.Helper()
	e, db := workTemplateDBEngineAndDB(t)
	migration, err := os.ReadFile("../component/database/memory-nodes/migrations/20261007010000_pipeline_artifact_uploads.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), string(migration)); err != nil {
		t.Fatal(err)
	}
	s := newPipelinesLibraryStore(&AttachmentEngineAdapter{Engine: e}, uploader, "stream-test", quietLogger())
	s.uploadDB = func() *sql.DB { return db.DB }
	return s, e, db.DB
}

func pipelineStreamFile(owner, body string) pipelinesteps.StreamRunFile {
	sum := sha256.Sum256([]byte(body))
	return pipelinesteps.StreamRunFile{OwnerUserID: owner, WorkRunID: id.NewShortId(), StepKey: "build/image", Attempt: 1,
		Path: "output/archive.tar", Name: "archive.tar", MimeType: "application/x-tar", Size: int64(len(body)),
		SHA256: hex.EncodeToString(sum[:]), Body: strings.NewReader(body)}
}

func TestPipelineStreamDBLostLibraryWriteRecoversOnAnotherEngine(t *testing.T) {
	u := &pipelineStreamFake{}
	first, e, db := pipelineStreamDBStore(t, u)
	second, _, _ := pipelineStreamDBStore(t, u)
	f := pipelineStreamFile(id.NewShortId(), "stable artifact bytes")
	delegate := first.engine
	first.engine = pipelineStreamEngineFunc(func(ctx context.Context, q string) (any, error) {
		result, err := delegate.Execute(ctx, q)
		if strings.HasPrefix(q, "mutation recordStoredPipelineFile(") && err == nil {
			return nil, errors.New("injected lost reply after committed Library write")
		}
		return result, err
	})
	if result, err := first.StoreRunFileStream(context.Background(), f); err == nil || result.FileID != "" {
		t.Fatalf("lost row reply returned success: %+v %v", result, err)
	}
	identity, _ := first.streamIdentity(f)
	var fileID, object, state, etag string
	if err := db.QueryRow(`SELECT file_id, object_key, state, etag FROM pipeline_artifact_uploads WHERE intent_id=$1`, pipelineIntentID(identity)).Scan(&fileID, &object, &state, &etag); err != nil {
		t.Fatal(err)
	}
	if state != "blob_verified" || etag == "" {
		t.Fatalf("lost row reply lost verified reservation: %s %s", state, etag)
	}
	// Recovery must not require the original source. The object already exists.
	f.Body = pipelineUnreadableSource{}
	got, err := second.StoreRunFileStream(context.Background(), f)
	if err != nil || got.FileID != fileID || got.Receipt == nil || got.Receipt.Object != object || got.Receipt.ETag != etag {
		t.Fatalf("replacement engine: %+v %v", got, err)
	}
	again, err := second.StoreRunFileStream(context.Background(), f)
	if err != nil || again.Receipt == nil || *again.Receipt != *got.Receipt || u.writes != 1 {
		t.Fatalf("repeated recovery changed identity: %+v %v writes=%d", again, err, u.writes)
	}
	row, err := second.pipelineLibraryRow(auth.ContextWithUserActor(context.Background(), f.OwnerUserID), fileID)
	if err != nil || row["status"] != "ready" {
		t.Fatalf("ready row: %v %v", row, err)
	}
	// The new mutation is not a way for an ordinary caller to manufacture a
	// verified artifact. Its arguments alone cannot replace the native gate.
	call, err := langparser.RenderCall("recordStoredPipelineFile", map[string]any{
		"fileId": fileID, "producedByRunId": f.WorkRunID, "producedByStepKey": f.StepKey,
		"name": "replacement.tar", "mimeType": f.MimeType, "size": 0,
		"sha256": strings.Repeat("0", 64), "blobUrl": "https://blob.example/replacement", "format": "other",
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := auth.ContextWithUserActor(context.Background(), f.OwnerUserID)
	if _, err := e.Execute(ownerCtx, "mutation "+call); err == nil {
		t.Fatal("ordinary actor reached server-only verified-file write")
	}
	if _, err := e.Execute(auth.ContextWithInternalOrigin(ownerCtx), "mutation "+call); err != nil {
		t.Fatal(err)
	}
	row, err = second.pipelineLibraryRow(ownerCtx, fileID)
	if err != nil || row["status"] != "ready" || row["name"] != f.Name || row["sha256"] != f.SHA256 {
		t.Fatalf("replayed record overwrote existing file: %v %v", row, err)
	}
	// An edited object with identical bytes has a different ETag and cannot
	// silently become the release evidence recorded earlier.
	u.mu.Lock()
	v := u.objects[object]
	v.ETag = "replacement-version"
	u.objects[object] = v
	u.mu.Unlock()
	if _, err := second.StoreRunFileStream(context.Background(), f); err == nil || !strings.Contains(err.Error(), "version changed") {
		t.Fatalf("replaced object adopted: %v", err)
	}
}

type pipelineUnreadableSource struct{}

func (pipelineUnreadableSource) Read([]byte) (int, error) {
	return 0, errors.New("source must not be read on recovery")
}

func TestPipelineStreamDBMetadataDisagreementRefusedBeforeUpload(t *testing.T) {
	u := &pipelineStreamFake{}
	s, _, db := pipelineStreamDBStore(t, u)
	f := pipelineStreamFile(id.NewShortId(), "abc")
	x, err := s.streamIdentity(f)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithUserActor(context.Background(), x.OwnerUserID)
	if _, err := s.reserveStream(ctx, db, x); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*pipelinesteps.StreamRunFile){
		func(f *pipelinesteps.StreamRunFile) { f.Size++ },
		func(f *pipelinesteps.StreamRunFile) { f.SHA256 = strings.Repeat("0", 64) },
		func(f *pipelinesteps.StreamRunFile) { f.Name = "another.tar" },
		func(f *pipelinesteps.StreamRunFile) { f.MimeType = "text/plain" },
	} {
		changed := f
		mutate(&changed)
		if _, err := s.StoreRunFileStream(ctx, changed); err == nil || !strings.Contains(err.Error(), "immutable metadata") {
			t.Fatalf("changed identity accepted: %+v %v", changed, err)
		}
	}
	if u.writes != 0 {
		t.Fatal("disagreement moved bytes")
	}
}

func TestPipelineStreamDBConcurrentCapacityAndGenerationFence(t *testing.T) {
	u := &pipelineStreamFake{}
	a, _, adb := pipelineStreamDBStore(t, u)
	b, _, bdb := pipelineStreamDBStore(t, u)
	a.quotaBytes, b.quotaBytes = 10, 10
	owner := id.NewShortId()
	ctx := memql.ContextWithFreshRead(auth.ContextWithUserActor(context.Background(), owner))
	f := pipelineStreamFile(owner, "123456")
	x, _ := a.streamIdentity(f)
	y := x
	y.Path = "other.tar"
	start := make(chan struct{})
	type outcome struct {
		intent pipelineUploadIntent
		err    error
	}
	answers := make(chan outcome, 2)
	for _, item := range []struct {
		store    *pipelinesLibraryStore
		db       *sql.DB
		identity pipelineStreamIdentity
	}{{a, adb, x}, {b, bdb, y}} {
		go func() {
			<-start
			i, err := item.store.reserveStream(ctx, item.db, item.identity)
			answers <- outcome{i, err}
		}()
	}
	close(start)
	var winner pipelineUploadIntent
	successes := 0
	for range 2 {
		r := <-answers
		if r.err == nil {
			winner = r.intent
			successes++
		} else if !strings.Contains(r.err.Error(), "quota") {
			t.Fatal(r.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent admissions=%d; expected one", successes)
	}
	// A replacement can reconcile the same intent even when there is no
	// capacity for another file. It keeps the identity and advances the fence.
	next, err := b.reserveStream(ctx, bdb, winner.Identity)
	if err != nil || next.FileID != winner.FileID || next.Generation != winner.Generation+1 {
		t.Fatalf("replacement: %+v %v", next, err)
	}
	blob := azureblob.VerifiedBlob{URL: "https://blob.example/object", ETag: "v1", Size: x.Size, SHA256: x.SHA256}
	if err := savePipelineVerified(ctx, adb, winner, blob); !errors.Is(err, errPipelineUploadStale) {
		t.Fatalf("stale save: %v", err)
	}
	if err := a.finalizeStream(ctx, adb, winner); !errors.Is(err, errPipelineUploadStale) {
		t.Fatalf("stale finalizer: %v", err)
	}
	if err := savePipelineVerified(ctx, bdb, next, blob); err != nil {
		t.Fatal(err)
	}
	var reserved int64
	if err := bdb.QueryRow("SELECT SUM(size_bytes) FROM pipeline_artifact_uploads WHERE owner_user_id=$1 AND state<>'ready'", owner).Scan(&reserved); err != nil || reserved != 6 {
		t.Fatalf("reservation lost: %d %v", reserved, err)
	}
}

func TestPipelineStreamDBBadBytesKeepCapacityAndNeverCreateReadyRow(t *testing.T) {
	u := &pipelineStreamFake{}
	s, _, db := pipelineStreamDBStore(t, u)
	s.quotaBytes = 3
	f := pipelineStreamFile(id.NewShortId(), "abc")
	f.Body = bytes.NewBufferString("bad")
	if got, err := s.StoreRunFileStream(context.Background(), f); err == nil || got.FileID != "" {
		t.Fatalf("bad bytes succeeded: %+v %v", got, err)
	}
	x, _ := s.streamIdentity(f)
	var state string
	if err := db.QueryRow("SELECT state FROM pipeline_artifact_uploads WHERE intent_id=$1", pipelineIntentID(x)).Scan(&state); err != nil || state != "reserved" {
		t.Fatalf("failed upload state: %s %v", state, err)
	}
	other := f
	other.Path = "new.tar"
	if _, err := s.StoreRunFileStream(context.Background(), other); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("failed upload freed uncertain capacity: %v", err)
	}
	// A correct same-identity retry is the reconciliation operation.
	f.Body = strings.NewReader("abc")
	if got, err := s.StoreRunFileStream(context.Background(), f); err != nil || got.FileID == "" {
		t.Fatalf("same identity recovery: %+v %v", got, err)
	}
}

func TestPipelineStreamDBReadyTransitionFailureIsNotSuccess(t *testing.T) {
	u := &pipelineStreamFake{}
	s, _, _ := pipelineStreamDBStore(t, u)
	delegate := s.engine
	failing := pipelineStreamEngineFunc(func(ctx context.Context, q string) (any, error) {
		if strings.Contains(q, "setLibraryFileStatus(") {
			return nil, fmt.Errorf("injected ready write failure")
		}
		return delegate.Execute(ctx, q)
	})
	s.engine, s.library = failing, server.NewEngineLibraryStore(failing)
	f := pipelineStreamFile(id.NewShortId(), "abc")
	if got, err := s.StoreRunFileStream(context.Background(), f); err == nil || got.FileID != "" {
		t.Fatalf("ready failure succeeded: %+v %v", got, err)
	}
	s.engine, s.library = delegate, server.NewEngineLibraryStore(delegate)
	f.Body = pipelineUnreadableSource{}
	if got, err := s.StoreRunFileStream(context.Background(), f); err != nil || got.FileID == "" {
		t.Fatalf("ready recovery: %+v %v", got, err)
	}
}

func TestPipelineStreamDBAdmissionSerializesFinalization(t *testing.T) {
	u := &pipelineStreamFake{}
	a, _, adb := pipelineStreamDBStore(t, u)
	b, _, bdb := pipelineStreamDBStore(t, u)
	a.quotaBytes, b.quotaBytes = 20, 20
	f := pipelineStreamFile(id.NewShortId(), "123456")
	x, _ := a.streamIdentity(f)
	ctx, cancel := context.WithTimeout(auth.ContextWithUserActor(context.Background(), f.OwnerUserID), 10*time.Second)
	defer cancel()
	i, err := a.reserveStream(ctx, adb, x)
	if err != nil {
		t.Fatal(err)
	}
	v, err := u.CreateVerifiedStream(ctx, x.Container, i.Object, f.Body, x.Size, x.SHA256, x.MimeType)
	if err != nil {
		t.Fatal(err)
	}
	if err := savePipelineVerified(ctx, adb, i, v); err != nil {
		t.Fatal(err)
	}
	i.URL, i.ETag = v.URL, v.ETag
	readFiles, resume := make(chan struct{}), make(chan struct{})
	delegate := a.engine
	a.engine = pipelineStreamEngineFunc(func(ctx context.Context, q string) (any, error) {
		result, err := delegate.Execute(ctx, q)
		if q == "query libraryFileSizesForOwner()" {
			close(readFiles)
			select {
			case <-resume:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return result, err
	})
	admitted, finalized := make(chan error, 1), make(chan error, 1)
	other := x
	other.Path = "second.tar"
	go func() { _, err := a.reserveStream(ctx, adb, other); admitted <- err }()
	select {
	case <-readFiles:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { finalized <- b.finalizeStream(ctx, bdb, i) }()
	// If finalization does not share the owner lock, it can finish while the
	// admission is suspended between its quota reads. The pre-fix finalizer
	// does exactly that and this assertion fails.
	select {
	case err := <-finalized:
		close(resume)
		t.Fatalf("finalizer crossed the admission's owner lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(resume)
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	if err := <-finalized; err != nil {
		t.Fatal(err)
	}
}

func TestPipelineStreamDBRetainedBytesSurviveEditableFileChanges(t *testing.T) {
	u := &pipelineStreamFake{}
	s, e, db := pipelineStreamDBStore(t, u)
	s.quotaBytes = 10
	f := pipelineStreamFile(id.NewShortId(), "123456")
	got, err := s.StoreRunFileStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithUserActor(context.Background(), f.OwnerUserID)
	// With an exact live row the retained receipt counts once, not twice.
	x, _ := s.streamIdentity(pipelineStreamFile(f.OwnerUserID, "1234"))
	if _, err := s.reserveStream(ctx, db, x); err != nil {
		t.Fatalf("exact retained live file counted twice: %v", err)
	}
	// A direct owner edit is intentionally possible through the ordinary
	// mutable Library API. It must not free the immutable object's six bytes.
	call, err := langparser.RenderCall("createLibraryFile", map[string]any{
		"fileId": got.FileID, "name": "changed", "mimeType": "text/plain", "size": 0,
		"sha256": strings.Repeat("0", 64), "blobUrl": "https://blob.example/changed", "source": "uploaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(ctx, "mutation "+call); err != nil {
		t.Fatal(err)
	}
	next := pipelineStreamFile(f.OwnerUserID, "x")
	if _, err := s.StoreRunFileStream(ctx, next); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("editable row released immutable capacity: %v", err)
	}
}
