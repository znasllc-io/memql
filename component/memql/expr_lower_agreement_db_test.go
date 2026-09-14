package memql

import (
	"context"
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
// All three must agree on EVERY row, the mistyped and malformed fixture rows
// included, and no row is allowed to depart. The first two are intersected by
// the executor, so a disagreement between them drops rows silently; the third
// is the same expression in an in-process position, so a disagreement there
// is one predicate selecting different rows depending on where it is written.
//
// A stored value of the wrong type for an operation is where the evaluators
// once parted (memql#5369): EvalExpr refused it (in_requires_list,
// operand_type, condition_not_boolean) or read an object as a one-element
// list, while the SQL, which can only answer about a row, answered. Now every
// evaluator reads it as data (expr_stored.go): not equal, not ordered, not a
// member, not true, and counted by what it holds -- an array's elements, a
// string's characters, zero for anything else.
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
		// A bare field as an operand and as a ternary branch: a condition in
		// both evaluators, so a stored non-boolean there is not true.
		`row.irB || row.irS == "a"`, `row.irS == nil ? row.irB : false`,
		// Two fields of one row.
		`row.irA == row.irZ`, `row.irA != row.irZ`, `row.irA < row.irZ`, `row.irA >= row.irZ`,
	}
}

func TestV1LoweredBodiesAgreeAcrossBothEvaluators(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	owner, rows, names := irSeededRows(t, ctx, db)
	env := LowerEnv{Position: tiers.PositionQueryFilter, Args: map[string]ArgType{}}

	bodies := irV1Bodies()
	require.GreaterOrEqual(t, len(bodies), 50, "the v1 matrix shrank to %d bodies", len(bodies))

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
	t.Logf("three-way agreement held for %d lowered forms over %d fixture rows, with no row allowed to depart", checked, len(rows))
}
