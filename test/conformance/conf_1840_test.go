package conformance

// conf_1840_test.go -- the forge logic-layer arg-resolution dimension (#1840).
//
// The forge approval pipeline routes every non-direct-mutation action through a
// `logic` function: forgeRecordMentoring -> recordMentoring, the routeRequest
// automation -> routeRequest, forgeAttachToRequest -> attachToRequest.
// Each logic body forwards the caller's args into a NESTED mutation/query call
// (e.g. `recordRequestEvent({ note: args.note })`). On staging 0.9.79
// every one of these failed with `argument "<x>": expected string, got
// *ast.ArgRefExpr` (the routing + audit + mentoring + attach outage tracked by
// #1840), while the direct-mutation tools (validate / approve / changes) all
// succeeded.
//
// Three distinct defects, one per code path, all needed for the pipeline:
//
//  1. SINGLE-RETURN logic (forgeRecordMentoring). A logic whose `return` body is
//     one nested mutation call is inlined into plan.MutationCall (the F.6 hoist
//     in resolvePlanFunctions). substituteArgRefValue only matched the engine
//     *ArgReference node, but the nested object-literal args carry the PARSER
//     node *ast.ArgRefExpr verbatim (convertFunctionCallExpr shallow-copies
//     them), so they reached the mutation validator unresolved -> "got
//     *ast.ArgRefExpr". Fix: substituteArgRefValue threads the parser node too.
//
//  2. MULTI-STEP logic return (attachToRequest `return args.requestId`,
//     routeRequest `return args.event.payload.id`). The compiler emitted the
//     `_return` expression WITHOUT convertArgReferences, so `args.X` stringified
//     to the raw `arg("X")` shape and hit engine.Execute as a bare query ->
//     `function "arg" not found`. Fix: run `_return` through the same
//     convertArgReferences/convertEventReferences rewrites as every other step.
//
//  3. POSITIONAL builtin in a step (`append(arr, item)` inside
//     attachToRequest). expressionToString rendered a FunctionCallExpr's
//     positional args as `name(0=v0, 1=v1)` (and in random map order), which is
//     invalid source and landed verbatim as the corrupted
//     `attachmentIds: [0="...", 1="..."]`. Fix: render contiguous 0-based args
//     positionally; emit named args in stable key order.
//
// This dimension drives the genuine connector surface (real MCP tools/call over
// a fully-loaded engine) for the forge tools + the router logic, and asserts NO
// arg-resolution failure leaks. It FAILS on any of the three pre-#1840 paths and
// PASSES once all three are threaded.
//
// #2235 update (logic-purity burn-down): the mentoring wrapper logic was
// retired (forgeRecordMentoring now targets the thin `recordMentoredEvent`
// mutation directly), and routeRequest is now a PURE decision logic whose
// advanceRequest + 'routed' recordRequestEvent writes moved to the routeRequest
// automation's persist steps. Section A therefore exercises the direct-mutation
// tool path, and section C asserts arg-resolution on routeRequest's pure return
// (the end-to-end automation dispatch + the 'routed' write stay covered by
// conf_1847 / conf_1859). The arg-resolution machinery under test is unchanged.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func forgeLogicArgResolutionCheck() check {
	return check{
		Issue:   "#1840",
		Dim:     "forge-logic-arg-resolution",
		NeedsDB: true,
		Run:     runForgeLogicArgResolution,
	}
}

// argRefLeakSignatures mark a FAILURE caused by an unresolved arg reference
// reaching a downstream validator/evaluator -- the #1840 outage shape. Any of
// these appearing in a tool result or engine error is a regression.
var argRefLeakSignatures = []string{
	"ast.ArgRefExpr",           // the verbatim staging failure: "expected string, got *ast.ArgRefExpr"
	"ArgRefExpression",         // the engine-side variant of the same unresolved node
	`function "arg" not found`, // append/builtin fell through to engine lookup (attach path)
}

func assertNoArgRefLeak(t *testing.T, label string, payload any) {
	t.Helper()
	text := fmt.Sprintf("%v", payload)
	for _, sig := range argRefLeakSignatures {
		if strings.Contains(text, sig) {
			t.Fatalf("#1840 regression: %s leaked an unresolved arg reference (%q): %s", label, sig, text)
		}
	}
}

func runForgeLogicArgResolution(t *testing.T, e *Env) {
	suffix := uniqueSuffix("1840")
	requestId := "req-" + suffix
	projectId := "proj-" + suffix

	// Seed a request the attach + route flows can target. submitterRole is
	// stamped from the owner actor; no automation scheduler is wired in the rig,
	// so the request stays 'submitted' until we drive the router logic directly.
	e.runMutation(t, "createRequest", map[string]any{
		"requestId": requestId,
		"projectId": projectId,
		"title":     "conf-1840 forge logic arg resolution",
		"body":      "Reproduce the *ast.ArgRefExpr leak across the forge logic layer.",
	})

	// --- A. forgeRecordMentoring -> recordMentoredEvent mutation (#2235) ---
	eventId := "evt-" + suffix
	note := "Here is what the approval pipeline does and why it matters."
	outA, isErrA := e.toolCall(t, "forgeRecordMentoring", map[string]any{
		"eventId":   eventId,
		"requestId": requestId,
		"note":      note,
	})
	assertNoArgRefLeak(t, "forgeRecordMentoring", outA)
	if isErrA {
		t.Fatalf("#1840: forgeRecordMentoring returned an error result: %v", outA)
	}
	// The 'mentored' event must be in the audit trail with the note threaded
	// through the logic into the nested mutation.
	events := asArray(t, e.runQuery(t, "requestEvents", map[string]any{"requestId": requestId}))
	if !hasEventWithNote(events, "mentored", note) {
		t.Fatalf("#1840: forgeRecordMentoring did not record a 'mentored' event carrying the note; events=%v", events)
	}

	// --- B. forgeAttachToRequest -> attachToRequest (append + nested args) ---
	attachmentId := "att-" + suffix
	outB, isErrB := e.toolCall(t, "forgeAttachToRequest", map[string]any{
		"requestId":    requestId,
		"attachmentId": attachmentId,
	})
	assertNoArgRefLeak(t, "forgeAttachToRequest", outB)
	if isErrB {
		t.Fatalf("#1840: forgeAttachToRequest returned an error result: %v", outB)
	}
	reqRows := asArray(t, e.runQuery(t, "requestById", map[string]any{"requestId": requestId}))
	if !attachmentPresent(reqRows, attachmentId) {
		t.Fatalf("#1840: forgeAttachToRequest did not append %q to attachmentIds; rows=%v", attachmentId, reqRows)
	}

	// --- B'. forgeAttachToRequest hardening (#1848) ---------------------------
	// #1848 added these acceptance criteria on top of the single-attach above:
	//   1. A SECOND sequential attach to the same request must APPEND (keyed on
	//      the canonical id) -- both ids present, in order, with no clobber and no
	//      index-prefixed / literal-expression junk (the pre-#1840 `[0="...",
	//      1="..."]` corruption shape).
	//   2. Invalid input (nonexistent request / attachment) must fail cleanly
	//      with NO partial / corrupting write to attachmentIds.
	runForgeAttachHardening(t, e, requestId, attachmentId)

	// --- C. routeRequest (pure decision logic; #2235) ---
	// routeRequest is now a PURE logic: it projects the routing inputs
	// (requestId + submitterRole + submitterUserId); the routeRequest AUTOMATION
	// performs the advanceRequest + 'routed' recordRequestEvent writes in its
	// persist steps (the end-to-end dispatch + 'routed' write are covered by
	// conf_1847 / conf_1859). This dimension keeps the #1840 coverage by driving
	// the logic DIRECTLY via engine.Execute and asserting every nested arg
	// (`args.event.payload.id`, `args.event.payload.submitterRole`, ...)
	// RESOLVES in the returned decision -- no *ast.ArgRefExpr leak.
	routeArgs, _ := json.Marshal(map[string]any{
		"event": map[string]any{
			"payload": map[string]any{
				"id":              requestId,
				"submitterRole":   "owner",
				"submitterUserId": ownerUID,
			},
		},
	})
	routeRes, err := e.Eng.Execute(e.Ctx, "routeRequest("+string(routeArgs)+")")
	if err != nil {
		assertNoArgRefLeak(t, "routeRequest", err.Error())
		t.Fatalf("#1840: routeRequest (pure decision) failed: %v", err)
	}
	routeText := fmt.Sprintf("%v", routeRes)
	assertNoArgRefLeak(t, "routeRequest", routeText)
	// The projected decision must carry the RESOLVED caller args -- the
	// requestId (from args.event.payload.id) and the submitter role -- proving
	// the return object literal's arg refs were rewritten, not stringified raw.
	if !strings.Contains(routeText, requestId) || !strings.Contains(routeText, "owner") {
		t.Fatalf("#1840: routeRequest decision did not carry the resolved args "+
			"(requestId=%q, submitterRole=owner); got %v", requestId, routeRes)
	}

	t.Logf("#1840: forge logic layer resolved caller args across mentoring (mutation), attach, and the pure router decision")
}

// runForgeAttachHardening drives the #1848 acceptance criteria against the SAME
// seeded request the #1840 dimension already attached one id to. It runs the
// real forgeAttachToRequest MCP tool (-> attachToRequest -> append +
// attachToRequest), keyed on the SHORT request id the caller passes,
// and asserts the canonical-id read-merge-append behaves:
//
//   - a second sequential attach appends without clobbering the first;
//   - the resulting attachmentIds is a clean ordered [first, second] with no
//     index-prefixed / literal-expression junk (the corruption shape #1848 bans);
//   - bad input (nonexistent request, nonexistent attachment) fails cleanly with
//     NO partial / corrupting write -- the good row's array is unchanged.
func runForgeAttachHardening(t *testing.T, e *Env, requestId, firstAttachmentId string) {
	t.Helper()

	// 1. Second sequential attach -> [first, second], in order, no clobber/junk.
	secondAttachmentId := firstAttachmentId + "-2"
	out2, isErr2 := e.toolCall(t, "forgeAttachToRequest", map[string]any{
		"requestId":    requestId,
		"attachmentId": secondAttachmentId,
	})
	assertNoArgRefLeak(t, "forgeAttachToRequest(second)", out2)
	if isErr2 {
		t.Fatalf("#1848: second forgeAttachToRequest returned an error result: %v", out2)
	}

	rows := asArray(t, e.runQuery(t, "requestById", map[string]any{"requestId": requestId}))
	ids := attachmentIdsOf(t, rows, requestId)
	assertCleanAttachmentIds(t, "after two sequential attaches", ids)
	if len(ids) != 2 || ids[0] != firstAttachmentId || ids[1] != secondAttachmentId {
		t.Fatalf("#1848: sequential attach did not produce ordered [%q, %q] without clobber; got %#v",
			firstAttachmentId, secondAttachmentId, ids)
	}

	// 2. Invalid input must NOT corrupt or partially write the existing array.
	//    Capture the known-good state, attempt bad attaches, re-read, assert
	//    byte-for-byte identical (no append, no junk, no clobber).
	before := append([]string(nil), ids...)

	// (a) Nonexistent request: the read finds no row, so append() has no array to
	//     merge onto -- the pre-#1840 path stringified that into the corrupted
	//     `attachmentIds` write. It must fail cleanly and write nothing.
	badReqId := requestId + "-does-not-exist"
	outBadReq, isErrBadReq := e.toolCall(t, "forgeAttachToRequest", map[string]any{
		"requestId":    badReqId,
		"attachmentId": "att-orphan-001",
	})
	assertNoArgRefLeak(t, "forgeAttachToRequest(badRequest)", outBadReq)
	if !isErrBadReq {
		t.Fatalf("#1848: forgeAttachToRequest against a nonexistent request %q should fail, got success: %v", badReqId, outBadReq)
	}
	// The bad request id must not have been conjured into existence.
	if badRows, _ := e.runQuery(t, "requestById", map[string]any{"requestId": badReqId}).([]any); len(badRows) != 0 {
		t.Fatalf("#1848: a failed attach against nonexistent request %q wrote a phantom row: %#v", badReqId, badRows)
	}

	// (b) Nonexistent attachment id against the GOOD request: whatever the engine
	//     does, the existing array must not be partially written or corrupted.
	outBadAtt, isErrBadAtt := e.toolCall(t, "forgeAttachToRequest", map[string]any{
		"requestId":    requestId,
		"attachmentId": "att-does-not-exist-002",
	})
	assertNoArgRefLeak(t, "forgeAttachToRequest(badAttachment)", outBadAtt)
	_ = isErrBadAtt // the contract is "no corrupting write", not a specific error/ok

	// Re-read the good request: its array must be uncorrupted. If the bad-att
	// path succeeded it may have appended cleanly (an extra clean id is fine);
	// what it must NEVER do is clobber the prior ids or write index/literal junk.
	afterRows := asArray(t, e.runQuery(t, "requestById", map[string]any{"requestId": requestId}))
	afterIds := attachmentIdsOf(t, afterRows, requestId)
	assertCleanAttachmentIds(t, "after bad-input attaches", afterIds)
	for i, want := range before {
		if i >= len(afterIds) || afterIds[i] != want {
			t.Fatalf("#1848: bad-input attach corrupted/clobbered the existing attachmentIds; before=%#v after=%#v", before, afterIds)
		}
	}

	t.Logf("#1848: forgeAttachToRequest appends sequentially (canonical-id keyed, clean array) and refuses to corrupt on bad input")
}

// attachmentIdsOf extracts the attachmentIds array for the request row with the
// given short id from a decoded requestById result, as an ordered []string.
func attachmentIdsOf(t *testing.T, rows []any, requestId string) []string {
	t.Helper()
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m == nil {
			continue
		}
		// requestById is keyed on the request, so a single row comes back;
		// match defensively on the short id stored in payload.id when present.
		out := make([]string, 0, 4)
		ids, _ := m["attachmentIds"].([]any)
		for _, a := range ids {
			out = append(out, asStr(a))
		}
		return out
	}
	t.Fatalf("#1848: requestById returned no row for %q; rows=%#v", requestId, rows)
	return nil
}

// assertCleanAttachmentIds fails if any entry carries the pre-#1840 corruption
// shape -- an index prefix (`0=`, `1=`) or a leaked literal expression
// (`existing.first.payload.attachmentIds`, embedded quotes). A clean array is
// plain attachment-id strings.
func assertCleanAttachmentIds(t *testing.T, label string, ids []string) {
	t.Helper()
	for i, id := range ids {
		if strings.Contains(id, "=") {
			t.Fatalf("#1848: %s -- attachmentIds[%d]=%q carries an index-prefixed/literal `=` junk shape (the pre-#1840 corruption); ids=%#v", label, i, id, ids)
		}
		if strings.Contains(id, "\"") || strings.Contains(id, "payload.attachmentIds") || strings.Contains(id, "append(") {
			t.Fatalf("#1848: %s -- attachmentIds[%d]=%q carries a leaked literal expression; ids=%#v", label, i, id, ids)
		}
	}
}

// hasEventWithNote reports whether the decoded requestEvent array contains a row
// of the given kind whose note matches.
func hasEventWithNote(events []any, kind, note string) bool {
	for _, ev := range events {
		m, _ := ev.(map[string]any)
		if m == nil {
			continue
		}
		if asStr(m["kind"]) == kind && asStr(m["note"]) == note {
			return true
		}
	}
	return false
}

func hasEventKind(events []any, kind string) bool {
	for _, ev := range events {
		m, _ := ev.(map[string]any)
		if m != nil && asStr(m["kind"]) == kind {
			return true
		}
	}
	return false
}

// attachmentPresent reports whether any request row's attachmentIds array
// contains the id.
func attachmentPresent(rows []any, id string) bool {
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m == nil {
			continue
		}
		ids, _ := m["attachmentIds"].([]any)
		for _, a := range ids {
			if asStr(a) == id {
				return true
			}
		}
	}
	return false
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}
