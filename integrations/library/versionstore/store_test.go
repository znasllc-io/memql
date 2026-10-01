package versionstore

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestOnlyDocumentVersionWritesReceiveInternalOrigin(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	engine, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"appendDocumentVersion", "updateGeneratedOutputContent"} {
		fn, err := engine.Functions().Get(name)
		if err != nil || fn == nil || !fn.ServerOnly {
			t.Fatalf("%s must require internal origin: %v", name, err)
		}
	}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "person", Role: auth.RoleWriter})
	for _, query := range []string{"query generatedOutputById(outputId: \"other\")", "mutation createArtifact(title: \"bad\")"} {
		if _, err := Execute(ctx, nil, query); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("unexpected write admitted: %v", err)
		}
	}
	if auth.OriginFromContext(ctx).IsInternal() {
		t.Fatal("internal origin escaped to caller")
	}
}
