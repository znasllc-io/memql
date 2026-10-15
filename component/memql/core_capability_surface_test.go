package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Identity, edge and workbench load the embedded engine without a product
// bundle. Audit the actual tree they boot, not a registry fixture that supplies
// the product's Canvas tool and hides the missing dependency.
func TestCoreMachineCapabilitiesLoadWithoutProductCanvas(t *testing.T) {
	registry := newToolRegistry()
	_, err := LoadUnifiedTools(nil, registry)
	require.NoError(t, err)
	require.False(t, registry.Has("canvasPublish"))
	require.Empty(t, VerifyCapabilityToolsRegistered(registry.Has, nil))
	for _, slug := range []string{"computer-use-headless", "computer-use-embodied", "workbench-use"} {
		for _, name := range ExpandCapabilitySlugs([]string{slug}) {
			require.True(t, registry.Has(HarnessToolBinding(name)), "%s advertises missing tool %s", slug, name)
		}
	}
	seeds := NewSeedRegistry()
	_, err = LoadUnifiedSeeds(nil, seeds)
	require.NoError(t, err)
	seed, ok := seeds.Get("operator-computer-use")
	require.True(t, ok)
	row := buildArgsFromBody(seed.Body, seed.UseConcept, seed.Name, "")
	require.NotContains(t, row["toolSlugs"], "canvasPublish")
	// A product may still grant its presentation tool explicitly; removing
	// the core dependency must not remove tools the product actually declares.
	require.Contains(t, ExpandCapabilitySlugs([]string{"workbench-use", "canvasPublish"}), "canvasPublish")
}
