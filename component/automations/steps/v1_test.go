package steps

// v1_test.go -- the step executors' v1 halves (memql#5367): the forEach clone
// as a caller of the run's data model, the query executor's in-process
// expressions, the event and switch executors, the literal renderer, and the
// two behaviour fixes v1 exists to make -- each driven through the REAL
// executors, with only the engine-bound function executor replaced.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/ast"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// prepareV1 unmarshals compiled v1 JSON and prepares it, as every loader
// path does.
func prepareV1(t *testing.T, js string) *automations.Automation {
	t.Helper()
	var a automations.Automation
	if err := json.Unmarshal([]byte(js), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := automations.PrepareExpressions(&a); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return &a
}

// recordingFunctions stands in for the engine-bound function executor: it
// evaluates a v1 step's arguments exactly as FunctionExecutor's v1 half does
// (ResolveV1Map), records them, and answers with a canned result by name.
type recordingFunctions struct {
	mu      sync.Mutex
	calls   []recordedCall
	results map[string]any
}

type recordedCall struct {
	step string
	name string
	args map[string]any
}

func (f *recordingFunctions) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	now := time.Now()
	res := &automations.StepResult{StepId: step.ID, StartedAt: now, CompletedAt: now}
	if step.Exprs == nil {
		return nil, fmt.Errorf("step %q is not a v1 step", step.ID)
	}
	args, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{step: step.ID, name: step.Function.Name, args: args})
	f.mu.Unlock()
	res.Status = "success"
	res.Result = f.results[step.Function.Name]
	if res.Result == nil {
		res.Result = map[string]any{"ok": true}
	}
	return res, nil
}

func (f *recordingFunctions) named(name string) []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedCall
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// recordingEvents wraps the REAL event executor -- so topic and payload are
// evaluated by its v1 half -- and records what it published.
type recordingEvents struct {
	mu        sync.Mutex
	published []map[string]any
}

func (r *recordingEvents) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	res, err := (&EventExecutor{}).Execute(ctx, step, stepCtx)
	if err == nil && res != nil {
		if out, ok := res.Result.(map[string]any); ok {
			r.mu.Lock()
			r.published = append(r.published, out)
			r.mu.Unlock()
		}
	}
	return res, err
}

func (r *recordingEvents) topics() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.published {
		out = append(out, fmt.Sprint(p["topic"]))
	}
	return out
}

// v1Registry is the real registry with the engine-bound function executor
// replaced and the event executor recorded.
func v1Registry(funcs *recordingFunctions, evs *recordingEvents) *Registry {
	r := NewRegistry()
	r.Register(automations.StepTypeFunction, funcs)
	r.Register(automations.StepTypeEvent, evs)
	return r
}

// parseV1Logic parses a logic source and returns its body.
func parseV1Logic(t *testing.T, src string) *langparser.AutomationDef {
	t.Helper()
	normalised, err := langparser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	f, err := langparser.ParseFile(normalised)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, d := range f.Definitions {
		if fn, ok := d.(*langparser.FunctionDef); ok {
			if body, ok := fn.Body.(*langparser.AutomationDef); ok {
				return body
			}
		}
	}
	t.Fatal("no logic body in the source")
	return nil
}

// runV1Logic runs a v1 logic body through the LogicRunner over the real
// registry (function executor replaced), with an event bus for publishEvent.
func runV1Logic(t *testing.T, name, src string, funcs *recordingFunctions, args map[string]any) (any, *recordingEvents, error) {
	t.Helper()
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	eng := &memql.MemQLEngine{}
	eng.SetEventBus(bus)
	evs := &recordingEvents{}
	runner := automations.NewLogicRunner(eng, v1Registry(funcs, evs), nil)
	out, err := runner.RunLogic(context.Background(), name, parseV1Logic(t, src), args)
	return out, evs, err
}

// bundle is a query step's result as the step registry records it: the
// engine envelope whose Bundle carries node maps.
func bundle(nodes ...map[string]any) map[string]any {
	list := make([]any, len(nodes))
	for i, n := range nodes {
		list[i] = n
	}
	return map[string]any{"Bundle": map[string]any{"nodes": list}}
}

// ---------------------------------------------------------------------------
// the behaviour fixes
// ---------------------------------------------------------------------------

// reminderLogic is dsl/identity/logic.memql's accountDeletionReminder25Days in
// the edition-2026 grammar: the [P25D, P26D) window is decided per row by a
// date comparison against `now`.
const reminderLogic = `logic accountDeletionReminder25Days {
  args {
    event object!
  }
  body {
    candidates := query usersInDeletionCooldown()

    for item := range candidates.nodes() {
      emitReminder := if addDuration(item.payload.deletionScheduledAt, "P25D") < now && addDuration(item.payload.deletionScheduledAt, "P26D") >= now {
        publishEvent(
          topic: "identity.deletion.reminder",
          payload: {
            userId: item.id,
            deletionScheduledAt: item.payload.deletionScheduledAt,
            milestoneDays: 25
          }
        )
      }
    }

    return candidates.count()
  }
}`

// TestV1ReminderGateIsTrueOnADueItem: the identity reminder gate
// `addDuration(item.payload.deletionScheduledAt, "P25D") < now &&
// addDuration(..., "P26D") >= now` is TRUE for a user 25.5 days into the
// cooldown, and false either side of the window -- so exactly the due user is
// reminded.
func TestV1ReminderGateIsTrueOnADueItem(t *testing.T) {
	at := func(daysAgo float64) string {
		return time.Now().UTC().Add(-time.Duration(daysAgo * 24 * float64(time.Hour))).Format(time.RFC3339)
	}
	funcs := &recordingFunctions{results: map[string]any{
		"usersInDeletionCooldown": bundle(
			map[string]any{"id": "u-due", "concept": "v1:identity:user", "payload": map[string]any{"deletionScheduledAt": at(25.5)}},
			map[string]any{"id": "u-early", "concept": "v1:identity:user", "payload": map[string]any{"deletionScheduledAt": at(10)}},
			map[string]any{"id": "u-late", "concept": "v1:identity:user", "payload": map[string]any{"deletionScheduledAt": at(27)}},
		),
	}}
	out, evs, err := runV1Logic(t, "accountDeletionReminder25Days", reminderLogic, funcs, map[string]any{"event": map[string]any{}})
	if err != nil {
		t.Fatalf("RunLogic: %v", err)
	}
	if out != int64(3) {
		t.Fatalf("return candidates.count() = %#v, want 3", out)
	}
	evs.mu.Lock()
	defer evs.mu.Unlock()
	if len(evs.published) != 1 {
		t.Fatalf("published %d reminders (%v), want exactly the due user's", len(evs.published), evs.published)
	}
	payload, _ := evs.published[0]["payload"].(map[string]any)
	if evs.published[0]["topic"] != "identity.deletion.reminder" || payload["userId"] != "u-due" {
		t.Fatalf("published %#v, want the reminder for u-due", evs.published[0])
	}
	if payload["milestoneDays"] != float64(25) {
		t.Fatalf("milestoneDays = %#v (%T), want the literal 25", payload["milestoneDays"], payload["milestoneDays"])
	}
}

// conflictLogic is dsl/data/logic.memql's conflictDetection in the
// edition-2026 grammar.
const conflictLogic = `logic conflictDetection {
  args {
    event object!
  }
  body {
    matchingConfirmed := query detectConflicts( partitionId: args.event.payload.partitionId, recordType: args.event.payload.recordType )
    emitConflicts := if !matchingConfirmed.empty() { publishEvent( topic: "data.conflicts.detected", payload: { partitionId: args.event.payload.partitionId, matchCount: matchingConfirmed.count(), matches: matchingConfirmed.nodes(), requiresHumanApproval: true } ) }
    return emitConflicts
  }
}`

// TestV1NotEmptyIsTrueWhenThereAreMatches: `!matchingConfirmed.empty()` is
// TRUE when the query matched rows -- the conflict event is published with
// the rows and their count -- and false when it matched none.
func TestV1NotEmptyIsTrueWhenThereAreMatches(t *testing.T) {
	event := map[string]any{"payload": map[string]any{"partitionId": "p-1", "recordType": "invoice"}}

	funcs := &recordingFunctions{results: map[string]any{
		"detectConflicts": bundle(
			map[string]any{"id": "r-1", "payload": map[string]any{"naturalKeyValue": "k"}},
			map[string]any{"id": "r-2", "payload": map[string]any{"naturalKeyValue": "k"}},
		),
	}}
	_, evs, err := runV1Logic(t, "conflictDetection", conflictLogic, funcs, map[string]any{"event": event})
	if err != nil {
		t.Fatalf("RunLogic: %v", err)
	}
	// The query's arguments were evaluated values, not reference text.
	calls := funcs.named("detectConflicts")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, map[string]any{"partitionId": "p-1", "recordType": "invoice"}) {
		t.Fatalf("detectConflicts was called with %#v", calls)
	}
	if got := evs.topics(); len(got) != 1 || got[0] != "data.conflicts.detected" {
		t.Fatalf("published %v, want the conflict event", got)
	}
	payload, _ := evs.published[0]["payload"].(map[string]any)
	if payload["matchCount"] != int64(2) || payload["partitionId"] != "p-1" || payload["requiresHumanApproval"] != true {
		t.Fatalf("conflict payload = %#v", payload)
	}
	if matches, _ := payload["matches"].([]any); len(matches) != 2 {
		t.Fatalf("matches = %#v, want the two rows", payload["matches"])
	}

	// Control: no matches, no event.
	none := &recordingFunctions{results: map[string]any{"detectConflicts": bundle()}}
	_, evs, err = runV1Logic(t, "conflictDetection", conflictLogic, none, map[string]any{"event": event})
	if err != nil {
		t.Fatalf("RunLogic (no matches): %v", err)
	}
	if got := evs.topics(); len(got) != 0 {
		t.Fatalf("published %v with no matches", got)
	}
}

// ---------------------------------------------------------------------------
// the forEach clone as a caller
// ---------------------------------------------------------------------------

// TestV1ForEachDataModel: inside a forEach body -- the clone of the run's
// evaluator -- a v1 step reads its loop variable under its own name, `index`,
// the event, the args, earlier steps, the actor and the clock; the filter and
// the nested condition are v1 conditions over the same clone.
func TestV1ForEachDataModel(t *testing.T) {
	a := prepareV1(t, `{
		"name": "forEachDataModel",
		"steps": [{"id": "loop", "type": "forEach", "forEach": {
			"source": "args.items",
			"filter": "it.keep == true",
			"as": "it",
			"do": [{"id": "visit", "type": "function",
				"condition": "index >= 0 && it.name != \"skipme\"",
				"function": {"name": "visit", "args": {
					"name":  {"$expr": "it.name"},
					"idx":   {"$expr": "index"},
					"ep":    {"$expr": "event.payload.x"},
					"ax":    {"$expr": "args.x"},
					"prior": {"$expr": "steps.s.result.y"},
					"user":  {"$expr": "actor.userId"},
					"clock": {"$expr": "now"}
				}}}]
		}}]
	}`)
	ev := automations.NewEvaluator()
	ev.SetCustom("event", map[string]any{"topic": "t", "payload": map[string]any{"x": "hello"}})
	ev.SetCustom("args", map[string]any{"x": "ex", "items": []any{
		map[string]any{"name": "a", "keep": true},
		map[string]any{"name": "b", "keep": false},
		map[string]any{"name": "skipme", "keep": true},
		map[string]any{"name": "c", "keep": true},
	}})
	ev.SetCustom("actor", auth.ActorEnvelopeMap(&auth.AccessContext{UserId: "user-7"}))
	ev.SetCustom("timestamp", time.Now().UTC().Format(time.RFC3339))
	ev.SetStepResult("s", &automations.StepResult{StepId: "s", Status: "success", Result: map[string]any{"y": "Y"}})

	funcs := &recordingFunctions{}
	reg := v1Registry(funcs, &recordingEvents{})
	res, err := (&ForEachExecutor{Registry: reg}).Execute(context.Background(), a.Steps[0], &Context{Evaluator: ev})
	if err != nil {
		t.Fatalf("forEach: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("forEach status %q: %s", res.Status, res.Error)
	}
	visits := funcs.named("visit")
	if len(visits) != 2 {
		t.Fatalf("visited %d items (%#v), want a and c (b filtered out, skipme skipped by the condition)", len(visits), visits)
	}
	for i, want := range []struct {
		name string
		idx  int
	}{{"a", 0}, {"c", 2}} {
		got := visits[i].args
		if got["name"] != want.name || got["idx"] != want.idx {
			t.Errorf("visit %d = %#v, want name %q index %d", i, got, want.name, want.idx)
		}
		for k, v := range map[string]any{"ep": "hello", "ax": "ex", "prior": "Y", "user": "user-7"} {
			if got[k] != v {
				t.Errorf("visit %d: %s = %#v, want %#v", i, k, got[k], v)
			}
		}
		if s, _ := got["clock"].(string); s == "" {
			t.Errorf("visit %d: clock = %#v, want the run clock", i, got["clock"])
		}
	}
}

// ---------------------------------------------------------------------------
// the executors' v1 halves
// ---------------------------------------------------------------------------

// TestV1QueryStepEvaluatesInProcess: a query step whose expression is not a
// construct call is evaluated in process -- no engine is configured here, and
// none is needed -- and its value is the step's result.
func TestV1QueryStepEvaluatesInProcess(t *testing.T) {
	a := prepareV1(t, `{"name":"q","steps":[
		{"id":"total","type":"query","query":{"query":"args.a + args.b * 2"}},
		{"id":"absent","type":"query","query":{"query":"args.missing"}}]}`)
	ev := automations.NewEvaluator()
	ev.SetCustom("args", map[string]any{"a": float64(1), "b": float64(3)})
	for i, want := range []any{float64(7), nil} {
		res, err := (&QueryExecutor{}).Execute(context.Background(), a.Steps[i], &Context{Evaluator: ev})
		if err != nil {
			t.Fatalf("step %s: %v", a.Steps[i].ID, err)
		}
		if res.Status != "success" || !reflect.DeepEqual(res.Result, want) {
			t.Fatalf("step %s = %#v (%s), want %#v", a.Steps[i].ID, res.Result, res.Status, want)
		}
	}
}

// TestV1EventStepEvaluatesTopicAndPayload: the event executor's topic is a
// v1 expression (a literal written as one, or a reference) and its payload
// leaves are values; an absent leaf is omitted.
func TestV1EventStepEvaluatesTopicAndPayload(t *testing.T) {
	a := prepareV1(t, `{"name":"e","steps":[
		{"id":"pub","type":"event","event":{"topic":{"$expr":"\"app.\" + args.kind"},"payload":{
			"who":{"$expr":"args.who"},"gone":{"$expr":"args.missing"},"lit":"args.who"}}}]}`)
	ev := automations.NewEvaluator()
	ev.SetCustom("args", map[string]any{"kind": "done", "who": "w-1"})
	bus := events.NewBus()
	defer bus.Close()
	res, err := (&EventExecutor{}).Execute(context.Background(), a.Steps[0], &Context{Evaluator: ev, EventBus: bus})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	out := res.Result.(map[string]any)
	if out["topic"] != "app.done" {
		t.Fatalf("topic = %#v", out["topic"])
	}
	if want := map[string]any{"who": "w-1", "lit": "args.who"}; !reflect.DeepEqual(out["payload"], want) {
		t.Fatalf("payload = %#v, want %#v", out["payload"], want)
	}
}

// TestV1SwitchSubject: a v1 switch subject selects its case by value.
func TestV1SwitchSubject(t *testing.T) {
	a := prepareV1(t, `{"name":"sw","steps":[
		{"id":"route","type":"switch","switch":{"expression":"args.n > 1 ? \"many\" : \"one\"",
			"cases":{"many":{"steps":[{"id":"m","type":"function","function":{"name":"many"}}]},
			         "one":{"steps":[{"id":"o","type":"function","function":{"name":"one"}}]}}}}]}`)
	ev := automations.NewEvaluator()
	ev.SetCustom("args", map[string]any{"n": float64(3)})
	funcs := &recordingFunctions{}
	res, err := (&SwitchExecutor{Registry: v1Registry(funcs, &recordingEvents{})}).Execute(context.Background(), a.Steps[0], &Context{Evaluator: ev})
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if res.Metadata["matchedCase"] != "many" || len(funcs.named("many")) != 1 || len(funcs.named("one")) != 0 {
		t.Fatalf("switch matched %v (calls %#v), want case many", res.Metadata["matchedCase"], funcs.calls)
	}
}

// TestRenderMemQLDataQuotesReferenceText: an evaluated value is DATA. A
// string that reads like a reference is quoted -- the legacy renderer passed
// it through bare, and the engine re-read it as a reference.
func TestRenderMemQLDataQuotesReferenceText(t *testing.T) {
	for _, s := range []string{"event.payload.x", "steps.a.result", "$args.x", "item", "concat(a, b)"} {
		if got := renderMemQLData(s); got != langparser.QuoteString(s) {
			t.Errorf("renderMemQLData(%q) = %s, want it quoted", s, got)
		}
		if got := renderMemQLData(map[string]any{"k": []any{s}}); !strings.Contains(got, langparser.QuoteString(s)) {
			t.Errorf("renderMemQLData nested %q = %s, want it quoted", s, got)
		}
	}
	for v, want := range map[any]string{int64(3): "3", float64(2.5): "2.5", true: "true"} {
		if got := renderMemQLData(v); got != want {
			t.Errorf("renderMemQLData(%#v) = %s, want %s", v, got, want)
		}
	}
	if got := renderMemQLData(nil); got != "null" {
		t.Errorf("renderMemQLData(nil) = %s", got)
	}
}

// TestV1ConstructCallText: a construct call renders as `name(k: <literal>)`
// with its arguments evaluated -- sorted, an absent one omitted, an explicit
// nil passed as null -- and refuses positional arguments.
func TestV1ConstructCallText(t *testing.T) {
	ev := automations.NewEvaluator()
	ev.SetCustom("args", map[string]any{"id": "event.payload.x"})
	node, err := langparser.ParseV1Expression(`query rowsFor(id: args.id, gone: args.missing, none: nil, n: 1 + 1)`)
	if err != nil {
		t.Fatal(err)
	}
	call, ok := node.(*ast.CallExpr)
	if !ok || call.Kind != "query" {
		t.Fatalf("parsed %T %+v, want a query construct call", node, node)
	}
	got, err := v1ConstructCallText(context.Background(), ev, call)
	if err != nil {
		t.Fatal(err)
	}
	if want := `rowsFor(id: "event.payload.x", n: 2, none: null)`; got != want {
		t.Fatalf("call text = %s, want %s", got, want)
	}
	positional := &ast.CallExpr{Kind: "query", Name: "rowsFor", Args: []ast.ExpressionNode{&ast.LiteralExpr{Value: "x"}}}
	if _, err := v1ConstructCallText(context.Background(), ev, positional); err == nil {
		t.Fatal("a positional construct-call argument was accepted")
	}
}
