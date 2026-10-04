package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server/uploadsession"
)

type replicaFileExecutor struct{ engine *memql.MemQLEngine }

func (e replicaFileExecutor) Execute(ctx context.Context, q string) (any, error) {
	return e.engine.Execute(ctx, q)
}

func TestFileVersionConflictAcrossReplicas(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "file version replicas", dbtest.DSN(), nil)
		return
	}
	if _, err = memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	open := func() *EngineLibraryStore {
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
		return NewEngineLibraryStore(replicaFileExecutor{e}, func() *sql.DB { return db.DB })
	}
	a, b := open(), open()
	marker := fmt.Sprintf("file-edit-%d", time.Now().UnixNano())
	owner := marker + "-owner"
	ctx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner}), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	if err = a.CreateFile(ctx, LibraryFileCreateParams{FileId: marker, Name: "draft.txt", MimeType: "text/plain", Size: 4, BlobUrl: "library/" + owner + "/" + marker + "/initial", Format: "text", Source: "uploaded", UploadedFromWorkerId: "machine-1", UploadedFromPath: "/reports/draft.txt"}); err != nil {
		t.Fatal(err)
	}
	// The base travels through the real DSL store, not a replica's memory or a
	// test fake. A missing field in the session shape breaks this assertion.
	sessionA, sessionB := uploadsession.NewStore(a.engine), uploadsession.NewStore(b.engine)
	if err = sessionA.Create(ctx, uploadsession.CreateParams{UploadId: marker + "-upload", Name: "draft.txt", Size: 4, FileId: marker, BlobPath: "library/" + owner + "/" + marker + "/upload", ChunkSize: 4, TargetArtifactId: marker + "-artifact", ExpectedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	persisted, err := sessionB.ByID(memql.ContextWithFreshRead(ctx), marker+"-upload")
	if err != nil || persisted == nil || persisted.ExpectedVersion != 1 {
		t.Fatalf("session lost its base across replicas: %v %#v", err, persisted)
	}
	head, err := a.File(ctx, LibraryFileConceptRef(marker))
	if err != nil || head == nil {
		t.Fatalf("head: %v %#v", err, head)
	}
	if _, err = b.File(ctx, LibraryFileConceptRef(marker)); err != nil {
		t.Fatal(err)
	} // cache B's old head
	snapshot := LibraryVersionSnapshot{VersionId: marker + "-v1", FileId: marker, VersionNumber: 1, Name: head.Name, MimeType: head.MimeType, Size: 4, BlobUrl: head.BlobUrl, Format: "text"}
	next := LibraryHeadMove{FileId: marker, VersionNumber: 2, Name: "draft.txt", MimeType: "text/plain", Size: 5, BlobUrl: head.BlobUrl + "-next", Format: "text"}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range []*EngineLibraryStore{a, b} {
		wg.Add(1)
		go func(s *EngineLibraryStore) { defer wg.Done(); <-start; errs <- s.SupersedeFile(ctx, snapshot, next) }(store)
	}
	close(start)
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrFileVersionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	latest, err := b.File(memql.ContextWithFreshRead(ctx), LibraryFileConceptRef(marker))
	if err != nil || latest.VersionNumber != 2 || latest.BlobUrl != next.BlobUrl {
		t.Fatalf("lost head: %v %#v", err, latest)
	}
	if latest.UploadedFromWorkerId != "" || latest.UploadedFromPath != "" {
		t.Fatal("editor save invented machine provenance")
	}
	if memql.BareShortId(latest.BackupWorkerId) != "machine-1" || latest.BackupPath != "/reports/draft.txt" || latest.LinkState != "stale" {
		t.Fatalf("editor save detached the backup: %#v", latest)
	}
	linked, err := b.FileByUploadedFrom(memql.ContextWithFreshRead(ctx), "machine-1", "/reports/draft.txt")
	if err != nil || linked == nil || linked.VersionNumber != 2 {
		t.Fatalf("Cockpit can no longer resolve its watched file: %v %#v", err, linked)
	}
	old, err := b.FileVersion(ctx, marker, 1)
	if err != nil || old == nil || old.BlobUrl != head.BlobUrl {
		t.Fatalf("lost original bytes: %v %#v", err, old)
	}
}
