package fileversion

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
// package's call-origin allowlist entry cites (epic memql#4806).
//
// This package stamps internal origin on a REQUEST-DERIVED context, which
// call_origin.go treats as the dangerous shape unless three things hold.
// Each is asserted here, against the real loaded registry where the claim is
// about the DSL:
//
//  1. THE STAMP IS REQUIRED, not decorative: createLibraryFileVersion and
//     supersedeLibraryFileHead are @serverOnly in the loaded registry, so an
//     unstamped call is refused by the engine.
//  2. THE STAMP CANNOT UNLOCK ANYTHING FOR THE CALLER: no Store method
//     returns a context, so the stamped context dies inside the one call
//     (the memql#2989 escalation shape).
//  3. NOTHING CALLER-CHOSEN NAMES AN OWNER: no rendered write carries
//     ownerUserId -- the mutation stamps it from the actor already on the
//     caller's context. There are no reads in this package at all, which is
//     the strongest form of the third property its sibling asserts.

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

func TestVersionConstructsAreServerOnlyInTheLoadedRegistry(t *testing.T) {
	eng := loadedEngine(t)
	for _, name := range []string{"createLibraryFileVersion", "supersedeLibraryFileHead"} {
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

func TestSupersedeStampsOriginAndNeverNamesOwner(t *testing.T) {
	rec := &recordingExecutor{}
	s := NewStore(rec)
	callerCtx := auth.ContextWithUserActor(context.Background(), "user-a")

	if err := s.Supersede(callerCtx,
		Snapshot{
			VersionId: DerivedVersionId("f-1", 1), FileId: "f-1", VersionNumber: 1,
			Name: "q3.pdf", MimeType: "application/pdf", Size: 10,
			Sha256: "abc", BlobUrl: "library/user-a/f-1/q3.pdf", Format: "pdf",
			UploadedAt: "2026-08-01T10:00:00Z",
		},
		Head{
			FileId: "f-1", VersionNumber: 2, Name: "q3-final.pdf",
			MimeType: "application/pdf", Size: 20, BlobUrl: "library/user-a/f-1/k-9/q3-final.pdf",
			Format: "pdf",
		}); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	if len(rec.statements) != 2 {
		t.Fatalf("rendered %d statements, want 2", len(rec.statements))
	}
	// The order IS the contract (design D3): the snapshot freezes the
	// outgoing head before the head moves, so a crash between them can
	// only ever duplicate a version, never lose one.
	if !strings.HasPrefix(rec.statements[0], "createLibraryFileVersion(") {
		t.Errorf("the first write is %q -- the snapshot must land before the head moves", rec.statements[0])
	}
	if !strings.HasPrefix(rec.statements[1], "supersedeLibraryFileHead(") {
		t.Errorf("the second write is %q -- the head move must follow the snapshot", rec.statements[1])
	}
	for i, q := range rec.statements {
		if !rec.origins[i] {
			t.Errorf("a @serverOnly version write executed without internal origin: %s", q)
		}
		if strings.Contains(q, "ownerUserId") {
			t.Errorf("the rendered write names ownerUserId -- the mutation stamps it, and a "+
				"caller-supplied value is exactly what @serverSet/@serverOnly exist to refuse:\n  %s", q)
		}
	}
	// The caller's own context must still be client-origin after the calls:
	// the stamp lives on a derived local, never on anything shared.
	if auth.OriginFromContext(callerCtx).IsInternal() {
		t.Fatalf("the caller's context became internal-origin -- the stamp escaped")
	}
}

// TestTheHeadMoveWritesTheBlanksItMustNotInherit: an update{} is a read-merge,
// so an OMITTED sha256 leaves the previous version's hash describing bytes
// that are gone, and an omitted uploadedFrom* leaves a machine that did not
// send these bytes. Both are false facts rather than missing ones, on fields
// a person reads as evidence -- so the head args carry the blanks explicitly.
func TestTheHeadMoveWritesTheBlanksItMustNotInherit(t *testing.T) {
	rec := &recordingExecutor{}
	s := NewStore(rec)
	ctx := auth.ContextWithUserActor(context.Background(), "user-a")

	// A chunked new version from a browser: no hash yet, no machine.
	if err := s.Supersede(ctx,
		Snapshot{
			VersionId: DerivedVersionId("f-1", 1), FileId: "f-1", VersionNumber: 1,
			Name: "clip.mov", MimeType: "video/quicktime", Size: 1,
			Sha256: "deadbeef", BlobUrl: "library/user-a/f-1/clip.mov",
			UploadedFromWorkerId: "wrk-1", UploadedFromWorkerName: "MacBook-Pro",
			UploadedFromPath: "/Users/a/clip.mov", UploadedAt: "2026-08-01T10:00:00Z",
		},
		Head{
			FileId: "f-1", VersionNumber: 2, Name: "clip.mov",
			MimeType: "video/quicktime", Size: 2, BlobUrl: "library/user-a/f-1/k-9/clip.mov",
		}); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	head := rec.statements[1]
	for _, want := range []string{
		`sha256: ""`,
		`uploadedFromWorkerId: ""`,
		`uploadedFromWorkerName: ""`,
		`uploadedFromPath: ""`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("the head move omits %s, so the read-merge keeps the PREVIOUS version's "+
				"value on bytes it does not describe:\n  %s", want, head)
		}
	}
	// The snapshot, by contrast, is an insert of a frozen row: the fields it
	// does carry are the outgoing head's real ones.
	snap := rec.statements[0]
	for _, want := range []string{`sha256: "deadbeef"`, `uploadedFromWorkerName: "MacBook-Pro"`} {
		if !strings.Contains(snap, want) {
			t.Errorf("the snapshot lost %s -- a version's own facts are frozen with it:\n  %s", want, snap)
		}
	}
}

func TestSupersedeRefusesAShapeThatCouldCorruptHistory(t *testing.T) {
	s := NewStore(&recordingExecutor{})
	ctx := context.Background()
	cases := []struct {
		name string
		snap Snapshot
		head Head
	}{
		{"different files", Snapshot{FileId: "f-1", VersionNumber: 1}, Head{FileId: "f-2", VersionNumber: 2}},
		{"no advance", Snapshot{FileId: "f-1", VersionNumber: 2}, Head{FileId: "f-1", VersionNumber: 2}},
		{"going backwards", Snapshot{FileId: "f-1", VersionNumber: 3}, Head{FileId: "f-1", VersionNumber: 2}},
		{"no file", Snapshot{VersionNumber: 1}, Head{VersionNumber: 2}},
	}
	for _, tc := range cases {
		if err := s.Supersede(ctx, tc.snap, tc.head); err == nil {
			t.Errorf("%s: Supersede accepted a shape that would leave the history unreadable", tc.name)
		}
	}
}

// TestVersionCallSitesResolveThroughTheRealEngine: every statement this store
// renders, parsed by the real front end -- the memql#4256 class, the same
// defence artifact_handler_test.go and the session store carry.
func TestVersionCallSitesResolveThroughTheRealEngine(t *testing.T) {
	eng := loadedEngine(t)
	awkward := "O'Brien \"the\" <file> & co \\ line\nbreak é.mov"
	rec := &parsingExecutor{t: t, engine: eng}
	s := NewStore(rec)
	ctx := auth.ContextWithUserActor(context.Background(), "user-a")

	if err := s.Supersede(ctx,
		Snapshot{
			VersionId: DerivedVersionId("f-1", 7), FileId: "f-1", VersionNumber: 7,
			Name: awkward, MimeType: "video/quicktime", Size: 5 << 30,
			Sha256: "abc", BlobUrl: "library/user-a/f-1/" + awkward, Format: "other",
			Summary:              awkward,
			UploadedFromWorkerId: "wrk-1", UploadedFromWorkerName: awkward,
			UploadedFromPath: `C:\Users\O'Brien\raw "takes"\clip é.mov`,
			UploadedAt:       "2026-08-01T10:00:00Z",
		},
		Head{
			FileId: "f-1", VersionNumber: 8, Name: awkward, MimeType: "video/quicktime",
			Size: 6 << 30, BlobUrl: "library/user-a/f-1/k-9/" + awkward, Format: "other",
			UploadedFromWorkerId: "wrk-1", UploadedFromWorkerName: awkward,
			UploadedFromPath: `C:\Users\O'Brien\raw "takes"\clip é.mov`,
		}); err != nil {
		t.Errorf("supersede call sites: %v", err)
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
