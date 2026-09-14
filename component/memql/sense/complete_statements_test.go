package sense

import (
	"strings"
	"testing"
)

// complete_statements_test.go -- completion and hover in a body written in
// statements (edition 2026, epic memql#5370).

// statementRegistry registers one function of each kind a statement calls.
type statementRegistry struct{ fakeRegistry }

func (statementRegistry) FunctionNames() []string {
	return []string{"activeUsers", "saveThing", "decideThing", "sweepAll"}
}

func (statementRegistry) FunctionGet(name string) (*FunctionInfo, bool) {
	kinds := map[string]string{"activeUsers": "query", "saveThing": "mutation", "decideThing": "logic", "sweepAll": "automation"}
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

func TestStatementAutomationLineStart(t *testing.T) {
	items := statementCompletions(New(&statementRegistry{}), statementAutomationSrc)
	got := labelsOfItems(items)
	for _, want := range []string{"if", "for", "switch", "parallel", "publish", "return", "retry", "rows", "row", "args", "event", "query", "mutation"} {
		if !got[want] {
			t.Errorf("a statement automation's line start does not offer %q", want)
		}
	}
	for _, absent := range []string{"body", "range", "continue", "break", "when", "limit"} {
		if got[absent] {
			t.Errorf("a statement automation's line start offers %q, which a statement body refuses", absent)
		}
	}
	// A construct is offered by its kind: the call is written with it.
	if it, ok := itemByLabel(items, "saveThing"); !ok || it.InsertText != "mutation saveThing(" {
		t.Errorf("saveThing inserts %q, want the call with its kind", it.InsertText)
	}
	if it, ok := itemByLabel(items, "sweepAll"); !ok || it.InsertText != "automation sweepAll(" {
		t.Errorf("sweepAll inserts %q, want the call with its kind", it.InsertText)
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

func TestStatementLogicLineStart(t *testing.T) {
	src := "logic decides {\n  args {\n    n any\n  }\n  doubled := args.n * 2\n  "
	items := statementCompletions(New(&statementRegistry{}), src)
	got := labelsOfItems(items)
	for _, want := range []string{"return", "if", "for", "doubled", "args"} {
		if !got[want] {
			t.Errorf("a statement logic's line start does not offer %q", want)
		}
	}
	for _, absent := range []string{"body", "publish", "range", "continue", "break", "n"} {
		if got[absent] {
			t.Errorf("a statement logic's line start offers %q", absent)
		}
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
