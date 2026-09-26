package memql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

func TestWorkModelCancellationObservesAnotherReplicasJournalWrite(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	owner := "v1:identity:user:" + id.NewShortId()
	// Separate contexts have no shared local session/cancellation state.
	actor := func() context.Context {
		return auth.ContextWithAccess(auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: owner}), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	}
	runID := runMutation(t, auth.ContextWithInternalOrigin(actor()), e, "createWorkRun", map[string]any{"runId": id.NewShortId(), "automationName": "shared", "templateFingerprint": "test", "triggeredBy": "manual", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339)})
	ctx, cancel := context.WithCancelCause(common.ContextWithRun(actor(), common.RunContext{RunId: runID, OwnerUserId: owner}))
	defer cancel(nil)
	observe := e.modelCancellation(ctx, cancel)
	observe(airoute.CallObservation{ID: "model", Phase: "running"})
	runMutation(t, auth.ContextWithInternalOrigin(actor()), e, "updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true, "cancelledBy": owner})
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("model ignored durable Stop on another replica")
	}
	var stopped *WorkCancelledError
	require.ErrorAs(t, context.Cause(ctx), &stopped)
	require.Equal(t, owner, stopped.By)
	observe(airoute.CallObservation{ID: "model", Phase: "failed"})
}
