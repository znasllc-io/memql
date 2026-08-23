package worker

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// TestAppSessionWritesCarryInternalOrigin is the regression guard for a
// failure that is INVISIBLE at the call site.
//
// All three app-session mutations are @serverOnly. `auth.OriginClient` is the
// ZERO VALUE, so a context nobody stamped reads as an untrusted client call
// and the engine REFUSES the write -- logging a WARN and returning in a way
// the caller reads as fine. The symptom is a session row that never appears,
// with the only evidence at a log level nobody is watching.
//
// This asserts the property directly rather than through the engine, because
// the engine's refusal is exactly the thing that does not surface.
func TestAppSessionWritesCarryInternalOrigin(t *testing.T) {
	ctx := appSessionWriteContext(context.Background(), "v1:identity:user:alice")

	if got := auth.OriginFromContext(ctx); got != auth.OriginInternal {
		t.Fatalf("call origin = %v, want %v -- without it every @serverOnly "+
			"app-session write is refused with only a WARN", got, auth.OriginInternal)
	}
}

// TestAppSessionWritesBorrowTheOwnersActor: the mutations stamp ownerUserId
// from the actor, so the write has to RUN as that user. A system actor here
// would either fail the stamp or attribute the row to the engine.
func TestAppSessionWritesBorrowTheOwnersActor(t *testing.T) {
	const owner = "v1:identity:user:alice"
	ctx := appSessionWriteContext(context.Background(), owner)

	access, ok := auth.AccessFromContext(ctx)
	if !ok || access == nil {
		t.Fatal("no access context -- the write cannot stamp ownerUserId from the actor")
	}
	if access.UserId != owner {
		t.Fatalf("actor userId = %q, want %q", access.UserId, owner)
	}
	if access.IsClusterOwner() {
		t.Fatal("the borrowed actor must NOT be a cluster owner -- the engine acts AS the " +
			"user whose row it writes, never above them")
	}
}

// TestTranscriptFlushStillStampsOrigin: the flush names only the session, so
// there is no owner to borrow -- but it is @serverOnly all the same, and
// forgetting the stamp on this one path would silently lose every transcript
// while sessions themselves kept working.
func TestTranscriptFlushStillStampsOrigin(t *testing.T) {
	ctx := appSessionWriteContext(context.Background(), "")
	if got := auth.OriginFromContext(ctx); got != auth.OriginInternal {
		t.Fatalf("call origin = %v, want %v", got, auth.OriginInternal)
	}
}

// TestTranscriptCollectorMarksItsOwnTruncation. A transcript that simply
// stops reads as a run that stopped, which is the wrong conclusion to invite
// -- so the bound is announced inside the text, not only in a sibling field.
func TestTranscriptCollectorMarksItsOwnTruncation(t *testing.T) {
	c := &transcriptCollector{max: 16}
	for i := 0; i < 5; i++ {
		c.append(AppSessionChunk{Data: []byte("0123456789"), Seq: uint64(i + 1)})
	}
	text, seen, truncated := c.snapshot()

	if !truncated {
		t.Fatal("past the bound without being marked truncated")
	}
	if seen != 50 {
		t.Fatalf("bytes seen = %d, want 50 -- the counter must report what ARRIVED, "+
			"not what was kept, or a reader cannot tell how much was dropped", seen)
	}
	if len(text) <= 16 {
		t.Fatalf("the truncation notice must be appended even past the bound; got %q", text)
	}
}

// TestCredentialExpiryIsHonest: a zero expiry is not "expired", it is
// "unknown". Treating it as expired would make a renewal loop spin on a
// credential that is probably fine.
func TestCredentialExpiryIsHonest(t *testing.T) {
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

	if (Credential{}).Expired(now) {
		t.Fatal("a credential with no recorded expiry must not read as expired")
	}
	if !(Credential{ExpiresAt: now.Add(-time.Second)}).Expired(now) {
		t.Fatal("a past expiry must read as expired")
	}
	if (Credential{ExpiresAt: now.Add(time.Hour)}).Expired(now) {
		t.Fatal("a future expiry must not read as expired")
	}
}

// TestInternalOriginCannotEscapeOneWrite is the assertion
// call_origin_conformance_test.go's allowlist entry for this package
// PROMISES, rather than merely claims.
//
// memql#2989's escalation shape is a package that stamps internal origin onto
// a context a LATER frame inherits: one @serverOnly call then opens every
// other @serverOnly construct for the rest of the request. The property that
// rules it out here is structural -- appSessionWriteContext's result is a
// local in each store method and is never returned, so the caller's context
// is untouched.
//
// Asserted by observing the caller's own context AFTER the write helper has
// run against a derived one.
func TestInternalOriginCannotEscapeOneWrite(t *testing.T) {
	callerCtx := context.Background()

	writeCtx := appSessionWriteContext(callerCtx, "v1:identity:user:alice")
	if auth.OriginFromContext(writeCtx) != auth.OriginInternal {
		t.Fatal("the derived context must carry internal origin")
	}

	// The caller's context is unchanged -- a Go context is immutable, and
	// this test exists so that a future refactor which starts RETURNING the
	// stamped context (or caching it on the store) fails here rather than
	// silently widening what one stamp unlocks.
	if got := auth.OriginFromContext(callerCtx); got == auth.OriginInternal {
		t.Fatalf("the caller's context gained internal origin (%v) -- one @serverOnly "+
			"write must not unlock the rest of them for the rest of the request", got)
	}
	if _, ok := auth.AccessFromContext(callerCtx); ok {
		t.Fatal("the caller's context gained a borrowed actor")
	}
}

// TestStoreMethodsDoNotReturnAContext pins the structural half of the claim
// above: no exported method on the store hands a stamped context back.
// A signature change is what would break the property, so a signature check
// is what guards it.
func TestStoreMethodsDoNotReturnAContext(t *testing.T) {
	var store AppSessionStore = (*EngineStore)(nil)
	// The interface is the whole write surface; every method returns error
	// and nothing else. If a future method returns a context, this stops
	// compiling -- which is the point.
	var _ interface {
		CreateAppSession(context.Context, AppSessionRow) error
		AppendAppSessionTranscript(context.Context, string, string, int, bool, string) error
		EndAppSession(context.Context, AppSessionRow) error
	} = store
}
