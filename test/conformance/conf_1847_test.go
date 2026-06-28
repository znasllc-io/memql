package conformance

// conf_1847_test.go -- the automation-step multi-step-logic dispatch dimension
// (#1847, reopened).
//
// The staging-confirmed root cause: when a `logic` function with a MULTI-STEP
// body is invoked as an AUTOMATION STEP (`step run { logic routeRequest {
// event: event } }`), only its `return` expression is evaluated -- the
// intermediate side-effecting steps (the guarded `advanceRequest`
// transitions + the `recordRequestEvent` audit write) are SKIPPED. The
// SAME logic invoked via the TOOL path (engine.Execute / RunLogic) runs every
// step. On staging the routeRequest automation fired + "completed" but logged
// only `step=_return`, so a fresh owner submission stayed `submitted` with an
// empty history.
//
// The #1840 dimension (conf_1840_test.go) drove routeRequest DIRECTLY via
// `e.Eng.Execute("routeRequest(...)")` -- the tool/engine path -- which is
// exactly why the bug slipped through 3x: nothing exercised the node.created ->
// automation -> multi-step-logic dispatch end to end. This dimension closes
// that gap by running the REAL `routeRequest` automation (loaded from the
// embedded DSL) through a REAL automation Executor with a REAL node.created
// triggering event, then asserting the side effects PERSISTED:
//
//   - the request advanced submitted -> queued (the owner fast-track
//     advanceRequest step), and
//   - a `routed` v1:forge:requestEvent was written (the recordRequestEvent
//     step).
//
// Neither side effect is referenced by routeRequest's `return
// args.event.payload.id`, so a dispatch that only evaluates the return drops
// both. It FAILS on pre-fix main (the staging shape) and PASSES once the
// automation-step logic invocation runs the full body.
//
// #2235 update (logic-purity burn-down): routeRequest + recordTransition are
// now PURE decision logics; their advanceRequest / recordRequestEvent writes
// moved into the automations' explicit decide->persist steps (ADR §2.1
// single-writer). This dimension's contract is unchanged and still the right
// end-to-end check: driving the REAL automation must still PERSIST the side
// effects (now via the persist steps instead of the logic body). It would FAIL
// if a persist step were dropped or mis-wired.

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/events"
)

func automationLogicFullBodyCheck() check {
	return check{
		Issue:   "#1847",
		Dim:     "automation-step-logic-fullbody",
		NeedsDB: true,
		Run:     runAutomationLogicFullBody,
	}
}

func runAutomationLogicFullBody(t *testing.T, e *Env) {
	suffix := uniqueSuffix("1847")

	// --- A. routeRequest (node.created -> routeRequest, owner fast-track) ---
	routeRequestId := "req-route-" + suffix
	e.runMutation(t, "createRequest", map[string]any{
		"requestId": routeRequestId,
		"projectId": "proj-" + suffix,
		"title":     "conf-1847 automation-step multi-step-logic dispatch",
		"body":      "Owner submission must auto-route to queued + record a 'routed' event.",
	})

	// Read the row back as the shaped request (carrying the canonical `id` the
	// graph.node.created event payload would). The automation's
	// `advanceRequest({ requestId: args.event.payload.id })` targets that
	// id, and `requestById` filters on it.
	beforeRoute := requestRow(t, e, routeRequestId)
	if got := asStr(beforeRoute["status"]); got != "submitted" {
		t.Fatalf("#1847 precondition: new request should be 'submitted', got %v", beforeRoute["status"])
	}
	canonicalRouteId := asStr(beforeRoute["id"])

	// Drive the REAL routeRequest automation with the REAL node.created event,
	// exactly as the scheduler does on a v1:forge:request insert. The event
	// payload IS the request row (the graph.node.created envelope carries the
	// created node's payload + canonical id), so submitterRole == "owner" is what
	// routeRequest branches on.
	fireForgeAutomation(t, e, "routeRequest", "node.created", events.KindNodeCreated, cloneStringMap(beforeRoute))

	// The owner fast-track transition (advanceRequest -> "queued") is a
	// step whose result the `return args.event.payload.id` never references. If
	// the automation-step dispatch only evaluated the return, this stays
	// "submitted" -- the staging failure.
	afterRoute := requestRow(t, e, canonicalRouteId)
	if got := asStr(afterRoute["status"]); got != "queued" {
		t.Fatalf("#1847: routeRequest did not advance the owner request to 'queued' "+
			"(the automation's advanceRequest persist step was skipped); status=%v", afterRoute["status"])
	}

	// The 'routed' audit event (recordRequestEvent) is the other
	// unreferenced side-effecting step. The audit event is keyed by the SHORT
	// request id (recordRequestEvent normalizes requestId via shortId(),
	// #1859) -- read it back by the short id. (The dedicated short-vs-canonical
	// audit-key contract lives in conf_1859_test.go.)
	routedEvents := asArray(t, e.runQuery(t, "requestEvents", map[string]any{"requestId": routeRequestId}))
	if !hasEventKind(routedEvents, "routed") {
		t.Fatalf("#1847: routeRequest did not write a 'routed' requestEvent "+
			"(the automation's recordRequestEvent persist step was skipped); events=%v", routedEvents)
	}

	// --- B. recordTransition (node.updated -> recordTransition) ----------
	// recordTransition's automation appends the audit event via a guarded
	// recordRequestEvent persist step (one per mapped toStatus). A status
	// transition to "queued" must append an 'approved' event.
	transitionRequestId := "req-transition-" + suffix
	e.runMutation(t, "createRequest", map[string]any{
		"requestId": transitionRequestId,
		"projectId": "proj-" + suffix,
		"title":     "conf-1847 recordTransition side-effect step",
		"body":      "A status transition must append its requestEvent via the node.updated automation.",
	})
	canonicalTransitionId := asStr(requestRow(t, e, transitionRequestId)["id"])

	// Move it to "queued" (the row now carries status=queued) and fire the
	// node.updated automation with oldStatus=submitted, exactly as the engine
	// publishes graph.node.updated (#1158 stamps event.oldStatus).
	e.runMutation(t, "advanceRequest", map[string]any{
		"requestId": transitionRequestId,
		"status":    "queued",
	})
	updatedEvent := cloneStringMap(requestRow(t, e, canonicalTransitionId))
	updatedEvent["oldStatus"] = "submitted"
	fireForgeAutomation(t, e, "recordTransition", "node.updated", events.KindNodeUpdated, updatedEvent)

	// Read by the SHORT id -- recordRequestEvent keys the event under the
	// normalized short requestId (shortId(), #1859).
	transitionEvents := asArray(t, e.runQuery(t, "requestEvents", map[string]any{"requestId": transitionRequestId}))
	if !hasEventKind(transitionEvents, "approved") {
		t.Fatalf("#1847: recordTransition did not write an 'approved' requestEvent on the queued transition "+
			"(the automation's guarded recordRequestEvent persist step was skipped); events=%v", transitionEvents)
	}

	t.Logf("#1847: automation-step multi-step-logic dispatch ran the FULL body (side-effecting steps persisted) for routeRequest + recordTransition")
}

// fireForgeAutomation loads the named forge automation from the embedded DSL and
// runs it through a REAL automation Executor with a REAL triggering event whose
// payload is `payload`. This is the genuine node.created/node.updated ->
// automation -> multi-step-logic dispatch path (the one the conformance Env's
// direct engine.Execute calls bypass), so the side-effecting intermediate steps
// MUST run for the automation to do its job.
func fireForgeAutomation(t *testing.T, e *Env, automationName, topic string, kind events.Kind, payload map[string]any) {
	t.Helper()

	loader := automations.NewLoader(automations.LoaderOptions{Logger: e.Eng.Logger})
	automation, err := loader.LoadByName(automationName)
	if err != nil {
		t.Fatalf("#1847: load automation %q from DSL: %v", automationName, err)
	}

	executor := automations.NewExecutor(automations.ExecutorOptions{
		Logger:       e.Eng.Logger,
		Engine:       e.Eng,
		EventBus:     e.Eng.EventBus(),
		StepRegistry: automationSteps.NewRegistry(),
	})
	defer executor.Close()

	event := events.NewEvent(topic, kind, payload)
	exec, err := executor.ExecuteWithEvent(e.Ctx, automation, "conformance", &event)
	if err != nil {
		t.Fatalf("#1847: execute automation %q: %v", automationName, err)
	}
	if exec != nil && exec.Status == "failed" {
		t.Fatalf("#1847: automation %q execution failed: %s", automationName, exec.Error)
	}
}

// requestRow resolves a single v1:forge:request by id (short id at creation
// time, canonical id thereafter) via requestById and returns the shaped
// row. Fails if zero or multiple rows come back.
func requestRow(t *testing.T, e *Env, requestId string) map[string]any {
	t.Helper()
	rows := asArray(t, e.runQuery(t, "requestById", map[string]any{"requestId": requestId}))
	if len(rows) != 1 {
		t.Fatalf("#1847: requestById(%q) returned %d rows, want 1; rows=%v", requestId, len(rows), rows)
	}
	m, _ := rows[0].(map[string]any)
	if m == nil {
		t.Fatalf("#1847: requestById(%q) row is not an object: %#v", requestId, rows[0])
	}
	return m
}

// cloneStringMap shallow-copies a map[string]any so a test can layer extra
// envelope fields (e.g. event.oldStatus) without mutating the read-back row.
func cloneStringMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}
