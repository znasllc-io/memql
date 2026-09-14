package memql

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_lower_codes_test.go -- every lowering refusal names its rule id (D24:
// "every refusal names the construct, the position, the rule id and the
// replacement"), and a spec or trait over rows does not read the actor
// (authoring rule 26). The codes are the stable half of a refusal: the corpus
// pins them (a refused case carrying one must name it in `code`), and the
// load report, the lint pass and the authoring diagnostics carry them as a
// field beside the text.

// lowerSpecBodyEnv is lowerTestEnv at the spec-body position: the ticket's
// fields, no arguments, the same predicate registry.
func lowerSpecBodyEnv(t *testing.T) LowerEnv {
	env := lowerTestEnv(t)
	env.Position = tiers.PositionSpecBody
	env.Args = nil
	return env
}

// requireLowerCode lowers src and asserts the refusal carries code, both as
// the error's RuleCode and in brackets at the end of its text.
func requireLowerCode(t *testing.T, env LowerEnv, src, code string) *LowerError {
	t.Helper()
	_, err := lowerSource(t, env, src)
	require.Error(t, err, "%s lowered; want a %s refusal", src, code)
	var le *LowerError
	require.True(t, errors.As(err, &le), "%s: the refusal is not a LowerError: %v", src, err)
	require.Equal(t, code, le.RuleCode(), "%s: %v", src, err)
	require.True(t, strings.HasSuffix(err.Error(), "["+code+"]"), "%s: the text does not end with its code: %v", src, err)
	return le
}

func TestLower_EveryRefusalKindCarriesItsRuleCode(t *testing.T) {
	filter := lowerTestEnv(t)
	spec := lowerSpecBodyEnv(t)
	for _, c := range []struct {
		name string
		env  LowerEnv
		src  string
		code string
	}{
		// The generic kind: a node with no form at its position.
		{"an in-process function over the row", filter, `row => lower(row.title) == "x"`, LowerCodeRefused},
		{"arithmetic over the row", filter, `row => row.priority * 2 > 4`, LowerCodeRefused},
		{"a construct call in a predicate", filter, `row => query other().count() > 0`, LowerCodeRefused},
		{"an argument in a spec body", spec, `row => row.status == args.status`, LowerCodeRefused},
		// A name the position does not bind.
		{"a bare payload field", filter, `row => status == "open"`, LowerCodeUnknownName},
		{"an undeclared argument", filter, `row => row.status == args.missing`, LowerCodeUnknownName},
		{"a predicate nothing registers", filter, `row => isNothing(row)`, LowerCodeUnknownName},
		// A field nothing declares.
		{"a field the concept does not declare", filter, `row => row.statuss == "open"`, LowerCodeUnknownField},
		{"a member the actor envelope does not have", filter, `row => row.ownerUserId == actor.nickname`, LowerCodeUnknownField},
		// A read through an optional object without `.?`.
		{"an optional hop written with a dot", filter, `row => row.lineage.planId == "p"`, LowerCodeOptionalHop},
		// A predicate applied to the wrong subject.
		{"a context spec applied to the row", filter, `row => isAdmin(row)`, LowerCodeContextSpecOnRow},
		{"a row spec applied to the actor", filter, `row => isOpen(actor)`, LowerCodeRowPredicateOnActor},
		// A condition that is not boolean.
		{"a string field as the condition", filter, `row => row.title`, LowerCodeNotBoolean},
		{"a list as the condition", filter, `row => ["a"]`, LowerCodeNotBoolean},
		{"a count as the condition", filter, `row => row.tags.count()`, LowerCodeNotBoolean},
		// The actor, read from a predicate over rows.
		{"the actor compared in a row spec", spec, `row => actor.userId == row.ownerUserId`, LowerCodeActorInRowPredicate},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireLowerCode(t, c.env, c.src, c.code)
		})
	}

	// The in-process positions' refusals carry theirs too.
	e := &MemQLEngine{specs: newSpecRegistry()}
	fn := &Function{Name: "q"}
	err := e.validateRefine(fn, &RefineExpression{Lambda: refineLambda(t, `row => row.tags.any(a => row.tags.any(b => row.tags.any(c => a == c)))`)})
	var le *LowerError
	require.True(t, errors.As(err, &le), "the refine's cost refusal: %v", err)
	require.Equal(t, LowerCodeCostOverBudget, le.RuleCode())
	err = e.validateRefine(fn, &RefineExpression{Lambda: refineLambda(t, `row => event.x == 1`)})
	require.True(t, errors.As(err, &le), "the refine's unknown-name refusal: %v", err)
	require.Equal(t, LowerCodeUnknownName, le.RuleCode())
	err = CheckConditionFields(refineLambda(t, `row => row.title`), lowerTestConcept(t), tiers.PositionTriggerFilter)
	require.True(t, errors.As(err, &le), "the condition check's refusal: %v", err)
	require.Equal(t, LowerCodeNotBoolean, le.RuleCode())
}

// TestLower_ARowPredicateDoesNotReadTheActor: a spec or trait over rows is the
// same predicate for every caller, and the ownership test is what row-authz
// looks for in a QUERY FILTER -- so its body may not read the `actor` root,
// in any of the places the root can appear (authoring rule 26). The refusal
// names the position, the rule and both ways out.
func TestLower_ARowPredicateDoesNotReadTheActor(t *testing.T) {
	spec := lowerSpecBodyEnv(t)
	for _, src := range []string{
		`row => actor.userId == row.ownerUserId`,
		`row => row.ownerUserId == actor.userId`,
		`row => row.status == "open" && actor.isClusterOwner`,
		`row => !actor.isClusterOwner`,
		`row => isAdmin(actor)`,
		`row => row.tags.any(t => t == actor.userId)`,
		`row => references(p => p.createdBy == actor.userId)`,
	} {
		le := requireLowerCode(t, spec, src, LowerCodeActorInRowPredicate)
		require.Equal(t, tiers.PositionSpecBody, le.Position, src)
		msg := le.Error()
		require.Contains(t, msg, "does not lower in a spec or trait body: a spec or trait over rows does not read the actor", src)
		require.Contains(t, msg, "Compare in the query filter (`row.ownerUserId == actor.userId`), or bind an @actor shape", src)
	}
	// A trait is a predicate over rows too: lowered with no concept, the same
	// refusal.
	trait := spec
	trait.Concept = nil
	requireLowerCode(t, trait, `row => actor.role == "admin"`, LowerCodeActorInRowPredicate)

	// The controls. A spec over an @actor shape reads the envelope as its
	// parameter; a row spec reads the clock; a query filter reads the actor
	// -- that is where the ownership conjunct belongs.
	actorSpec := LowerEnv{Position: tiers.PositionSpecBody, ShapeKeys: map[string]string{"role": "actor.role", "userId": "actor.userId"}}
	_, err := lowerSource(t, actorSpec, `actor => actor.role == "owner"`)
	require.NoError(t, err, "a spec over an @actor shape reads the envelope through its parameter")
	_, err = lowerSource(t, spec, `row => row.dueAt < now`)
	require.NoError(t, err, "a row spec reads the clock")
	_, err = lowerSource(t, lowerTestEnv(t), `row => row.ownerUserId == actor.userId || actor.isClusterOwner == true`)
	require.NoError(t, err, "a query filter reads the actor")
}

// TestInit_RefusesARowSpecReadingTheActor: the refusal at boot, through the
// real loader, carried on the load report with its code -- and the query that
// states the same test in its own filter loads beside it.
func TestInit_RefusesARowSpecReadingTheActor(t *testing.T) {
	eng, err := bootLowerTree(t, map[string]string{
		"concepts.memql": lowerInitConcepts + `
/// A note the caller owns.
concept note {
  ownerUserId  string   @description("Owner.")
  body         string   @description("Body.")
}
`,
		"specs.memql": `use lowerinit.concepts.{ note }

/// Refused at Init: a row spec asking who the caller is.
spec note isCallersNote = row => row.ownerUserId == actor.userId
`,
		"queries.memql": `use lowerinit.concepts.{ note }

/// The caller's notes: the ownership test in the query's own filter.
@actor
query note callersNotes {
  filter   row => row.ownerUserId == actor.userId
  paginate 20
}
`,
	})
	require.Error(t, err, "strict boot refuses a row spec that reads the actor")
	var found bool
	for _, s := range eng.loadReport.Skipped {
		if s.Name != "isCallersNote" {
			continue
		}
		found = true
		require.Equal(t, LowerCodeActorInRowPredicate, s.Code, "the load report carries the rule id as a field")
		require.Contains(t, s.Err, "`actor.userId` does not lower in a spec or trait body: a spec or trait over rows does not read the actor")
		require.True(t, strings.HasSuffix(s.Err, "[lower_actor_in_row_predicate]"), s.Err)
	}
	require.True(t, found, "no load-report entry for isCallersNote: %v", lowerInitSkips(eng))
	require.Len(t, lowerInitSkips(eng), 1, "only the spec is refused: %v", lowerInitSkips(eng))
	_, err = eng.Functions().Get("callersNotes")
	require.NoError(t, err, "the query reading the actor in its filter registered")
}

// TestLintAndAuthoringCarryTheLowerCode: the two other places a lowering
// refusal is reported -- the offline lint pass and a session define's
// diagnostics -- carry the code as a field, not only in the text.
func TestLintAndAuthoringCarryTheLowerCode(t *testing.T) {
	root := withLanguageLines(fstest.MapFS{
		"lowerinit/concepts.memql": {Data: []byte(lowerInitConcepts)},
		"lowerinit/queries.memql": {Data: []byte(`use lowerinit.concepts.{ ticket }

/// Refused: title is a string, and a filter is a condition.
query ticket titleAsFilter {
  filter   row => row.title
  paginate 20
}
`)},
	})
	diags, _, err := LintUnifiedTree(nil, root)
	require.NoError(t, err)
	var linted bool
	for _, d := range diags {
		if strings.Contains(d.Message, "titleAsFilter") {
			linted = true
			require.Equal(t, LowerCodeNotBoolean, d.Code, d.Message)
		}
	}
	require.True(t, linted, "no lint diagnostic for titleAsFilter: %+v", diags)

	eng := bootAuthoringEngine(t)
	var res SessionDefineResult
	withExpressionsV1(t, func() {
		res, err = eng.DefineSessionBundle(NewAuthoredRuntimeRegistry(), "owner-1", `use lowerinit.concepts.{ ticket }

/// Refused: title is a string, and a filter is a condition.
query ticket sessionTitleAsFilter {
  filter   row => row.title
  paginate 20
}
`, "")
	})
	require.Error(t, err)
	d := diagnosticFor(t, res.Diagnostics, "query", "sessionTitleAsFilter")
	require.Equal(t, LowerCodeNotBoolean, d.Code, d.Error)
	require.True(t, strings.HasSuffix(d.Error, "[lower_not_boolean]"), d.Error)
}
