package calls

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsageRemainderStoredBoundsAndLegacyAbsence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		valid bool
		want  int64
	}{
		{"legacy absent", nil, true, 0},
		{"zero", 0, true, 0},
		{"largest fraction", usageUnitsPerCent - 1, true, usageUnitsPerCent - 1},
		{"negative", -1, false, 0},
		{"unsettled whole cent", usageUnitsPerCent, false, 0},
		{"invalid text", "invalid", false, 0},
		{"wrong type", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := map[string]any{"id": "receipt", "siteId": "site", "keyId": "key", "kind": "settlement", "requestDigest": strings.Repeat("f", 64), "createdAt": time.Now().UTC(), "balanceCents": 1000, "creditedCents": 0, "debitedCents": 0}
			if tc.value != nil {
				row["usageRemainderUnits"] = tc.value
			}
			entry, err := simulationWalletFrom(row)
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, tc.want, entry.UsageRemainderUnits)
			} else {
				require.Error(t, err)
			}
		})
	}
}
