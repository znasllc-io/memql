package installation

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type selectionRetirementFiles struct{ *retirementFilesFixture }

func (selectionRetirementFiles) OpenRunFileReceipt(context.Context, pipelinesteps.RunFileReceiptScope, string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	return pipelinesteps.StoredFileReceipt{}, nil, errors.New("cancellation must not reopen source")
}

func TestSelectedPreparationCancellationRecoversLegacyScopeWithoutReleasingAnotherHead(t *testing.T) {
	db, peerDB := journalDB(t)
	scope, _ := preparationFixture(t)
	// The production refusal predates the trusted event field. Retain its
	// exact legacy scope rather than silently upgrading a dispatched request.
	for role, spec := range scope.Captures {
		spec.Event = ""
		scope.Captures[role] = spec
	}
	j := preparationConnection(db)
	r, err := j.reserve(captureOperator(auth.RoleDeveloper, scope.RequestedBy), scope)
	require.NoError(t, err)
	_, _, err = j.beginCapture(captureOperator(auth.RoleDeveloper, scope.RequestedBy), scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate")
	require.NoError(t, err)
	files, executor := selectionRetirementFiles{retirementFiles(scope)}, &retirementStopFixture{fail: true}
	workflow, err := loadRetirementWorkflow(strings.Repeat("e", 40))
	require.NoError(t, err)
	p := &Preparer{host: &preparationHost{journal: j, executor: executor, files: files, retirement: workflow}}
	_, err = p.CancelPreparation(operator(auth.RoleOwner, "another-operator"), scope.InstallationID, r.ID)
	require.ErrorContains(t, err, "original admitted operator")
	require.Zero(t, executor.calls)
	require.Zero(t, files.calls)
	actor := operator(auth.RoleDeveloper, scope.RequestedBy)
	_, err = p.CancelPreparation(actor, scope.InstallationID, r.ID)
	require.ErrorContains(t, err, "cleanup remains unconfirmed")
	held, err := j.get(captureOperator(auth.RoleDeveloper, scope.RequestedBy), scope.InstallationID, r.ID)
	require.NoError(t, err)
	view := selectedPreparation(held, scope.RequestedBy)
	require.True(t, view.CanCancel)
	require.False(t, view.CanResume)
	require.Equal(t, "retiring", view.State)
	require.False(t, selectedPreparation(held, "another-operator").CanCancel)
	// A fresh host resumes bounded cleanup, never source execution.
	p.host.journal = preparationConnection(peerDB)
	for range 4 {
		view, err = p.CancelPreparation(actor, scope.InstallationID, r.ID)
		require.NoError(t, err)
		if view.State == "cancelled" {
			break
		}
	}
	require.Equal(t, "cancelled", view.State)
	require.False(t, view.CanCancel)
	require.Empty(t, executor.requests)
	require.Len(t, files.retired, 4)
	next := scope
	next.RequestID = "successor-request"
	for role, spec := range next.Captures {
		spec.RunID = preparationSourceRun(next.InstallationID, next.RequestID, next.RequestedBy)
		spec.WorkRunID = spec.RunID
		next.Captures[role] = spec
	}
	successor, err := p.host.journal.reserve(captureOperator(auth.RoleDeveloper, scope.RequestedBy), next)
	require.NoError(t, err)
	_, err = p.CancelPreparation(actor, scope.InstallationID, r.ID)
	require.NoError(t, err)
	var preparing string
	require.NoError(t, peerDB.QueryRow(`SELECT active_preparation_id FROM installation_revision_heads WHERE installation_id=$1`, scope.InstallationID).Scan(&preparing))
	require.Equal(t, successor.ID, preparing)
}
