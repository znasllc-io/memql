package sense

import (
	"fmt"
	"testing"
)

func probeReg() *stubRegistry {
	return &stubRegistry{
		functions: map[string]*FunctionInfo{
			"listOrders": {Name: "listOrders", Kind: "query"},
			"writeThing": {Name: "writeThing", Kind: "mutation"},
			"dayRollup":  {Name: "dayRollup", Kind: "logic"},
		},
		concepts: map[string]*ConceptInfo{
			"v1:demo:thing": {Name: "v1:demo:thing", Fields: []FieldInfo{
				{Name: "title", Type: "text"},
			}},
		},
	}
}

// helper: classify at end of src
func classifyEnd(src string) CursorContext {
	lines := splitLines(src)
	last := lines[len(lines)-1]
	return analyzeCursorContext(src, len(lines), len(last)+1)
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	out = append(out, cur)
	return out
}

func kindName(k ContextKind) string {
	switch k {
	case ContextTopLevel:
		return "TopLevel"
	case ContextFuncBody:
		return "FuncBody"
	case ContextConceptDef:
		return "ConceptDef"
	case ContextConstructConcept:
		return "ConstructConcept"
	case ContextInvocation:
		return "Invocation"
	case ContextFuncCallArgs:
		return "FuncCallArgs"
	case ContextFieldAccess:
		return "FieldAccess"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

func TestProbe_Scenarios(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"args-field-named-query", "mutate Thing doThing {\n  args {\n    query "},
		{"args-field-named-logic", "mutate Thing doThing {\n  args {\n    logic "},
		{"concept-field-named-query", "concept Thing {\n  query "},
		{"concept-field-named-mutation", "concept Thing {\n  mutation "},
		{"funccall-arg-query", "logic doThing {\n  x = foo(query "},
		{"query-body-field-named-logic", "query Thing listThings {\n  filter {\n    logic "},
		{"legit-logic-invocation", "logic doThing {\n  query "},
		{"cond-var-query", "logic doThing {\n  cond(query "},
	}
	s := New(probeReg())
	for _, c := range cases {
		ctx := classifyEnd(c.src)
		items := s.Complete(c.src, len(splitLines(c.src)), len(splitLines(c.src)[len(splitLines(c.src))-1])+1, "x/f.memql")
		var labels []string
		for _, it := range items {
			labels = append(labels, it.Label)
		}
		t.Logf("[%s] ctx=%s prefix=%q constructKey=%q  -> completions=%v",
			c.name, kindName(ctx.Kind), ctx.Prefix, ctx.ConstructKey, labels)
	}
}

// Compare: args block field-TYPE completion when the field name is ordinary
// vs when it is `query`. If ordinary offers types but `query` offers query
// functions, that is the suppression regression.
func TestProbe_ArgsFieldTypeSuppression(t *testing.T) {
	s := New(probeReg())
	ordinary := "mutate Thing doThing {\n  args {\n    reason "
	shadowed := "mutate Thing doThing {\n  args {\n    query "
	for _, src := range []string{ordinary, shadowed} {
		lines := splitLines(src)
		items := s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "x/f.memql")
		var labels []string
		for _, it := range items {
			labels = append(labels, it.Label+"/"+it.Detail)
		}
		t.Logf("src=%q\n   completions=%v", src, labels)
	}
}
