package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestTextConcatIncludesRefusesCoercingAndUntypedReceivers(t *testing.T) {
	for _, receiver := range []string{
		`row.title + row.email`, `row.title + 1`, `row.title + true`,
		`row.priority + " "`, `row.score + " "`, `row.urgent + " "`,
		`row.tags + " "`, `row.info + " "`, `row.blob + " "`,
		`row.extras.unknown + " "`, `row.id + " "`,
		`row.title + args.name`, `row.title + nil`,
	} {
		t.Run(receiver, func(t *testing.T) {
			_, err := lowerSource(t, lowerTestEnv(t), `row => (`+receiver+`).includes("a", true)`)
			require.Error(t, err)
		})
	}
	env := lowerTestEnv(t)
	env.Concept = nil
	_, err := lowerSource(t, env, `row => (row.title + " " + row.email).includes("a", true)`)
	require.Error(t, err, "the text receiver must have declared field types")
}

func TestTextConcatIncludesCanonicalFormAndBoundLiterals(t *testing.T) {
	const literal = `' OR TRUE -- %_`
	call := `row => (row.title + ` + langparser.QuoteString(literal) + ` + row.email).includes("needle", true)`
	ir, err := lowerSource(t, lowerTestEnv(t), call)
	require.NoError(t, err)
	compiled := irCompile(t, ir)
	require.NotContains(t, compiled.sql, literal)
	require.Equal(t, []any{literal, "needle"}, compiled.args)
	clone := cloneExpressionNode(ir)
	require.Equal(t, canonicalExpression(ir), canonicalExpression(clone))
	other, err := lowerSource(t, lowerTestEnv(t), `row => (row.title + "_" + row.email).includes("needle", true)`)
	require.NoError(t, err)
	require.NotEqual(t, canonicalExpression(ir), canonicalExpression(other), "literal separators are part of the result-cache key")
}

func TestTextConcatIncludesWalkersKeepNestedFieldsAndIsolateRewrites(t *testing.T) {
	ir, err := lowerSource(t, lowerTestEnv(t), `ticket => (ticket.?lineage.planId + " " + ticket.?lineage.runId).includes("needle", true)`)
	require.NoError(t, err)
	original := canonicalExpression(ir)
	usage := newSpecUsage()
	usage.collect(ir)
	require.Equal(t, map[string][]string{
		"lineage.planId": {"lineage", "planId"},
		"lineage.runId":  {"lineage", "runId"},
	}, usage.payloadPaths)
	var refs []string
	walkSpecRefs(ir, func(field FieldReference) { refs = append(refs, strings.Join(field.Parts, ".")) })
	require.Equal(t, []string{"payload.lineage.planId", "payload.lineage.runId"}, refs)
	cloned := cloneExpressionNode(ir)
	require.NoError(t, rewriteSpecFields(cloned, func(field FieldReference) (FieldReference, error) {
		// Deliberately mutate the supplied slice: the mapper must own it.
		field.Parts[1] = "renamed"
		field.Raw = strings.Join(field.Parts, ".")
		return field, nil
	}))
	require.Contains(t, canonicalExpression(cloned), "payload.renamed.planid")
	require.Equal(t, original, canonicalExpression(ir), "rewriting a caller's clone must preserve the registered receiver")
	require.NoError(t, rewriteFilterFieldRefs(cloned))
	require.Equal(t, original, canonicalExpression(ir))
	require.NotContains(t, canonicalExpression(cloned), "$textconcat")
	compiled := irCompile(t, cloned)
	require.Contains(t, compiled.sql, "renamed")
	require.NotContains(t, compiled.sql, "lineage")
}

func TestTextConcatIncludesArgumentExpansionDoesNotMutateCachedReceiver(t *testing.T) {
	withFakePlanConstantEvaluator(t)
	ir, err := lowerSource(t, lowerTestEnv(t), `row => (row.title + " " + row.email).includes(args.status, true)`)
	require.NoError(t, err)
	original := canonicalExpression(ir)
	validator := planConstValidator(nil)
	for _, needle := range []string{"Jane Doe", "Jane%_ D'oe", ""} {
		expanded, err := validator.expandExpressionWithArgs(ir, map[string]any{"status": needle})
		require.NoError(t, err)
		compiled := irCompile(t, expanded)
		if needle == "" {
			require.Equal(t, "FALSE", compiled.sql)
		} else {
			require.Equal(t, []any{" ", needle}, compiled.args)
		}
		require.Equal(t, original, canonicalExpression(ir), "each call must keep its own needle")
	}
}

// The narrow receiver is defined for declared text fields. Exercise valid
// strings, null and absent values against PostgreSQL and both interpreters;
// non-text schema declarations are refused separately, without JSON coercion.
func TestTextConcatIncludesSQLIRAndEvaluatorAgreeDB(t *testing.T) {
	_, db, _ := sharedReadMergeEngine(t)
	rows := []string{
		`{}`, `{"title":null,"email":null}`, `{"title":"","email":""}`,
		`{"title":"Jane","email":"Doe"}`, `{"title":"JANE","email":"DOE"}`,
		`{"title":"Jane%_","email":"D'oe"}`, `{"title":"Jane500","email":"D'oe"}`,
		`{"title":"Crème","email":"BRÛLÉE"}`, `{"title":"Solo"}`, `{"email":"Only"}`,
	}
	for _, needle := range []string{"Jane Doe", "NE%_ D'O", "crème brûlée", "solo ", " only", "", "  ", "missing"} {
		for _, ignoreCase := range []bool{false, true} {
			body := fmt.Sprintf(`(row.title + " " + row.email).includes(%s, %t)`, langparser.QuoteString(needle), ignoreCase)
			for _, source := range []string{"row => " + body, "row => !(" + body + ")"} {
				lambda, err := langparser.ParseV1Lambda(source)
				require.NoError(t, err)
				ir, err := lowerSource(t, lowerTestEnv(t), source)
				require.NoError(t, err)
				compiled := irCompile(t, ir)
				for index, payload := range rows {
					node := memorynodes.MemoryNode{ID: fmt.Sprintf("concat-%d", index), Payload: json.RawMessage(payload)}
					exprRow, err := NewExprRow(node)
					require.NoError(t, err)
					want, err := EvalCondition(context.Background(), lambda.Body, MapScope{"row": exprRow}, EvalOptions{})
					require.NoError(t, err)
					got, err := nodeMatchesExpression(node, ir, map[string]map[string]any{})
					require.NoError(t, err)
					require.Equal(t, want, got, "%s over %s", source, payload)
					var sqlGot bool
					args := append(append([]any(nil), compiled.args...), payload)
					err = db.NewRaw("SELECT COALESCE(("+compiled.sql+"), false) FROM (SELECT ?::jsonb AS payload) AS fixture", args...).Scan(context.Background(), &sqlGot)
					require.NoError(t, err)
					require.Equal(t, want, sqlGot, "%s over %s; SQL %s", source, payload, compiled.sql)
				}
			}
		}
	}
	// A nested field and datetime string use the same declared-text rule.
	for _, receiver := range []string{`row.?lineage.planId + " " + row.?lineage.runId`, `"due " + row.dueAt`} {
		ir, err := lowerSource(t, lowerTestEnv(t), "row => ("+receiver+").includes(\"x\")")
		require.NoError(t, err)
		require.True(t, strings.Contains(canonicalExpression(ir), "textConcat("))
	}
}
