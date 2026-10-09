package app

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/library"
)

func TestLibraryRevisionFilesUseRealVersionStoreAcrossReplicas(t *testing.T) {
	if available, err := dbtest.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	} else if !available {
		dbtest.Unreachable(t, "file revision storage", dbtest.DSN(), nil)
		return
	}
	owner := fmt.Sprintf("revision-file-%d", time.Now().UnixNano())
	ctx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner}), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	stores := make([]*server.EngineLibraryStore, 0, 2)
	for n := 0; n < 2; n++ {
		engine, db := workTemplateDBEngineAndDB(t)
		if err := engine.RegisterIntegration(library.NewIntegration(engine, func() *sql.DB { return db.DB })); err != nil {
			t.Fatal(err)
		}
		stores = append(stores, server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: engine}, func() *sql.DB { return db.DB }))
	}
	file := owner + "-document"
	if err := stores[0].CreateFile(ctx, server.LibraryFileCreateParams{FileId: file, Name: "Imported note.md", MimeType: "text/markdown", Size: 8, BlobUrl: "https://storage/original", Sha256: "original", Source: "uploaded", Format: "markdown"}); err != nil {
		t.Fatal(err)
	}
	upload := &revisionFileFixture{}
	write := library.RevisionFileWrite{FileID: "v1:library:file:" + file, Version: 1, BlobURL: "https://storage/original", OperationID: "approved-run", Content: []byte("Approved bytes")}
	for _, store := range stores {
		adapter := &libraryRevisionFiles{store: store, uploader: upload, bucket: "library"}
		if err := adapter.SaveRevision(ctx, write); err != nil {
			t.Fatal(err)
		}
	}
	head, err := stores[1].File(memql.ContextWithFreshRead(ctx), server.LibraryFileConceptRef(file))
	if err != nil {
		t.Fatal(err)
	}
	prior, err := stores[1].FileVersion(memql.ContextWithFreshRead(ctx), file, 1)
	if err != nil {
		t.Fatal(err)
	}
	if head.VersionNumber != 2 || prior == nil || prior.BlobUrl != "https://storage/original" || upload.uploads != 1 {
		t.Fatalf("bad version chain: head=%+v prior=%+v uploads=%d", head, prior, upload.uploads)
	}
}
