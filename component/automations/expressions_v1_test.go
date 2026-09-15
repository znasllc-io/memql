package automations

// expressions_v1_test.go -- a v1 automation end to end through this
// package's callers (memql#5367): the load (PrepareExpressions and the
// outer-expression name rule), and the data model every caller evaluates over
// -- the executor's statement conditions and arguments, resume, the trigger
// filter and the LogicRunner. Each data-model test drives the REAL caller
// with a probe step registry that evaluates the step's parsed arguments over
// the Evaluator the caller seeded, so what is asserted is what a step
// executor would see.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/memql"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// loadV1 turns compiled v1 JSON into a prepared Automation the way the
// LogicRunner's body compile does (parseJSON runs PrepareExpressions), then
// applies the load-time name rule the tree loader applies to the trigger
// filter and the preconditions.
func loadV1(t *testing.T, js string) *Automation {
	t.Helper()
	a, err := NewLoader(LoaderOptions{}).parseJSON([]byte(js), "test:v1")
	if err != nil {
		t.Fatalf("load v1 automation: %v", err)
	}
	if err := validateOuterExpressionNames(a); err != nil {
		t.Fatalf("v1 name rule: %v", err)
	}
	return a
}

// v1ProbeCall is one step the probe registry ran, with the values its parsed
// expressions evaluated to over the caller's Evaluator.
type v1ProbeCall struct {
	stepID string
	name   string
	values map[string]any
}

// v1ProbeRegistry stands in for the step executors: a function step's
// arguments and an event step's topic and payload are evaluated exactly as the
// real executors evaluate them (ResolveV1Map / EvalV1). results serves a
// function step's result by name; fail fails one.
type v1ProbeRegistry struct {
	mu      sync.Mutex
	calls   []v1ProbeCall
	results map[string]any
	fail    map[string]string
}

func (r *v1ProbeRegistry) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	now := time.Now()
	res := &StepResult{StepId: step.ID, StartedAt: now, CompletedAt: now}
	call := v1ProbeCall{stepID: step.ID}
	var result any
	var err error
	switch {
	case step.Exprs == nil:
		err = fmt.Errorf("step %q reached the probe without parsed expressions", step.ID)
	case step.Function != nil:
		call.name = step.Function.Name
		call.values, err = stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
		result = r.results[call.name]
		if result == nil {
			result = map[string]any{"ok": true}
		}
	case step.Event != nil:
		var topic any
		topic, err = stepCtx.Evaluator.EvalV1(ctx, step.Exprs.Topic)
		if err == nil {
			call.name = "event:" + V1Text(topic)
			call.values, err = stepCtx.Evaluator.ResolveV1Map(ctx, step.Event.Payload)
			result = map[string]any{"topic": V1Text(topic), "payload": call.values}
		}
	default:
		err = fmt.Errorf("the probe does not run a %s step", step.Type)
	}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	if err == nil {
		if msg := r.fail[call.name]; msg != "" {
			err = errors.New(msg)
		}
	}
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res, err
	}
	res.Status = "success"
	res.Result = result
	return res, nil
}

// call returns the last run of the named step, failing the test when it never
// ran.
func (r *v1ProbeRegistry) call(t *testing.T, name string) v1ProbeCall {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].name == name {
			return r.calls[i]
		}
	}
	var ran []string
	for _, c := range r.calls {
		ran = append(ran, c.stepID+"/"+c.name)
	}
	t.Fatalf("step %q never ran (ran: %v)", name, ran)
	return v1ProbeCall{}
}

func (r *v1ProbeRegistry) ran(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.name == name {
			return true
		}
	}
	return false
}

// wantValues compares the named values of a call; a want of wantAny only
// requires the key to be present.
func wantValues(t *testing.T, c v1ProbeCall, want map[string]any) {
	t.Helper()
	for k, w := range want {
		got, present := c.values[k]
		if w == wantPresent {
			if !present {
				t.Errorf("step %s: %s is absent, want a value", c.stepID, k)
			}
			continue
		}
		if !present {
			t.Errorf("step %s: %s is absent, want %#v", c.stepID, k, w)
			continue
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("step %s: %s = %#v (%T), want %#v (%T)", c.stepID, k, got, got, w, w)
		}
	}
}

// wantPresent is a wantValues expectation that only requires a key.
var wantPresent = &struct{ present bool }{true}

// wantNowNear asserts a `now` value is an RFC3339 instant within a few
// seconds of the test's own clock -- the run clock is the seeded timestamp,
// whose resolution is a second.
func wantNowNear(t *testing.T, c v1ProbeCall, key string) {
	t.Helper()
	s, ok := c.values[key].(string)
	if !ok {
		t.Fatalf("step %s: %s = %#v, want an RFC3339 string", c.stepID, key, c.values[key])
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("step %s: %s = %q is not RFC3339: %v", c.stepID, key, s, err)
	}
	if d := time.Since(at); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("step %s: %s = %s is %s from now", c.stepID, key, s, d)
	}
}

// probeDataModelArgs are the value leaves the per-caller tests evaluate: one
// per root of the run's data model, and the name a statement bound.
const probeDataModelArgs = `{
	"eventPayload": {"$expr": "event.payload.x"},
	"eventTopic":   {"$expr": "event.topic"},
	"bound":        {"$expr": "s.y"},
	"argsX":        {"$expr": "args.x"},
	"actorUser":    {"$expr": "actor.userId"},
	"clock":        {"$expr": "now"},
	"cfg":          {"$expr": "config"},
	"literal":      "event.payload.x"
}`

// probeDataModelAutomation is a v1 automation declaring `x`, whose first
// statement binds `s` to {y: "Y"} and whose second, gated on s, probes the
// data model.
func probeDataModelAutomation(name string) string {
	return `{
	"name": "` + name + `",
	"args": {"fields": [{"name": "x", "type": "string", "optional": true}]},
	"steps": [
		{"id": "s", "type": "function", "binds": "s", "function": {"name": "seed", "kind": "builtin"}},
		{"id": "probe", "type": "function",
		 "condition": "s.y == \"Y\"",
		 "function": {"name": "probe", "kind": "builtin", "args": ` + probeDataModelArgs + `}}
	]
}`
}

func probeRegistry() *v1ProbeRegistry {
	return &v1ProbeRegistry{results: map[string]any{"seed": map[string]any{"y": "Y"}}}
}

// ---------------------------------------------------------------------------
// the load
// ---------------------------------------------------------------------------

// TestStepArgumentIsAnExpression: the loaded step holds a PARSED node for an
// expression argument -- not a string -- and a plain string argument stays a
// literal, even when its text reads like a reference. Run, the two differ:
// the expression reads the event, the literal is its own text.
func TestStepArgumentIsAnExpression(t *testing.T) {
	a := loadV1(t, `{
		"name": "argIsExpr",
		"steps": [{"id": "call", "type": "function",
			"condition": "event.payload.x != nil",
			"function": {"name": "probe", "args": {
				"ref": {"$expr": "event.payload.x"},
				"lit": "event.payload.x",
				"nested": {"list": [1, {"$expr": "args.missing ?? 0"}]}
			}}}]
	}`)
	step := a.Steps[0]
	if step.Exprs == nil || step.Exprs.Condition == nil {
		t.Fatalf("the step's expressions were not parsed at load: %+v", step.Exprs)
	}
	leaf, ok := step.Function.Args["ref"].(*ExprLeaf)
	if !ok {
		t.Fatalf("args.ref is %T, want *ExprLeaf (a parsed node, not a string)", step.Function.Args["ref"])
	}
	if _, isMember := leaf.Node.(*ast.MemberExpr); !isMember || ast.FormatExpr(leaf.Node) != "event.payload.x" {
		t.Fatalf("args.ref node = %T %q", leaf.Node, ast.FormatExpr(leaf.Node))
	}
	if lit, _ := step.Function.Args["lit"].(string); lit != "event.payload.x" {
		t.Fatalf("args.lit = %#v, want the literal string", step.Function.Args["lit"])
	}
	nested := step.Function.Args["nested"].(map[string]any)["list"].([]any)
	if _, ok := nested[1].(*ExprLeaf); !ok {
		t.Fatalf("a leaf inside a list is %T, want *ExprLeaf", nested[1])
	}
	// The prepared value re-serialises to the compiled form.
	b, err := json.Marshal(step.Function.Args["ref"])
	if err != nil || string(b) != `{"$expr":"event.payload.x"}` {
		t.Fatalf("a prepared leaf marshals to %s (%v)", b, err)
	}

	probe := &v1ProbeRegistry{}
	ev := events.NewEvent("probe.fired", events.KindMessage, map[string]any{"x": "hello"})
	if _, err := NewExecutor(ExecutorOptions{StepRegistry: probe}).ExecuteWithEvent(context.Background(), a, "test", &ev); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantValues(t, probe.call(t, "probe"), map[string]any{
		"ref":    "hello",
		"lit":    "event.payload.x",
		"nested": map[string]any{"list": []any{float64(1), int64(0)}},
	})
}

// TestPrepareExpressionsRefuses: an automation is refused at LOAD for a parse
// error, a trigger filter that is not a one-parameter lambda, and an
// expression over the M tier's static cost limit -- and one built in Go,
// never loaded, is prepared by the executor before its first run: its
// expressions gate the run exactly as a loaded automation's do, and one that
// does not parse refuses the run before anything executes.
func TestPrepareExpressionsRefuses(t *testing.T) {
	scan := "args.a.any(x => args.b.any(y => args.c.any(z => z == y && y == x)))"
	node, err := languageParser.ParseV1Expression(scan)
	if err != nil {
		t.Fatalf("parse %s: %v", scan, err)
	}
	if cost := memql.EstimateCost(node); cost <= tiers.MaxStaticCost {
		t.Fatalf("precondition: %s has a static cost of %d, not above %d", scan, cost, tiers.MaxStaticCost)
	}
	cases := map[string]struct{ js, want string }{
		"condition parse error": {
			`{"name":"p","steps":[{"id":"s","type":"function","condition":"a ==","function":{"name":"f"}}]}`,
			`step "s" condition`,
		},
		"value leaf parse error": {
			`{"name":"p","steps":[{"id":"s","type":"function","function":{"name":"f","args":{"x":{"$expr":"1 +"}}}}]}`,
			`step "s" args.x`,
		},
		"trigger filter that is not a lambda": {
			`{"name":"p","trigger":{"event":"t","filter":"status == \"a\""},"steps":[{"id":"s","type":"function","function":{"name":"f"}}]}`,
			`trigger filter`,
		},
		"two-parameter trigger filter": {
			`{"name":"p","trigger":{"event":"t","filter":"(a, b) => a == b"},"steps":[{"id":"s","type":"function","function":{"name":"f"}}]}`,
			`one-parameter lambda`,
		},
		"over the static cost limit": {
			`{"name":"p","steps":[{"id":"s","type":"function","condition":` + mustJSONString(scan) + `,"function":{"name":"f"}}]}`,
			`tiers.MaxStaticCost`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewLoader(LoaderOptions{}).parseJSON([]byte(c.js), "test:v1")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a load refusal containing %q, got %v", c.want, err)
			}
		})
	}

	t.Run("never prepared", func(t *testing.T) {
		var a Automation
		if err := json.Unmarshal([]byte(`{"name":"raw","steps":[
			{"id":"skipped","type":"function","condition":"1 == 2","function":{"name":"f"}},
			{"id":"ran","type":"function","condition":"1 == 1","function":{"name":"f"}}]}`), &a); err != nil {
			t.Fatal(err)
		}
		reg := &v1ProbeRegistry{}
		if _, err := NewExecutor(ExecutorOptions{StepRegistry: reg}).Execute(context.Background(), &a, "test"); err != nil {
			t.Fatalf("an automation built in Go must be prepared on demand and run: %v", err)
		}
		reg.mu.Lock()
		var ran []string
		for _, c := range reg.calls {
			ran = append(ran, c.stepID)
		}
		reg.mu.Unlock()
		if strings.Join(ran, ",") != "ran" {
			t.Fatalf("ran %v, want only the step whose condition holds", ran)
		}

		var bad Automation
		if err := json.Unmarshal([]byte(`{"name":"rawBad","steps":[{"id":"s","type":"function","condition":"a ==","function":{"name":"f"}}]}`), &bad); err != nil {
			t.Fatal(err)
		}
		if _, err := NewExecutor(ExecutorOptions{StepRegistry: &v1ProbeRegistry{}}).Execute(context.Background(), &bad, "test"); err == nil || !strings.Contains(err.Error(), `step "s" condition`) {
			t.Fatalf("want the on-demand preparation to refuse the parse error, got %v", err)
		}
	})
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// the data model, caller by caller
// ---------------------------------------------------------------------------

// TestV1DataModel_Executor: the executor's statement condition and arguments.
func TestV1DataModel_Executor(t *testing.T) {
	probe := probeRegistry()
	a := loadV1(t, probeDataModelAutomation("dataModelExecutor"))
	ctx := auth.ContextWithUserActor(context.Background(), "user-7")
	ev := events.NewEvent("probe.fired", events.KindMessage, map[string]any{"x": "hello"})
	exec, err := NewExecutor(ExecutorOptions{StepRegistry: probe}).ExecuteWithEvent(ctx, a, "test", &ev)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if exec.Status == "failed" {
		t.Fatalf("run failed: %s", exec.Error)
	}
	c := probe.call(t, "probe") // ran: the condition over s held
	wantValues(t, c, map[string]any{
		"eventPayload": "hello",
		"eventTopic":   "probe.fired",
		"bound":        "Y",
		"argsX":        "hello",
		"actorUser":    "user-7",
		"cfg":          wantPresent,
		"literal":      "event.payload.x",
	})
	wantNowNear(t, c, "clock")
}

// TestV1DataModel_Resume: a resumed run reads the journal's event, the args
// bound from it, and the names its statements bound before the failure.
func TestV1DataModel_Resume(t *testing.T) {
	probe := probeRegistry()
	a := loadV1(t, probeDataModelAutomation("dataModelResume"))
	journal := &RunJournal{
		RunId:          "run-resume-1",
		AutomationName: a.Name,
		FailedStep:     "probe",
		StepStates:     states("s", StepState{Status: "done"}, "probe", StepState{Status: "failed", Attempt: 1}),
		Steps: map[string]*MinimalStepResult{
			"s": {StepId: "s", Status: "success", Result: map[string]any{"y": "Y"}, Value: map[string]any{"y": "Y"}},
		},
		TriggerEvent: map[string]any{"topic": "probe.fired", "kind": "message", "payload": map[string]any{"x": "hello"}},
	}
	ctx := auth.ContextWithUserActor(context.Background(), "user-7")
	exec, err := NewExecutor(ExecutorOptions{StepRegistry: probe}).ResumeFrom(ctx, journal, a, &ResumeOptions{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if exec.Status == "failed" {
		t.Fatalf("resumed run failed: %s", exec.Error)
	}
	c := probe.call(t, "probe")
	wantValues(t, c, map[string]any{
		"eventPayload": "hello",
		"eventTopic":   "probe.fired",
		"bound":        "Y",
		"argsX":        "hello",
		"actorUser":    "user-7",
		"cfg":          wantPresent,
		"literal":      "event.payload.x",
	})
	wantNowNear(t, c, "clock")
	if probe.ran("seed") {
		t.Error("resume re-ran the completed statement `s`")
	}
}

// TestV1DataModel_LogicRunner: a logic's statements -- compiled and run by
// the LogicRunner -- read their args (the event under them), the actor, the
// clock and the names earlier statements bound; an expression statement and
// the return evaluate without an engine round trip.
func TestV1DataModel_LogicRunner(t *testing.T) {
	name, body := compiledLogic(t, `logic probeLogic {
  args {
    event object!
    x string
  }
  s := builtin seed()
  total := 1 + 2
  builtin probe(argsEvent: args.event.payload.x, argsX: args.x, actorUser: actor.userId, clock: now, bound: s.y, total: total, cfg: config)
  return s.y + "!"
}`)
	probe := probeRegistry()
	runner := NewLogicRunner(&memql.MemQLEngine{}, probe, nil)
	ctx := auth.ContextWithUserActor(context.Background(), "user-7")
	out, err := runner.RunLogicBody(ctx, name, body, map[string]any{
		"event": map[string]any{"topic": "t", "payload": map[string]any{"x": "hello"}},
		"x":     "ex",
	})
	if err != nil {
		t.Fatalf("RunLogicBody: %v", err)
	}
	if out != "Y!" {
		t.Fatalf("return = %#v, want \"Y!\" (an expression return evaluated in process)", out)
	}
	c := probe.call(t, "probe")
	wantValues(t, c, map[string]any{
		"argsEvent": "hello",
		"argsX":     "ex",
		"actorUser": "user-7",
		"bound":     "Y",
		"total":     int64(3),
		"cfg":       wantPresent,
	})
	wantNowNear(t, c, "clock")
}

// TestV1LogicStatementShapes: the shapes a logic's statements use for choice,
// concatenation and presence -- `(p ? a : b)`, `a + b`, `x != nil`, `??` and
// `nil` -- run as expression statements, as a construct call's named
// arguments, inside a map literal with explicit keys, and as the condition of
// an `if`.
func TestV1LogicStatementShapes(t *testing.T) {
	name, body := compiledLogic(t, `logic shapes {
  args {
    p bool
    a string
    b string
    maybe string
  }
  pick := (args.p ? args.a : args.b)
  nested := args.p ? (args.a == "x" ? "ax" : "a") : "b"
  joined := "P-" + (args.maybe ?? "30") + "D"
  known := args.maybe != nil
  unset := args.maybe ?? nil
  builtin probe(pick: pick, nested: nested, joined: joined, known: known, unset: unset, payload: { userId: args.a, flags: { known: known, none: nil } })
  if args.p && known {
    builtin note(pick: pick)
  }
  return { pick: pick, joined: joined, known: known }
}`)
	for _, tc := range []struct {
		args map[string]any
		want map[string]any
		ret  map[string]any
	}{
		{
			args: map[string]any{"p": true, "a": "x", "b": "y", "maybe": "7"},
			want: map[string]any{"pick": "x", "nested": "ax", "joined": "P-7D", "known": true, "unset": "7",
				"payload": map[string]any{"userId": "x", "flags": map[string]any{"known": true, "none": nil}}},
			ret: map[string]any{"pick": "x", "joined": "P-7D", "known": true},
		},
		{
			args: map[string]any{"p": false, "a": "x", "b": "y"},
			want: map[string]any{"pick": "y", "nested": "b", "joined": "P-30D", "known": false, "unset": nil,
				"payload": map[string]any{"userId": "x", "flags": map[string]any{"known": false, "none": nil}}},
			ret: map[string]any{"pick": "y", "joined": "P-30D", "known": false},
		},
	} {
		probe := &v1ProbeRegistry{}
		out, err := NewLogicRunner(&memql.MemQLEngine{}, probe, nil).RunLogicBody(context.Background(), name, body, tc.args)
		if err != nil {
			t.Fatalf("RunLogicBody(%v): %v", tc.args, err)
		}
		wantValues(t, probe.call(t, "probe"), tc.want)
		// A call is a statement of its own, run under the if's condition.
		if ran := probe.ran("note"); ran != (tc.args["p"] == true && tc.args["maybe"] != nil) {
			t.Fatalf("RunLogicBody(%v): the call under the if ran=%v", tc.args, ran)
		}
		if !reflect.DeepEqual(out, tc.ret) {
			t.Fatalf("RunLogicBody(%v) = %#v, want %#v", tc.args, out, tc.ret)
		}
	}
}

// TestV1LogicReturnOnlyBodyRuns: a body that is one `return` of an expression
// runs on the LogicRunner like any other statement body: it compiles to its
// one return step, which the runner evaluates in process.
func TestV1LogicReturnOnlyBodyRuns(t *testing.T) {
	_, steps := compiledLogic(t, `logic returnOnly {
  args {
    x string
  }
  return args.x ?? "none"
}`)
	runner := NewLogicRunner(&memql.MemQLEngine{}, &v1ProbeRegistry{}, nil)
	for arg, want := range map[string]string{"ex": "ex", "": "none"} {
		out, err := runner.RunLogicBody(context.Background(), "returnOnly", steps, map[string]any{"x": arg})
		if err != nil {
			t.Fatalf("RunLogicBody: %v", err)
		}
		if out != want {
			t.Fatalf("return = %#v, want %q", out, want)
		}
	}
}

// ---------------------------------------------------------------------------
// the trigger filter
// ---------------------------------------------------------------------------

// graphCreatedEvent is a graph.node.created event as executor_mutation.go
// publishes it: the stored payload flattened onto the top level and again
// under `payload`, with the node's intrinsics beside them.
func graphCreatedEvent(concept, id string, payload map[string]any) events.Event {
	flat := map[string]any{
		"id": id, "nodeId": id, "concept": concept, "nodeType": "node",
		"actor": "user-7", "createdAt": "2026-09-13T10:00:00Z",
		"payload": payload,
	}
	for k, v := range payload {
		flat[k] = v
	}
	ev := events.NewEvent("graph.node.created."+concept, events.KindNodeCreated, flat)
	ev.Metadata = map[string]string{"actor": "user-7"}
	return ev
}

// TestTriggerFilterStartsWithLoadsAndFires: a v1 `@filter(row => ...)` loads
// (parsed once into Trigger.FilterLambda) and decides whether the automation
// fires, reading the triggering ROW -- its payload bare, its intrinsics
// (`row.concept`, `row.id`) as columns -- and `startsWith` over it.
func TestTriggerFilterStartsWithLoadsAndFires(t *testing.T) {
	const concept = "v1:probe:thing"
	a := loadV1(t, `{
		"name": "archivedOnly",
		"trigger": {
			"event": "graph.node.created.`+concept+`",
			"filter": "row => row.status startsWith \"arch\" && row.concept == \"`+concept+`\" && row.id != \"\""
		},
		"steps": [{"id": "fire", "type": "function", "function": {"name": "fire", "args": {"status": {"$expr": "event.payload.status"}}}}]
	}`)
	if a.Trigger.FilterLambda == nil || a.Trigger.FilterLambda.Params[0] != "row" {
		t.Fatalf("the filter was not parsed into a lambda at load: %+v", a.Trigger)
	}

	bus := events.NewBus()
	defer bus.Close()
	var buf bytes.Buffer
	s := newMinimalScheduler(&buf, bus)
	probe := &v1ProbeRegistry{}
	s.eventExecutor = NewExecutor(ExecutorOptions{Logger: s.logger, EventBus: bus, StepRegistry: probe})
	s.automations[a.Name] = a
	if err := s.subscribeToEventTrigger(a); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	bus.PublishSync(graphCreatedEvent(concept, "thing-1", map[string]any{"status": "active"}))
	if probe.ran("fire") {
		t.Fatalf("fired for status \"active\"; scheduler log:\n%s", buf.String())
	}
	bus.PublishSync(graphCreatedEvent(concept, "thing-2", map[string]any{"status": "archived"}))
	c := probe.call(t, "fire")
	wantValues(t, c, map[string]any{"status": "archived"})
}

// TestTriggerFilterScope: beside its row, a v1 filter reads the rest of the
// filter's state -- the args binding validated before it, the event envelope
// and the actor, which for a bus trigger is the denying no-caller envelope.
// An args field is read args.<field>: a bare name is refused at load.
func TestTriggerFilterScope(t *testing.T) {
	const concept = "v1:probe:thing"
	a := loadV1(t, `{
		"name": "scoped",
		"args": {"fields": [{"name": "status", "type": "string", "optional": true}]},
		"trigger": {
			"event": "graph.node.created.`+concept+`",
			"filter": "row => row.status == args.status && args.status == \"archived\" && event.topic startsWith \"graph.node.created.\" && actor.userId == \"\" && !actor.isClusterOwner"
		},
		"steps": [{"id": "fire", "type": "function", "function": {"name": "fire"}}]
	}`)
	for _, c := range []struct {
		status string
		fires  bool
	}{{"active", false}, {"archived", true}} {
		ev := graphCreatedEvent(concept, "thing-"+c.status, map[string]any{"status": c.status})
		bound, _, err := bindEventArgs(a, &ev)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		got, err := evaluateTriggerFilter(a, &ev, bound)
		if err != nil {
			t.Fatalf("filter over status %q: %v", c.status, err)
		}
		if got != c.fires {
			t.Fatalf("filter over status %q = %v, want %v", c.status, got, c.fires)
		}
	}
}

// TestTriggerRow: the row a filter's parameter binds, built from the parts of
// a graph event a payload key cannot shadow.
func TestTriggerRow(t *testing.T) {
	ev := graphCreatedEvent("v1:probe:thing", "thing-9", map[string]any{"status": "x", "concept": "shadowed"})
	row := TriggerRow(&ev)
	if row.ID != "thing-9" || row.Concept != "v1:probe:thing" || row.Type != "node" || row.CreatedBy != "user-7" {
		t.Fatalf("row intrinsics = %+v", row)
	}
	if row.Payload["status"] != "x" || row.CreatedAt.IsZero() {
		t.Fatalf("row payload/createdAt = %+v", row)
	}
	plain := events.NewEvent("app.custom", events.KindMessage, map[string]any{"status": "y"})
	if got := TriggerRow(&plain); got.Payload["status"] != "y" || got.ID != "" {
		t.Fatalf("a non-graph event's row = %+v, want its payload and no intrinsics", got)
	}
}

// ---------------------------------------------------------------------------
// the retired spellings
// ---------------------------------------------------------------------------

// TestEveryAutomationIsPreparedAsV1: every automation is edition 2026 -- its
// conditions parse at load -- and a spelling only the string evaluator read
// is refused at load rather than run: a bare word is an unknown name (the
// string evaluator compared it as its own text), and a `$`-prefixed reference
// does not parse.
func TestEveryAutomationIsPreparedAsV1(t *testing.T) {
	a, err := NewLoader(LoaderOptions{}).parseJSON([]byte(`{
		"name": "gate",
		"steps": [
			{"id": "gate", "type": "function", "condition": "event.payload.status == \"active\"", "function": {"name": "f"}}
		]
	}`), "test:gate")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if a.Steps[0].Exprs == nil || a.Steps[0].Exprs.Condition == nil {
		t.Fatal("the automation's condition was not parsed at load")
	}

	for name, cond := range map[string]string{
		"bare word":        `args.status == active`,
		"dollar reference": `$args.status == "active"`,
	} {
		t.Run(name, func(t *testing.T) {
			src := `@trigger(event="probe.fired")
automation retiredGate {
  args {
    status string
  }
  if ` + cond + ` {
    builtin f()
  }
}`
			if _, err := NewLoader(LoaderOptions{}).CompileSource(src, "test:retired"); err == nil {
				t.Fatalf("the retired spelling %s loaded; it must be refused at load, not decided at run time", cond)
			}
		})
	}
}

// TestV1StringFieldsAreValueLeaves: a string-typed field that holds a value
// (an event topic) carries a value leaf -- a JSON literal or `{"$expr": ...}`
// -- which the Go config decodes and re-encodes unchanged, and which
// PrepareExpressions turns into a node: a literal node for a literal, the
// parsed expression for an expression.
func TestV1StringFieldsAreValueLeaves(t *testing.T) {
	const js = `{"name":"leaves","steps":[
		{"id":"pub","type":"event","event":{"topic":{"$expr":"\"app.\" + args.kind"},"payload":{"a":1}}},
		{"id":"lit","type":"event","event":{"topic":"app.static"}}
	]}`
	a := loadV1(t, js)
	byID := map[string]*Step{}
	for _, s := range a.Steps {
		byID[s.ID] = s
	}
	for id, want := range map[string]string{"pub": `"app." + args.kind`, "lit": `"app.static"`} {
		if got := ast.FormatExpr(byID[id].Exprs.Topic); got != want {
			t.Errorf("step %s topic node = %s, want %s", id, got, want)
		}
	}
	if byID["lit"].Event.Topic != "app.static" {
		t.Errorf("a literal topic decodes into the Go field: got %q", byID["lit"].Event.Topic)
	}

	// The configs re-encode to the compiled JSON.
	var original map[string]any
	if err := json.Unmarshal([]byte(js), &original); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(a.Steps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var steps []map[string]any
	if err := json.Unmarshal(b, &steps); err != nil {
		t.Fatal(err)
	}
	for i, s := range original["steps"].([]any) {
		orig := s.(map[string]any)
		for _, cfg := range []string{"event"} {
			if want, ok := orig[cfg]; ok {
				if !reflect.DeepEqual(steps[i][cfg], want) {
					t.Errorf("step %v %s re-encodes as %#v, want %#v", orig["id"], cfg, steps[i][cfg], want)
				}
			}
		}
	}

	// Evaluated, the topic is a value.
	e := NewEvaluator()
	e.SetCustom("args", map[string]any{"kind": "done"})
	if v, err := e.EvalV1(context.Background(), byID["pub"].Exprs.Topic); err != nil || v != "app.done" {
		t.Errorf("topic = %#v, %v", v, err)
	}
}

// TestTriggerFilterMustBeALambda: a trigger filter is a one-parameter lambda
// over the triggering row, parsed at load and decided over that row. The
// string evaluator's filter -- a bare condition over `payload` -- is refused
// at load, rather than handed to anything as text.
func TestTriggerFilterMustBeALambda(t *testing.T) {
	const concept = "v1:probe:thing"
	a, err := NewLoader(LoaderOptions{}).parseJSON([]byte(`{
		"name": "lambdaFilter",
		"trigger": {"event": "graph.node.created.`+concept+`", "filter": "row => row.status == \"archived\""},
		"steps": [{"id": "fire", "type": "function", "function": {"name": "fire"}}]
	}`), "test:lambda")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if a.Trigger.FilterLambda == nil {
		t.Fatal("the filter was not parsed as a lambda")
	}
	for status, want := range map[string]bool{"archived": true, "active": false} {
		ev := graphCreatedEvent(concept, "thing-"+status, map[string]any{"status": status})
		got, err := evaluateTriggerFilter(a, &ev, nil)
		if err != nil || got != want {
			t.Fatalf("filter over %q = %v, %v; want %v", status, got, err, want)
		}
	}
	// A filter that is not a lambda is refused at load.
	_, err = NewLoader(LoaderOptions{}).parseJSON([]byte(`{
		"name": "plainFilter",
		"trigger": {"event": "graph.node.created.`+concept+`", "filter": "payload.status == \"archived\""},
		"steps": [{"id": "fire", "type": "function", "function": {"name": "fire"}}]
	}`), "test:plain")
	if err == nil {
		t.Fatal("a filter that is not a lambda loaded")
	}
}
