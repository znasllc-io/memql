package memql

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_lower_test.go -- Lower's mapping and its refusals (epic memql#5363,
// task memql#5366). Engine-free: each case parses a v1 lambda, lowers its body
// against a concept built from DSL source, and compares canonicalExpression of
// the IR -- the same rendering the result cache keys on, so a case pins what
// the executor will actually run.

// lowerTestConceptSource declares a field of every type the lowering types
// against: scalars, a list of scalars, an optional and a @required nested
// block, a map, and the three shapes whose keys the declaration does NOT close
// -- an untyped field, a bare object and an @open block.
const lowerTestConceptSource = `concept ticket {
  blob         any
  info         object
  extras       object @open {
    known  string
  }
  title        string
  status       string!
  email        string
  priority     int
  score        float
  urgent       bool
  tags         []string
  nums         []int
  dueAt        datetime
  ownerUserId  string
  overage      float
  reported     float
  labels       map[string]string
  lineage {
    planId  string
    runId   string!
  }
  @required
  settings {
    mode  string
  }
}`

func lowerTestConcept(t *testing.T) *memoryNodes.Concept {
	t.Helper()
	file, err := languageParser.ParseFile(lowerTestConceptSource)
	require.NoError(t, err)
	var decl *languageParser.ConceptDecl
	for _, d := range file.Definitions {
		if cd, ok := d.(*languageParser.ConceptDecl); ok {
			decl = cd
		}
	}
	require.NotNil(t, decl)
	c, err := memoryNodes.BuildConceptFromDecl(decl, "v1:lowertest:ticket")
	require.NoError(t, err)
	return c
}

// lowerTestSpecs is the predicate registry the kind checks read.
func lowerTestSpecs() func(string) (*Spec, bool) {
	specs := map[string]*Spec{
		"isOpen":         {Name: "isOpen", Kind: SpecKindRow},
		"isActiveRecord": {Name: "isActiveRecord", Kind: SpecKindRow, IsTrait: true},
		"isAdmin":        {Name: "isAdmin", Kind: SpecKindContext},
	}
	return func(name string) (*Spec, bool) {
		s, ok := specs[name]
		return s, ok
	}
}

func lowerTestEnv(t *testing.T) LowerEnv {
	return LowerEnv{
		Position: tiers.PositionQueryFilter,
		Concept:  lowerTestConcept(t),
		Args: map[string]ArgType{
			"status": "string", "list": "array", "flag": "bool", "n": "int", "email": "string",
		},
		Predicate: lowerTestSpecs(),
	}
}

// lowerSource parses a v1 lambda and lowers its body against env.
func lowerSource(t *testing.T, env LowerEnv, src string) (ExpressionNode, error) {
	t.Helper()
	lam, err := languageParser.ParseV1Lambda(src)
	require.NoError(t, err, "parse %s", src)
	require.Len(t, lam.Params, 1)
	env.Param = lam.Params[0]
	return Lower(lam.Body, env)
}

func TestLower_MapsEveryV1ShapeToTheIR(t *testing.T) {
	env := lowerTestEnv(t)
	cases := []struct{ src, want string }{
		// Comparisons, both orientations.
		{`row => row.status == "open"`, `payload.status=="open"`},
		{`row => "open" == row.status`, `payload.status=="open"`},
		{`row => row.priority > 3`, `payload.priority>3`},
		{`row => 3 < row.priority`, `payload.priority>3`},
		// Arguments and the actor, as today's IR.
		{`row => row.status == args.status`, `payload.status==&{status}`},
		{`row => row.ownerUserId == actor.userId`, `payload.owneruserid==&{userId}`},
		// Intrinsics are columns.
		{`row => row.id == args.status`, `id==&{status}`},
		{`row => row.provenance.kind == "automation"`, `provenance.kind=="automation"`},
		// Row-independent values are plan constants.
		{`row => row.createdAt < now`, `createdAt<planConst(now)`},
		{`row => row.dueAt < addDuration(now, "P1D")`, `payload.dueat<planConst(addDuration(now, "P1D"))`},
		{`row => row.status == lower(args.email)`, `payload.status==planConst(lower(args.email))`},
		{`row => row.status in [args.status, "b"]`, `payload.statusinplanConst([args.status, "b"])`},
		// Membership, both directions.
		{`row => "a" in row.tags`, `payload.tagshas"a"`},
		{`row => row.status in ["b", "a"]`, `payload.statusin["a","b"]`},
		{`row => row.status in args.list`, `payload.statusin&{list}`},
		{`row => row.status in []`, `const(false)`},
		// The one notion of unset rides the value.
		{`row => row.status == nil`, `payload.status==<nil>`},
		{`row => row.status == ""`, `payload.status==""`},
		{`row => row.status != nil`, `payload.status!=<nil>`},
		// A column is never unset.
		{`row => row.id == nil`, `const(false)`},
		{`row => row.id != ""`, `const(true)`},
		// Negation and bare booleans.
		{`row => !(row.status == "a")`, `!(payload.status=="a")`},
		{`row => row.urgent`, `payload.urgent==true`},
		{`row => !row.urgent`, `!(payload.urgent==true)`},
		// Predicates, kind-checked.
		{`row => row.status == "a" && isOpen(row)`, `AND(payload.status=="a",spec(isopen))`},
		{`row => isAdmin(actor) || row.status == "a"`, `OR(payload.status=="a",spec(isadmin))`},
		{`row => isActiveRecord(row)`, `spec(isactiverecord)`},
		// Collection predicates over a row array.
		{`row => row.tags.any(t => t == "x")`, `payload.tags.any($elem=="x")`},
		{`row => row.tags.all(t => t startsWith "a")`, `payload.tags.all($elemstartsWith"a")`},
		{`row => row.nums.any(n => n > args.n)`, `payload.nums.any($elem>&{n})`},
		{`row => row.tags.count() > 2`, `payload.tags.count()>2`},
		{`row => 2 < row.tags.count()`, `payload.tags.count()>2`},
		// Strings.
		{`row => row.title.includes("x")`, `payload.titleincludes"x"`},
		{`row => row.status startsWith args.status`, `payload.statusstartsWith&{status}`},
		// A blank needle or prefix, and an ordering against nothing, match nothing.
		{`row => row.title.includes(" ")`, `const(false)`},
		{`row => row.status startsWith ""`, `const(false)`},
		{`row => row.priority > nil`, `const(false)`},
		// The optional-argument guard: a plan constant beside the term it guards.
		{`row => args.status == nil || row.status == args.status`, `OR(payload.status==&{status},planConst(args.status == nil))`},
		// Ternaries: a boolean one over the row, and one whose condition is a plan constant.
		{`row => row.urgent ? row.priority > 1 : row.priority > 5`,
			`OR(AND(!(payload.urgent==true),payload.priority>5),AND(payload.priority>1,payload.urgent==true))`},
		{`row => args.flag ? row.priority > 1 : row.status == "a"`,
			`OR(AND(!(planConst(args.flag)),payload.status=="a"),AND(payload.priority>1,planConst(args.flag)))`},
		// Traversals, on a row of their own.
		{`row => childOf(p => p.status == "x")`, `childof(payload.status=="x")`},
		{`row => contains("members", p => p.id == args.status)`, `contains(id==&{status}|as=members)`},
		// Two fields of one row.
		{`row => row.overage > row.reported`, `payload.overage>field(payload.reported)`},
		// Nested fields: `.?` through an optional block, `.` through a required one.
		{`row => row.?lineage.planId == "p"`, `payload.lineage.planid=="p"`},
		{`row => row.settings.mode == "x"`, `payload.settings.mode=="x"`},
		{`row => row.?labels.team == "a"`, `payload.labels.team=="a"`},
		// Keys the declaration does not close are the author's to answer for,
		// and an untyped field reads alike through `.` and `.?`: an untyped
		// field, a bare object, an @open block's undeclared key beside its
		// declared one.
		{`row => row.blob.deep.path == "x"`, `payload.blob.deep.path=="x"`},
		{`row => row.?blob.k == 1`, `payload.blob.k==1`},
		{`row => row.?info.anything == "x"`, `payload.info.anything=="x"`},
		{`row => row.?extras.undeclared == "x"`, `payload.extras.undeclared=="x"`},
		{`row => row.?extras.known == "x"`, `payload.extras.known=="x"`},
		// Constants.
		{`row => true`, `const(true)`},
		{`row => args.flag`, `planConst(args.flag)`},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			ir, err := lowerSource(t, env, tc.src)
			require.NoError(t, err)
			require.Equal(t, tc.want, canonicalExpression(ir))
		})
	}
}

func TestLower_ShapeAndTraitBindings(t *testing.T) {
	t.Run("a context spec reads the envelope through its @actor shape", func(t *testing.T) {
		env := LowerEnv{Position: tiers.PositionSpecBody,
			ShapeKeys: map[string]string{"role": "actor.role", "isClusterOwner": "actor.isClusterOwner"}}
		ir, err := lowerSource(t, env, `actor => actor.role == "admin"`)
		require.NoError(t, err)
		require.Equal(t, `actor.role=="admin"`, canonicalExpression(ir))
		ir, err = lowerSource(t, env, `actor => actor.isClusterOwner`)
		require.NoError(t, err)
		require.Equal(t, `actor.isclusterowner==true`, canonicalExpression(ir))
	})
	t.Run("a row shape maps keys to stored paths, an actor key being a value", func(t *testing.T) {
		env := LowerEnv{Position: tiers.PositionSpecBody,
			ShapeKeys: map[string]string{"status": "payload.status", "createdAt": "createdAt", "userId": "actor.userId"}}
		ir, err := lowerSource(t, env, `row => row.status == "x" && row.createdAt < now`)
		require.NoError(t, err)
		require.Equal(t, `AND(createdAt<planConst(now),payload.status=="x")`, canonicalExpression(ir))
		ir, err = lowerSource(t, env, `row => row.status == row.userId`)
		require.NoError(t, err)
		require.Equal(t, `payload.status==&{userId}`, canonicalExpression(ir))
		_, err = lowerSource(t, env, `row => row.title == "x"`)
		require.ErrorContains(t, err, "is not a projected key of the bound shape")
	})
	t.Run("a trait reads any field, at any depth", func(t *testing.T) {
		env := LowerEnv{Position: tiers.PositionSpecBody}
		ir, err := lowerSource(t, env, `row => row.anything == 1 && row.deep.path == "x"`)
		require.NoError(t, err)
		require.Equal(t, `AND(payload.anything==1,payload.deep.path=="x")`, canonicalExpression(ir))
	})
}

// TestLower_RefusesWithTheNodeThePositionAndTheNearestSpelling pins every
// refusal to its three parts (D11, D24): the node as written, the position,
// and a fix carrying the nearest pushdown spelling in backticks.
func TestLower_RefusesWithTheNodeThePositionAndTheNearestSpelling(t *testing.T) {
	env := lowerTestEnv(t)
	cases := []struct {
		name string
		src  string
		node string   // the refused node as FormatExpr prints it
		want []string // further fragments: the reason and the spelling
	}{
		{"an in-process function over the row", `row => lower(row.email) == "x"`,
			"lower(row.email)", []string{"`lower` runs in process and reads the row", "Compare against a computed value instead: `row.email == lower(\"x\")`"}},
		{"arithmetic over the row", `row => row.priority + 1 > 2`,
			"row.priority + 1", []string{"arithmetic over the row runs in process", "`row.priority > 1`"}},
		{"addDuration over the row", `row => addDuration(row.dueAt, "P1D") < now`,
			`addDuration(row.dueAt, "P1D")`, []string{"`row.dueAt < addDuration(now, \"-P1D\")`"}},
		{"a query result as a collection", `row => query openTickets().any(t => t == row.id)`,
			"", []string{"unbounded source"}},
		{"a string used as a condition", `row => row.title && row.urgent`,
			"row.title", []string{"`row.title` is a string, and a condition must be boolean", "`row.title != nil`"}},
		{"a number used as a condition", `row => row.priority`,
			"row.priority", []string{"is a number, and a condition must be boolean", "`row.priority > 0`"}},
		{"a read through an optional block without .?", `row => row.lineage.planId == "p"`,
			"row.lineage.planId", []string{"`row.lineage` is an optional object", "`row.?lineage.planId`"}},
		{"a read through an optional bare object without .?", `row => row.info.anything == "x"`,
			"row.info.anything", []string{"`row.info` is an optional object", "`row.?info.anything`"}},
		{"an undeclared key of a CLOSED block", `row => row.?lineage.plan == "p"`,
			"row.?lineage.plan", []string{"is not a declared field of v1:lowertest:ticket", "Did you mean `row.?lineage.planId`?"}},
		{"a negated traversal", `row => !childOf(p => p.status == "x")`,
			`!childOf(p => p.status == "x")`, []string{"complement of a row set", "`childOf(p => !(p.status == \"x\"))`"}},
		{"an undeclared field", `row => row.statuss == "x"`,
			"row.statuss", []string{"is not a declared field of v1:lowertest:ticket", "Did you mean `row.status`?"}},
		{"a row predicate applied to the actor", `row => isOpen(actor)`,
			"isOpen(actor)", []string{"a predicate over rows", "`isOpen(row)`"}},
		{"a context spec applied to the row", `row => isAdmin(row)`,
			"isAdmin(row)", []string{"a context spec over the actor", "`isAdmin(actor)`"}},
		{"an unknown predicate", `row => isNothing(row)`,
			"isNothing(row)", []string{"is not a spec, trait or catalog function known here"}},
		{"a value ternary over the row", `row => (row.urgent ? row.title : row.status) == "x"`,
			"row.urgent ? row.title : row.status", []string{"a value ternary over the row runs in process", "Write the boolean form: `", `row.title == "x"`}},
		{"an undeclared argument", `row => row.status == args.missing`,
			"args.missing", []string{"`args.missing` is not a declared argument"}},
		{"where().count() over the row", `row => row.tags.where(t => t == "x").count() > 0`,
			"", []string{"`row.tags.any(t => t == \"x\")`"}},
		{"an ordering against a boolean", `row => row.priority < true`,
			"row.priority < true", []string{"booleans are not ordered"}},
		{"in over a string field", `row => "x" in row.title`,
			`"x" in row.title`, []string{"`row.title.includes(\"x\")`"}},
		{"?? over the row", `row => (row.status ?? "open") == "open"`,
			`row.status ?? "open"`, []string{"`??` over the row runs in process", "row.status != nil"}},
		{"includes on a list", `row => row.tags.includes("x")`,
			`row.tags.includes("x")`, []string{"`\"x\" in row.tags`"}},
		{"an unknown name", `row => event.payload.x == 1`,
			"event", []string{"`event` is not defined here"}},
		{"a predicate over an element", `row => row.tags.any(t => isOpen(t))`,
			"isOpen(t)", []string{"is an element of a list"}},
		{"a string count", `row => row.title.count() > 3`,
			"row.title.count()", []string{"`.count()` on a string"}},
		{"a traversal reading the outer row", `row => childOf(p => p.status == row.status)`,
			"row.status", []string{"reads only its own row"}},
		{"a prefix test on a column", `row => row.id startsWith "x"`,
			`row.id startsWith "x"`, []string{"is a row column"}},
		{"the row as a condition", `row => row`,
			"row", []string{"is the row itself, not a condition"}},
		{"a mixed membership list", `row => row.status in ["a", 1]`,
			`row.status in ["a", 1]`, []string{"mixes number and string"}},
		{"a list compared with ==", `row => row.status == row.tags`,
			"row.status == row.tags", []string{"is a list"}},
		{"a reserved parameter", `args => args.x == 1`,
			"args => ...", []string{"reserved name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lowerSource(t, env, tc.src)
			require.Error(t, err)
			var lerr *LowerError
			require.True(t, errors.As(err, &lerr), "a refusal is a *LowerError: %v", err)
			msg := err.Error()
			if tc.node != "" {
				require.Equal(t, tc.node, lerr.Node, "the refused node")
			}
			require.Equal(t, tiers.PositionQueryFilter, lerr.Position)
			require.Contains(t, msg, "does not lower in a query filter", "the position")
			require.NotEmpty(t, lerr.Fix, "the nearest spelling")
			for _, frag := range tc.want {
				require.Contains(t, msg, frag)
			}
		})
	}
}

func TestLower_SpecBodyRefusesArguments(t *testing.T) {
	env := LowerEnv{Position: tiers.PositionSpecBody, Concept: lowerTestConcept(t)}
	_, err := lowerSource(t, env, `row => row.status == args.status`)
	require.ErrorContains(t, err, "a spec or trait takes no arguments")
	require.ErrorContains(t, err, "does not lower in a spec or trait body")
}

// TestLower_DefersThePredicateKindCheckWithoutARegistry pins the load-time
// contract: a query is lowered while specs are still loading, with no
// registry, and the application lowers to a reference the Init pass checks.
func TestLower_DefersThePredicateKindCheckWithoutARegistry(t *testing.T) {
	env := lowerTestEnv(t)
	env.Predicate = nil
	ir, err := lowerSource(t, env, `row => isAdmin(row)`)
	require.NoError(t, err)
	require.Equal(t, `spec(isadmin)`, canonicalExpression(ir))
}

// TestLowerError_ReadsAsOneSentence pins the rendering the coordinator's
// example names.
func TestLowerError_ReadsAsOneSentence(t *testing.T) {
	err := &LowerError{Node: "lower(row.email)", Position: tiers.PositionQueryFilter,
		Reason: "`lower` runs in process and reads the row", Fix: "Compare against a computed value instead: `row.email == lower(args.email)`"}
	require.Equal(t, "`lower(row.email)` does not lower in a query filter: `lower` runs in process and reads the row. "+
		"Compare against a computed value instead: `row.email == lower(args.email)`", err.Error())
	require.False(t, strings.HasSuffix(err.Error(), "."))
}
