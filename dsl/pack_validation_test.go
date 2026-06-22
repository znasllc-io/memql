package dsl_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/dsl"
)

// TestValidatePackDomain exercises the pure namespace-ownership rule
// without touching the global plugin registry, so the four outcomes are
// deterministic regardless of test ordering or what other tests have
// registered.
func TestValidatePackDomain(t *testing.T) {
	core := []string{"cognition", "common", "providers"}
	existing := []string{"copresent", "acmepack"}

	t.Run("fresh unique domain accepted", func(t *testing.T) {
		require.NoError(t, dsl.ValidatePackDomain("widgets", core, existing))
	})

	t.Run("core domain collision rejected", func(t *testing.T) {
		err := dsl.ValidatePackDomain("cognition", core, existing)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "core embedded domain")
		assert.Contains(t, err.Error(), "cognition")
	})

	t.Run("duplicate plugin domain rejected", func(t *testing.T) {
		err := dsl.ValidatePackDomain("copresent", core, existing)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already registered")
		assert.Contains(t, err.Error(), "copresent")
	})

	t.Run("empty domain rejected", func(t *testing.T) {
		require.Error(t, dsl.ValidatePackDomain("", core, existing))
	})

	t.Run("whitespace-only domain rejected", func(t *testing.T) {
		require.Error(t, dsl.ValidatePackDomain("   ", core, existing))
	})

	t.Run("slash domain rejected", func(t *testing.T) {
		err := dsl.ValidatePackDomain("a/b", core, existing)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not contain '/'")
	})

	t.Run("surrounding whitespace trimmed before checks", func(t *testing.T) {
		// "  cognition  " trims to a core domain and must be rejected as
		// a collision, not accepted as a distinct name.
		err := dsl.ValidatePackDomain("  cognition  ", core, existing)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "core embedded domain")
	})

	t.Run("empty core and existing sets accept any sane domain", func(t *testing.T) {
		require.NoError(t, dsl.ValidatePackDomain("anything", nil, nil))
	})
}
