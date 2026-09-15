package memql

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// logic_body_load_test.go -- an edition-2026 logic body compiles at LOAD
// (epic memql#5370, task memql#5372): the function loader runs
// compiler.CompileBody over it, so every scope problem and every D14 construct
// rule refuses the load, and a clean body carries its compiled step list.
// What the bodies do at run time is component/automations/steps'
// logic_v1_corpus_test.go, which runs every logic of the tree on the
// LogicRunner against its goldens.

// loadLogicV1 builds one logic construct from source, through the loader's
// own entry point.
func loadLogicV1(t *testing.T, name, src string) (*Function, error) {
	t.Helper()
	return BuildFunctionConstruct(src, name, "unified:probe/logic.memql", memoryNodes.DefaultRegistry())
}

func TestNativeLogicCompilesAtLoad(t *testing.T) {
	fn, err := tryParseNewFunctionSyntax("routeStatus", "logic", `/// Route by role.
logic routeStatus {
  args {
    role any
  }
  r := args.role ?? ""
  return r == "owner" ? "queued" : "needs_validation"
}`, "forge.logic.memql", newMemoryRegistry(map[string]*memoryNodes.Concept{}))
	if err != nil {
		t.Fatalf("a clean statement body refused the load: %v", err)
	}
	if len(fn.LogicBody) != 2 || fn.LogicBody[0]["id"] != "r" || fn.LogicBody[1]["type"] != "return" {
		t.Fatalf("LogicBody = %v, want the binding and the return, in source order", fn.LogicBody)
	}
	if fn.Expr != nil {
		t.Fatalf("a statement body has no expression form: Expr %v", fn.Expr)
	}
	if clone := fn.clone(); len(clone.LogicBody) != 2 {
		t.Fatalf("the registry's clone dropped LogicBody: %v", clone.LogicBody)
	}
}

func TestNativeLogicProblemsRefuseTheLoad(t *testing.T) {
	cases := []struct {
		name, fn, src, want string
	}{
		{"forward reference", "early", `logic early {
  x := y
  y := 1
  return x
}`, "[body_forward_reference]"},
		{"publish", "announce", `logic announce {
  publish "things.happened" { at: now }
  return 1
}`, "[body_publish_in_logic]"},
		{"an automation call", "sweepAll", `logic sweepAll {
  automation sweep(limit: 10)
  return 1
}`, "[body_call_not_in_logic]"},
		{"no final return", "noReturn", `logic noReturn {
  x := 1
}`, "[body_logic_return]"},
		{"an automation returned", "sweepOne", `logic sweepOne {
  return automation sweep(limit: 1)
}`, "[body_call_not_in_logic]"},
		// A construct call is a statement of its own -- the runner journals
		// it -- so one nested in an expression names the fix: bind it first.
		{"a call nested in a return", "nestedReturn", `logic nestedReturn {
  return (query things()).count()
}`, "[body_call_in_expression]"},
		{"a call nested in a statement", "nestedStatement", `logic nestedStatement {
  n := (query things()).count()
  return n
}`, "[body_call_in_expression]"},
		{"a call nested in an argument", "nestedArgument", `logic nestedArgument {
  rows := query things(of: query other())
  return rows
}`, "[body_call_in_expression]"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := tryParseNewFunctionSyntax(c.fn, "logic", c.src, "x.logic.memql", newMemoryRegistry(map[string]*memoryNodes.Concept{}))
			if err == nil {
				t.Fatalf("the load accepted it; it should refuse with %s", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not carry %s", err, c.want)
			}
		})
	}
}

// corpusLogic loads every logic construct of the tree through the loader's
// entry point, keyed by path and name. It fails the test on any construct
// that does not load.
func corpusLogic(t *testing.T) map[string]*Function {
	t.Helper()
	files := corpusFiles(t, false) // the tree as it is

	out := map[string]*Function{}
	var failures []string
	for _, f := range files {
		for _, slice := range ExtractFunctionSlices(f.Content) {
			if slice.Kind != languageParser.FunctionTypeLogic {
				continue
			}
			fn, err := dispatchPerConstructParser(slice, "unified:"+f.Path, memoryNodes.DefaultRegistry())
			if err != nil {
				failures = append(failures, f.Path+" "+slice.Name+": "+err.Error())
				continue
			}
			out[f.Path+" "+slice.Name] = fn
		}
	}
	require.Emptyf(t, failures, "%d logic constructs do not load:\n%s", len(failures), strings.Join(failures, "\n"))
	return out
}

// TestV1CorpusLogicBodiesBuild: every logic construct of the tree loads, as
// a statement body (fn.LogicBody): one runner runs every logic, whether its
// body is one `return` or many statements. What each of them does at run time
// is pinned construct by construct by component/automations/steps'
// TestLogicCorpusRuns, against its goldens.
func TestV1CorpusLogicBodiesBuild(t *testing.T) {
	_, err := LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	v1 := corpusLogic(t)
	require.Greater(t, len(v1), 25, "the tree has dozens of logic constructs; a small count means the walk went blind")

	var off []string
	for key, n := range v1 {
		if n.LogicBody == nil {
			off = append(off, key)
		}
	}
	sort.Strings(off)
	require.Emptyf(t, off, "%d logic constructs do not load as a statement body:\n%s", len(off), strings.Join(off, "\n"))
	t.Logf("%d logic constructs, every one a statement body", len(v1))
}
