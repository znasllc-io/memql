package automations

// loop_graph_filter_test.go -- the three-valued trigger-filter decision the
// static loop graph refines its edges with (memql#5381, D-I of the loop
// protection plan).
//
// Two properties are pinned. The table: each operator under D8's absence
// table and Kleene logic, as the plan names them. And soundness: every
// answer decideFilter DECIDES is the answer the runtime's own filter gives
// for every way of filling in what the row leaves unknown -- because a
// `false` removes an edge, and a false that the runtime would not give hides
// a cycle.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// filterCase is one filter decided against one row.
type filterCase struct {
	name   string
	filter string
	row    knownRow
	want   tri
	// why, when set, must appear in the reason of an unknown answer.
	why string
	// declares is the judged automation's args block: nil declares every
	// field the filter reads through args; an empty, non-nil list declares
	// none of them.
	declares []string
}

// declaredArgsOf is the args block a case's automation declares.
func declaredArgsOf(t *testing.T, tc filterCase) map[string]bool {
	t.Helper()
	names := tc.declares
	if names == nil {
		names = argsRead(mustLambda(t, tc.filter))
	}
	if len(names) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// argsRead is every top-level field a filter reads through args, sorted.
func argsRead(lam *ast.LambdaExpr) []string {
	set := map[string]bool{}
	ast.MemberPaths(lam.Body, func(root string, fields []string) {
		if root == "args" && len(fields) > 0 {
			set[fields[0]] = true
		}
	})
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

const filterConcept = "v1:t:thing"

// newRow is an insert of a new row: fields it does not set are absent.
func newRow(fields map[string]any) knownRow {
	return knownRow{Concept: filterConcept, Fields: fields, OthersKnown: true, FirstVersion: triTrue}
}

// updateRow is an update: fields it does not set keep what is stored.
func updateRow(fields map[string]any) knownRow {
	return knownRow{Concept: filterConcept, Fields: fields, FirstVersion: triFalse}
}

func filterCases() []filterCase {
	return []filterCase{
		// Literals and the lambda's parameter.
		{name: "true", filter: `row => true`, row: updateRow(nil), want: triTrue},
		{name: "false", filter: `row => false`, row: updateRow(nil), want: triFalse},
		{name: "nil is not a condition", filter: `row => nil`, row: updateRow(nil), want: triFalse},

		// == and != under the absence table.
		{name: "== set, equal", filter: `row => row.status == "open"`, row: updateRow(map[string]any{"status": "open"}), want: triTrue},
		{name: "== set, different", filter: `row => row.status == "open"`, row: updateRow(map[string]any{"status": "done"}), want: triFalse},
		{name: "== absent", filter: `row => row.status == "open"`, row: newRow(nil), want: triFalse},
		{name: "== unknown", filter: `row => row.status == "open"`, row: updateRow(nil), want: triUnknown, why: "status"},
		{name: "!= absent is true", filter: `row => row.status != "open"`, row: newRow(nil), want: triTrue},
		{name: "!= set, equal", filter: `row => row.status != "open"`, row: newRow(map[string]any{"status": "open"}), want: triFalse},
		{name: "!= unknown", filter: `row => row.status != "open"`, row: updateRow(nil), want: triUnknown},
		{name: `absent == ""`, filter: `row => row.status == ""`, row: newRow(nil), want: triTrue},
		{name: "absent == nil", filter: `row => row.status == nil`, row: newRow(nil), want: triTrue},
		{name: `"" == nil`, filter: `row => row.status == nil`, row: newRow(map[string]any{"status": ""}), want: triTrue},
		{name: `absent != ""`, filter: `row => row.status != ""`, row: newRow(nil), want: triFalse},
		{name: "a nil literal written is unset", filter: `row => row.status != nil`, row: newRow(map[string]any{"status": nil}), want: triFalse},
		{name: "whitespace is a value", filter: `row => row.status == ""`, row: newRow(map[string]any{"status": " "}), want: triFalse},
		{name: "typed equality", filter: `row => row.n == "1"`, row: newRow(map[string]any{"n": int64(1)}), want: triFalse},
		{name: "numbers across int and float", filter: `row => row.n == 1`, row: newRow(map[string]any{"n": 1.0}), want: triTrue},
		{name: "false is a value, not unset", filter: `row => row.flag == nil`, row: newRow(map[string]any{"flag": false}), want: triFalse},
		{name: "a bool field as the condition", filter: `row => row.archived`, row: newRow(map[string]any{"archived": true}), want: triTrue},
		{name: "an absent field as the condition", filter: `row => row.archived`, row: newRow(nil), want: triFalse},

		// The ordered comparisons: numbers and strings of one type; absent is
		// false both ways, and so is a type mismatch.
		{name: "< numbers", filter: `row => row.n < 3`, row: newRow(map[string]any{"n": int64(2)}), want: triTrue},
		{name: ">= numbers", filter: `row => row.n >= 3`, row: newRow(map[string]any{"n": 2.5}), want: triFalse},
		{name: "< strings", filter: `row => row.s < "m"`, row: newRow(map[string]any{"s": "a"}), want: triTrue},
		{name: "> strings", filter: `row => row.s > "m"`, row: newRow(map[string]any{"s": "a"}), want: triFalse},
		{name: "< absent", filter: `row => row.n < 3`, row: newRow(nil), want: triFalse},
		{name: ">= absent", filter: `row => row.n >= 3`, row: newRow(nil), want: triFalse},
		{name: "< mixed types", filter: `row => row.n < 3`, row: newRow(map[string]any{"n": "2"}), want: triFalse},
		{name: "< unknown", filter: `row => row.n < 3`, row: updateRow(nil), want: triUnknown},
		{name: "< a bool side decides whatever the other", filter: `row => row.flag < row.n`, row: knownRow{Concept: filterConcept, Fields: map[string]any{"flag": true}}, want: triFalse},

		// in over a list literal: membership is ==, the unset rule included.
		{name: "in, member", filter: `row => row.status in ["open", "held"]`, row: newRow(map[string]any{"status": "held"}), want: triTrue},
		{name: "in, not a member", filter: `row => row.status in ["open", "held"]`, row: newRow(map[string]any{"status": "done"}), want: triFalse},
		{name: "in, absent", filter: `row => row.status in ["open", "held"]`, row: newRow(nil), want: triFalse},
		{name: `in, absent in a list holding ""`, filter: `row => row.status in ["open", ""]`, row: newRow(nil), want: triTrue},
		{name: "in, unknown", filter: `row => row.status in ["open", "held"]`, row: updateRow(nil), want: triUnknown},
		{name: "in, the empty list holds nothing", filter: `row => row.status in []`, row: updateRow(nil), want: triFalse},
		{name: "in, an absent element contributes nothing", filter: `row => row.status in [row.other]`, row: newRow(map[string]any{"status": ""}), want: triFalse},
		{name: "in, an unknown element", filter: `row => row.status in ["open", row.other]`, row: knownRow{Concept: filterConcept, Fields: map[string]any{"status": "done"}}, want: triUnknown, why: "other"},
		{name: "in, a member beside an unknown element", filter: `row => row.status in [row.other, "done"]`, row: knownRow{Concept: filterConcept, Fields: map[string]any{"status": "done"}}, want: triTrue},

		// startsWith: a blank prefix is dropped, an empty set matches nothing,
		// and the subject must be a string.
		{name: "startsWith, a match", filter: `row => row.name startsWith "ab"`, row: newRow(map[string]any{"name": "abc"}), want: triTrue},
		{name: "startsWith, no match", filter: `row => row.name startsWith "ab"`, row: newRow(map[string]any{"name": "xab"}), want: triFalse},
		{name: "startsWith, any of a list", filter: `row => row.name startsWith ["x", "ab"]`, row: newRow(map[string]any{"name": "abc"}), want: triTrue},
		{name: "startsWith, absent", filter: `row => row.name startsWith "ab"`, row: newRow(nil), want: triFalse},
		{name: "startsWith, not a string", filter: `row => row.name startsWith "1"`, row: newRow(map[string]any{"name": int64(12)}), want: triFalse},
		{name: "startsWith, unknown", filter: `row => row.name startsWith "ab"`, row: updateRow(nil), want: triUnknown},
		{name: "startsWith, only blank prefixes", filter: `row => row.name startsWith ["", " "]`, row: updateRow(nil), want: triFalse},

		// && || ! under Kleene logic.
		{name: "&& false dominates unknown", filter: `row => row.a == 1 && row.b == 2`, row: knownRow{Concept: filterConcept, Fields: map[string]any{"b": int64(3)}}, want: triFalse},
		{name: "&& false dominates unknown, left", filter: `row => row.b == 2 && row.a == 1`, row: knownRow{Concept: filterConcept, Fields: map[string]any{"b": int64(3)}}, want: triFalse},
		{name: "&& true and unknown", filter: `row => row.a == 1 && row.b == 2`, row: knownRow{Concept: filterConcept, Fields: map[string]any{"b": int64(2)}}, want: triUnknown, why: "a"},
		{name: "&& both true", filter: `row => row.a == 1 && row.b == 2`, row: newRow(map[string]any{"a": int64(1), "b": int64(2)}), want: triTrue},
		{name: "|| true dominates unknown", filter: `row => row.a == 1 || row.b == 2`, row: knownRow{Concept: filterConcept, Fields: map[string]any{"b": int64(2)}}, want: triTrue},
		{name: "|| false and unknown", filter: `row => row.a == 1 || row.b == 2`, row: knownRow{Concept: filterConcept, Fields: map[string]any{"b": int64(3)}}, want: triUnknown, why: "a"},
		{name: "|| both false", filter: `row => row.a == 1 || row.b == 2`, row: newRow(nil), want: triFalse},
		{name: "! negates", filter: `row => !(row.status == "done")`, row: newRow(map[string]any{"status": "done"}), want: triFalse},
		{name: "! of absent is true", filter: `row => !row.archived`, row: newRow(nil), want: triTrue},
		{name: "! of unknown", filter: `row => !(row.status == "done")`, row: updateRow(nil), want: triUnknown},
		{name: "the first unknown is named", filter: `row => row.a == 1 && row.b == 2`, row: updateRow(nil), want: triUnknown, why: "a"},

		// ?? is blank-coalescing.
		{name: "?? absent falls through", filter: `row => (row.status ?? "open") == "open"`, row: newRow(nil), want: triTrue},
		{name: "?? blank falls through", filter: `row => (row.status ?? "open") == "open"`, row: newRow(map[string]any{"status": "  "}), want: triTrue},
		{name: "?? a value is kept", filter: `row => (row.status ?? "open") == "open"`, row: newRow(map[string]any{"status": "done"}), want: triFalse},
		{name: "?? false is kept", filter: `row => row.flag ?? true`, row: newRow(map[string]any{"flag": false}), want: triFalse},
		{name: "?? unknown", filter: `row => (row.status ?? "open") == "open"`, row: updateRow(nil), want: triUnknown},
		{name: "?? to absent is unset", filter: `row => (row.a ?? row.b) == nil`, row: newRow(nil), want: triTrue},

		// Parentheses are transparent.
		{name: "parens", filter: `row => ((row.status == "open"))`, row: newRow(map[string]any{"status": "open"}), want: triTrue},

		// args.<f> reads the written field; args.firstVersion reads the
		// write's first-version answer; row.concept is the concept.
		{name: "args reads the written field", filter: `row => args.status == "open"`, row: newRow(map[string]any{"status": "open"}), want: triTrue},
		{name: "firstVersion of an update", filter: `row => args.firstVersion == true`, row: updateRow(nil), want: triFalse},
		{name: "firstVersion of a new row", filter: `row => args.firstVersion == true`, row: newRow(nil), want: triTrue},
		{name: "firstVersion unknown", filter: `row => args.firstVersion == true`, row: knownRow{Concept: filterConcept, FirstVersion: triUnknown}, want: triUnknown, why: "firstVersion"},
		{name: "row.concept", filter: `row => row.concept == "v1:t:thing"`, row: updateRow(nil), want: triTrue},
		{name: "row.concept, another", filter: `row => row.concept == "v1:t:other"`, row: updateRow(nil), want: triFalse},
		// args.concept is the event's, which a stored `concept` field would
		// shadow in the flattened payload: decided only where every field is
		// known.
		{name: "args.concept of a new row", filter: `row => args.concept == "v1:t:thing"`, row: newRow(nil), want: triTrue},
		{name: "args.concept of an update", filter: `row => args.concept == "v1:t:thing"`, row: updateRow(nil), want: triUnknown, why: "concept"},
		{name: "a parameter of another name", filter: `x => x.status == "open"`, row: newRow(map[string]any{"status": "open"}), want: triTrue},

		// An args field the judged automation does not declare is absent at
		// run time (args binds declared fields only; with no block, args is
		// absent), whatever the write sets -- firstVersion included.
		{name: "undeclared args.f != a value the write sets", filter: `row => args.status != "done"`, row: updateRow(map[string]any{"status": "done"}), declares: []string{}, want: triTrue},
		{name: "undeclared args.f == a value the write sets", filter: `row => args.status == "done"`, row: newRow(map[string]any{"status": "done"}), declares: []string{}, want: triFalse},
		{name: "undeclared args.f is absent over an unknown row", filter: `row => args.status == "open"`, row: updateRow(nil), declares: []string{}, want: triFalse},
		{name: "undeclared args.f == nil", filter: `row => args.status == nil`, row: newRow(map[string]any{"status": "open"}), declares: []string{}, want: triTrue},
		{name: "undeclared args.f under !", filter: `row => !(args.status == "done")`, row: newRow(map[string]any{"status": "done"}), declares: []string{}, want: triTrue},
		{name: "undeclared args.f under ??", filter: `row => (args.status ?? "none") == "none"`, row: newRow(map[string]any{"status": "done"}), declares: []string{}, want: triTrue},
		{name: "undeclared firstVersion != true", filter: `row => args.firstVersion != true`, row: newRow(nil), declares: []string{}, want: triTrue},
		{name: "undeclared firstVersion == true never holds", filter: `row => args.firstVersion == true`, row: newRow(nil), declares: []string{}, want: triFalse},
		{name: "undeclared args.concept", filter: `row => args.concept == "v1:t:thing"`, row: newRow(nil), declares: []string{}, want: triFalse},
		{name: "a declared field beside an undeclared one", filter: `row => args.kind == "a" && args.status != "done"`, row: newRow(map[string]any{"kind": "a", "status": "done"}), declares: []string{"kind"}, want: triTrue},
		{name: "an undeclared field beside a declared unknown one", filter: `row => args.kind == "a" || args.status == "done"`, row: updateRow(map[string]any{"status": "done"}), declares: []string{"kind"}, want: triUnknown, why: "kind"},

		// Reads through an object.
		{name: "through a set object", filter: `row => row.meta.kind == "a"`, row: newRow(map[string]any{"meta": map[string]any{"kind": "a"}}), want: triTrue},
		{name: "through an absent object", filter: `row => row.?meta.kind == "a"`, row: newRow(nil), want: triFalse},

		// Anything else is unknown, named.
		{name: "an intrinsic other than concept", filter: `row => row.id == "x"`, row: newRow(nil), want: triUnknown, why: "id"},
		{name: "a predicate call", filter: `row => isActiveRecord(row)`, row: newRow(nil), want: triUnknown, why: "isActiveRecord(row)"},
		{name: "a method call", filter: `row => row.tags.count() > 0`, row: newRow(nil), want: triUnknown, why: "row.tags.count()"},
		{name: "a ternary", filter: `row => row.a == 1 ? true : false`, row: newRow(nil), want: triUnknown},
		{name: "arithmetic", filter: `row => row.n + 1 == 2`, row: newRow(map[string]any{"n": int64(1)}), want: triUnknown},
		{name: "the actor", filter: `row => actor.isClusterOwner == true`, row: newRow(nil), want: triUnknown, why: "actor.isClusterOwner"},
		{name: "a call decided false beside it", filter: `row => row.status == "done" && isActiveRecord(row)`, row: newRow(map[string]any{"status": "open"}), want: triFalse},
	}
}

func mustLambda(t *testing.T, src string) *ast.LambdaExpr {
	t.Helper()
	lam, err := languageParser.ParseV1Lambda(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return lam
}

func TestDecideFilter(t *testing.T) {
	for _, tc := range filterCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, why := decideFilter(mustLambda(t, tc.filter), tc.row, declaredArgsOf(t, tc))
			if got != tc.want {
				t.Fatalf("%s over %+v (declaring %v) = %s (%s), want %s", tc.filter, tc.row, tc.declares, got, why, tc.want)
			}
			if got == triUnknown && !strings.Contains(why, tc.why) {
				t.Errorf("the reason %q does not name %q", why, tc.why)
			}
			if got == triUnknown && why == "" {
				t.Error("an unknown answer carries no reason")
			}
		})
	}
}

// A nil filter holds for every write: the automation has no @filter.
func TestDecideFilterNoFilterHolds(t *testing.T) {
	if got, _ := decideFilter(nil, updateRow(nil), nil); got != triTrue {
		t.Fatalf("no filter = %s, want true", got)
	}
}

// The reason an unknown field gives is the row's own explanation of it, which
// is what lets an undecided edge say what the write does to that field.
func TestDecideFilterAsksTheRowWhy(t *testing.T) {
	row := updateRow(nil)
	row.explain = func(field string) string { return "advanceThing leaves " + field + " as stored" }
	_, why := decideFilter(mustLambda(t, `row => row.status == "open"`), row, nil)
	if why != "advanceThing leaves status as stored" {
		t.Fatalf("reason = %q", why)
	}
}

// completionValues are the values an unknown field is filled with when a
// decided answer is checked against the runtime.
var completionValues = []any{absentCompletion{}, nil, "", "  ", "open", "done", "ab", int64(1), 2.5, true, false}

// absentCompletion marks a field left out of the completed row.
type absentCompletion struct{}

// gateVariant is one way the soundness gate judges a case: the event the
// write publishes, and the args block of the automation judged.
type gateVariant struct {
	name string
	// updated judges the graph.node.updated event an update publishes,
	// which carries no firstVersion of its own; else graph.node.created.
	updated bool
	// block is the judged automation's args block: "case" declares what the
	// case declares, "unrelated" one field the filter never reads, "none"
	// no block at all -- the two ways a field ends up undeclared.
	block string
}

var gateVariants = []gateVariant{
	{name: "created", block: "case"},
	{name: "updated", updated: true, block: "case"},
	{name: "created, an unrelated args block", block: "unrelated"},
	{name: "created, no args block", block: "none"},
	{name: "updated, no args block", updated: true, block: "none"},
}

// TestDecideFilterAgreesWithTheRuntime is the soundness gate. For every case
// and every variant the graph DECIDES, each field the filter reads that the
// row does not know is filled with every value of completionValues -- one
// field at a time and all together -- and the completed write is turned into
// the graph event executeWrite publishes, which the runtime's own filter
// (evaluateTriggerFilter, the path the scheduler fires through) must answer
// the same way. The variants hold the judged automation's args block to the
// runtime too: a field it does not declare is not bound, and neither is any
// with no block.
func TestDecideFilterAgreesWithTheRuntime(t *testing.T) {
	checked := map[string]int{}
	for _, tc := range filterCases() {
		lam := mustLambda(t, tc.filter)
		for _, v := range gateVariants {
			declares := declaredArgsOf(t, tc)
			switch v.block {
			case "unrelated":
				declares = map[string]bool{"unrelatedArg": true}
			case "none":
				declares = nil
			}
			row := tc.row
			if v.updated {
				// What writeRow says of the updated topic: the event carries
				// no firstVersion of its own.
				row.FirstVersion = triUnknown
			}
			got, _ := decideFilter(lam, row, declares)
			if got == triUnknown {
				continue
			}
			for _, completion := range completions(unknownFieldsRead(lam, row)) {
				ev, a := completedEvent(t, tc, row, completion, v.updated, declares)
				bound, _, err := bindEventArgs(a, ev)
				if err != nil {
					t.Fatalf("%s (%s): bind: %v", tc.name, v.name, err)
				}
				fires, err := evaluateTriggerFilter(a, ev, bound)
				if err != nil {
					// A filter the runtime refuses does not fire.
					fires = false
				}
				if fires != (got == triTrue) {
					t.Errorf("%s (%s): decideFilter says %s, the runtime says %v for the completion %v", tc.name, v.name, got, fires, completion)
				}
				checked[v.name]++
			}
		}
	}
	for _, v := range gateVariants {
		if checked[v.name] < 50 {
			t.Fatalf("checked %d completions for %q -- the gate went blind: %v", checked[v.name], v.name, checked)
		}
	}
	t.Logf("completions checked against the runtime, per variant: %v", checked)
}

// unknownFieldsRead is every top-level field the filter reads, through its
// parameter or args, that the row neither knows nor knows absent.
func unknownFieldsRead(lam *ast.LambdaExpr, row knownRow) []string {
	set := map[string]bool{}
	ast.MemberPaths(lam.Body, func(root string, fields []string) {
		if (root != lam.Params[0] && root != "args") || len(fields) == 0 {
			return
		}
		f := fields[0]
		if _, ok := row.Fields[f]; ok || row.Absent[f] || row.OthersKnown {
			return
		}
		set[f] = true
	})
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// completions fills each field with each value alone, and every field with
// each value at once; a row with no unknown field has the one completion.
func completions(fields []string) []map[string]any {
	out := []map[string]any{{}}
	for _, v := range completionValues {
		all := map[string]any{}
		for _, f := range fields {
			out = append(out, map[string]any{f: v})
			all[f] = v
		}
		if len(fields) > 1 {
			out = append(out, all)
		}
	}
	return out
}

// completedEvent is the event executeWrite publishes for row with completion
// filled in -- graph.node.created, carrying the write's firstVersion after
// the flattened payload, or graph.node.updated, carrying none -- and an
// automation on it whose args block declares exactly declares (none, for
// nil).
func completedEvent(t *testing.T, tc filterCase, row knownRow, completion map[string]any, updated bool, declares map[string]bool) (*events.Event, *Automation) {
	t.Helper()
	stored := map[string]any{}
	for k, v := range row.Fields {
		stored[k] = v
	}
	for k, v := range completion {
		if _, isAbsent := v.(absentCompletion); isAbsent {
			continue
		}
		stored[k] = v
	}
	payload := map[string]any{"id": "x-1", "nodeId": "x-1", "concept": row.Concept, "nodeType": "instance"}
	for k, v := range stored {
		payload[k] = v
	}
	payload["payload"] = stored
	base := events.TopicGraphNodeUpdated
	if !updated {
		base = events.TopicGraphNodeCreated
		switch row.FirstVersion {
		case triTrue:
			payload["firstVersion"] = true
		case triFalse:
			payload["firstVersion"] = false
		}
	}
	topic := events.BuildTopicWithConcept(base, row.Concept)
	ev := &events.Event{Topic: topic, Payload: payload}

	a := &Automation{Name: "probe", Trigger: &TriggerConfig{Event: topic, Filter: tc.filter}}
	if len(declares) > 0 {
		a.Args = &ArgsSchema{}
		names := make([]string, 0, len(declares))
		for n := range declares {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a.Args.Fields = append(a.Args.Fields, &ArgsField{Name: n, Type: "any", Optional: true})
		}
	}
	if err := PrepareExpressions(a); err != nil {
		t.Fatalf("%s: prepare: %v", tc.name, err)
	}
	return ev, a
}

func (absentCompletion) String() string { return "<absent>" }

var _ fmt.Stringer = absentCompletion{}
