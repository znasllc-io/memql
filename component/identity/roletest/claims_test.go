package roletest

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIntegerClaimUsesDecodedJWTNumbers(t *testing.T) {
	var claims map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"exp":1792100000,"revocation_epoch":1}`), &claims))
	for _, value := range []any{claims["exp"], int64(1792100000), int(1792100000), json.Number("1792100000")} {
		n, ok := integerClaim(value)
		require.True(t, ok)
		require.Equal(t, int64(1792100000), n)
	}
	n, ok := integerClaim(claims["revocation_epoch"])
	require.True(t, ok)
	require.Equal(t, int64(1), n)
	for _, value := range []any{nil, "1792100000", true, 1.5, math.NaN(), math.Inf(1), float64(math.MaxInt64), json.Number("1.5"), json.Number("9223372036854775808")} {
		_, ok := integerClaim(value)
		require.False(t, ok, "%v", value)
	}
}
