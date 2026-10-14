package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/core/common"
)

func TestCapabilityInstallationSelectsOnlyRegisteredToolKnowledge(t *testing.T) {
	domains, chunks, err := selectCapabilityChunks(map[string]any{"domainIds": []any{"computer-use", "workbench", "work-guidance"}})
	require.NoError(t, err)
	require.Len(t, domains, 3)
	require.NotEmpty(t, chunks)
	for _, chunk := range chunks {
		require.Contains(t, domains, chunk.Domain)
	}
	for _, args := range []map[string]any{
		{}, {"domainIds": []string{}}, {"domainIds": []any{1}},
		{"domainIds": []string{"computer-use", "unknown"}},
		{"domainIds": []string{"computer-use", "computer-use"}},
		{"domainIds": []string{"physics_quantum_mechanics"}},
		{"domainIds": []string{"computer-use"}, "text": "caller publication"},
		{"domainIds": []string{"computer-use"}, "forceIngest": true},
	} {
		_, _, err := selectCapabilityChunks(args)
		require.Error(t, err, "%v", args)
	}
}

func TestCapabilityInstallationRefusesNonOperatorBeforeAnyDependencies(t *testing.T) {
	i := &Integration{}
	owner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})
	for _, ctx := range []context.Context{
		context.Background(), auth.ContextWithUserActor(context.Background(), "reader"),
		common.ContextWithRun(owner, common.RunContext{RunId: "owned-agent-run"}),
	} {
		_, err := i.installCapabilityCorporaHandler(ctx, map[string]any{"domainIds": []string{"computer-use"}}, 0)
		require.ErrorContains(t, err, "requires")
	}
}

func TestCapabilityInstallationRecipeStopsAtFirstFailedChunk(t *testing.T) {
	calls := []string{}
	refused := errors.New("embedding refused")
	_, err := workflowhost.Run(context.Background(), "knowledgeInstallCapabilityCorporaWorkflow", map[string]any{"chunkIds": []string{"first", "failed", "never"}}, workflowhost.Options{Operations: map[string]workflowhost.Operation{
		"knowledgeInstallCapabilityChunk": func(_ context.Context, args map[string]any) (any, error) {
			key := stringArg(args, "chunkId")
			calls = append(calls, key)
			if key == "failed" {
				return nil, refused
			}
			return nil, nil
		},
	}})
	require.ErrorIs(t, err, refused)
	require.Equal(t, []string{"first", "failed"}, calls)
}
