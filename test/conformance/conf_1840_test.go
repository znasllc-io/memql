package conformance

// conf_1840_test.go -- the forge logic-layer arg-resolution dimension (#1840).
//
// The forge approval pipeline routes every non-direct-mutation action through a
// `logic` function: forgeRecordMentoring -> logicRecordMentoring, the routeRequest
// automation -> logicRouteRequest, forgeAttachToRequest -> logicAttachToRequest.
// Each logic body forwards the caller's args into a NESTED mutation/query call
// (e.g. `mutationRecordRequestEvent({ note: args.note })`). On staging 0.9.79
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
//  2. MULTI-STEP logic return (logicAttachToRequest `return args.requestId`,
//     logicRouteRequest `return args.event.payload.id`). The compiler emitted the
//     `_return` expression WITHOUT convertArgReferences, so `args.X` stringified
//     to the raw `arg("X")` shape and hit engine.Execute as a bare query ->
//     `function "arg" not found`. Fix: run `_return` through the same
//     convertArgReferences/convertEventReferences rewrites as every other step.
//
//  3. POSITIONAL builtin in a step (`append(arr, item)` inside
//     logicAttachToRequest). expressionToString rendered a FunctionCallExpr's
//     positional args as `name(0=v0, 1=v1)` (and in random map order), which is
//     invalid source and landed verbatim as the corrupted
//     `attachmentIds: [0="...", 1="..."]`. Fix: render contiguous 0-based args
//     positionally; emit named args in stable key order.
//
// This dimension drives the genuine connector surface (real MCP tools/call over
// a fully-loaded engine) for the three logic-backed forge tools + the multi-step
// router logic, and asserts NO arg-resolution failure leaks. It FAILS on any of
// the three pre-#1840 paths and PASSES once all three are threaded.

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
	e.runMutation(t, "mutationCreateRequest", map[string]any{
		"requestId": requestId,
		"projectId": projectId,
		"title":     "conf-1840 forge logic arg resolution",
		"body":      "Reproduce the *ast.ArgRefExpr leak across the forge logic layer.",
	})

	// --- A. forgeRecordMentoring -> logicRecordMentoring (single-return F.6) ---
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
	events := asArray(t, e.runQuery(t, "queryRequestEvents", map[string]any{"requestId": requestId}))
	if !hasEventWithNote(events, "mentored", note) {
		t.Fatalf("#1840: forgeRecordMentoring did not record a 'mentored' event carrying the note; events=%v", events)
	}

	// --- B. forgeAttachToRequest -> logicAttachToRequest (append + nested args) ---
	attachmentId := "att-" + suffix
	outB, isErrB := e.toolCall(t, "forgeAttachToRequest", map[string]any{
		"requestId":    requestId,
		"attachmentId": attachmentId,
	})
	assertNoArgRefLeak(t, "forgeAttachToRequest", outB)
	if isErrB {
		t.Fatalf("#1840: forgeAttachToRequest returned an error result: %v", outB)
	}
	reqRows := asArray(t, e.runQuery(t, "queryRequestById", map[string]any{"requestId": requestId}))
	if !attachmentPresent(reqRows, attachmentId) {
		t.Fatalf("#1840: forgeAttachToRequest did not append %q to attachmentIds; rows=%v", attachmentId, reqRows)
	}

	// --- C. logicRouteRequest (multi-step LogicRunner; the routeRequest spine) ---
	// Drive the router logic exactly as the node.created automation does -- a
	// single `event` object whose payload carries the submitter role + id. Owner
	// fast-tracks to 'queued' and records a 'routed' audit event; every nested
	// mutation arg (`args.event.payload.id`, ...) must resolve.
	routeArgs, _ := json.Marshal(map[string]any{
		"event": map[string]any{
			"payload": map[string]any{
				"id":              requestId,
				"submitterRole":   "owner",
				"submitterUserId": ownerUID,
			},
		},
	})
	if _, err := e.Eng.Execute(e.Ctx, "logicRouteRequest("+string(routeArgs)+")"); err != nil {
		assertNoArgRefLeak(t, "logicRouteRequest", err.Error())
		t.Fatalf("#1840: logicRouteRequest (multi-step) failed: %v", err)
	}
	routedEvents := asArray(t, e.runQuery(t, "queryRequestEvents", map[string]any{"requestId": requestId}))
	if !hasEventKind(routedEvents, "routed") {
		t.Fatalf("#1840: logicRouteRequest did not record a 'routed' audit event; events=%v", routedEvents)
	}

	t.Logf("#1840: forge logic layer resolved caller args across mentoring, attach, and the multi-step router")
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
