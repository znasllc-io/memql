package conformance

// The forge regression exercises the full before-write routing body, then
// the event-bound audit body. Routing changes the initial row; recording an
// audit remains a real automation execution with a separate persisted effect.

import (
	"strings"
	"testing"
	"time"

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
	installForgeBeforeWrite(t, e)
	suffix := uniqueSuffix("1847")

	// --- A. routeRequest (node.created -> routeRequest, owner fast-track) ---
	routeRequestId := "req-route-" + suffix
	captured := make(chan events.Event, 16)
	unsubscribe := e.Eng.EventBus().Subscribe("graph.node.#", func(ev events.Event) {
		id, _ := ev.Payload["id"].(string)
		if id == routeRequestId || strings.HasSuffix(id, ":"+routeRequestId) {
			captured <- ev
		}
	})
	defer unsubscribe()
	e.runMutation(t, "createRequest", map[string]any{
		"requestId": routeRequestId,
		"projectId": "proj-" + suffix,
		"title":     "conf-1847 automation-step multi-step-logic dispatch",
		"body":      "Owner submission must auto-route to queued + record a 'routed' event.",
	})

	// The hook must have routed in the original write, before any event run.
	afterRoute := requestRow(t, e, routeRequestId)
	if got := asStr(afterRoute["status"]); got != "queued" {
		t.Fatalf("#1847: before-write routing left owner request at %v", afterRoute["status"])
	}
	select {
	case ev := <-captured:
		if ev.Kind != events.KindNodeCreated || ev.Payload["status"] != "queued" || ev.Payload["firstVersion"] != true {
			t.Fatalf("before-write must publish the initial routed row: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing initial request event")
	}
	select {
	case ev := <-captured:
		t.Fatalf("before-write emitted a second request event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
	payload := cloneStringMap(afterRoute)
	payload["firstVersion"] = true
	fireForgeAutomation(t, e, "recordRouted", "node.created", events.KindNodeCreated, payload)

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

	loader := automations.NewLoader(automations.LoaderOptions{Logger: e.Eng.Logger, Registry: e.Registry, Functions: e.Eng.Functions()})
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

// Install only for tests which exercise the new write contract; each Env is
// shared by the conformance dimensions, so restore its hook set afterwards.
func installForgeBeforeWrite(t *testing.T, e *Env) {
	t.Helper()
	loader := automations.NewLoader(automations.LoaderOptions{Logger: e.Eng.Logger, Registry: e.Registry, Functions: e.Eng.Functions()})
	a, err := loader.LoadByName("routeRequest")
	if err != nil {
		t.Fatal(err)
	}
	automations.InstallBeforeWriteHooks(e.Eng, []*automations.Automation{a}, automationSteps.NewRegistry(), e.Eng.Logger)
	t.Cleanup(func() { e.Eng.SetBeforeWriteHooks(nil) })
}
