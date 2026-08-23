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
