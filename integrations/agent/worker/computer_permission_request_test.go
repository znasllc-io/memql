//go:build agent

package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestComputerScopeNeverDefaultsToBroadSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
	}{
		{"omitted operation", nil},
		{"null operation", map[string]any{"operation": nil}},
		{"empty operation", map[string]any{"operation": map[string]any{}}},
		{"unknown session", map[string]any{"sessionKind": "read"}},
		{"directory disguised as session", map[string]any{"sessionKind": "fs_list"}},
		{"read-only write session", map[string]any{"sessionKind": "file_write"}},
		{"both contracts", map[string]any{"sessionKind": "desktop", "operation": map[string]any{"tool": "workerHost", "action": "fs_list", "args": map[string]any{"path": "/home/person"}, "directory": "/home/person"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			i := &Integration{engine: &memql.MemQLEngine{}, ScopeRequester: func(context.Context, string, string, map[string]any) (string, error) {
				writes++
				return "", errors.New("unexpected approval")
			}}
			ctx := common.ContextWithRun(asPerson(ownerA), common.RunContext{OwnerUserId: ownerA, RunId: "run", GoalId: "goal"})
			args := map[string]any{"intent": "List home", "requestedScope": "observe", "summary": "List /home/person", "agentId": "agent"}
			for k, v := range tc.extra {
				args[k] = v
			}
			_, err := i.handleRequestScope(ctx, args, 0)
			require.Error(t, err)
			require.Zero(t, writes, "an incomplete operation must not create an approval or park the run")
		})
	}
}

func TestExplicitComputerSessionCannotReplaceDirectoryOrCommandPermission(t *testing.T) {
	for _, kind := range []string{"desktop", "file_write", "http", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			req := approvedRequest()
			req.RequireLabels = map[string]string{"os": "darwin"}
			now := fleetNow()
			row := grantRow(req, now.Add(time.Hour))
			subject := row["subject"].(map[string]any)
			subject["sessionKind"], subject["sessionScope"] = kind, "full"
			delete(subject, "scope")
			row["artifactHash"] = work.ArtifactHash(subject)
			for _, action := range []string{"fs_read", "fs_list", "fs_stat", "exec", "fs_write", "http_fetch", "screenshot"} {
				req.Tool, req.Action = "workerHost", action
				if action == "screenshot" {
					req.Tool = "workerComputer"
				}
				allowed := kind == "desktop" && action == "screenshot" || kind == "file_write" && action == "fs_write" || kind == "http" && action == "http_fetch"
				if allowed {
					require.Equal(t, "full", approvedComputerScope([]map[string]any{row}, req, now), action)
				} else {
					require.Empty(t, approvedComputerScope([]map[string]any{row}, req, now), action)
				}
			}
		})
	}
}
