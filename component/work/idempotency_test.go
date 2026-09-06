package work

import "testing"

func TestIdempotencyKey_IsRunStepAttempt(t *testing.T) {
	if got := IdempotencyKey("run-1", "send", 1); got != "run-1:send:1" {
		t.Fatalf("IdempotencyKey = %q", got)
	}
	if IdempotencyKey("run-1", "send", 1) == IdempotencyKey("run-1", "send", 2) {
		t.Fatal("a retry is a DIFFERENT attempt and therefore a different key, or a retried side effect would be suppressed as a duplicate of its own first try")
	}
}

// The outbox row id IS the idempotency handle: stageOutboundRequest takes
// requestId as the row id and @createOnly("status","attempts") keeps a
// re-stage from rewinding a sent row. So the derivation must be a stable
// function of the logical side effect and nothing else.
func TestOutboundRequestId_StableAndScopedToTheEffect(t *testing.T) {
	a := OutboundRequestId("run-1", "send", 1)
	if a != OutboundRequestId("run-1", "send", 1) {
		t.Fatal("not stable; a re-stage would create a second row and the message would go twice")
	}
	if a == OutboundRequestId("run-1", "send", 2) {
		t.Fatal("attempt must be part of it")
	}
	if a == OutboundRequestId("run-2", "send", 1) {
		t.Fatal("run must be part of it")
	}
	if len(a) != 64 {
		t.Fatalf("want a sha256 hex handle, got %d chars: %q", len(a), a)
	}
}
