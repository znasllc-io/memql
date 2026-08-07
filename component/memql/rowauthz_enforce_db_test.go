package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// Row-authz enforcement, end to end against a real store (memql#3172).
//
// Postgres-gated like its neighbours -- readMergeTestEngine skips when
// no DB is reachable. CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.
//
// The two claims that can only be made here: that an enforced read
// actually returns a NARROWED set, and that two callers issuing the
// identical query string inside the cache TTL never see each other's
// rows.

// rowAuthzCallerCtx is a caller as the request path builds one: an
// AccessContext (which is what actor.userId resolves from) plus a
// TokenInfo (which is what the mutation executor attributes writes to).
func rowAuthzCallerCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.Role("writer"),
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// resultBlob renders whatever a read returned -- shaped output or raw
// bundle -- so a test can ask whether a given row id is in it without
// depending on the projection's shape.
func resultBlob(t *testing.T, res *ExecuteResult) string {
	t.Helper()
	if res == nil {
		return ""
	}
	raw, err := json.Marshal(res.OutputPayload())
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return string(raw)
}

// THE NARROWING, and the cache leak, in one fixture: two callers, one
// declared concept, one row each.
func TestEnforcedReadNarrowsAndTheCacheDoesNotLeakAcrossCallers(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3172")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	noteA := "note-a-" + suffix
	noteB := "note-b-" + suffix

	ctxA := rowAuthzCallerCtx(userA)
	ctxB := rowAuthzCallerCtx(userB)

	// createNote stamps ownerUserId from actor.userId, so each row is
	// genuinely owned by the caller that wrote it.
	runMutation(t, ctxA, eng, "createNote", map[string]any{
		"noteId": noteA, "title": "A's note", "body": "belongs to A",
	})
	runMutation(t, ctxB, eng, "createNote", map[string]any{
		"noteId": noteB, "title": "B's note", "body": "belongs to B",
	})

	// --- 1. THE NARROWED SET ---
	//
	// `notes()` is a declared-concept construct. A sees exactly A's row.
	resA, err := eng.Execute(ctxA, "notes()")
	if err != nil {
		t.Fatalf("A reading notes(): %v", err)
	}
	blobA := resultBlob(t, resA)
	if !strings.Contains(blobA, noteA) {
		t.Errorf("caller A cannot see their own note: %s", blobA)
	}
	if strings.Contains(blobA, noteB) {
		t.Fatalf("caller A was returned caller B's note over a concept declaring the owned "+
			"tier: %s", blobA)
	}

	// --- 2. THE CACHE ---
	//
	// B issues the IDENTICAL query string, inside the default TTL. The
	// refused implementation injected into a local and never mutated
	// plan.Root, so planReferencesActor stayed false, the key stayed
	// shared, and B was served A's cached rows (memql#3172 finding 2).
	resB, err := eng.Execute(ctxB, "notes()")
	if err != nil {
		t.Fatalf("B reading notes(): %v", err)
	}
	blobB := resultBlob(t, resB)
	if strings.Contains(blobB, noteA) {
		t.Fatalf("caller B was served caller A's rows from the result cache: identical query "+
			"string, same TTL window, declared concept (memql#3172 finding 2).\n  B saw: %s", blobB)
	}
	if !strings.Contains(blobB, noteB) {
		t.Errorf("caller B cannot see their own note: %s", blobB)
	}

	// And A reading again (now served from cache) still sees only A.
	resA2, err := eng.Execute(ctxA, "notes()")
	if err != nil {
		t.Fatalf("A re-reading notes(): %v", err)
	}
	if blob := resultBlob(t, resA2); strings.Contains(blob, noteB) {
		t.Fatalf("A's second read (cache hit) carried B's row: %s", blob)
	}
}

// FINDING 1 end to end: a RAW query string naming another caller's row
// by id. There is no declared binding on this path -- handleExecuteQuery
// passes a client-supplied string straight to the engine -- so the
// per-row gate is the only thing between the caller and the row.
func TestRawQueryStringCannotReachAnotherCallersRow(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3172raw")
	userA := "user-a-" + suffix
	userB := "user-b-" + suffix
	noteB := "note-b-" + suffix

	ctxA := rowAuthzCallerCtx(userA)
	ctxB := rowAuthzCallerCtx(userB)

	runMutation(t, ctxB, eng, "createNote", map[string]any{
		"noteId": noteB, "title": "B's note", "body": "belongs to B",
	})

	spellings := map[string]string{
		"by id":            fmt.Sprintf(`row.id=="%s:%s"`, declaredOwnedConcept, noteB),
		"concept equality": fmt.Sprintf(`concept=="%s"`, declaredOwnedConcept),
		"top-level or": fmt.Sprintf(`concept=="%s" || row.id=="%s:%s"`,
			declaredOwnedConcept, declaredOwnedConcept, noteB),
	}
	for name, query := range spellings {
		t.Run(name, func(t *testing.T) {
			res, err := eng.Execute(ctxA, query)
			if err != nil {
				// A refusal is an acceptable outcome; being shown the row is not.
				t.Logf("%s refused: %v", name, err)
				return
			}
			if blob := resultBlob(t, res); strings.Contains(blob, noteB) {
				t.Fatalf("a raw query string spelled %q returned caller B's row to caller A. "+
					"The tier must be enforced from the DECLARATION, not from whether the "+
					"filter happens to be a top-level `concept==<id>` equality "+
					"(memql#3172 finding 1).\n  A saw: %s", name, blob)
			}
		})
	}
}

// The owner still reads their own row through every one of those
// spellings -- enforcement narrows to the caller, it does not empty the
// concept.
func TestEnforcementDoesNotHideTheCallersOwnRows(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	suffix := uniqueSuffix("rowauthz3172own")
	userA := "user-a-" + suffix
	noteA := "note-a-" + suffix
	ctxA := rowAuthzCallerCtx(userA)

	runMutation(t, ctxA, eng, "createNote", map[string]any{
		"noteId": noteA, "title": "A's note", "body": "belongs to A",
	})

	for name, query := range map[string]string{
		"named construct": "notes()",
		"raw by id":       fmt.Sprintf(`row.id=="%s:%s"`, declaredOwnedConcept, noteA),
		"raw by concept":  fmt.Sprintf(`concept=="%s"`, declaredOwnedConcept),
	} {
		t.Run(name, func(t *testing.T) {
			res, err := eng.Execute(ctxA, query)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if blob := resultBlob(t, res); !strings.Contains(blob, noteA) {
				t.Fatalf("%s: the owner cannot read their own row: %s", name, blob)
			}
		})
	}
}

// FINDING 4 end to end: a read of a declared owned concept from a
// context carrying no caller identity is REFUSED, not answered with
// whatever an `ownerUserId = <empty string>` term happens to match.
func TestActorlessReadOfADeclaredConceptIsRefused(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	_, err := eng.Execute(context.Background(), "notes()")
	if err == nil {
		t.Fatal("a read over a concept declaring the owned tier ran with no caller identity " +
			"(memql#3172 finding 4)")
	}
	if !strings.Contains(err.Error(), "row-authz") {
		t.Fatalf("the refusal does not name row-authz, so an operator cannot act on it: %v", err)
	}
}
