package memql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapabilityAuditResolvesCoreToolsDespiteBundleCollisions(t *testing.T) {
	registry := newToolRegistry()
	for _, origin := range []string{"worker/tools.memql", "testproduct/tools.memql"} {
		for _, name := range []string{"workerHost", "workerComputer", "workerStatus", "requestComputerUseScope"} {
			require.NoError(t, registry.Upsert(&Tool{Name: name, Origin: origin}))
		}
	}
	require.NoError(t, registry.Upsert(&Tool{Name: "canvasPublish", Origin: "canvas/tools.memql"}))
	_, err := registry.Get("workerHost")
	require.ErrorContains(t, err, "declared in 2 namespaces")
	missing := VerifyCapabilityToolsRegistered(registry.Has, nil)
	for _, name := range []string{"workerHost", "workerComputer", "workerStatus", "requestComputerUseScope"} {
		require.NotContains(t, missing, name)
	}
	// A product namesake cannot make the single-tool core workbench surface
	// available. A missing worker primitive still makes its bundle incomplete.
	require.NoError(t, registry.Upsert(&Tool{Name: "workbenchHost", Origin: "testproduct/tools.memql"}))
	require.Contains(t, UnavailableCapabilitySlugs(registry.Has), "workbench-use")
	partial := newToolRegistry()
	require.NoError(t, partial.Upsert(&Tool{Name: "workerStatus", Origin: "worker/tools.memql"}))
	require.NoError(t, partial.Upsert(&Tool{Name: "workerHost", Origin: "testproduct/tools.memql"}))
	require.Contains(t, VerifyCapabilityToolsRegistered(partial.Has, nil), "workerHost")
}
