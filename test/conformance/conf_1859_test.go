package conformance

// conf_1859_test.go -- the forge-audit short-id normalization dimension (#1859).
//
// Staging-confirmed root cause (0.9.84): forge routing works (#1847) and the
// audit events ARE written, but `forgeRequestHistory(<shortId>)` returns empty
// because the automation path keys the events under the CANONICAL request id.
//
//   the routeRequest / recordTransition automations write (since #2235,
//   event-bound persist steps) `recordRequestEvent { requestId: event.payload.id,
//   ... }`, and a node.created / node.updated event's `payload.id` is the
//   CANONICAL node id (`v1:forge:request:r-...`). The TOOL path (recordMentoredEvent,
//   the forgeRecordMentoring handler since #2235; formerly the recordMentoring
//   logic) passes the SHORT tool arg, so its `mentored` event is keyed short. The
//   audit trail splits across two id forms; requestEvents matches the
//   field literally, so a short-id read only finds one form.
//
// The fix normalizes on the WRITE side: `recordRequestEvent` stores
// `requestId: shortId(args.requestId)`. shortId() strips the
// `v1:forge:request:` concept prefix and is idempotent, so:
//   - the automation path (canonical payload.id) is reduced to the short id;
//   - the tool path (already short) is unchanged (no-op strip).
// Every requestEvent.requestId then lands in one short form and
// requestEvents(<shortId>) returns the full trail.
//
// This dimension drives the REAL routeRequest + recordTransition automations
// (the canonical-id write path) AND a direct short-id write (the tool path),
// then asserts BOTH are found under the SHORT id, and that no event row is
// stored under the canonical id. It FAILS on pre-fix main (events keyed
// canonical, short-id read empty) and PASSES once the write normalizes to short.

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

func forgeAuditShortIdCheck() check {
	return check{
		Issue:   "#1859",
		Dim:     "forge-audit-shortid-normalize",
		NeedsDB: true,
		Run:     runForgeAuditShortId,
	}
}

func runForgeAuditShortId(t *testing.T, e *Env) {
	suffix := uniqueSuffix("1859")

	// --- A. routeRequest (node.created -> 'routed', automation/canonical path) ---
	shortId := "req-1859-" + suffix
	e.runMutation(t, "createRequest", map[string]any{
		"requestId": shortId,
		"projectId": "proj-" + suffix,
		"title":     "conf-1859 forge audit short-id normalization",
		"body":      "Owner submission must auto-route + key its audit events by the SHORT request id.",
	})

	before := requestRow(t, e, shortId)
	canonicalId := asStr(before["id"])
	if canonicalId == shortId {
		t.Fatalf("#1859 precondition: expected the stored id to be canonicalized away from the short tool id %q, got %q",
			shortId, canonicalId)
	}

	// Drive the REAL routeRequest automation with the REAL node.created event,
	// whose payload IS the request row carrying the CANONICAL id. routeRequest's
	// recordRequestEvent therefore receives the canonical id -- which the
	// shortId() normalization must reduce back to the short form on write.
	fireForgeAutomation(t, e, "routeRequest", "node.created", events.KindNodeCreated, cloneStringMap(before))

	// THE #1859 acceptance: the 'routed' audit event is found by the SHORT id.
	// Pre-fix this is empty (the event is keyed under the canonical id).
	routedByShort := asArray(t, e.runQuery(t, "requestEvents", map[string]any{"requestId": shortId}))
	if !hasEventKind(routedByShort, "routed") {
		t.Fatalf("#1859: requestEvents(<shortId>=%q) did not return the 'routed' event "+
			"(the automation path keyed it under the canonical id %q instead of normalizing to short); events=%v",
			shortId, canonicalId, routedByShort)
	}

	// And there must be NO event row stored under the canonical id form -- the
	// write side normalized everything to short, so the split is gone.
	routedByCanonical := asArray(t, e.runQuery(t, "requestEvents", map[string]any{"requestId": canonicalId}))
	if len(routedByCanonical) != 0 {
		t.Fatalf("#1859: requestEvents(<canonicalId>=%q) returned %d event(s); expected 0 -- "+
			"every requestEvent.requestId must be the short form; events=%v",
			canonicalId, len(routedByCanonical), routedByCanonical)
	}

	// --- B. recordTransition (node.updated -> 'approved', automation/canonical) ---
	e.runMutation(t, "advanceRequest", map[string]any{
		"requestId": shortId,
		"status":    "queued",
	})
	updated := cloneStringMap(requestRow(t, e, canonicalId))
	updated["oldStatus"] = "submitted"
	fireForgeAutomation(t, e, "recordTransition", "node.updated", events.KindNodeUpdated, updated)

	afterTransition := asArray(t, e.runQuery(t, "requestEvents", map[string]any{"requestId": shortId}))
	if !hasEventKind(afterTransition, "approved") {
		t.Fatalf("#1859: requestEvents(<shortId>=%q) did not return the 'approved' event after the queued "+
			"transition (recordTransition keyed it under the canonical id); events=%v", shortId, afterTransition)
	}

	// --- C. tool path (recordMentoredEvent -> 'mentored', short id) ----------
	// The mentoring tool forwards the caller's SHORT requestId verbatim. After the
	// fix shortId() is a no-op on an already-short id, so the tool-written event
	// stays short and must STILL be found under the short id (no regression), and
	// it must sit alongside the automation-written events under ONE id form.
	// (recordMentoredEvent normalizes via shortId() identically; here we drive
	// recordRequestEvent directly with kind="mentored" to exercise the same
	// short-keyed write the tool produces.)
	e.runMutation(t, "recordRequestEvent", map[string]any{
		"requestId": shortId, // short tool arg, exactly as recordMentoredEvent passes it
		"kind":      "mentored",
		"note":      "conf-1859 mentoring touch",
	})

	full := asArray(t, e.runQuery(t, "requestEvents", map[string]any{"requestId": shortId}))
	for _, kind := range []string{"routed", "approved", "mentored"} {
		if !hasEventKind(full, kind) {
			t.Fatalf("#1859: the full audit trail read by the SHORT id %q is missing the %q event -- "+
				"automation-path (canonical) and tool-path (short) events are not unified under one id form; events=%v",
				shortId, kind, full)
		}
	}

	t.Logf("#1859: forge audit trail keyed consistently by the short id %q -- routed/approved (automation/canonical write) "+
		"+ mentored (tool/short write) all found under one short id; zero events under the canonical id %q",
		shortId, canonicalId)
}
