package automations

// journal_trigger.go -- what of its triggering event a run records on its row
// (openRunArgs' triggerEvent).
//
// ===========================================================================
// WHY THE RECORDED PAYLOAD IS PROJECTED
// ===========================================================================
// An event firing records its trigger -- topic, kind, payload and cause -- on
// the run's FIRST version, and the row is append-only with a read-merge write:
// every later version (each step receipt's updateWorkRun, the close) copies
// the whole trigger again. The payload of a graph event is the whole row that
// fired it, so a run fired by a staged inbound delivery carried the delivery's
// headers and body, about 24 KB raw, on each of its three or more versions (a
// 1% sample of production's 2026-10-15 chunk). Yet the automations that fire
// on those rows declare `args { id any }` and read nothing else of it: the
// builtin they call re-reads the row by id.
//
// So a run records only the payload keys it can read, when that is decidable
// at load (computeJournalTriggerReads): the keys its declared args bind, the
// keys the dedup key keeps (Automation.Reads), `id` / `nodeId`, which the
// per-row budget reads on an adopted run (budgetRowId), and any
// `event.payload.<key>` path a prepared expression names. An automation's own
// statements can no longer write that last form -- the compiler refuses a
// dotted `event.<field>` read and names `args.<field>` -- so in practice a
// projected run records its declared args; the path is still read so an
// expression that reaches the analysis another way is not dropped. One key is
// kept beside those whatever the body reads, because its reader is the executor
// and not the body: `accountId`, the triggering row's account (recordedTrigger).
// Everything that reads a recorded trigger back sees what it reads:
//
//   - resume (resume.go) rebinds args from the recorded payload and sets it as
//     the `event` root; adoption (ExecuteAdopted, a run with no steps yet)
//     re-executes from it.
//   - the run's account: a run acts in the account of the row that triggered
//     it, and a resumed or adopted run reads that account back off the recorded
//     payload, with the concept taken from the recorded topic
//     (contextWithTriggerAccount). Projected away, the second attempt would act
//     in no account where the first acted in one.
//   - the cause: a root run re-derives its correlation id from the WHOLE
//     payload's fingerprint (rootCorrelation), so a projected root run records
//     the correlation its first attempt derived as its cause
//     ({correlationId}), which runCause continues exactly as it would the
//     root (same correlation, depth one, the run alone in its chain). A run in
//     a chain records its parent cause whole, as before.
//   - integrations/work's replan context reads the payload as variables when a
//     run has none; the declared args are what a replan can use.
//
// WHAT KEEPS THE WHOLE PAYLOAD. nil from the analysis: a body that names
// `event` bare (`logic x(event: event)` hands the whole event on), reads
// `event.payload` whole or by a computed key, or reaches a logic whose body
// names `event` at all; a call the function registry cannot resolve; and any
// precondition, which may consult the event. And only an EVENT FIRING
// (triggeredBy `event:<topic>`) is projected: a sub-automation's synthetic
// event carries its call's arguments, which child continuation compares whole
// (child_continuation.go), and an http trigger's or an operator's run keeps
// what it was handed.
//
// THE TRADE-OFF: a run that would read a payload key the analysis did not see
// reads nothing on resume. That is why every unresolvable shape keeps the
// whole payload rather than guessing, and why the analysis walks every step
// container by reflection rather than by the step types known today.
// Tests: journal_trigger_test.go; over real rows,
// TestResumeV1_DB_AProjectedTriggerResumesWithWhatItReads (resume_v1_db_test.go).

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/work"
)

// computeJournalTriggerReads is the payload keys a run of a can read from its
// triggering event, sorted, or nil when it may read the whole payload (see the
// file comment for what decides nil). Called at load, after
// prepareExpressions and computeReads.
func computeJournalTriggerReads(a *Automation, source FunctionSource) []string {
	if a == nil || source == nil || len(a.Preconditions) > 0 {
		return nil
	}
	fields := map[string]bool{"id": true, "nodeId": true}
	if a.Args != nil {
		for _, f := range a.Args.Fields {
			if f != nil && f.Name != "" {
				fields[f.Name] = true
			}
		}
	}
	// The dedup key's projection, so the chain head an adopted run recomputes
	// from its recorded trigger (chainEventData) sees every key it keeps.
	for _, f := range a.Reads {
		fields[f] = true
	}
	whole := false
	readsOf := func(n ast.ExpressionNode) {
		ast.WalkV1(n, func(e ast.ExpressionNode) bool {
			if root, path, ok := ast.MemberPath(e); ok {
				if root == "event" {
					switch {
					case len(path) >= 2 && path[0] == "payload":
						fields[path[1]] = true
					case len(path) >= 1 && path[0] != "payload":
						// The envelope's own fields (topic, kind, cause): recorded whole.
					default:
						whole = true
					}
				}
				return false
			}
			if id, ok := e.(*ast.IdentExpr); ok && id.Name == "event" {
				whole = true
			}
			return true
		})
	}
	calls := scanTriggerReads(reflect.ValueOf(a.Steps), readsOf)
	if whole || !logicCallsLeaveTheEventAlone(calls, source, map[string]bool{}) {
		return nil
	}
	out := make([]string, 0, len(fields))
	for f := range fields {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// scanTriggerReads hands every prepared expression under v to reads and
// answers the names of every function step reachable from it, through any
// step container: by reflection, so a container added later is walked too.
func scanTriggerReads(v reflect.Value, reads func(ast.ExpressionNode)) []string {
	var calls []string
	var scan func(reflect.Value)
	scan = func(v reflect.Value) {
		if !v.IsValid() || ((v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()) {
			return
		}
		if v.CanInterface() {
			if n, ok := v.Interface().(ast.ExpressionNode); ok {
				reads(n)
				return
			}
			if s, ok := v.Interface().(*Step); ok && s.Function != nil {
				calls = append(calls, s.Function.Name)
			}
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			scan(v.Elem())
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() {
					scan(v.Field(i))
				}
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				scan(v.MapIndex(k))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				scan(v.Index(i))
			}
		}
	}
	scan(v)
	return calls
}

// logicCallsLeaveTheEventAlone reports whether every call can be resolved and
// none reaches a logic body that names `event`. A query, mutation or builtin
// sees only the arguments its call passes, which the caller's own scan read;
// a logic's statements run beside the caller's and could read the ambient
// event, so its body is scanned, and the logic it calls in turn.
func logicCallsLeaveTheEventAlone(calls []string, source FunctionSource, visiting map[string]bool) bool {
	reg := source.Registry()
	for _, name := range calls {
		target, ok := reg[name]
		if !ok {
			return false
		}
		if target.ConstructKind != work.ConstructLogic {
			continue
		}
		if visiting[name] {
			continue
		}
		src, ok := source.(registrySource)
		if !ok || src.functions == nil || src.functions[name] == nil {
			return false
		}
		raw, err := json.Marshal(src.functions[name].LogicBody)
		if err != nil {
			return false
		}
		body := &Automation{}
		if json.Unmarshal(raw, &body.Steps) != nil || PrepareExpressions(body) != nil {
			return false
		}
		names := false
		inner := scanTriggerReads(reflect.ValueOf(body.Steps), func(n ast.ExpressionNode) {
			ast.WalkV1(n, func(e ast.ExpressionNode) bool {
				if root, _, ok := ast.MemberPath(e); ok {
					if root == "event" {
						names = true
					}
					return false
				}
				if id, ok := e.(*ast.IdentExpr); ok && id.Name == "event" {
					names = true
				}
				return true
			})
		})
		if names {
			return false
		}
		visiting[name] = true
		if !logicCallsLeaveTheEventAlone(inner, source, visiting) {
			return false
		}
	}
	return true
}

// recordedTrigger is the triggerEvent a run records: the event's topic, kind
// and payload -- the payload projected to what an event firing's run can read
// (computeJournalTriggerReads) -- and its cause. parent is the run's parent
// cause; a projected root run records its own correlation in its place (the
// file comment says why).
func recordedTrigger(automation *Automation, triggeredBy string, ev *events.Event, parent events.Cause) map[string]any {
	payload, projected := ev.Payload, false
	if automation != nil && automation.JournalTriggerReads != nil && isEventFiring(triggeredBy) && ev.Payload != nil {
		payload, projected = make(map[string]any, len(automation.JournalTriggerReads)+1), true
		for _, key := range automation.JournalTriggerReads {
			if v, ok := ev.Payload[key]; ok {
				payload[key] = v
			}
		}
		// THE ROW'S ACCOUNT IS KEPT WHATEVER THE BODY READS. It is not one of
		// the run's reads, so it is added here rather than to
		// JournalTriggerReads, which stays the answer to "what can the body
		// see": the executor reads it, to place a resumed or adopted run in
		// the account its first attempt acted in (contextWithTriggerAccount).
		// Kept as the row had it and only when the row had it, so a row with no
		// account records no key rather than an empty one. Whether it MEANS an
		// account is decided when it is read back, from the concept on the
		// recorded topic; a cluster-wide row's field of the same name is
		// recorded and never consulted.
		if v, ok := ev.Payload[triggerAccountField]; ok {
			payload[triggerAccountField] = v
		}
	}
	trigger := map[string]any{
		"topic":   ev.Topic,
		"kind":    ev.Kind.String(),
		"payload": payload,
	}
	switch {
	case !parent.IsZero():
		if projected && parent.CorrelationId == "" {
			// runCause fell back to the root's correlation for a parent that
			// carried none; a resume could not derive it again from the
			// projected payload.
			parent = parent.Clone()
			parent.CorrelationId = rootCorrelation(ev)
		}
		trigger["cause"] = parent
	case projected:
		trigger["cause"] = events.Cause{CorrelationId: rootCorrelation(ev)}
	}
	return trigger
}

// isEventFiring reports the scheduler's event firing (scheduler.go), the only
// run whose recorded trigger is projected.
func isEventFiring(triggeredBy string) bool {
	return strings.HasPrefix(triggeredBy, TriggeredByEventPrefix) && len(triggeredBy) > len(TriggeredByEventPrefix)
}
