package installation

import (
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"testing"
)

func TestSelectedCancellationReleasesOnlyOriginalUnstartedPlanAcrossHosts(t *testing.T) {
	ctx, first, peer, preparation, plan := promotedReopenFixture(t)
	public := &Preparer{host: &preparationHost{journal: peer}}
	owner := operator(auth.RoleOwner, preparation.Scope.RequestedBy)
	_, err := public.CancelSelected(operator(auth.RoleOwner, "another-operator"), plan.Plan.InstallationID, plan.ID)
	require.ErrorContains(t, err, "only the requester")
	_, err = public.CancelSelected(operator(auth.RoleReader, preparation.Scope.RequestedBy), plan.Plan.InstallationID, plan.ID)
	require.Error(t, err)
	cancelled, err := public.CancelSelected(owner, plan.Plan.InstallationID, plan.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", cancelled.State)
	require.True(t, cancelled.CanManage)
	again, err := public.CancelSelected(owner, plan.Plan.InstallationID, plan.ID)
	require.NoError(t, err)
	require.Equal(t, cancelled, again, "an uncertain cancellation reply recovers the same receipt")
	var active any
	require.NoError(t, first.db().QueryRow(`SELECT active_plan_id FROM installation_revision_heads WHERE installation_id=$1`, plan.Plan.InstallationID).Scan(&active))
	require.Nil(t, active)
	// A successor can reserve the installation, and retrying the historical
	// cancellation must never release that successor's slot.
	successor := testPlan()
	successor.InstallationID, successor.RequestedBy = plan.Plan.InstallationID, preparation.Scope.RequestedBy
	journal := &revisionJournal{db: first.db}
	next, err := journal.reserve(ctx, successor)
	require.NoError(t, err)
	_, err = public.CancelSelected(owner, plan.Plan.InstallationID, plan.ID)
	require.NoError(t, err)
	var held string
	require.NoError(t, first.db().QueryRow(`SELECT active_plan_id FROM installation_revision_heads WHERE installation_id=$1`, plan.Plan.InstallationID).Scan(&held))
	require.Equal(t, next.ID, held)
	_, err = journal.begin(ctx, next.Plan.InstallationID, next.ID, next.Plan.ExecutionWorkflowDigest)
	require.NoError(t, err)
	_, err = public.CancelSelected(owner, next.Plan.InstallationID, next.ID)
	require.ErrorContains(t, err, "effect may have started")
	require.NoError(t, first.db().QueryRow(`SELECT active_plan_id FROM installation_revision_heads WHERE installation_id=$1`, plan.Plan.InstallationID).Scan(&held))
	require.Equal(t, next.ID, held)
}
