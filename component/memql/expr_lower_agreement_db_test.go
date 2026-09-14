package memql

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// expr_lower_agreement_db_test.go -- edition-2026 bodies, lowered by Lower,
// agree THREE ways over the shared fixture rows (memql#5366; D7: "the SQL
// lowering and the in-process evaluator consume the same AST and are held
// equal"):
//
//  1. the SQL the executor compiles from the lowered IR, run RAW;
//  2. the in-process twin that post-filters it (nodeMatchesExpression);
//  3. EvalExpr over the same row (NewExprRow) -- the evaluator a refine clause,
//     an automation condition and every other in-process position run.
//
// The first two must agree on EVERY row: the executor intersects them, so a
// disagreement drops rows silently. EvalExpr must agree on every row whose
// field holds a value of the type the body reads -- which is every row of a
// concept whose schema declared the field, since the write path validates
// against it. The rows on which it still departs from the SQL are named below
// (irEvalDivergentRows) so the allowance cannot grow silently: each must be
// exercised, and no other row may diverge.
//
// A stored value that is present and not a list, where the body expects one --
// the right side of `in`, the receiver of any(), all() or count() -- used to
// be refused (in_requires_list, operand_type) or read as a one-element list
// when an object; it now answers as the pushdown does: not a member, no
// element passes, none counted (expr_stored.go, memql#5369). Two departures
// remain, each a question of the language rather than a bug in one side:
//
//   - A BOOLEAN POSITION holding a string ("true", "maybe"): D8 refuses a
//     non-boolean condition at run time (condition_not_boolean), and a refine
//     clause fails the read naming the row (refine_test.go); the SQL cannot
//     refuse one row, so it reads "not true".
//   - count() over a stored STRING in a list-or-untyped field: EvalExpr counts
//     its characters (string.count, which is where Lower sends a string's
//     count), and the pushdown's count of a non-array is 0.
//
// Every body runs bare and negated, the negation lowered from source.

// irV1Bodies is the matrix: every shape Lower emits, over the fields the
// fixtures type (irS strings, irN numbers, irB booleans, irO an object,
// irTags / irNums / irItems arrays, irA / irZ two comparable fields).
func irV1Bodies() []string {
	return []string{
		// The absence table.
		`row.irS == ""`, `row.irS != ""`, `row.irS == nil`, `row.irS != nil`,
		`row.irS == "a"`, `row.irS != "a"`, `row.irO.k == ""`, `row.irO.k == "a"`,
		// Typed comparisons, byte-order strings.
		`row.irS < "a"`, `row.irS >= "a"`, `row.irS > "E"`,
		`row.irN == 0`, `row.irN > 0`, `row.irN <= 1`, `row.irN == 1`, `row.irS == "1"`,
		`row.irB == true`, `row.irB != false`, `row.irB`,
		// Membership, both directions, unset members and needles.
		`row.irS in ["a", "e"]`, `row.irS in ["", "a"]`, `row.irS in [nil, "a"]`,
		`"a" in row.irTags`, `1 in row.irNums`, `nil in row.irTags`, `"" in row.irTags`,
		// Prefixes and substrings, blank needles included.
		`row.irS startsWith "a"`, `row.irS startsWith ["b", "é"]`,
		`row.irS.includes("b")`, `row.irS.includes("")`,
		// Collection predicates over row arrays.
		`row.irTags.any(t => t == "a")`, `row.irTags.any(t => t != "a")`,
		`row.irTags.all(t => t == "a")`, `row.irTags.all(t => t != "")`,
		`row.irTags.count() > 1`, `row.irTags.count() == 0`,
		`row.irNums.any(n => n > 1)`, `row.irNums.all(n => n > 0)`,
		`row.irItems.any(i => i.qty > 0)`, `row.irItems.all(i => i.qty > 0)`, `row.irItems.any(i => i.qty == nil)`,
		`row.irItems.any(i => i.tags.any(t => t == "x"))`,
		`row.irTags.any(t => t == "a") && row.irS == nil`,
		// Compositions and ternaries.
		`row.irS == "a" || row.irN > 0`, `row.irS != "" && !(row.irS < "b")`,
		`row.irB ? row.irN > 0 : row.irS == "a"`, `row.irS == "a" ? true : row.irN == 1`,
		// Two fields of one row.
		`row.irA == row.irZ`, `row.irA != row.irZ`, `row.irA < row.irZ`, `row.irA >= row.irZ`,
	}
}

// irEvalDivergentRows are the mistyped fixture rows on which EvalExpr departs
// from the SQL (see the file header), and how: "refuses" -- it errors where
// the SQL answers -- and/or "answers" -- it returns the other boolean.
var irEvalDivergentRows = map[string][]string{
	"irB a string true":         {"refuses"}, // row.irB as a condition: D8's run-time refusal
	"irB malformed":             {"refuses"}, // the same, over "maybe"
	"irTags a non-array scalar": {"answers"}, // row.irTags.count() over "a": characters, not elements
}

func TestV1LoweredBodiesAgreeAcrossBothEvaluators(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	owner, rows, names := irSeededRows(t, ctx, db)
	env := LowerEnv{Position: tiers.PositionQueryFilter, Args: map[string]ArgType{}}

	bodies := irV1Bodies()
	require.GreaterOrEqual(t, len(bodies), 50, "the v1 matrix shrank to %d bodies", len(bodies))

	diverged := map[string]map[string]bool{}
	checked := 0
	for _, body := range bodies {
		for _, src := range []string{"row => " + body, "row => !(" + body + ")"} {
			lam, err := languageParser.ParseV1Lambda(src)
			require.NoError(t, err, src)
			env.Param = lam.Params[0]
			ir, err := Lower(lam.Body, env)
			if err != nil {
				t.Errorf("%s: does not lower: %v", src, err)
				continue
			}
			compiled, ok := eng.tryCompileCombinedFilter(ctx, ir, irAgreementConcept)
			if !ok {
				t.Errorf("%s: lowered to %s, which does not compile", src, canonicalExpression(ir))
				continue
			}
			sqlIDs, err := irSQLMatches(t, ctx, db, owner, compiled)
			if err != nil {
				t.Errorf("%s: SQL failed: %v\n  SQL: %s", src, err, compiled.sql)
				continue
			}
			twin := irInProcessMatches(t, rows, ir)
			if strings.Join(sqlIDs, ",") != strings.Join(twin, ",") {
				t.Errorf("%s: the SQL and the in-process twin DISAGREE\n  only SQL:        %v\n  only in process: %v\n  SQL: %s",
					src, irNamed(irDifference(sqlIDs, twin), names), irNamed(irDifference(twin, sqlIDs), names), compiled.sql)
				continue
			}

			inSQL := map[string]bool{}
			for _, id := range sqlIDs {
				inSQL[id] = true
			}
			for _, row := range rows {
				exprRow, err := NewExprRow(row)
				require.NoError(t, err)
				verdict, evalErr := EvalCondition(ctx, lam.Body, MapScope{lam.Params[0]: exprRow}, EvalOptions{})
				name := names[row.ID]
				if evalErr == nil && verdict == inSQL[row.ID] {
					continue
				}
				how := "answers"
				if evalErr != nil {
					how = "refuses"
				}
				allowed := false
				for _, a := range irEvalDivergentRows[name] {
					allowed = allowed || a == how
				}
				if allowed {
					if diverged[name] == nil {
						diverged[name] = map[string]bool{}
					}
					diverged[name][how] = true
					continue
				}
				if evalErr != nil {
					t.Errorf("%s: EvalExpr refuses row %q (%v) where the SQL answers %v", src, name, evalErr, inSQL[row.ID])
				} else {
					t.Errorf("%s: EvalExpr answers %v on row %q, the SQL %v", src, verdict, name, inSQL[row.ID])
				}
			}
			checked++
		}
	}
	if t.Failed() {
		return
	}
	require.Equal(t, 2*len(bodies), checked)
	var unexercised []string
	for name, kinds := range irEvalDivergentRows {
		for _, how := range kinds {
			if !diverged[name][how] {
				unexercised = append(unexercised, name+" ("+how+")")
			}
		}
	}
	sort.Strings(unexercised)
	require.Empty(t, unexercised, "every allowed divergence must actually occur, or the allowance is stale")
	t.Logf("three-way agreement held for %d lowered forms over %d fixture rows; EvalExpr departed only on the %d named mistyped rows",
		checked, len(rows), len(diverged))
}
