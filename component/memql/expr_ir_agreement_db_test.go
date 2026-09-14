package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// expr_ir_agreement_db_test.go -- the SQL pushdown and its in-process twin
// agree ROW FOR ROW on the edition-2026 IR (memql#5366), against a real
// Postgres.
//
// WHY THIS IS THE TEST THAT MATTERS. executeCombinedFilterQuery scans in SQL
// and then re-runs the whole predicate in process on every candidate
// (latestMatchingNodes), so the rows a read returns are the INTERSECTION of
// the two halves. Where they disagree, rows vanish with no error -- and under
// a negation a disagreement flips direction, so a divergence that used to be
// masked by the intersection starts dropping rows the author asked for. The
// absence table (null, missing, "", " ") and byte-order strings are exactly
// where SQL's three-valued logic and a locale collation part company with Go.
//
// THE METHOD, and why the halves are run SEPARATELY. Running the executor and
// asserting its answer would compare the intersection with itself. So for
// every predicate:
//
//  1. its compiled SQL runs RAW over the seeded rows (no post-filter);
//  2. nodeMatchesExpression runs over the SAME rows as stored (read back from
//     the database, so the in-process side sees the bytes Postgres holds);
//  3. the two id sets must be equal -- and then the executor path
//     (evaluateExpressionSetWithContext) must return that same set.
//
// Every predicate runs twice, bare and under a NotExpression.
//
// THE FIXTURES ARE TYPED BY FIELD, AND THEN MISTYPED ON PURPOSE. Each field
// has a declared type the way a production field does (irS strings, irN
// numbers, irB booleans, irO an object, irTags / irNums / irItems arrays), and
// every unset shape -- key missing, JSON null, "" -- plus the whitespace value
// " " appears in each. Then a few rows put the WRONG type in a field: a string
// "1" where a number belongs, a number 1 where a string does, "true" where a
// boolean does, and two malformed strings ("abc", "maybe"). Before memql#5366
// those rows could not sit in this matrix at all -- the comparison cast the
// extracted text by the literal's type, and `'abc'::numeric` is a Postgres
// ERROR that fails the read. Comparisons are TYPED now, the casts are behind
// jsonb_typeof guards, and a mistyped row is simply not equal, not ordered and
// not a member, on both halves.
// The field names are prefixed so no other suite's rows on the shared database
// carry them.

// irAgreementConcept is tier-free and ungated, so nothing narrows the scan
// before the predicate does, and it is written RAW, so no automation fires on
// a seeded row -- the property keysetConcept documents for the same concept.
const irAgreementConcept = "v1:platform:missingCapability"

type irAgreementFixture struct {
	name    string
	payload string
}

func irAgreementFixtures() []irAgreementFixture {
	return []irAgreementFixture{
		{"every field absent", `{}`},
		{"every field JSON null", `{"irS":null,"irN":null,"irB":null,"irO":null,"irTags":null,"irNums":null,"irItems":null}`},
		{"irS empty", `{"irS":""}`},
		{"irS space", `{"irS":" "}`},
		{"irS a", `{"irS":"a","irN":0,"irB":false}`},
		{"irS b", `{"irS":"b","irN":1,"irB":true}`},
		{"irS ab", `{"irS":"ab","irN":-1}`},
		{"irS e-acute", `{"irS":"é","irN":2.5}`},
		{"irS E", `{"irS":"E","irN":10}`},
		{"irS e", `{"irS":"e"}`},
		{"irS z", `{"irS":"z"}`},
		{"irS number zero", `{"irS":0}`},
		{"irS boolean false", `{"irS":false}`},
		{"irO k a", `{"irO":{"k":"a"}}`},
		{"irO k empty", `{"irO":{"k":""}}`},
		{"irO empty object", `{"irO":{}}`},
		{"irO k null", `{"irO":{"k":null}}`},
		{"irTags empty", `{"irTags":[]}`},
		{"irTags a", `{"irTags":["a"]}`},
		{"irTags a b", `{"irTags":["a","b"]}`},
		{"irTags b c", `{"irTags":["b","c"]}`},
		{"irTags a non-array scalar", `{"irTags":"a"}`},
		{"irTags blank and null", `{"irTags":["",null]}`},
		{"irNums empty", `{"irNums":[]}`},
		{"irNums 1 2 3", `{"irNums":[1,2,3]}`},
		{"irNums zero", `{"irNums":[0]}`},
		{"irNums a non-array scalar", `{"irNums":7}`},
		{"irItems qty 1 and 0", `{"irItems":[{"qty":1},{"qty":0}]}`},
		{"irItems qty 2", `{"irItems":[{"qty":2}]}`},
		{"irItems no qty", `{"irItems":[{"k":"x"}]}`},
		{"irItems empty", `{"irItems":[]}`},
		{"irItems qty null", `{"irItems":[{"qty":null}]}`},
		{"irItems nested tags", `{"irItems":[{"tags":["x"]},{"tags":[]}]}`},
		// The mistyped rows (see the file header).
		{"irN a string one", `{"irN":"1"}`},
		{"irS a number one", `{"irS":1}`},
		{"irB a string true", `{"irB":"true"}`},
		{"irN malformed", `{"irN":"abc"}`},
		{"irB malformed", `{"irB":"maybe"}`},
		{"irTags mixed types", `{"irTags":["a",1,true,null]}`},
		{"irNums strings of digits", `{"irNums":["1","2"]}`},
	}
}

// irAgreementPredicates is the matrix: every comparison operator against "",
// "a", 0, true and nil on the field of the matching type, the three
// collection predicates, and a few compositions.
func irAgreementPredicates() []ExpressionNode {
	var out []ExpressionNode
	ordered := []ComparisonOperator{OpEq, OpNe, OpLt, OpLe, OpGt, OpGe}

	for _, value := range []string{"", "a"} {
		for _, op := range ordered {
			out = append(out, irPayloadCmp("irS", op, value))
			out = append(out, irPayloadCmp("irO.k", op, value))
		}
	}
	for _, op := range ordered {
		out = append(out, irPayloadCmp("irN", op, int64(0)))
	}
	// The coordinator's pinned typed cases: a stored "1" against 1 and a stored
	// 1 against "1" here; a stored "true" against true is irB == true below.
	out = append(out,
		irPayloadCmp("irN", OpEq, int64(1)),
		irPayloadCmp("irS", OpEq, "1"),
	)
	for _, op := range []ComparisonOperator{OpEq, OpNe} {
		out = append(out, irPayloadCmp("irB", op, true), irPayloadCmp("irB", op, false))
	}
	for _, field := range []string{"irS", "irN", "irB", "irO", "irO.k", "irTags", "irNums", "irItems"} {
		out = append(out, irPayloadCmp(field, OpMissing, nil), irPayloadCmp(field, OpNotMissing, nil))
	}
	out = append(out,
		irPayloadCmp("irS", OpIn, []any{"a", "e"}),
		irPayloadCmp("irS", OpIn, []any{"", "a"}),
		irPayloadCmp("irS", OpIn, []any{nil, "a"}),
		irPayloadCmp("irS", OpOut, []any{"a", "e"}),
		irPayloadCmp("irTags", OpHas, nil),
		irPayloadCmp("irS", OpStartsWith, "a"),
		irPayloadCmp("irS", OpStartsWith, "é"),
		irPayloadCmp("irN", OpIn, []any{int64(0), int64(1)}),
		irPayloadCmp("irTags", OpHas, "a"),
		irPayloadCmp("irNums", OpHas, int64(1)),
	)

	tags := func(method string, pred ExpressionNode) ExpressionNode {
		return &ArrayPredicateExpression{Field: irPayloadField("irTags"), Method: method, Param: "t", Pred: pred}
	}
	nums := func(method string, pred ExpressionNode) ExpressionNode {
		return &ArrayPredicateExpression{Field: irPayloadField("irNums"), Method: method, Param: "n", Pred: pred}
	}
	items := func(method string, pred ExpressionNode) ExpressionNode {
		return &ArrayPredicateExpression{Field: irPayloadField("irItems"), Method: method, Param: "i", Pred: pred}
	}
	count := func(field string, op ComparisonOperator, n int64) ExpressionNode {
		return &ArrayPredicateExpression{Field: irPayloadField(field), Method: ArrayMethodCount, CountOp: op, CountValue: n}
	}
	out = append(out,
		tags(ArrayMethodAny, irElementCmp("", OpEq, "a")),
		tags(ArrayMethodAny, irElementCmp("", OpNe, "a")),
		tags(ArrayMethodAny, irElementCmp("", OpEq, "")),
		tags(ArrayMethodAny, irElementCmp("", OpMissing, nil)),
		tags(ArrayMethodAny, irElementCmp("", OpStartsWith, "a")),
		tags(ArrayMethodAny, irElementCmp("", OpLt, "b")),
		tags(ArrayMethodAny, &LogicalExpression{Op: LogicalOr, Left: irElementCmp("", OpEq, "a"), Right: irElementCmp("", OpEq, "c")}),
		tags(ArrayMethodAny, &NotExpression{Target: irElementCmp("", OpEq, "a")}),
		tags(ArrayMethodAll, irElementCmp("", OpEq, "a")),
		tags(ArrayMethodAll, irElementCmp("", OpNe, "")),
		tags(ArrayMethodAll, irElementCmp("", OpStartsWith, "a")),
		tags(ArrayMethodAll, irElementCmp("", OpNotMissing, nil)),
		count("irTags", OpEq, 0),
		count("irTags", OpGt, 1),
		count("irTags", OpGe, 1),
		count("irTags", OpNe, 2),
		count("irTags", OpLt, 2),
		count("irTags", OpLe, 1),
		nums(ArrayMethodAny, irElementCmp("", OpGt, int64(1))),
		nums(ArrayMethodAll, irElementCmp("", OpGt, int64(0))),
		nums(ArrayMethodAny, irElementCmp("", OpEq, int64(0))),
		count("irNums", OpEq, 3),
		items(ArrayMethodAny, irElementCmp("qty", OpGt, int64(0))),
		items(ArrayMethodAll, irElementCmp("qty", OpGt, int64(0))),
		items(ArrayMethodAny, irElementCmp("qty", OpMissing, nil)),
		items(ArrayMethodAll, irElementCmp("qty", OpNotMissing, nil)),
		count("irItems", OpGe, 1),
		items(ArrayMethodAny, &ArrayPredicateExpression{Field: irElementField("tags"), Method: ArrayMethodAny, Pred: irElementCmp("", OpEq, "x")}),
		// A row comparison inside an element predicate, correlated to the
		// outer row.
		tags(ArrayMethodAny, &LogicalExpression{Op: LogicalAnd, Left: irElementCmp("", OpEq, "a"), Right: irPayloadCmp("irS", OpMissing, nil)}),
	)

	out = append(out,
		&LogicalExpression{Op: LogicalOr, Left: irPayloadCmp("irS", OpEq, "a"), Right: irPayloadCmp("irN", OpGt, int64(0))},
		&LogicalExpression{Op: LogicalAnd, Left: irPayloadCmp("irS", OpNe, ""), Right: &NotExpression{Target: irPayloadCmp("irS", OpLt, "b")}},
		&LogicalExpression{Op: LogicalOr, Left: tags(ArrayMethodAll, irElementCmp("", OpEq, "a")), Right: irPayloadCmp("irTags", OpMissing, nil)},
	)
	return out
}

// irSeededRows seeds the fixtures under a run-unique owner and reads them
// back as stored.
func irSeededRows(t *testing.T, ctx context.Context, db *bun.DB) (string, []memorynodes.MemoryNode, map[string]string) {
	t.Helper()
	owner := fmt.Sprintf("irx:%s-%d", uniqueSuffix("agree"), time.Now().UnixNano())
	names := map[string]string{}
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for i, fx := range irAgreementFixtures() {
		require.True(t, json.Valid([]byte(fx.payload)), "fixture %q", fx.name)
		id := fmt.Sprintf("%s:%s-%02d", irAgreementConcept, strings.TrimPrefix(owner, "irx:"), i)
		node := &memorynodes.MemoryNode{
			ID:         id,
			Concept:    irAgreementConcept,
			CreatedBy:  owner,
			CreatedAt:  base.Add(time.Duration(i) * time.Second),
			Payload:    json.RawMessage(fx.payload),
			Provenance: json.RawMessage(`{"kind":"direct","name":"ir-agreement"}`),
		}
		_, err := db.NewInsert().Model(node).On(`CONFLICT (id, "createdAt") DO NOTHING`).Exec(ctx)
		require.NoError(t, err, "seed %s", fx.name)
		names[id] = fx.name
	}
	var rows []memorynodes.MemoryNode
	require.NoError(t, db.NewSelect().Model(&rows).
		Where("concept = ?", irAgreementConcept).
		Where(`"createdBy" = ?`, owner).
		Scan(ctx))
	require.Len(t, rows, len(irAgreementFixtures()), "every fixture must read back exactly once")
	return owner, rows, names
}

// irSQLMatches runs a compiled fragment RAW over the seeded rows.
func irSQLMatches(t *testing.T, ctx context.Context, db *bun.DB, owner string, compiled compiledExpression) ([]string, error) {
	t.Helper()
	args := append([]any{irAgreementConcept, owner}, compiled.args...)
	var ids []string
	err := db.NewRaw(`SELECT id FROM "MemoryNodes" WHERE concept = ? AND "createdBy" = ? AND (`+compiled.sql+`)`, args...).Scan(ctx, &ids)
	sort.Strings(ids)
	return ids, err
}

// irInProcessMatches runs the in-process twin over the stored rows.
func irInProcessMatches(t *testing.T, rows []memorynodes.MemoryNode, expr ExpressionNode) []string {
	t.Helper()
	cache := map[string]map[string]any{}
	var ids []string
	for _, row := range rows {
		match, err := nodeMatchesExpression(row, expr, cache)
		require.NoError(t, err, "in process: %s", canonicalExpression(expr))
		if match {
			ids = append(ids, row.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func irNamed(ids []string, names map[string]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, names[id])
	}
	sort.Strings(out)
	return out
}

func irDifference(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func TestIRPushdownAgreesWithTheInProcessTwin(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	owner, rows, names := irSeededRows(t, ctx, db)

	scope := &LogicalExpression{Op: LogicalAnd,
		Left:  irConceptEq(irAgreementConcept),
		Right: &ComparisonExpression{Field: FieldReference{Raw: "createdBy", Parts: []string{"createdBy"}}, Operator: OpEq, Value: owner},
	}

	predicates := irAgreementPredicates()
	// A matrix that silently shrank would still be green, so its size is
	// pinned from below.
	require.GreaterOrEqual(t, len(predicates), 80, "the agreement matrix shrank to %d predicates", len(predicates))

	checked := 0
	for _, base := range predicates {
		for _, pred := range []ExpressionNode{base, &NotExpression{Target: base}} {
			label := canonicalExpression(pred)
			compiled, ok := eng.tryCompileCombinedFilter(ctx, pred, irAgreementConcept)
			if !ok {
				t.Errorf("%s: does not compile to SQL", label)
				continue
			}
			sqlIDs, err := irSQLMatches(t, ctx, db, owner, compiled)
			if err != nil {
				t.Errorf("%s: SQL failed: %v\n  SQL: %s", label, err, compiled.sql)
				continue
			}
			inProcess := irInProcessMatches(t, rows, pred)
			if strings.Join(sqlIDs, ",") != strings.Join(inProcess, ",") {
				t.Errorf("%s: the SQL and the in-process twin DISAGREE\n  only SQL:        %v\n  only in process: %v\n  SQL: %s",
					label, irNamed(irDifference(sqlIDs, inProcess), names), irNamed(irDifference(inProcess, sqlIDs), names), compiled.sql)
				continue
			}

			// The executor path: the combined scan plus the post-filter must
			// return exactly the agreed set.
			set, err := eng.evaluateExpressionSetWithContext(ctx, &LogicalExpression{Op: LogicalAnd, Left: scope, Right: pred}, nil, 0, nil, irAgreementConcept)
			if err != nil {
				t.Errorf("%s: executor path failed: %v", label, err)
				continue
			}
			executed := make([]string, 0, len(set))
			for id := range set {
				executed = append(executed, id)
			}
			sort.Strings(executed)
			if strings.Join(executed, ",") != strings.Join(inProcess, ",") {
				t.Errorf("%s: the executor returned %v, both halves agreed on %v", label, irNamed(executed, names), irNamed(inProcess, names))
			}
			checked++
		}
	}
	if !t.Failed() {
		require.Equal(t, 2*len(predicates), checked, "every predicate, bare and negated, must have been checked")
	}
	t.Logf("agreement held for %d predicate forms (%d predicates, each bare and negated) over %d fixture rows",
		checked, len(predicates), len(rows))
}

// NEGATIVE CONTROL for the NOT spelling: a bare SQL NOT over a three-valued
// operand DISAGREES with the in-process negation on the rows where the operand
// is NULL. If this ever agreed, the COALESCE in compileNotSQL would be
// decoration and the matrix above would not be evidence for it.
//
// Which rows those are is worth knowing: under the typed guard the operand is
// NULL only for a MISSING key (jsonb_typeof of SQL NULL is NULL). A JSON-null
// value has jsonb_typeof 'null' -- a real text -- so the guard is plainly
// FALSE there, a bare NOT already keeps that row, and it is not in the
// difference.
func TestIRNegationWithoutCoalesceWouldDisagree(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	owner, rows, names := irSeededRows(t, ctx, db)

	operand := irPayloadCmp("irS", OpEq, "a")
	inner, ok := eng.tryCompileCombinedFilter(ctx, operand, irAgreementConcept)
	require.True(t, ok)
	bare := compiledExpression{sql: "(NOT " + inner.sql + ")", args: inner.args}
	sqlIDs, err := irSQLMatches(t, ctx, db, owner, bare)
	require.NoError(t, err)
	inProcess := irInProcessMatches(t, rows, &NotExpression{Target: operand})

	missing := irNamed(irDifference(inProcess, sqlIDs), names)
	require.NotEmpty(t, missing, "a bare NOT must drop the missing-key rows the in-process twin keeps")
	require.Contains(t, missing, "every field absent")
	require.NotContains(t, missing, "every field JSON null", "the typed guard is FALSE, not NULL, for a JSON null")
	require.Empty(t, irDifference(sqlIDs, inProcess), "and it adds none")
}

// The matrix above is only evidence if its predicates actually SELECT: a
// harness whose every predicate matched nothing would agree perfectly. So a
// few rows the absence table decides are pinned by name, and the matrix must
// produce many distinct row sets.
func TestIRAgreementMatrixIsNotDegenerate(t *testing.T) {
	_, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	_, rows, names := irSeededRows(t, ctx, db)

	holds := func(expr ExpressionNode, include []string, exclude []string) {
		t.Helper()
		got := irNamed(irInProcessMatches(t, rows, expr), names)
		for _, name := range include {
			require.Contains(t, got, name, "%s must select %q", canonicalExpression(expr), name)
		}
		for _, name := range exclude {
			require.NotContains(t, got, name, "%s must not select %q", canonicalExpression(expr), name)
		}
	}
	holds(irPayloadCmp("irS", OpEq, ""),
		[]string{"every field absent", "every field JSON null", "irS empty"}, []string{"irS space", "irS a"})
	holds(irPayloadCmp("irS", OpNe, ""),
		[]string{"irS space", "irS a", "irS number zero"}, []string{"every field absent", "irS empty"})
	holds(irPayloadCmp("irS", OpLt, "a"),
		[]string{"irS E", "irS space", "irS empty"}, []string{"irS e", "irS e-acute", "every field absent", "irS number zero"})
	holds(irPayloadCmp("irN", OpEq, int64(1)),
		[]string{"irS b"}, []string{"irN a string one", "irN malformed"})
	holds(irPayloadCmp("irS", OpEq, "1"),
		nil, []string{"irS a number one"})
	holds(irPayloadCmp("irB", OpEq, true),
		[]string{"irS b"}, []string{"irB a string true", "irB malformed"})
	holds(&ArrayPredicateExpression{Field: irPayloadField("irTags"), Method: ArrayMethodAll, Pred: irElementCmp("", OpEq, "a")},
		[]string{"every field absent", "every field JSON null", "irTags empty", "irTags a"}, []string{"irTags a b", "irTags a non-array scalar"})

	distinct := map[string]bool{}
	for _, pred := range irAgreementPredicates() {
		distinct[strings.Join(irInProcessMatches(t, rows, pred), ",")] = true
	}
	require.GreaterOrEqual(t, len(distinct), 30, "the matrix must select many different row sets, got %d", len(distinct))
}

// THE `== ""` RULE'S ONE TREE CONSUMER THAT CHANGES ANSWER (memql#5366).
//
// Two filters in the tree test a field against "": workApprovalsForOwner
// (`decision == ""`) and authSessionsForSelf (`revokedAt == ""`). The plan
// assumed every writer stamps both fields, leaving stored rows' answers
// unchanged. That holds for approvals -- createWorkApproval stamps
// `decision: ""` and every Go writer goes through it -- and does NOT hold for
// sessions: createAuthSession stamps `revokedReason: ""` but never
// `revokedAt`, which has no default. So under the old rule (`== ""` excludes
// an absent field) authSessionsForSelf could not return a single session that
// had never been revoked -- the identity service's "my sessions" page listed
// nothing -- and under the edition-2026 rule it returns exactly the live ones,
// which is what its doc comment has always said it does.
func TestEqualsEmptyString_AuthSessionsForSelfListsTheLiveSessions(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	user := "v1:identity:user:" + id.NewShortId()
	writeCtx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(nil, user))
	session := func(expiresAt time.Time) string {
		t.Helper()
		sessionID := "v1:identity:authSession:" + id.NewShortId()
		_, err := eng.Execute(writeCtx, fmt.Sprintf(
			`createAuthSession(sessionId: %q, subject: %q, tokenHash: %q, source: "bff_exchange", userId: %q, expiresAt: %q)`,
			sessionID, user, id.NewShortId(), user, expiresAt.UTC().Format(time.RFC3339),
		))
		require.NoError(t, err)
		return sessionID
	}
	live := session(time.Now().Add(time.Hour))
	revoked := session(time.Now().Add(time.Hour))
	expired := session(time.Now().Add(-time.Hour))
	_, err := eng.Execute(writeCtx, fmt.Sprintf(`revokeAuthSession(sessionId: %q, revokedReason: "user_action")`, revoked))
	require.NoError(t, err)

	res, err := eng.Execute(auth.ContextWithUserActor(nil, user), `query authSessionsForSelf()`)
	require.NoError(t, err)
	require.NotNil(t, res)
	got := map[string]bool{}
	if res.Bundle != nil {
		for _, node := range res.Bundle.Nodes {
			if node != nil {
				got[node.Id] = true
			}
		}
	}
	require.True(t, got[live], "a session that was never revoked has no revokedAt key, and `revokedAt == \"\"` must match it")
	require.False(t, got[revoked], "a revoked session carries a revokedAt stamp and must be excluded")
	require.False(t, got[expired], "an expired session must be excluded by `expiresAt > now`")
}

// irLocaleCollations is where the collation control looks for a LOCALE
// collation when the database's own default already orders by byte. A fixed
// list, so the only text interpolated into the control's SQL is a constant
// from this file.
var irLocaleCollations = []string{"en-US-x-icu", "und-x-icu", "en_US.utf8", "en_US.UTF-8", "en_US"}

// NEGATIVE CONTROL for the collation: a locale ordering disagrees with Go's
// byte order on these fixtures, so without `COLLATE "C"` the SQL half would
// order strings differently from the in-process half.
//
// The uncollated fragment -- what the pushdown emitted before -- inherits the
// database's DEFAULT collation. Where that default is itself byte order (a
// "C" database, like the throwaway one this lane runs against), the
// uncollated SQL cannot show the hazard, so the control spells the locale
// explicitly: it is then asking what the old fragment did on a database
// initialised under that locale, which is the database the collation clause
// protects. It skips only when no locale collation is installed at all.
func TestIRStringOrderingWithoutCollationWouldDisagree(t *testing.T) {
	_, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()

	var defaultCollation string
	require.NoError(t, db.NewRaw(`SELECT datcollate FROM pg_database WHERE datname = current_database()`).Scan(ctx, &defaultCollation))

	// The control differs from production in the collation and NOTHING
	// else: the compiled fragment with `COLLATE "C"` removed (so the database
	// default applies) or swapped for a locale. A control that also dropped
	// the typed guard would disagree on the mistyped rows and prove nothing
	// about the collation.
	compiled, err := compilePayloadComparison([]string{"irS"}, OpLt, "a")
	require.NoError(t, err)
	require.Contains(t, compiled.sql, ` COLLATE "C"`)
	fragment := strings.Replace(compiled.sql, ` COLLATE "C"`, "", 1)
	under := fmt.Sprintf("the database default %q", defaultCollation)
	switch strings.ToUpper(strings.TrimSpace(defaultCollation)) {
	case "C", "POSIX", "C.UTF-8", "C.UTF8", "UCS_BASIC":
		var available []string
		require.NoError(t, db.NewRaw(`SELECT collname FROM pg_collation WHERE collname IN (?)`, bun.In(irLocaleCollations)).Scan(ctx, &available))
		chosen := ""
		for _, candidate := range irLocaleCollations {
			for _, name := range available {
				if name == candidate && chosen == "" {
					chosen = candidate
				}
			}
		}
		if chosen == "" {
			t.Skipf("the database orders by byte by default (%q) and has none of %v installed, so no locale "+
				"ordering is available to disagree with (the agreement matrix still ran)", defaultCollation, irLocaleCollations)
		}
		fragment = strings.Replace(compiled.sql, ` COLLATE "C"`, fmt.Sprintf(" COLLATE %q", chosen), 1)
		under = fmt.Sprintf("the locale collation %q", chosen)
	}

	owner, rows, names := irSeededRows(t, ctx, db)
	sqlIDs, err := irSQLMatches(t, ctx, db, owner, compiledExpression{sql: fragment, args: []any{"a"}})
	require.NoError(t, err)
	inProcess := irInProcessMatches(t, rows, irPayloadCmp("irS", OpLt, "a"))
	require.NotEqual(t, sqlIDs, inProcess,
		"under %s the ordering must disagree with byte order somewhere in the fixtures (only SQL %v, only in process %v)",
		under, irNamed(irDifference(sqlIDs, inProcess), names), irNamed(irDifference(inProcess, sqlIDs), names))
	t.Logf("under %s: only SQL %v, only in process %v", under,
		irNamed(irDifference(sqlIDs, inProcess), names), irNamed(irDifference(inProcess, sqlIDs), names))
}
