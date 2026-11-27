package calls

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestWorkflowResourcesStoredBindingsAreCompleteAndPrivate(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	r := callrules.WorkflowRun{ID: "resource-run", Definition: workflowResourceDefinition(), ResourceBindings: map[string]callrules.WorkflowResourceBinding{"display": {WorkflowResourceRef: callrules.WorkflowResourceRef{ResourceID: "owner-source", Revision: 1}}}}
	workflowEnter(&r, "demo", now)
	state := &workflowState{Run: &r}
	require.True(t, workflowValidResources(state), "an approved source may wait for its owner to publish")
	partial := r.ResourceBindings["display"]
	partial.LeaseID = "lease-one"
	r.ResourceBindings["display"] = partial
	require.False(t, workflowValidResources(state), "partial lease proof cannot survive a durable read")
	partial.Identity = "native-resource-identity"
	expires := now.Add(time.Minute)
	partial.ExpiresAt = &expires
	r.ResourceBindings["display"] = partial
	require.True(t, workflowValidResources(state))
	guest := workflowProjection(r, seatRow{ID: "guest"}, false, now)
	require.Nil(t, guest.ResourceBindings)
	require.Equal(t, "Required source", guest.Definition.Resources[0].Label)
	require.Empty(t, guest.Definition.Resources[0].Kind)
	require.Equal(t, "Reviewed demonstration", r.Definition.Resources[0].Label)
	require.Equal(t, "synthetic", r.Definition.Resources[0].Kind)
	require.Equal(t, partial, r.ResourceBindings["display"], "privacy projection cannot erase persisted native pins")
	r.ResourceBindings["extra"] = partial
	require.False(t, workflowValidResources(state), "persisted maps cannot add undeclared resource authority")
	require.False(t, workflowValidResources(&workflowState{Binding: &callrules.WorkflowBinding{ResourceBindings: map[string]callrules.WorkflowResourceRef{"display": {ResourceID: "owner-source", Revision: 2147483648}}}}))
}

func TestWorkflowResourcesResumeOffersAnExplicitLeaseCheck(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	r := callrules.WorkflowRun{ID: "resource-run", Definition: workflowResourceDefinition()}
	workflowEnter(&r, "demo", now)
	r.Status = "blocked"
	for _, tc := range []struct {
		blockers []string
		allowed  bool
	}{{nil, true}, {[]string{"bound_resource_lease_unavailable"}, true}, {[]string{"bound_resource_unavailable"}, false}, {[]string{"bound_resource_lease_unavailable", "media_readiness_unavailable"}, false}, {[]string{"insights_or_account_unavailable"}, false}} {
		r.Blockers = tc.blockers
		projected := workflowProjection(r, seatRow{ID: "host", Host: true}, false, now)
		if tc.allowed {
			require.Contains(t, projected.AllowedActions, "resume")
		} else {
			require.NotContains(t, projected.AllowedActions, "resume")
		}
	}
	r.Definition = workflowParallelDefinition()
	workflowEnter(&r, "fork", now)
	r.Parallel.Branches[0].Status = "blocked"
	r.Parallel.Branches[0].Blockers = []string{"bound_resource_lease_unavailable"}
	projected := workflowProjection(r, seatRow{ID: "host", Host: true}, false, now)
	require.Contains(t, projected.AllowedActions, "resume", "only the global command may check a replacement shared lease")
}
