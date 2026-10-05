//go:build agent

package worker

import (
	"context"
	"errors"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"maps"
	"testing"
	"time"
)

type grantStore struct {
	*fakeStore
	rows     []map[string]any
	prefsErr error
}

func (s *grantStore) UserPreferences(ctx context.Context, owner string) (Preferences, error) {
	if s.prefsErr != nil {
		return Preferences{}, s.prefsErr
	}
	return s.fakeStore.UserPreferences(ctx, owner)
}
func (s *grantStore) WorkComputerScope(_ context.Context, r Request, now time.Time) (string, error) {
	return approvedComputerScope(s.rows, r, now), nil
}
func grantRow(r Request, until time.Time) map[string]any {
	subject := map[string]any{"agentId": r.AgentId, "scope": "observe", "requireLabels": map[string]any{"os": "darwin"}}
	return map[string]any{"ownerUserId": r.OwnerUserId, "runId": r.RunId, "decision": "approved", "expiresAt": until.Format(time.RFC3339Nano), "subject": subject, "artifactHash": work.ArtifactHash(subject)}
}
func TestOwnedComputerGrantGatesActualForwardedDispatch(t *testing.T) {
	calls := 0
	h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		calls++
		return okResult(d.GetCallId()), nil
	}, func(c *Candidate) { c.Labels = map[string]string{"os": "darwin"} })
	req := h.request()
	req.RequireLabels = map[string]string{"os": "darwin"}
	ctx := common.ContextWithRun(authorityCtx(t, h.owner), common.RunContext{RunId: req.RunId, GoalId: "goal", OwnerUserId: h.owner, StepKey: "research"})
	// No grant interface or local state can be treated as approval.
	result, err := h.dispatch.Dispatch(ctx, req)
	if err != nil || result.OK || calls != 0 {
		t.Fatalf("missing grant dispatched: %+v %v", result, err)
	}
	store := &grantStore{fakeStore: h.store, rows: []map[string]any{grantRow(req, fleetNow().Add(time.Hour))}}
	h.dispatch.store = store
	result, err = h.dispatch.Dispatch(ctx, req)
	if err != nil || !result.OK || calls != 1 {
		t.Fatalf("approved cross-replica read failed: %+v %v", result, err)
	}
	store.prefsErr = errors.New("storage unavailable")
	result, err = h.dispatch.Dispatch(ctx, req)
	if err != nil || result.OK || calls != 1 {
		t.Fatalf("unreadable kill switch allowed dispatch: %+v %v", result, err)
	}
	store.prefsErr = nil
	req.Action = "exec"
	result, err = h.dispatch.Dispatch(ctx, req)
	if err != nil || result.OK || calls != 1 {
		t.Fatalf("read-only approval allowed exec: %+v %v", result, err)
	}
	req.Action = "fs_read"
	store.rows[0]["expiresAt"] = fleetNow().Add(-time.Second).Format(time.RFC3339Nano)
	result, err = h.dispatch.Dispatch(ctx, req)
	if err != nil || result.OK || calls != 1 {
		t.Fatalf("expired approval dispatched: %+v %v", result, err)
	}
}
func TestComputerGrantCannotChangeOwnerRunAgentHashOrMachineSet(t *testing.T) {
	req := approvedRequest()
	req.RequireLabels = map[string]string{"os": "darwin"}
	now := fleetNow()
	original := grantRow(req, now.Add(time.Hour))
	for _, field := range []string{"ownerUserId", "runId", "artifactHash", "expiresAt", "decision"} {
		t.Run(field, func(t *testing.T) {
			row := maps.Clone(original)
			row[field] = "different"
			if scope := approvedComputerScope([]map[string]any{row}, req, now); scope != "" {
				t.Fatalf("%s changed but grant survived", field)
			}
		})
	}
	for _, change := range []string{"agent", "labels"} {
		r := req
		if change == "agent" {
			r.AgentId = "another"
		} else {
			r.RequireLabels = map[string]string{"os": "linux"}
		}
		if approvedComputerScope([]map[string]any{original}, r, now) != "" {
			t.Fatalf("grant crossed %s boundary", change)
		}
	}
	// Canonical and bare wire IDs denote the same owner/run/agent.
	row := maps.Clone(original)
	row["ownerUserId"] = "v1:identity:user:" + req.OwnerUserId
	row["runId"] = "v1:work:run:" + req.RunId
	if approvedComputerScope([]map[string]any{row}, req, now) != "observe" {
		t.Fatal("wire IDs lost a valid grant")
	}
}
