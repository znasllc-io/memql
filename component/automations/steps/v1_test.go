package steps

// v1_test.go -- the step executors over the statement runtime (memql#5367,
// epic memql#5370): a `for` as a caller of the run's data model, the event
// executor, a switch selecting its case, the literal renderer, and the two
// behaviour fixes v1 exists to make -- each driven through the REAL
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
// evaluates a call's arguments exactly as FunctionExecutor does
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
// evaluated by it -- and records what it published.
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

// runV1Automation runs a statement automation compiled from src through the
// real executor over the real registry (function executor replaced), on an
// event carrying payload and a bus the event executor publishes to.
func runV1Automation(t *testing.T, src string, funcs *recordingFunctions, payload map[string]any) (*recordingEvents, *automations.AutomationExecution) {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(src, "test.memql")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	evs := &recordingEvents{}
	ev := events.NewEvent("probe.fired", events.KindMessage, payload)
	exec, err := automations.NewExecutor(automations.ExecutorOptions{EventBus: bus, StepRegistry: v1Registry(funcs, evs)}).ExecuteWithEvent(context.Background(), a, "test", &ev)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	return evs, exec
}

// rows is a query's result as the engine answers it: its rows.
func rows(nodes ...map[string]any) *memql.ExecuteResult {
	list := make([]any, len(nodes))
	for i, n := range nodes {
		list[i] = n
	}
	return memql.NewResultWithOutput(list)
}

// ---------------------------------------------------------------------------
// the behaviour fixes
// ---------------------------------------------------------------------------

// reminderLogic is dsl/identity/logic.memql's usersDueDeletionReminder: the
// [from, to) window is decided per row by a date comparison against `now`.
const reminderLogic = `logic usersDueDeletionReminder {
  args {
    from string!
    to string!
  }
  candidates := query usersInDeletionCooldown()
  return candidates.nodes().where(u => addDuration(u.deletionScheduledAt, args.from) < now && addDuration(u.deletionScheduledAt, args.to) >= now)
}`

// TestV1ReminderGateIsTrueOnADueItem: the identity reminder window
// `addDuration(u.deletionScheduledAt, "P25D") < now &&
// addDuration(..., "P26D") >= now` is TRUE for a user 25.5 days into the
// cooldown, and false either side of the window -- so exactly the due user is
// reminded.
func TestV1ReminderGateIsTrueOnADueItem(t *testing.T) {
	at := func(daysAgo float64) string {
		return time.Now().UTC().Add(-time.Duration(daysAgo * 24 * float64(time.Hour))).Format(time.RFC3339)
	}
	due := map[string]any{"id": "u-due", "concept": "v1:identity:user", "payload": map[string]any{"deletionScheduledAt": at(25.5)}}
	funcs := &recordingFunctions{results: map[string]any{
		"usersInDeletionCooldown": rows(
			due,
			map[string]any{"id": "u-early", "concept": "v1:identity:user", "payload": map[string]any{"deletionScheduledAt": at(10)}},
			map[string]any{"id": "u-late", "concept": "v1:identity:user", "payload": map[string]any{"deletionScheduledAt": at(27)}},
		),
	}}
	body := compiledLogicForSteps(t, reminderLogic)
	runner := automations.NewLogicRunner(&memql.MemQLEngine{}, v1Registry(funcs, &recordingEvents{}), nil)
	out, err := runner.RunLogicBody(context.Background(), "usersDueDeletionReminder", body, map[string]any{"from": "P25D", "to": "P26D"})
	if err != nil {
		t.Fatalf("RunLogicBody: %v", err)
	}
	if !reflect.DeepEqual(out, []any{due}) {
		t.Fatalf("the window returned %#v, want exactly the due user's row", out)
	}
}

// conflictAutomation is dsl/data/automations.memql's conflictDetection.
const conflictAutomation = `@trigger(event="probe.fired")
automation conflictDetection {
  args {
    partitionId any
    recordType any
  }
  matchingConfirmed := query detectConflicts(partitionId: args.partitionId, recordType: args.recordType)
  if !matchingConfirmed.empty() {
    publish "data.conflicts.detected" { partitionId: args.partitionId, matchCount: matchingConfirmed.count(), matches: matchingConfirmed.nodes(), requiresHumanApproval: true }
  }
}`

// TestV1NotEmptyIsTrueWhenThereAreMatches: `!matchingConfirmed.empty()` is
// TRUE when the query matched rows -- the conflict event is published with
// the rows and their count -- and false when it matched none.
func TestV1NotEmptyIsTrueWhenThereAreMatches(t *testing.T) {
	payload := map[string]any{"partitionId": "p-1", "recordType": "invoice"}

	funcs := &recordingFunctions{results: map[string]any{
		"detectConflicts": rows(
			map[string]any{"id": "r-1", "payload": map[string]any{"naturalKeyValue": "k"}},
			map[string]any{"id": "r-2", "payload": map[string]any{"naturalKeyValue": "k"}},
		),
	}}
	evs, _ := runV1Automation(t, conflictAutomation, funcs, payload)
	// The query's arguments were evaluated values, not reference text.
	calls := funcs.named("detectConflicts")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, map[string]any{"partitionId": "p-1", "recordType": "invoice"}) {
		t.Fatalf("detectConflicts was called with %#v", calls)
	}
	if got := evs.topics(); len(got) != 1 || got[0] != "data.conflicts.detected" {
		t.Fatalf("published %v, want the conflict event", got)
	}
	published, _ := evs.published[0]["payload"].(map[string]any)
	if published["matchCount"] != int64(2) || published["partitionId"] != "p-1" || published["requiresHumanApproval"] != true {
		t.Fatalf("conflict payload = %#v", published)
	}
	if matches, _ := published["matches"].([]any); len(matches) != 2 {
		t.Fatalf("matches = %#v, want the two rows", published["matches"])
	}

	// Control: no matches, no event.
	none := &recordingFunctions{results: map[string]any{"detectConflicts": rows()}}
	evs, _ = runV1Automation(t, conflictAutomation, none, payload)
	if got := evs.topics(); len(got) != 0 {
		t.Fatalf("published %v with no matches", got)
	}
}

// ---------------------------------------------------------------------------
// the `for` as a caller
// ---------------------------------------------------------------------------

// TestV1ForEachDataModel: inside a `for` -- a frame of its own inside the
// run's -- a call reads its loop variable under its own name, the event, the
// args, a name an earlier statement bound, the actor and the clock; the filter
// and the nested condition are conditions over the same frame.
func TestV1ForEachDataModel(t *testing.T) {
	a := prepareV1(t, `{
		"name": "forEachDataModel",
		"args": {"fields": [{"name": "x", "type": "any", "optional": true}, {"name": "items", "type": "any", "optional": true}]},
		"steps": [
			{"id": "s", "type": "function", "binds": "s", "function": {"name": "seed", "kind": "builtin"}},
			{"id": "loop", "type": "forEach", "forEach": {
				"source": "args.items",
				"filter": "it.keep == true",
				"as": "it",
				"do": [{"id": "visit", "type": "function",
					"condition": "it.name != \"skipme\"",
					"function": {"name": "visit", "kind": "builtin", "args": {
						"name":  {"$expr": "it.name"},
						"ep":    {"$expr": "event.payload.x"},
						"ax":    {"$expr": "args.x"},
						"prior": {"$expr": "s.y"},
						"user":  {"$expr": "actor.userId"},
						"clock": {"$expr": "now"}
					}}}]
			}}
		]
	}`)
	funcs := &recordingFunctions{results: map[string]any{"seed": map[string]any{"y": "Y"}}}
	ctx := auth.ContextWithUserActor(context.Background(), "user-7")
	ev := events.NewEvent("probe.fired", events.KindMessage, map[string]any{"x": "ex", "items": []any{
		map[string]any{"name": "a", "keep": true},
		map[string]any{"name": "b", "keep": false},
		map[string]any{"name": "skipme", "keep": true},
		map[string]any{"name": "c", "keep": true},
	}})
	exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: v1Registry(funcs, &recordingEvents{})}).ExecuteWithEvent(ctx, a, "test", &ev)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	visits := funcs.named("visit")
	if len(visits) != 2 {
		t.Fatalf("visited %d items (%#v), want a and c (b filtered out, skipme skipped by the condition)", len(visits), visits)
	}
	for i, want := range []string{"a", "c"} {
		got := visits[i].args
		if got["name"] != want {
			t.Errorf("visit %d = %#v, want name %q", i, got, want)
		}
		for k, v := range map[string]any{"ep": "ex", "ax": "ex", "prior": "Y", "user": "user-7"} {
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
// the executors
// ---------------------------------------------------------------------------

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

// TestV1SwitchSubject: a switch statement's subject selects its case by
// value, and only that case's statements run.
func TestV1SwitchSubject(t *testing.T) {
	for _, c := range []struct {
		n    float64
		want string
	}{{3, "many"}, {1, "one"}} {
		funcs := &recordingFunctions{}
		runV1Automation(t, `@trigger(event="probe.fired")
automation route {
  args {
    n any
  }
  switch args.n > 1 ? "many" : "one" {
    case "many" {
      builtin many()
    }
    case "one" {
      builtin one()
    }
  }
}`, funcs, map[string]any{"n": c.n})
		other := map[string]string{"many": "one", "one": "many"}[c.want]
		if len(funcs.named(c.want)) != 1 || len(funcs.named(other)) != 0 {
			t.Fatalf("n=%v: calls %#v, want only case %s", c.n, funcs.calls, c.want)
		}
	}
}

// TestRenderMemQLDataQuotesReferenceText: an evaluated value is DATA. A
// string that reads like a reference is quoted -- passed through bare, the
// engine would re-read it as a reference.
func TestRenderMemQLDataQuotesReferenceText(t *testing.T) {
	for _, s := range []string{"event.payload.x", "args.x", "row.id", "item", "a + b"} {
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
