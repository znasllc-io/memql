package work

// idempotency.go -- side effects run under a key derived from the run,
// the step and the attempt (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section D
// "Side effects").
//
// The old rule was that a mutation or webhook step is never retried. The
// new rule is: retried when idempotent BY KEY, parked otherwise. That
// only works if the key is a stable function of the logical effect, so
// both halves live here rather than being spelled at each call site.
//
// ATTEMPT IS PART OF THE KEY, and that is the subtle half. A retry is a
// genuinely new attempt at the effect: keying only on (run, step) would
// make a deliberate retry look like a duplicate of its own first try and
// the outbox would suppress it. What the key protects against is the
// SAME attempt being staged twice -- a resumed run re-reaching a step it
// already staged -- which is exactly the case the journal cannot decide
// on its own.

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// IdempotencyKey is the human-readable form written onto
// v1:work:step.idempotencyKey.
func IdempotencyKey(runId, stepKey string, attempt int) string {
	return runId + ":" + stepKey + ":" + strconv.Itoa(attempt)
}

// OutboundRequestId is the row id a side effect stages under. It IS the
// idempotency handle: stageOutboundRequest takes requestId as the row id,
// and @createOnly("status", "attempts") means a re-stage refreshes the
// deliverable content without rewinding a row the drainer already sent.
// Hashed rather than used raw because a row id is one path segment and a
// step key may carry anything a template author wrote.
func OutboundRequestId(runId, stepKey string, attempt int) string {
	h := sha256.Sum256([]byte(IdempotencyKey(runId, stepKey, attempt)))
	return hex.EncodeToString(h[:])
}
