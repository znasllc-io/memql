package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/library"
)

func TestLibraryImageAssetReconcilesStoredBytesAndProvenance(t *testing.T) {
	first, _ := workTemplateDBEngineAndDB(t)
	second, _ := workTemplateDBEngineAndDB(t)
	owner := id.NewShortId()
	ctx := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner}), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	uploader := &pipelineStreamFake{}
	delegate := &AttachmentEngineAdapter{Engine: first}
	lost := pipelineStreamEngineFunc(func(ctx context.Context, query string) (any, error) {
		r, err := delegate.Execute(ctx, query)
		if err == nil && strings.HasPrefix(query, "mutation recordPreparedLibraryImage(") {
			return nil, errors.New("lost reply after durable row")
		}
		return r, err
	})
	a := &libraryImageAssets{engine: lost, store: server.NewEngineLibraryStore(delegate), uploader: uploader, bucket: "images-test"}
	bDelegate := &AttachmentEngineAdapter{Engine: second}
	b := &libraryImageAssets{engine: bDelegate, store: server.NewEngineLibraryStore(bDelegate), uploader: uploader, bucket: "images-test"}
	w := library.ImageAssetWrite{FileID: id.NewShortId(), Name: "portrait.png", MIME: "image/png", RunID: id.NewShortId(), StepKey: "images/0", Data: []byte("immutable image bytes"), Provenance: map[string]any{"requestId": "request-1", "mode": "generate", "model": "actual-local-model"}}
	if err := a.SaveImage(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveImage(ctx, w); err != nil {
		t.Fatal(err)
	}
	if uploader.writes != 1 {
		t.Fatal("recovery uploaded another blob")
	}
	row, err := b.store.File(ctx, server.LibraryFileConceptRef(w.FileID))
	if err != nil || row == nil || row.ImageProvenance["model"] != "actual-local-model" {
		t.Fatalf("metadata lost: %v %v", row, err)
	}
	w.Data = []byte("different image bytes")
	if err := b.SaveImage(ctx, w); err == nil {
		t.Fatal("immutable image silently replaced")
	}
	reader := auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner}), &auth.AccessContext{UserId: owner, Role: auth.RoleReader})
	if err := b.SaveImage(reader, w); err == nil {
		t.Fatal("reader wrote an image")
	}
}
