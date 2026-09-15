package sense

import (
	"sort"
	"strings"
	"testing"
)

// complete_statements_test.go -- completion and hover in a body written in
// statements (edition 2026, epic memql#5370).

// statementRegistry registers one function of each kind a statement calls.
type statementRegistry struct{ fakeRegistry }

func (statementRegistry) FunctionNames() []string {
	return []string{"activeUsers", "saveThing", "decideThing", "sweepAll", "notify"}
}

func (statementRegistry) FunctionGet(name string) (*FunctionInfo, bool) {
	kinds := map[string]string{"activeUsers": "query", "saveThing": "mutation", "decideThing": "logic", "sweepAll": "automation", "notify": "action"}
	if k, ok := kinds[name]; ok {
		return &FunctionInfo{Name: name, Kind: k}, true
	}
	return nil, false
}

// statementCompletions completes at the end of src.
func statementCompletions(s *Service, src string) []CompletionItem {
	lines := strings.Split(src, "\n")
	return s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "x/automations.memql")
}

func itemByLabel(items []CompletionItem, label string) (CompletionItem, bool) {
	for _, it := range items {
		if it.Label == label {
			return it, true
		}
	}
	return CompletionItem{}, false
}

// sortedLabels is the labels of items, sorted.
func sortedLabels(items []CompletionItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Label)
	}
	sort.Strings(out)
	return out
}

const statementAutomationSrc = `@trigger(event="x.y")
automation sweeps {
  args {
    limit any
  }
  rows := query activeUsers()
  for row in rows {
    mutation saveThing(id: row.id)
  }
  `

// What starts a statement is what a statement starts with: a keyword that
// opens one, or a construct call's kind. A root, a name bound above or a
// function at a line's start is refused, so none is offered there -- and
// neither is a clause that trails a statement, or a block that comes first.
func TestStatementAutomationLineStart(t *testing.T) {
	items := statementCompletions(New(&statementRegistry{}), statementAutomationSrc)
	got := labelsOfItems(items)
	for _, want := range []string{"if", "for", "switch", "parallel", "publish", "return", "query", "mutation", "logic", "builtin", "automation", "action"} {
		if !got[want] {
			t.Errorf("a statement automation's line start does not offer %q", want)
		}
	}
	for _, absent := range []string{"args", "precondition", "body", "step", "else", "case", "default", "branch", "retry", "wait", "on", "range", "when", "rows", "row", "event", "limit", "now"} {
		if got[absent] {
			t.Errorf("a statement automation's line start offers %q, which cannot start a statement there", absent)
		}
	}
	for _, it := range items {
		if it.Kind == "builtin" {
			t.Errorf("a statement's start offers the function %q: `%s(...)` there names no construct kind", it.Label, it.Label)
		}
	}
	// A construct is offered by its kind: the call is written with it.
	for name, insert := range map[string]string{"saveThing": "mutation saveThing(", "sweepAll": "automation sweepAll(", "notify": "action notify("} {
		if it, ok := itemByLabel(items, name); !ok || it.InsertText != insert {
			t.Errorf("%s inserts %q, want the call with its kind, %q", name, it.InsertText, insert)
		}
	}
}

func TestStatementAutomationExpressionReadsArgsDotted(t *testing.T) {
	// Inside a statement the expression completer answers; an argument is read
	// `args.x` there, and the bare name a legacy body resolved is refused.
	got := labelsOfItems(statementCompletions(New(&statementRegistry{}), statementAutomationSrc+"total := "))
	if got["limit"] {
		t.Error("a statement automation's expression offers the bare argument `limit`, which it refuses")
	}
	if !got["args"] || !got["rows"] {
		t.Errorf("a statement automation's expression does not offer args and the names bound above: %v", got)
	}
}

// An automation's return value is an expression no expression position of the
// legacy body claimed: it is completed as a step argument is.
func TestStatementAutomationReturnIsAnExpression(t *testing.T) {
	got := labelsOfItems(statementCompletions(New(&statementRegistry{}), statementAutomationSrc+"return "))
	if !got["args"] || !got["rows"] || got["if"] || got["limit"] {
		t.Errorf("an automation's return value must offer args and the names bound above, and no statement or bare argument: %v", got)
	}
}

func TestStatementLogicLineStart(t *testing.T) {
	src := "logic decides {\n  args {\n    n any\n  }\n  doubled := args.n * 2\n  "
	items := statementCompletions(New(&statementRegistry{}), src)
	got := labelsOfItems(items)
	for _, want := range []string{"return", "if", "for", "switch", "parallel", "query", "mutation", "logic", "builtin", "activeUsers", "decideThing"} {
		if !got[want] {
			t.Errorf("a statement logic's line start does not offer %q", want)
		}
	}
	// A logic may not publish or call an automation or an action (D14).
	for _, absent := range []string{"publish", "automation", "action", "sweepAll", "notify", "body", "args", "doubled", "n", "range", "else"} {
		if got[absent] {
			t.Errorf("a statement logic's line start offers %q", absent)
		}
	}
	if it, ok := itemByLabel(items, "decideThing"); !ok || it.InsertText != "logic decideThing(" {
		t.Errorf("decideThing inserts %q, want the call with its kind", it.InsertText)
	}
}

// The construct's blocks come before its first statement: args first, then
// an automation's named preconditions.
func TestStatementBlocksComeFirst(t *testing.T) {
	s := New(&statementRegistry{})
	const auto = "@trigger(event=\"x.y\")\nautomation a {\n  "
	cases := []struct {
		name, src    string
		want, absent []string
	}{
		{"a fresh automation", auto, []string{"args", "args { ... }", "precondition", "precondition <name> { ... }"}, nil},
		{"after the args block", auto + "args {\n    x any\n  }\n  ", []string{"precondition", "precondition <name> { ... }"}, []string{"args"}},
		{"after a precondition", auto + "precondition ready {\n    check: 1 == 1\n  }\n  ", []string{"precondition"}, []string{"args"}},
		{"after a statement", auto + "rows := query activeUsers()\n  ", nil, []string{"args", "precondition"}},
		{"a fresh logic", "logic l {\n  ", []string{"args", "args { ... }"}, []string{"precondition", "body"}},
		{"inside a statement's block", auto + "if 1 == 1 {\n    ", nil, []string{"args", "precondition"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items := statementCompletions(s, c.src)
			got := labelsOfItems(items)
			for _, w := range c.want {
				if !got[w] {
					t.Errorf("does not offer %q: %v", w, sortedLabels(items))
				}
			}
			for _, a := range c.absent {
				if got[a] {
					t.Errorf("offers %q: %v", a, sortedLabels(items))
				}
			}
			if it, ok := itemByLabel(items, "precondition <name> { ... }"); ok && it.InsertText != "precondition ${1:name} {\n\t$0\n}" {
				t.Errorf("the precondition snippet inserts %q: a precondition is named", it.InsertText)
			}
		})
	}
}

// A statement's block holds statements, a switch's holds its cases and a
// parallel's its branches; a branch cannot return; a map's braces hold keys.
func TestStatementStartsInsideBlocks(t *testing.T) {
	s := New(&statementRegistry{})
	const auto = "@trigger(event=\"x.y\")\nautomation a {\n  rows := query activeUsers()\n  "
	cases := []struct {
		name, src    string
		want, absent []string
		only         bool // want is the whole set
	}{
		{"a for's body", auto + "for row in rows {\n    ", []string{"if", "publish", "return", "mutation"}, []string{"step", "body", "args", "range"}, false},
		{"an else's body", auto + "if 1 == 1 {\n    builtin a()\n  } else {\n    ", []string{"if", "return"}, []string{"step"}, false},
		{"a switch", auto + "switch rows.count() {\n    ", []string{"case", "default"}, nil, true},
		{"a case's body", auto + "switch rows.count() {\n    case 1 {\n      ", []string{"if", "return", "mutation"}, []string{"case", "default"}, false},
		{"a parallel", auto + "parallel {\n    ", []string{"branch"}, nil, true},
		{"a branch's body", auto + "parallel {\n    branch one {\n      ", []string{"if", "mutation"}, []string{"return", "branch"}, false},
		{"a statement's block inside a branch", auto + "parallel {\n    branch one {\n      if 1 == 1 {\n        ", []string{"if"}, []string{"return"}, false},
		{"a publish payload", auto + "publish \"x.y\" {\n    ", nil, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items := statementCompletions(s, c.src)
			got := labelsOfItems(items)
			if c.only {
				want := append([]string(nil), c.want...)
				sort.Strings(want)
				if g := sortedLabels(items); strings.Join(g, ",") != strings.Join(want, ",") {
					t.Fatalf("offers %v, want exactly %v", g, want)
				}
				return
			}
			for _, w := range c.want {
				if !got[w] {
					t.Errorf("does not offer %q: %v", w, sortedLabels(items))
				}
			}
			for _, a := range c.absent {
				if got[a] {
					t.Errorf("offers %q: %v", a, sortedLabels(items))
				}
			}
		})
	}
}

// After a statement, on its line, what may follow it -- in the parser's order.
func TestStatementTrailingClauses(t *testing.T) {
	s := New(&statementRegistry{})
	const auto = "@trigger(event=\"x.y\")\nautomation a {\n  rows := query activeUsers()\n  "
	const branches = "parallel {\n    branch one {\n      mutation saveThing(id: 1)\n    }\n  } "
	cases := []struct {
		name, src string
		want      []string
	}{
		{"an if's block", "if 1 == 1 {\n    mutation saveThing(id: 1)\n  } ", []string{"else"}},
		{"an else", "if 1 == 1 {\n    mutation saveThing(id: 1)\n  } else ", []string{"if"}},
		{"an else-if's block", "if 1 == 1 {\n    builtin a()\n  } else if 2 == 2 {\n    builtin b()\n  } ", []string{"else"}},
		{"a for's block", "for row in rows {\n    mutation saveThing(id: row.id)\n  } ", []string{"on"}},
		{"a for's on", "for row in rows {\n    mutation saveThing(id: row.id)\n  } on ", []string{"error"}},
		{"a for's on error", "for row in rows {\n    mutation saveThing(id: row.id)\n  } on error ", []string{"continue"}},
		{"a parallel's block", branches, []string{"on", "wait"}},
		{"a parallel's wait", branches + "wait ", []string{"any"}},
		{"a parallel's wait any", branches + "wait any ", []string{"on"}},
		{"a switch's block", "switch rows.count() {\n    case 1 {\n      builtin a()\n    }\n  } ", nil},
		{"a call", "mutation saveThing(id: 1) ", []string{"on", "retry"}},
		{"a bound call", "x := query activeUsers() ", []string{"on", "retry"}},
		{"a retried call", "mutation saveThing(id: 1) retry(2) ", []string{"on"}},
		{"a call's on", "x := query activeUsers() on ", []string{"error"}},
		{"an action", "action notify(to: \"a\") ", []string{"on", "retry"}},
		{"an action's on", "action notify(to: \"a\") on ", []string{"error", "surface"}},
		{"a retried action's on", "action notify(to: \"a\") retry(1) on ", []string{"error"}},
		{"an action on a surface", "action notify(to: \"a\") on surface(\"desk\") ", []string{"on", "retry"}},
		{"a returned call", "return query activeUsers() ", []string{"retry"}},
		{"a call's on error continue", "mutation saveThing(id: 1) on error continue ", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sortedLabels(statementCompletions(s, auto+c.src))
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("offers %v, want %v", got, c.want)
			}
		})
	}
}

// A line inside an open parenthesis, or continuing the expression the line
// above ends in, is not where a statement starts: the expression completer
// answers there.
func TestStatementContinuationIsAnExpression(t *testing.T) {
	s := New(&statementRegistry{})
	cases := map[string]string{
		"a logic's continued condition": "logic l {\n  args {\n    a any\n  }\n  ok := args.a &&\n    ",
		"a call's argument list":        "@trigger(event=\"x.y\")\nautomation a {\n  mutation saveThing(\n    ",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			got := labelsOfItems(statementCompletions(s, src))
			if got["if"] || got["return"] || got["mutation"] {
				t.Errorf("a continued expression offers statement starts: %v", got)
			}
			if !got["args"] {
				t.Errorf("a continued expression does not offer the roots: %v", got)
			}
		})
	}
}

// A body still written in the retired forms keeps its completion until the
// flip: an automation with a step block still resolves its args bare (G2).
func TestLegacyAutomationBodyKeepsItsCompletion(t *testing.T) {
	items := New(nil).Complete(argsAutomationSrc, 8, 30, "x/automations.memql")
	if _, ok := itemByLabel(items, "environment"); !ok {
		t.Fatal("a legacy automation lost the bare args field it resolves")
	}
}

func TestHoverOnAStatementKeywordShowsTheStatement(t *testing.T) {
	src := "logic loops {\n  args {\n    items any\n  }\n  for item in args.items {\n    return item\n  }\n  return 0\n}"
	h := New(&stubRegistry{}).Hover(src, 5, 3, "x/logic.memql")
	if h == nil {
		t.Fatal("no hover on `for`")
	}
	if strings.Contains(h.Contents, ":= range") || !strings.Contains(h.Contents, "for item in <source>") {
		t.Errorf("hover on `for` does not show the statement: %s", h.Contents)
	}
}
