package installation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type retirementFilesFixture struct {
	mu       sync.Mutex
	ids      map[pipelinesteps.RunFileReceiptScope][]string
	fenced   map[pipelinesteps.RunFileReceiptScope]bool
	released map[string]bool
	retired  map[string]pipelinesteps.RetiredFileReceipt
	fail     string
	calls    int
}

func retirementFiles(scope preparationScope) *retirementFilesFixture {
	return &retirementFilesFixture{ids: map[pipelinesteps.RunFileReceiptScope][]string{
		retirementArtifactScope(scope.Captures["candidate"]): {fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3)},
		retirementArtifactScope(scope.Captures["rollback"]):  {fmt.Sprintf("%064x", 4)},
	}, fenced: map[pipelinesteps.RunFileReceiptScope]bool{}, released: map[string]bool{}, retired: map[string]pipelinesteps.RetiredFileReceipt{}}
}

func (f *retirementFilesFixture) effect(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, operation string) error {
	f.calls++
	a, _ := auth.AccessFromContext(ctx)
	if auth.OriginFromContext(ctx) != auth.OriginInternal || a == nil || !strings.HasSuffix(a.UserId, ":"+scope.OwnerUserID) {
		return errors.New("wrong native owner")
	}
	if _, ok := f.ids[scope]; !ok {
		return errors.New("foreign cleanup scope")
	}
	if f.fail == operation {
		f.fail = ""
		return errors.New("lost committed " + operation + " response")
	}
	return nil
}
func (f *retirementFilesFixture) FenceRunFileScope(ctx context.Context, scope pipelinesteps.RunFileReceiptScope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fenced[scope] = true
	return f.effect(ctx, scope, "fence")
}
func (f *retirementFilesFixture) ReadFencedRunFileIntents(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, cursor string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fenced[scope] {
		return nil, errors.New("unfenced")
	}
	if err := f.effect(ctx, scope, "inventory"); err != nil {
		return nil, err
	}
	var result []string
	for _, id := range f.ids[scope] {
		if id > cursor {
			result = append(result, id)
			if len(result) == 2 {
				break
			}
		}
	}
	return result, nil
}
func (f *retirementFilesFixture) PinRunFileReceipts(context.Context, pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	return nil, errors.New("retirement may not pin")
}
func (f *retirementFilesFixture) ReleaseRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fenced[ref.Scope] {
		return errors.New("unfenced release")
	}
	f.released[ref.ReferenceID] = true
	return f.effect(ctx, ref.Scope, "release")
}
func (f *retirementFilesFixture) RetireRunFile(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, id string) (pipelinesteps.RetiredFileReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fenced[scope] || !slices.Contains(f.ids[scope], id) {
		return pipelinesteps.RetiredFileReceipt{}, errors.New("foreign/unfenced artifact")
	}
	receipt := pipelinesteps.RetiredFileReceipt{IntentID: id, FileID: "file-" + id, TombstoneETag: "permanent-etag"}
	f.retired[id] = receipt
	return receipt, f.effect(ctx, scope, "retire")
}

type retirementStopFixture struct {
	captureExecutorFixture
	fail  bool
	calls int
}

func (f *retirementStopFixture) Cancel(_ context.Context, run string) error {
	f.calls++
	f.cancelledRun = run
	if f.fail {
		f.fail = false
		return errors.New("lost stop response")
	}
	return nil
}

func TestPreparationRetirementReconcilesLostRepliesAndReleasesOnlyItsHead(t *testing.T) {
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	cleanup := "memql-id:" + strings.Repeat("e", 64)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	for _, role := range []string{"candidate", "rollback"} {
		_, _, err = j.beginCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, role)
		require.NoError(t, err)
	}
	capture, err := newSourceCapture(scope.Captures["candidate"])
	require.NoError(t, err)
	receipt := sourceCaptureReceipt{ScopeDigest: capture.digest, IntentID: fmt.Sprintf("%064x", 1)}
	_, err = j.recordReceipt(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", receipt)
	require.NoError(t, err)
	_, err = j.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	r, err = peer.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	require.Equal(t, "retiring", r.State)
	_, _, err = j.beginCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "rollback")
	require.Error(t, err)
	_, err = j.recordReceipt(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", receipt)
	require.Error(t, err)
	_, err = j.cancelBeforeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest)
	require.Error(t, err)
	_, err = j.finishRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.Error(t, err)
	competingPlan := testPlan()
	competingPlan.RequestedBy = scope.RequestedBy
	_, err = (&revisionJournal{db: func() *sql.DB { return peerDB }}).reserve(ctx, competingPlan)
	require.ErrorIs(t, err, errBusy)
	files := retirementFiles(scope)
	_, err = j.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.Error(t, err)
	require.Zero(t, files.calls)
	executor := &retirementStopFixture{fail: true}
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
	require.Error(t, err)
	r, err = peer.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.False(t, r.Retirement.ProducerStopped)
	r, err = peer.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
	require.NoError(t, err)
	require.Equal(t, 2, executor.calls)
	require.Equal(t, scope.Captures["candidate"].RunID, executor.cancelledRun)
	for _, role := range []string{"candidate", "rollback"} {
		_, err = j.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.Error(t, err)
		files.fail = "fence"
		_, err = j.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.Error(t, err)
		_, err = peer.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.NoError(t, err)
		_, err = j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.Error(t, err)
		if role == "candidate" {
			files.fail = "release"
			_, err = j.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
			require.Error(t, err)
		}
		_, err = peer.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.NoError(t, err)
		files.fail = "inventory"
		_, err = j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.Error(t, err)
		for {
			r, err = peer.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
			require.NoError(t, err)
			if r.Retirement.Captures[role].Complete {
				break
			}
			page := r.Retirement.Captures[role].Page
			recoveredPage, err := j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
			require.NoError(t, err)
			require.Equal(t, page, recoveredPage.Retirement.Captures[role].Page, "lost page response must recover its pending intents without advancing")
			_, err = j.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, strings.Repeat("f", 64), files)
			require.Error(t, err)
			files.fail = "retire"
			_, err = j.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, page[0].IntentID, files)
			require.Error(t, err)
			current, err := peer.get(ctx, scope.InstallationID, r.ID)
			require.NoError(t, err)
			require.Empty(t, current.Retirement.Captures[role].Page[0].ReceiptDigest, "uncertain provider result cannot advance the cursor")
			for _, artifact := range page {
				_, err = peer.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, artifact.IntentID, files)
				require.NoError(t, err)
			}
		}
	}
	require.Len(t, files.released, 1, "only the capture with a committed receipt could have pinned")
	require.Len(t, files.retired, 4, "lost rollback capture receipt did not lose its upload")
	r, err = j.finishRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	require.Equal(t, "cancelled", r.State)
	require.True(t, r.Captures["rollback"].Started, "retirement must preserve dispatch history")
	next, _ := preparationFixture(t)
	next.RequestID = "next-request"
	for role, spec := range next.Captures {
		spec.RunID = preparationSourceRun(next.InstallationID, next.RequestID, next.RequestedBy)
		spec.WorkRunID = spec.RunID
		next.Captures[role] = spec
	}
	successor, err := peer.reserve(ctx, next)
	require.NoError(t, err)
	again, err := peer.finishRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	require.Equal(t, r, again)
	var active string
	require.NoError(t, db.QueryRow("SELECT active_preparation_id FROM installation_revision_heads WHERE installation_id=$1", scope.InstallationID).Scan(&active))
	require.Equal(t, successor.ID, active)
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
	require.Error(t, err)
}

func TestPreparationRetirementAuthorityHistoryAndParallelPageReceipts(t *testing.T) {
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	cleanup := "memql-id:" + strings.Repeat("e", 64)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	_, err = j.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	files := retirementFiles(scope)
	executor := &retirementStopFixture{}
	for _, bad := range []context.Context{context.Background(), operator(auth.RoleOwner, scope.RequestedBy), captureOperator(auth.RoleReader, scope.RequestedBy), captureOperator(auth.RoleOwner, "other")} {
		_, err = j.stopRetiringProducer(bad, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
		require.Error(t, err)
		_, err = j.fenceRetiringCapture(bad, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
		require.Error(t, err)
	}
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, scope.ConfigurationDigest, executor)
	require.Error(t, err)
	_, err = j.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, scope.ConfigurationDigest)
	require.Error(t, err)
	require.Zero(t, executor.calls)
	require.Zero(t, files.calls)
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
	require.NoError(t, err)
	_, err = j.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.NoError(t, err)
	_, err = j.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.NoError(t, err)
	r, err = j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.NoError(t, err)
	results := make(chan error, 2)
	for n, a := range r.Retirement.Captures["candidate"].Page {
		writer := j
		if n%2 == 0 {
			writer = peer
		}
		go func() {
			_, err := writer.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", a.IntentID, files)
			results <- err
		}()
	}
	for range 2 {
		require.NoError(t, <-results)
	}
	r, err = peer.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	for _, a := range r.Retirement.Captures["candidate"].Page {
		require.NotEmpty(t, a.ReceiptDigest)
	}
	body, err := os.ReadFile(retirementMigrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(body))
	require.ErrorContains(t, err, "cannot remove installation preparation retirement history")
	_, err = db.ExecContext(ctx, `UPDATE installation_preparations SET retirement=jsonb_set(retirement,'{captures,candidate,complete}','true') WHERE preparation_id=$1`, r.ID)
	require.NoError(t, err)
	_, err = peer.get(ctx, scope.InstallationID, r.ID)
	require.Error(t, err, "invented completion must not hide a retained artifact page")
}
