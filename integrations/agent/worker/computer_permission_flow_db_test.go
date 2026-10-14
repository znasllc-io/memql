//go:build agent

package worker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"github.com/znasllc-io/memql/core/id"
	. "github.com/znasllc-io/memql/integrations/agent/worker"
	workservice "github.com/znasllc-io/memql/integrations/work"
)

// Real request builder, two engines sharing Postgres, owner decision, durable
// authority and the actual NodeService forwarding envelopes. Only Cockpit's
// final filesystem operation is replaced with a harmless temporary directory.
func TestComputerPermissionDBFullApprovalAndForwardedListing(t *testing.T) {
	for _, mode := range []string{"once", "always"} {
		t.Run(mode, func(t *testing.T) {
			reachable, err := dbtest.EnsureSchema(context.Background())
			require.NoError(t, err)
			if !reachable {
				dbtest.Unreachable(t, "full permission flow", dbtest.DSN(), nil)
				return
			}
			db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
			t.Cleanup(func() { _ = db.Close() })
			gates, err := database.NewGatePool(dbtest.DSN(), 0)
			require.NoError(t, err)
			t.Cleanup(func() { _ = gates.Close() })
			_, err = memql.LoadUnifiedConcepts(nil)
			require.NoError(t, err)
			var engines []*memql.MemQLEngine
			var authority []*workservice.Integration
			for n := 0; n < 2; n++ {
				e, err := memql.New(db)
				require.NoError(t, err)
				require.NoError(t, e.Init(memorynodes.DefaultRegistry()))
				wi := workservice.New(e, slog.New(slog.NewTextHandler(io.Discard, nil)), func() *bun.DB { return db })
				wi.SetGateDB(func() *sql.DB { return gates })
				require.NoError(t, e.RegisterIntegration(wi))
				engines = append(engines, e)
				authority = append(authority, wi)
			}
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("fixture"), 0600))
			calls := 0
			h := NewPermissionFlowHopForTest(t, func(_ context.Context, dispatch *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				calls++
				require.Equal(t, "fs_list", dispatch.GetAction())
				require.NotNil(t, dispatch.GetComputerPermission())
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)
				require.Len(t, entries, 1)
				require.Equal(t, "visible.txt", entries[0].Name())
				return &memqlv1.ToolResult{CallId: dispatch.GetCallId(), Payload: &memqlv1.ToolResult_Success{Success: &memqlv1.Success{ResultJson: []byte(`{"ok":true}`)}}}, nil
			}, func(c *Candidate) {
				c.MachineId = "machine"
				c.ActionContracts = workerservice.ActionContracts{permission.Contract: permission.Version}
			})
			owner := "v1:identity:user:" + id.NewShortId()
			h.SetOwner(owner)
			w := h.Registry().WorkerById("laptop")
			w.OwnerUserId = owner
			w.Labels = map[string]string{"machineId": "machine"}
			w.CapabilityDescriptor = &workerservice.CapabilityDescriptor{ActionContracts: workerservice.ActionContracts{permission.Contract: permission.Version}}
			h.Origin().SetComputerPermissions(authority[0])
			h.Holder(authority[1])
			person := PermissionFlowPersonForTest(t, owner)
			execute := func(e *memql.MemQLEngine, ctx context.Context, kind, name string, args map[string]any) []map[string]any {
				q, err := parser.RenderCall(name, args)
				require.NoError(t, err)
				r, err := e.Execute(ctx, kind+" "+q)
				require.NoError(t, err)
				return memql.MaterializeRows(r.OutputPayload())
			}
			goal, run := id.NewShortId(), id.NewShortId()
			write := auth.ContextWithInternalOrigin(person)
			execute(engines[0], write, "mutation", "createWorkGoal", map[string]any{"goalId": goal, "statement": "List selected computer home", "origin": "user", "requestedVia": "ask"})
			execute(engines[0], write, "mutation", "createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "permission-test", "templateFingerprint": "fixture", "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			rc := common.RunContext{RunId: run, GoalId: goal, OwnerUserId: owner, StepKey: "read"}
			ctx := common.ContextWithRun(person, rc)
			choice, err := authority[0].AskFeedback(ctx, owner, run, "Which computer?", "choice", []map[string]any{{"label": "Laptop", "value": "laptop"}, {"label": "Other", "value": "other"}}, time.Time{})
			require.NoError(t, err)
			execute(engines[1], person, "query", "decideApproval", map[string]any{"approvalId": choice, "decision": "answered", "answer": map[string]any{"value": "laptop"}})
			req := h.Request()
			req.RunId = run
			req.Args = map[string]any{"path": dir}
			req.Action = "fs_list"
			req.RequireLabels = map[string]string{"machineId": "machine"}
			integration := NewIntegration(h.Origin(), h.Registry(), engines[0], slog.New(slog.NewTextHandler(io.Discard, nil)))
			integration.ScopeRequester = authority[0].AskComputerScope
			nodes, err := RequestScopeForTest(integration, ctx, map[string]any{"ownerUserId": owner, "agentId": req.AgentId, "intent": "List home", "requestedScope": "observe", "summary": "Read the selected directory", "requireLabels": map[string]any{"machineId": "machine"}, "operation": map[string]any{"tool": "workerHost", "action": "fs_list", "args": req.Args, "directory": dir}})
			require.NoError(t, err)
			require.Len(t, nodes, 1)
			var response map[string]any
			require.NoError(t, json.Unmarshal(nodes[0].Payload, &response))
			approval := response["approvalId"]
			require.Equal(t, 0, calls, "asking must never access the computer")
			execute(engines[1], person, "query", "decideApproval", map[string]any{"approvalId": approval, "decision": "approved", "answer": map[string]any{"permissionMode": mode}})
			result, err := h.Origin().Dispatch(ctx, req)
			require.NoError(t, err)
			require.True(t, result.OK, "%+v", result)
			require.Equal(t, 1, calls)
			result, err = h.Origin().Dispatch(ctx, req)
			require.NoError(t, err)
			if mode == "once" {
				require.False(t, result.OK)
				require.Equal(t, 1, calls, "one-time permission was consumed twice")
			} else {
				require.True(t, result.OK)
				require.Equal(t, 2, calls)
				// Rebuild the authority on both replicas: no permission cache survives.
				fresh := workservice.New(engines[1], slog.New(slog.NewTextHandler(io.Discard, nil)), func() *bun.DB { return db })
				fresh.SetGateDB(func() *sql.DB { return gates })
				h.Origin().SetComputerPermissions(fresh)
				h.Holder(fresh)
				result, err = h.Origin().Dispatch(ctx, req)
				require.NoError(t, err)
				require.True(t, result.OK)
				require.Equal(t, 3, calls)
				execute(engines[1], person, "query", "revokeComputerPermission", map[string]any{"permissionId": approval})
				result, err = h.Origin().Dispatch(ctx, req)
				require.NoError(t, err)
				require.False(t, result.OK)
				require.Equal(t, 3, calls)
			}
		})
	}
}
