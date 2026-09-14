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
// against it. It departs from the SQL on exactly two kinds of MISTYPED row,
// and the rows are named below (irEvalDivergentRows) so the allowance cannot
// grow silently: each must be exercised, and no other row may diverge.
//
//   - It REFUSES a value it cannot read at all -- a boolean position holding a
//     string, a list method or `in` over a scalar (condition_not_boolean,
//     in_requires_list, operand_type) -- where the SQL can only answer "no".
//     That is D8's runtime typing.
//   - It ANSWERS a list method over a non-list differently: it reads a lone
//     object as a one-element list and counts a string's characters, the
//     step-result convenience its collection methods keep
//     (exprCollection), where the pushdown's rules table answers any/all over
//     a non-array false and count 0. Recorded for the differential lane
//     (memql#5369), which decides which side a mistyped row should move.
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
	"irB a string true":         {"refuses"},
	"irB malformed":             {"refuses"},
	"irTags a non-array scalar": {"refuses", "answers"},
	"irNums a non-array scalar": {"refuses"},
	"irTags an object":          {"refuses", "answers"},
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
