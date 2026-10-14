package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkContextRecoveryReserveComesFromInstalledDSL(t *testing.T) {
	load := func(source string) workflowhost.Loader {
		return func(string) (*automations.Automation, error) {
			return automations.NewLoader(automations.LoaderOptions{}).CompileSource(source, "context-recovery.memql")
		}
	}
	reserve, err := workContextRecoveryReserve(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 3, reserve)
	reserve, err = workContextRecoveryReserve(context.Background(), load(`@template automation agentWorkContextRecovery { return 2 }`))
	require.NoError(t, err)
	require.Equal(t, 2, reserve, "the installed recipe chooses the preference, not a native constant")
	for _, value := range []string{"-1", "1.5", `"unlimited"`, "null"} {
		_, err := workContextRecoveryReserve(context.Background(), load(`@template automation agentWorkContextRecovery { return `+value+` }`))
		require.Error(t, err, value)
	}
}

type contextReserveCaptureEngine struct {
	workPromptEngine
	reserve int
	run     common.RunContext
}

func (e *contextReserveCaptureEngine) CompactWorkContext(ctx context.Context, messages []common.ChatMessage, _ []common.ToolDefinition, _ int) ([]common.ChatMessage, error) {
	e.reserve = memql.WorkContextRecoveryReserve(ctx)
	e.run, _ = common.RunFromContext(ctx)
	return messages, nil
}

func TestWorkContextRecoveryReserveReachesTheOwnedCompactor(t *testing.T) {
	e := &contextReserveCaptureEngine{}
	r := newTestReplier(e)
	run := common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner", StepKey: "step"}
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), run.OwnerUserId), run)
	messages := []common.ChatMessage{{Role: "user", Content: "Keep my instructions"}}
	got, err := r.compactWorkContext(ctx, messages, nil, 100)
	require.NoError(t, err)
	require.Equal(t, messages, got)
	require.Equal(t, 3, e.reserve)
	require.Equal(t, run, e.run)
}
