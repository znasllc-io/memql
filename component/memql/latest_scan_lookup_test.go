package memql

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// latestScanKeysSQL renders the keys query latestScanKeys builds for expr.
// Building a bun query opens no connection, so no database is needed.
func latestScanKeysSQL(t *testing.T, expr ExpressionNode, asOf *time.Time) string {
	t.Helper()
	e := &MemQLEngine{db: bun.NewDB(sql.OpenDB(pgdriver.NewConnector()), pgdialect.New())}
	keys := e.latestScanKeys(expr, asOf)
	if keys == nil {
		return ""
	}
	return keys.String()
}

// WHICH TERMS MAY NARROW THE ENUMERATION. Only a required equality or membership
// on a payload field a lookup index serves for the concept, with
// a value that is SET -- a lookup finds the versions carrying a value, and an
// unset field is carried by every version written before it was set, so the
// versions "lacking" a value are history, not matches.
func TestLatestScanKeysLookUpOnlyMandatorySetTermsOnALookupField(t *testing.T) {
	recipient := irConceptEq("v1:campaigns:recipient")
	digest := irPayloadCmp("emailDigest", OpEq, "d1")
	audience := irPayloadCmp("audienceId", OpEq, "v1:campaigns:audience:a")
	and := func(l, r ExpressionNode) ExpressionNode { return &LogicalExpression{Op: LogicalAnd, Left: l, Right: r} }
	or := func(l, r ExpressionNode) ExpressionNode { return &LogicalExpression{Op: LogicalOr, Left: l, Right: r} }

	for name, tc := range map[string]struct {
		expr ExpressionNode
		want []string
	}{
		"a lookup field":                  {and(recipient, digest), []string{`((payload->>'emailDigest') = 'd1')`}},
		"an Azure provider receipt":       {and(irConceptEq("v1:email:sendOperation"), irPayloadCmp("providerMessageId", OpEq, "message-id")), []string{`((payload->>'providerMessageId') = 'message-id')`}},
		"beside another equality":         {and(and(recipient, audience), digest), []string{`((payload->>'emailDigest') = 'd1')`, `((payload->>'audienceId') = 'v1:campaigns:audience:a')`}},
		"a whitespace value is a value":   {and(recipient, irPayloadCmp("emailDigest", OpEq, " ")), []string{`((payload->>'emailDigest') = ' ')`}},
		"under a disjunction":             {and(recipient, or(digest, audience)), nil},
		"under a negation":                {and(recipient, irNot(digest)), nil},
		"an inequality":                   {and(recipient, irPayloadCmp("emailDigest", OpNe, "d1")), nil},
		"the empty string is unset":       {and(recipient, irPayloadCmp("emailDigest", OpEq, "")), nil},
		"nil is unset":                    {and(recipient, irPayloadCmp("emailDigest", OpEq, nil)), nil},
		"one member":                      {and(recipient, irPayloadCmp("emailDigest", OpIn, []any{"d1"})), []string{`((payload->>'emailDigest') = 'd1')`}},
		"multiple members":                {and(recipient, irPayloadCmp("emailDigest", OpIn, []any{"d1", "d2"})), []string{`((payload->>'emailDigest') IN ('d1', 'd2'))`}},
		"string slice members":            {and(recipient, irPayloadCmp("emailDigest", OpIn, []string{"d1", "d2"})), []string{`((payload->>'emailDigest') IN ('d1', 'd2'))`}},
		"members under a disjunction":     {and(recipient, or(irPayloadCmp("emailDigest", OpIn, []any{"d1"}), audience)), nil},
		"members under a negation":        {and(recipient, irNot(irPayloadCmp("emailDigest", OpIn, []any{"d1"}))), nil},
		"excluded members":                {and(recipient, irPayloadCmp("emailDigest", OpOut, []any{"d1"})), nil},
		"unset among members":             {and(recipient, irPayloadCmp("emailDigest", OpIn, []any{"d1", ""})), nil},
		"nil among members":               {and(recipient, irPayloadCmp("emailDigest", OpIn, []any{"d1", nil})), nil},
		"mixed member types":              {and(recipient, irPayloadCmp("emailDigest", OpIn, []any{"d1", 5})), nil},
		"no members":                      {and(recipient, irPayloadCmp("emailDigest", OpIn, []any{})), nil},
		"a number":                        {and(recipient, irPayloadCmp("emailDigest", OpEq, 5)), nil},
		"a nested path":                   {and(recipient, irPayloadCmp("profile.emailDigest", OpEq, "d1")), nil},
		"no lookup index for the field":   {and(recipient, audience), nil},
		"no lookup index for the concept": {and(irConceptEq("v1:campaigns:audience"), digest), nil},
	} {
		t.Run(name, func(t *testing.T) {
			keys := latestScanKeysSQL(t, tc.expr, nil)
			require.NotEmpty(t, keys, "a concept-bound read still enumerates its latest keys")
			if tc.want == nil {
				require.NotContains(t, keys, "payload", "history enumeration must not read historical JSON values without a lookup index")
				return
			}
			for _, term := range tc.want {
				require.Contains(t, keys, term)
			}
		})
	}
	require.Empty(t, latestScanKeysSQL(t, digest, nil), "without a mandatory concept there is nothing to enumerate")
}

// The lookup answers asOf the way the concept-wide collapse did: the ids
// carrying the value at or before asOf, each at its latest version at or
// before asOf.
func TestLatestScanKeysLookupHonoursAsOf(t *testing.T) {
	asOf := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	keys := latestScanKeysSQL(t, &LogicalExpression{Op: LogicalAnd, Left: irConceptEq("v1:campaigns:recipient"), Right: irPayloadCmp("emailDigest", OpEq, "d1")}, &asOf)
	require.Contains(t, keys, `(payload->>'emailDigest') = 'd1'`)
	require.Equal(t, 2, strings.Count(keys, `"createdAt" <= `), "both the lookup and the latest-version probe stop at asOf: %s", keys)
}
