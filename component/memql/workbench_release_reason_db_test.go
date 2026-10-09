package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// releaseWorkspaceOnRunTerminal writes the reason declared by the workspace
// concept. Drive the real mutation through the engine and verify the appended
// row, so an enum mismatch cannot pass on a fake executor alone.
func TestWorkbenchRunTerminalReleasesWorkspaceInDatabase(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("workbench-run-terminal")
	owner := "workbench-owner-" + suffix
	workspaceId := "workspace-" + suffix
	ctx := auth.ContextWithUserActor(context.Background(), owner)

	storedId := runMutation(t, ctx, eng, "provisionWorkspace", map[string]any{
		"workspaceId": workspaceId,
		"runId":       "run-" + suffix,
		"storageRoot": "/tmp/" + workspaceId,
		"nodeId":      "workbench-test-node",
	})
	require.Equal(t, "v1:workbench:workspace:"+workspaceId, storedId)

	runMutation(t, ctx, eng, "releaseWorkspace", map[string]any{
		"workspaceId": workspaceId,
		"reason":      "run_terminal",
	})

	row := latestPayload(t, context.Background(), db, "v1:workbench:workspace", storedId)
	require.Equal(t, "released", row["status"])
	require.Equal(t, "run_terminal", row["releasedReason"])
	require.NotEmpty(t, row["releasedAt"])
}
