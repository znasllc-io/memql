package memql

import (
	"github.com/stretchr/testify/require"
	"testing"
)

// Public assertion and private account reads must narrow the latest-version
// scan to their indexed parent/digest before fetching public-key payloads.
func TestCallsAccountLookupsDoNotEnumerateOtherKeys(t *testing.T) {
	for _, tc := range []struct{ concept, field, value string }{
		{"v1:calls:credential", "credentialDigest", "digest"},
		{"v1:calls:credential", "keyId", "v1:calls:key:one"},
		{"v1:calls:seat", "credentialId", "one-credential"},
		{"v1:calls:seat", "keyId", "v1:calls:key:one"},
		{"v1:calls:history", "keyId", "v1:calls:key:one"},
	} {
		t.Run(tc.concept+"/"+tc.field, func(t *testing.T) {
			expr := &LogicalExpression{Op: LogicalAnd, Left: irConceptEq(tc.concept), Right: irPayloadCmp(tc.field, OpEq, tc.value)}
			sql := latestScanKeysSQL(t, expr, nil)
			require.Contains(t, sql, "payload->>'"+tc.field+"'")
			require.Contains(t, sql, "= '"+tc.value+"'")
		})
	}
}
