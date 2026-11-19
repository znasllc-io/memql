package sharestore

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// The named precondition test the call-origin allowlist entry cites: the stamp
// is required (the writes are @serverOnly and the concepts @serverWritten), it
// reaches only the four writes, no write may name an owner, and the caller's
// own context is still a client's afterwards.
func TestOnlyTheFourSharingWritesReceiveInternalOrigin(t *testing.T) {
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
	for _, name := range Writes {
		fn, err := engine.Functions().Get(name)
		if err != nil || fn == nil || !fn.ServerOnly {
			t.Fatalf("%s must be @serverOnly: %v", name, err)
		}
	}
	for _, concept := range []string{"v1:library:share", "v1:library:shareNotice"} {
		meta, err := memorynodes.Get(concept)
		if err != nil || meta == nil || !meta.ServerWritten {
			t.Fatalf("%s must be @serverWritten: %v", concept, err)
		}
	}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "person", Role: auth.RoleWriter})
	refusals := []struct {
		name string
		args map[string]any
		want string
	}{
		{"createArtifact", map[string]any{"title": "x"}, "not a file-sharing write"},
		{"putLibraryShare", map[string]any{"shareId": "s", "ownerUserId": "someone-else"}, "may not name it"},
		{"putLibraryShareNotice", map[string]any{"shareId": "s", "ownerUserId": "someone-else"}, "may not name it"},
	}
	for _, r := range refusals {
		if err := Write(ctx, engine, r.name, r.args); err == nil || !strings.Contains(err.Error(), r.want) {
			t.Fatalf("%s: want a refusal containing %q, got %v", r.name, r.want, err)
		}
	}
	if err := Write(context.Background(), engine, "endLibraryShare", map[string]any{"shareId": "s"}); err == nil {
		t.Fatal("a write with no actor must be refused before the engine is reached")
	}
	if auth.OriginFromContext(ctx).IsInternal() {
		t.Fatal("internal origin escaped to the caller's context")
	}
}
