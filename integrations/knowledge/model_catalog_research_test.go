package knowledge

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
	"strings"
	"testing"
)

func TestCatalogResearchReportRemainsEvidence(t *testing.T) {
	raw := `{"summary":"One candidate; capacity still needs verification.","findings":[{"modelId":"new-embed","category":"embeddings","runtime":"cloud","summary":"Hosted embedding model.","sources":["https://example.org/model"]}]}`
	r, err := parseCatalogResearchReport(raw)
	require.NoError(t, err)
	require.Equal(t, "cloud", r.Findings[0].Runtime)
	_, err = parseCatalogResearchReport(strings.ReplaceAll(raw, "https://example.org/model", "javascript:alert(1)"))
	require.Error(t, err)
	_, err = parseCatalogResearchReport(`{"summary":"Claim","findings":[{"modelId":"x","category":"text","runtime":"ollama","summary":"Claim","sources":[]}]}`)
	require.Error(t, err)
	_, err = parseCatalogResearchReport(strings.Repeat("x", 65537))
	require.Error(t, err)
}

func TestCatalogResearchCannotBypassLocalRunCeiling(t *testing.T) {
	i := &Integration{}
	for _, rc := range []common.RunContext{{}, {RunId: "run", GoalId: "goal", Routing: common.RouteChoice{Source: "fleet:strongest"}}, {RunId: "run", GoalId: "goal", Routing: common.RouteChoice{Source: "federation:strongest", NoPaidProviders: true}}} {
		_, err := i.handleModelCatalogResearchTurn(common.ContextWithRun(context.Background(), rc), nil, 0)
		require.ErrorContains(t, err, "local-only")
	}
}
