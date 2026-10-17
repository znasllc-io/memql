package work

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCatalogResearchIsOffWithoutExplicitOptIn(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("modelCatalogResearchSettings", map[string]any{"enabled": false, "ownerUserId": "owner"})
	_, err := i.handleOpenModelCatalogResearch(clusterOwnerContext("owner"), nil, 0)
	require.ErrorContains(t, err, "research is off")
	require.Empty(t, eng.callsTo("createWorkGoal"))
	require.Empty(t, eng.callsTo("createWorkRun"))
	_, err = i.handleOpenModelCatalogResearch(callerContext("person"), nil, 0)
	require.Error(t, err)
}
