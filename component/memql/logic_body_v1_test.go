package memql

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// logic_body_v1_test.go -- the logic-body bridge (logic_body_v1.go): what a
// body loads into, what load refuses, and every logic construct of the tree
// loading. What they do at run time is component/automations/steps'
// logic_v1_corpus_test.go, which runs what these build on the LogicRunner
// against its goldens.

// loadLogicV1 builds one logic construct from source, through the loader's
// own entry point.
func loadLogicV1(t *testing.T, name, src string) (*Function, error) {
	t.Helper()
	return BuildFunctionConstruct(src, name, "unified:probe/logic.memql", memorynodes.DefaultRegistry())
}

// TestLogicBodyV1Routing: the three shapes of a v1 body, and the runner each
// lands on. A statement before the return, or a return of an expression,
// runs on the LogicRunner; a return of a construct call runs through fn.Expr
// as a call node, its expression arguments carried as leaves for argument
// expansion.
func TestLogicBodyV1Routing(t *testing.T) {
	multi, err := loadLogicV1(t, "multi", `logic multi {
  args {
    x string!
  }
  body {
    y := args.x ?? ""
    return y
  }
}`)
	require.NoError(t, err)
	require.NotNil(t, multi.LogicSteps, "a body with a statement before its return runs on the LogicRunner")

	pure, err := loadLogicV1(t, "pure", `logic pure {
  args {
    gate object!
  }
  body {
    return args.gate.passed ?? false
  }
}`)
	require.NoError(t, err)
	require.NotNil(t, pure.LogicSteps, "a return of an expression runs on the LogicRunner, which evaluates it with EvalExpr")
	leaf, ok := pure.Expr.(*PlanConstExpression)
	require.True(t, ok, "fn.Expr carries the expression as one leaf, got %T", pure.Expr)
	require.Equal(t, "args.gate.passed ?? false", formatPlanConstant(leaf))

	call, err := loadLogicV1(t, "call", `logic call {
  args {
    asOf string!
    bump string
  }
  body {
    return query expiredThings( asOf: args.asOf, bump: args.bump ?? "patch", limit: 5, tags: ["a", "b"], at: now )
  }
}`)
	require.NoError(t, err)
	require.Nil(t, call.LogicSteps, "a return of a construct call dispatches through fn.Expr, as it did")
	fc, ok := call.Expr.(*FunctionCallExpression)
	require.True(t, ok, "fn.Expr is the call node, got %T", call.Expr)
	require.Equal(t, "expiredThings", fc.Name)
	require.Equal(t, int64(5), fc.Args["limit"], "a literal argument is its value")
	require.Equal(t, []any{"a", "b"}, fc.Args["tags"])
	for _, k := range []string{"asOf", "bump", "at"} {
		_, isLeaf := fc.Args[k].(*PlanConstExpression)
		require.Truef(t, isLeaf, "argument %s is an expression, carried as a leaf", k)
	}
}

// TestLogicBodyV1Refusals: what neither runner can run is a load error, in
// the author's terms.
func TestLogicBodyV1Refusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"nested in a return", `return (query things()).count()`, "a construct call is a statement of its own"},
		{"nested in a statement", `n := (query things()).count()
    return n`, "statement \"n\" calls `query things()` inside an expression"},
		{"nested in an argument", `rows := query things( of: query other() )
    return rows`, "calls `query other()` inside an expression"},
		{"a kind-less return call", `return things(a: 1)`, "a construct call names its kind"},
		{"an automation return", `return automation things()`, "a logic body returns a value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadLogicV1(t, "refused", "logic refused {\n  body {\n    "+tc.body+"\n  }\n}")
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestLogicBodyV1ArgumentsExpand: argument expansion evaluates a leaf over the
// call's arguments and the ambient envelope, passes an absent one as not
// passed, and hands every other argument to the legacy fold unchanged -- on
// both paths that expand a logic's return call, the F.6 mutation-leaf hoist
// included.
func TestLogicBodyV1ArgumentsExpand(t *testing.T) {
	call, err := loadLogicV1(t, "call", `@actor
logic call {
  args {
    asOf string!
    bump string
    gone string
    event object
  }
  body {
    return query expiredThings( asOf: args.asOf, bump: args.bump ?? "patch", gone: args.gone, who: actor.userId, topic: event.topic, stamp: now, day: addDuration(args.asOf, "P1D") )
  }
}`)
	require.NoError(t, err)
	v := &functionValidator{ambient: map[string]any{
		"actor": map[string]any{"userId": "user-1"},
		"now":   "2026-09-14T12:00:00.5Z",
	}}
	callArgs := map[string]any{"asOf": "2026-09-01T00:00:00Z", "event": map[string]any{"topic": "t.x"}}
	expanded, err := v.substituteArgRefsAndCallArgs(cloneExpressionNode(call.Expr), callArgs)
	require.NoError(t, err)
	got := expanded.(*FunctionCallExpression).Args
	require.Equal(t, map[string]any{
		"asOf":  "2026-09-01T00:00:00Z",
		"bump":  "patch",
		"who":   "user-1",
		"topic": "t.x",
		"stamp": "2026-09-14T12:00:00.5Z",
		"day":   "2026-09-02T00:00:00Z",
	}, got, "every leaf evaluated, the absent `gone` not passed")

	_, err = v.substituteArgRefsAndCallArgs(&FunctionCallExpression{Name: "x", Args: map[string]any{
		"bad": &PlanConstExpression{Expr: mustParseV1(t, `undeclaredRoot.x`)},
	}}, callArgs)
	require.ErrorContains(t, err, `argument "bad" of mutation-leaf call "x"`)
}

func mustParseV1(t *testing.T, src string) languageParser.ExpressionNode {
	t.Helper()
	n, err := languageParser.ParseV1Expression(src)
	require.NoError(t, err)
	return n
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
			fn, err := dispatchPerConstructParser(slice, "unified:"+f.Path, memorynodes.DefaultRegistry())
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
// a statement body (fn.LogicBody): since the bodies flip (epic memql#5370)
// one runner runs every logic, whether its body is one `return` or many
// statements. What each of them does at run time is pinned construct by
// construct by component/automations/steps' TestLogicCorpusRuns, against its
// goldens.
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

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
