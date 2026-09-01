package uploadsession

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
// package's call-origin allowlist entry cites (memql#4782).
//
// This package stamps internal origin on a REQUEST-DERIVED context, which
// call_origin.go treats as the dangerous shape unless three things hold.
// Each is asserted here, against the real loaded registry where the claim
// is about the DSL:
//
//  1. THE STAMP IS REQUIRED, not decorative: createUploadSession and
//     completeUploadSession are @serverOnly in the loaded registry, so an
//     unstamped call is refused by the engine -- the annotation is the
//     control this package's stamp exists to satisfy.
//  2. THE STAMP CANNOT UNLOCK ANYTHING FOR THE CALLER: no Store method
//     returns a context, so the stamped context dies inside the one call
//     and no later frame inherits it (the memql#2989 escalation shape).
//  3. NOTHING CALLER-CHOSEN NAMES AN OWNER: the rendered create call never
//     carries ownerUserId or status -- both are stamped by the mutation
//     from the actor already on the context, which is the caller's own.
//     The read (ByID) is deliberately NOT stamped at all: it runs under
//     the caller's actor so row admission IS the owner check.

type recordingExecutor struct {
	statements []string
	origins    []bool // IsInternal at execute time, per statement
}

func (r *recordingExecutor) Execute(ctx context.Context, q string) (any, error) {
	r.statements = append(r.statements, q)
	r.origins = append(r.origins, auth.OriginFromContext(ctx).IsInternal())
	return map[string]any{"data": []any{}}, nil
}

func TestSessionConstructsAreServerOnlyInTheLoadedRegistry(t *testing.T) {
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
	for _, name := range []string{"createUploadSession", "completeUploadSession"} {
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Fatalf("%s: not in the function registry: %v", name, err)
		}
		if !fn.ServerOnly {
			t.Errorf("%s is not @serverOnly in the loaded registry -- this package's internal-origin "+
				"stamp would then be decorative, and the allowlist entry's argument false", name)
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

func TestCreateStampsOriginAndNeverNamesOwnerOrStatus(t *testing.T) {
	rec := &recordingExecutor{}
	s := NewStore(rec)
	callerCtx := auth.ContextWithUserActor(context.Background(), "user-a")

	if err := s.Create(callerCtx, CreateParams{
		UploadId: "up-1", Name: "big.mp4", Size: 99, MimeType: "video/mp4",
		BlobPath: "library/user-a/f-1/big.mp4", FileId: "f-1", ChunkSize: 4,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Complete(callerCtx, "up-1"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := s.ByID(callerCtx, "up-1"); err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if len(rec.statements) != 3 {
		t.Fatalf("rendered %d statements, want 3", len(rec.statements))
	}
	// The two writes are stamped; the read is NOT -- it must run under the
	// caller's own actor so row admission is the owner check.
	if !rec.origins[0] || !rec.origins[1] {
		t.Errorf("a @serverOnly session write executed without internal origin (create=%v complete=%v)",
			rec.origins[0], rec.origins[1])
	}
	if rec.origins[2] {
		t.Errorf("the session READ executed with internal origin -- that read is the per-chunk " +
			"authorization, and stamping it would bypass the owner check it exists to be")
	}
	for _, banned := range []string{"ownerUserId", "status"} {
		if strings.Contains(rec.statements[0], banned) {
			t.Errorf("the rendered create names %q -- the mutation stamps it, and a caller-supplied "+
				"value is exactly what @serverSet/@serverOnly exist to refuse:\n  %s", banned, rec.statements[0])
		}
	}
	// The caller's own context must still be client-origin after the calls:
	// the stamp lives on a derived local, never on anything shared.
	if auth.OriginFromContext(callerCtx).IsInternal() {
		t.Fatalf("the caller's context became internal-origin -- the stamp escaped")
	}
}

// TestSessionCallSitesResolveThroughTheRealEngine: every statement this
// store renders, parsed by the real front end -- the memql#4256 class, the
// same defence artifact_handler_test.go carries for its own store.
func TestSessionCallSitesResolveThroughTheRealEngine(t *testing.T) {
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

	awkward := "O'Brien \"the\" <file> & co \\ line\nbreak é.mp4"
	rec := &parsingExecutor{t: t, engine: eng}
	s := NewStore(rec)
	ctx := auth.ContextWithUserActor(context.Background(), "user-a")

	if err := s.Create(ctx, CreateParams{
		UploadId: "up-1", Name: awkward, Size: 5 << 30, MimeType: "video/mp4",
		FolderId: "fold-1", Labels: []string{"raw", awkward},
		UploadedFromWorkerId: "wrk-1", UploadedFromWorkerName: awkward,
		UploadedFromPath: `C:\Users\O'Brien\raw "takes"\clip é.mp4`,
		BlobPath:         "library/user-a/f-1/" + awkward, FileId: "f-1", ChunkSize: 16 << 20,
	}); err != nil {
		t.Errorf("createUploadSession: %v", err)
	}
	if err := s.Complete(ctx, "up-1"); err != nil {
		t.Errorf("completeUploadSession: %v", err)
	}
	if _, err := s.ByID(ctx, "up-1"); err != nil {
		t.Errorf("uploadSessionById: %v", err)
	}
}

// parsingExecutor hands every statement to the real engine front end. Parse
// resolves the construct name against the loaded registry, so a call naming
// an undeclared construct or argument shape fails here rather than at
// execute on a live cluster.
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
