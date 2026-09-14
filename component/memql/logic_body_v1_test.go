package memql

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/functions"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// logic_body_v1_test.go -- the logic-body bridge (logic_body_v1.go): what a v1
// body loads into, what load refuses, and the tree migrated by the codemod
// loading every logic construct with the edition-2026 grammar. The run-time
// equivalence of the two grammars over the same tree is
// component/automations/steps' logic_v1_corpus_test.go, which runs what these
// build on the LogicRunner.

// loadLogicV1 builds one logic construct from source with the edition-2026
// grammar, through the loader's own entry point.
func loadLogicV1(t *testing.T, name, src string) (*Function, error) {
	t.Helper()
	saved := languageParser.DefaultOptions
	languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: true}
	defer func() { languageParser.DefaultOptions = saved }()
	return BuildFunctionConstruct(src, name, "unified:probe/logic.memql", memorynodes.DefaultRegistry())
}

// TestLogicBodyV1Routing: the three shapes of a v1 body, and the runner each
// lands on. A statement before the return, or a return of an expression,
// runs on the LogicRunner; a return of a construct call runs through fn.Expr
// as the call node the legacy conversion produces, its expression arguments
// carried as leaves for argument expansion.
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
	require.True(t, multi.LogicSteps.ExpressionsV1)

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

// corpusLogic loads every logic construct of the tree, migrated by the codemod
// or as it is, with the matching grammar, through the loader's entry point,
// keyed by path and name. It fails the test on any construct that does not
// load.
func corpusLogic(t *testing.T, migrated bool) map[string]*Function {
	t.Helper()
	files := corpusFiles(t, migrated)

	saved := languageParser.DefaultOptions
	languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: migrated}
	defer func() { languageParser.DefaultOptions = saved }()

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
	require.Emptyf(t, failures, "%d logic constructs do not load (migrated=%v):\n%s", len(failures), migrated, strings.Join(failures, "\n"))
	return out
}

// TestV1CorpusLogicBodiesBuild: every logic construct of the tree, migrated by
// the codemod, loads with the edition-2026 grammar -- the same set that loads
// today -- and lands on the runner its legacy build lands on, except that a
// one-`return` expression moves to the LogicRunner (see logic_body_v1.go). A
// return of a construct call keeps the legacy call node's name and argument
// names, which is what makes its dispatch the legacy dispatch.
func TestV1CorpusLogicBodiesBuild(t *testing.T) {
	_, err := LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	legacy := corpusLogic(t, false)
	v1 := corpusLogic(t, true)
	require.Greater(t, len(legacy), 30, "the tree has dozens of logic constructs; a small count means the walk went blind")
	require.Equal(t, len(legacy), len(v1), "the migrated tree loads the same logic constructs")

	keys := make([]string, 0, len(legacy))
	for k := range legacy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	calls, onRunner := 0, 0
	for _, key := range keys {
		l, n := legacy[key], v1[key]
		require.NotNilf(t, n, "%s loads today and not after the migration", key)
		if lc, ok := l.Expr.(*FunctionCallExpression); ok && l.LogicSteps == nil && isConstructName(lc.Name) {
			nc, ok := n.Expr.(*FunctionCallExpression)
			require.Truef(t, ok, "%s: a return of a construct call stays a call node, got %T", key, n.Expr)
			require.Nilf(t, n.LogicSteps, "%s: a return of a construct call dispatches through fn.Expr", key)
			require.Equalf(t, lc.Name, nc.Name, "%s: the call names the same construct", key)
			require.ElementsMatchf(t, mapKeys(lc.Args), mapKeys(nc.Args), "%s: the call passes the same arguments", key)
			calls++
			continue
		}
		require.NotNilf(t, n.LogicSteps, "%s runs on the LogicRunner", key)
		require.Truef(t, n.LogicSteps.ExpressionsV1, "%s: the body the runner receives is the v1 body", key)
		onRunner++
	}
	t.Logf("%d logic constructs: %d return a construct call through fn.Expr, %d run on the LogicRunner", len(keys), calls, onRunner)
}

// isConstructName reports whether a legacy call node names a construct rather
// than one of the in-process builtins the legacy conversion also spells as a
// call (cond, coalesce, concat, the date and string builtins).
func isConstructName(name string) bool {
	if _, catalog := functions.Lookup(name); catalog {
		return false
	}
	_, retired := functions.RetiredFunctions()[name]
	return !retired
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
