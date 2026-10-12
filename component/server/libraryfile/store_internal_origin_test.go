package libraryfile

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// store_internal_origin_test.go -- the named precondition test the root
// package's call-origin allowlist entry cites.
//
// This package stamps internal origin on a REQUEST-DERIVED context, the shape
// call_origin.go treats as dangerous unless what makes it safe is asserted.
// Each property is asserted here, against the real loaded registry where the
// claim is about the DSL:
//
//  1. THE STAMP IS REQUIRED, not decorative: createLibraryFile is @serverOnly
//     in the loaded registry, so an unstamped call is refused by the engine.
//  2. THE STAMP CANNOT UNLOCK ANYTHING FOR THE CALLER: no Store method
//     returns a context, and the caller's own context is still a client's
//     after the write.
//  3. NOTHING CALLER-CHOSEN NAMES AN OWNER: a write naming ownerUserId or
//     status is refused, and no rendered write carries either -- the
//     mutation stamps both from the actor already on the caller's context.
//  4. NOTHING CALLER-CHOSEN NAMES ANOTHER OWNER'S BYTES: blobUrl must sit
//     under the actor's own library/{owner}/ prefix, or nothing is executed.
//     This is the property the stamp would otherwise launder: the reason
//     createLibraryFile is @serverOnly at all is that blobUrl is read back
//     with the node's storage credential.
//  5. A STAMPED CREATE CANNOT TAKE AN EXISTING ROW: internal origin skips the
//     write guard's owner check on an existing id, so ownerUserId and blobUrl
//     are @createOnly and survive any later create onto the same id.

type recordingExecutor struct {
	statements []string
	origins    []bool // IsInternal at execute time, per statement
}

func (r *recordingExecutor) Execute(ctx context.Context, q string) (any, error) {
	r.statements = append(r.statements, q)
	r.origins = append(r.origins, auth.OriginFromContext(ctx).IsInternal())
	return map[string]any{"data": []any{}}, nil
}

func loadedEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	return eng
}

func fileArgs(owner, fileId string) map[string]any {
	return map[string]any{
		"fileId":   fileId,
		"name":     "q3.pdf",
		"mimeType": "application/pdf",
		"size":     9,
		"sha256":   strings.Repeat("ab", 32),
		"blobUrl":  "https://acct.blob.core.windows.net/memql-library/library/" + owner + "/" + fileId + "/q3.pdf",
		"source":   "uploaded",
		"format":   "pdf",
	}
}

func TestCreateLibraryFileIsServerOnlyAndKeepsItsOwnerAndKey(t *testing.T) {
	eng := loadedEngine(t)
	fn, err := eng.Functions().Get("createLibraryFile")
	if err != nil || fn == nil {
		t.Fatalf("createLibraryFile: not in the function registry: %v", err)
	}
	if !fn.ServerOnly {
		t.Error("createLibraryFile is not @serverOnly in the loaded registry -- a client can author " +
			"blobUrl again, and this package's internal-origin stamp is decorative")
	}
	if fn.MutationTemplate == nil {
		t.Fatal("createLibraryFile has no mutation template")
	}
	createOnly := map[string]bool{}
	for _, f := range fn.MutationTemplate.CreateOnlyFields {
		createOnly[f] = true
	}
	for _, f := range []string{"ownerUserId", "blobUrl"} {
		if !createOnly[f] {
			t.Errorf("createLibraryFile does not declare %q @createOnly -- a stamped create skips the "+
				"write guard's owner check, so a later create onto an existing id could re-own or "+
				"re-point that row", f)
		}
	}
}

func TestStoreMethodsDoNotReturnAContext(t *testing.T) {
	st := reflect.TypeOf(&Store{})
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	for i := 0; i < st.NumMethod(); i++ {
		m := st.Method(i)
		for out := 0; out < m.Type.NumOut(); out++ {
			if m.Type.Out(out).Implements(ctxType) {
				t.Errorf("Store.%s returns a context -- the stamped context must die inside the "+
					"call, or every later frame inherits the internal origin (memql#2989)", m.Name)
			}
		}
	}
}

func TestCreateStampsOriginAndNeverNamesOwner(t *testing.T) {
	rec := &recordingExecutor{}
	callerCtx := auth.ContextWithUserActor(context.Background(), "user-a")

	if err := NewStore(rec).Create(callerCtx, fileArgs("user-a", "f-1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(rec.statements) != 1 {
		t.Fatalf("rendered %d statements, want 1", len(rec.statements))
	}
	q := rec.statements[0]
	if !strings.HasPrefix(q, "mutation createLibraryFile(") {
		t.Errorf("rendered %q, want a createLibraryFile call", q)
	}
	if !rec.origins[0] {
		t.Errorf("the @serverOnly write executed without internal origin: %s", q)
	}
	for _, field := range []string{"ownerUserId", "status"} {
		if strings.Contains(q, field) {
			t.Errorf("the rendered write names %s -- the mutation stamps it:\n  %s", field, q)
		}
	}
	if auth.OriginFromContext(callerCtx).IsInternal() {
		t.Fatal("the caller's context became internal-origin -- the stamp escaped")
	}
}

// The upload route writes the path from the bare access id; a borrowed actor
// (the pipeline runner, the app-session recorder) may carry the canonical one.
// Both spell the same person, and refusing either would lose a real upload.
func TestCreateAdmitsTheActorsOwnKeyInEitherIdSpelling(t *testing.T) {
	cases := []struct {
		name, actor, pathOwner string
	}{
		{"bare actor, bare path", "u-1", "u-1"},
		{"canonical actor, bare path", "v1:identity:user:u-1", "u-1"},
		{"canonical actor, canonical path", "v1:identity:user:u-1", "v1:identity:user:u-1"},
	}
	for _, tc := range cases {
		rec := &recordingExecutor{}
		ctx := auth.ContextWithUserActor(context.Background(), tc.actor)
		if err := NewStore(rec).Create(ctx, fileArgs(tc.pathOwner, "f-1")); err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
		if len(rec.statements) != 1 {
			t.Errorf("%s: rendered %d statements, want 1", tc.name, len(rec.statements))
		}
	}
}

// Every refusal happens BEFORE the engine is reached: a refused write must
// leave no row, and a stamped call that reached the engine would be the
// laundering this package exists to contain.
func TestCreateRefusesBeforeTheEngineIsReached(t *testing.T) {
	actorCtx := auth.ContextWithUserActor(context.Background(), "user-a")
	withArg := func(k string, v any) map[string]any {
		a := fileArgs("user-a", "f-1")
		a[k] = v
		return a
	}
	cases := []struct {
		name string
		ctx  context.Context
		args map[string]any
	}{
		{"another owner's key", actorCtx, fileArgs("user-b", "f-1")},
		{"a key under nobody's library", actorCtx, withArg("blobUrl", "https://acct.blob.core.windows.net/memql-library/sites/user-a/index.html")},
		{"a traversal out of the actor's library", actorCtx, withArg("blobUrl", "library/user-a/../user-b/f-1/payroll.csv")},
		{"no key at all", actorCtx, withArg("blobUrl", "")},
		{"a key that is not a string", actorCtx, withArg("blobUrl", 42)},
		{"an owner named by the caller", actorCtx, withArg("ownerUserId", "user-a")},
		{"a status named by the caller", actorCtx, withArg("status", "ready")},
		{"no actor", context.Background(), fileArgs("user-a", "f-1")},
	}
	for _, tc := range cases {
		rec := &recordingExecutor{}
		if err := NewStore(rec).Create(tc.ctx, tc.args); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
		if len(rec.statements) != 0 {
			t.Errorf("%s: the engine was reached with %q", tc.name, rec.statements)
		}
	}
}

// TestCreateCallSitesResolveThroughTheRealEngine: every statement this store
// renders, parsed by the real front end -- the memql#4256 class.
func TestCreateCallSitesResolveThroughTheRealEngine(t *testing.T) {
	eng := loadedEngine(t)
	awkward := "O'Brien \"the\" <file> & co \\ line\nbreak é.md"
	rec := &parsingExecutor{t: t, engine: eng}
	ctx := auth.ContextWithUserActor(context.Background(), "user-a")

	args := fileArgs("user-a", "f-1")
	args["name"] = awkward
	args["summary"] = awkward
	args["blobUrl"] = "library/user-a/f-1/" + awkward
	args["uploadedFromWorkerName"] = awkward
	args["uploadedFromPath"] = `C:\Users\O'Brien\raw "takes"\clip é.mov`
	args["producedBy"] = map[string]any{"app": "claude-code", "model": awkward, "effort": "", "sessionId": "s-1"}
	args["size"] = int64(5 << 30)
	if err := NewStore(rec).Create(ctx, args); err != nil {
		t.Errorf("create call site: %v", err)
	}
}

type parsingExecutor struct {
	t      *testing.T
	engine *memqlengine.MemQLEngine
}

func (p *parsingExecutor) Execute(_ context.Context, q string) (any, error) {
	p.t.Helper()
	if _, err := p.engine.Parse(q); err != nil {
		return nil, err
	}
	return map[string]any{"data": []any{}}, nil
}
