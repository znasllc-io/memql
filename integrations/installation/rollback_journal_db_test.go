package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

func rollbackJournalFixture(t *testing.T) (context.Context, *revisionJournal, *revisionJournal, revisionRecord, rollbackPlan, *revisionControllerFixture) {
	t.Helper()
	db, peerDB := journalDB(t)
	preparations := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, preparations)
	r, err := preparations.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	j, peer := &revisionJournal{db: func() *sql.DB { return db }}, &revisionJournal{db: func() *sql.DB { return peerDB }}
	r, err = j.begin(ctx, plan.InstallationID, r.ID, plan.ExecutionWorkflowDigest)
	require.NoError(t, err)
	api := newRevisionControllerFixture(t, db, r)
	client, err := argocd.New(api)
	require.NoError(t, err)
	_, err = client.Apply(ctx, r.Plan.Intent)
	require.NoError(t, err)
	api.succeed(r.Plan.Intent.Revision)
	reversal, err := client.PlanRollback(ctx, r.Plan.Intent, "explicit-reversal")
	require.NoError(t, err)
	// This tests journal relationships; authenticated continuation and fresh
	// preservation/artifact verifiers are separate native host obligations.
	digest := "memql-id:" + strings.Repeat("f", 64)
	rollback := rollbackPlan{ParentID: r.ID, RequestedBy: plan.RequestedBy, WorkflowDigest: digest,
		ConfigurationDigest: digest, ArtifactDigest: digest, StorageDigest: digest, SensitiveDigest: digest,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), ArtifactExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Intent: reversal}
	return ctx, j, peer, r, rollback, api
}

type rollbackStartCheckedAPI struct {
	argocd.API
	db  *sql.DB
	key string
}

func (a rollbackStartCheckedAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if method == http.MethodPatch {
		var started bool
		if err := a.db.QueryRowContext(ctx, `SELECT started_at IS NOT NULL FROM installation_revision_rollbacks WHERE parent_plan_id=$1`, a.key).Scan(&started); err != nil || !started {
			return nil, errors.New("rollback write preceded its durable start")
		}
	}
	return a.API.Do(ctx, method, path, contentType, body)
}

func TestRollbackJournalAcrossReplicasRecoversLostEffectsAndRetainsHead(t *testing.T) {
	ctx, j, peer, parent, plan, api := rollbackJournalFixture(t)
	var wg sync.WaitGroup
	results, failures := make(chan revisionRecord, 12), make(chan error, 12)
	for n := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := j
			if n%2 != 0 {
				host = peer
			}
			r, err := host.reserveRollback(ctx, parent.Plan.InstallationID, parent.ID, plan)
			if err != nil {
				failures <- err
			} else {
				results <- r
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	var rollbackID string
	for r := range results {
		require.NotNil(t, r.Rollback)
		if rollbackID == "" {
			rollbackID = r.Rollback.ID
		}
		require.Equal(t, rollbackID, r.Rollback.ID)
		require.False(t, r.Rollback.Started)
		require.Equal(t, parent.SlotEpoch, r.SlotEpoch)
	}
	require.NotEmpty(t, rollbackID)
	_, err := j.begin(ctx, parent.Plan.InstallationID, parent.ID, parent.Plan.ExecutionWorkflowDigest)
	require.Error(t, err, "forward recipe is fenced before rollback dispatch")
	_, err = j.observe(ctx, parent.Plan.InstallationID, parent.ID, parent.ObservationVersion, argocd.Facts{Healthy: true})
	require.ErrorIs(t, err, errChanged, "a late forward observer cannot update the reversal")
	_, err = peer.beginRollback(ctx, parent.Plan.InstallationID, parent.ID, plan.WorkflowDigest)
	require.NoError(t, err)
	// Discard the first durable start response. A second host gets the same
	// started reversal and applies its exact intent through the real adapter.
	current, err := j.beginRollback(ctx, parent.Plan.InstallationID, parent.ID, plan.WorkflowDigest)
	require.NoError(t, err)
	require.True(t, current.Rollback.Started)
	checked := rollbackStartCheckedAPI{API: api, db: api.db, key: parent.ID}
	client, err := argocd.New(checked)
	require.NoError(t, err)
	api.loseReply = true
	_, err = client.Apply(ctx, current.Rollback.Plan.Intent)
	require.Error(t, err, "the remote reversal committed but its response was lost")
	current, err = peer.get(ctx, parent.Plan.InstallationID, parent.ID)
	require.NoError(t, err)
	fresh, err := argocd.New(checked)
	require.NoError(t, err)
	facts, err := fresh.Apply(ctx, current.Rollback.Plan.Intent)
	require.NoError(t, err)
	require.True(t, facts.IntentObserved)
	require.Equal(t, 2, api.writes)
	current, err = peer.observeRollback(ctx, parent.Plan.InstallationID, parent.ID, current.Rollback.ObservationVersion, facts)
	require.NoError(t, err)
	_, err = j.observeRollback(ctx, parent.Plan.InstallationID, parent.ID, 0, argocd.Facts{OperationSucceeded: true})
	require.ErrorIs(t, err, errChanged)
	api.succeed(plan.Intent.Revision)
	facts, err = fresh.Observe(ctx, plan.Intent)
	require.NoError(t, err)
	current, err = peer.observeRollback(captureOperator(auth.RoleOwner, "replacement-operator"), parent.Plan.InstallationID, parent.ID, current.Rollback.ObservationVersion, facts)
	require.NoError(t, err)
	require.True(t, current.Rollback.Observation.OperationSucceeded)
	require.Equal(t, "applying", current.State, "controller success is not verified installation completion")
	competing := testPlan()
	competing.RequestedBy = parent.Plan.RequestedBy
	_, err = peer.reserve(ctx, competing)
	require.ErrorIs(t, err, errBusy)
	_, err = peer.cancel(ctx, parent.Plan.InstallationID, parent.ID)
	require.Error(t, err)
	_, err = client.Apply(ctx, parent.Plan.Intent)
	require.ErrorIs(t, err, argocd.ErrChanged)
	require.Equal(t, 2, api.writes)
}

func TestRollbackJournalRefusesChangedAuthorityEvidenceAndReversal(t *testing.T) {
	ctx, j, _, parent, plan, api := rollbackJournalFixture(t)
	for _, mutate := range []func(*rollbackPlan){
		func(p *rollbackPlan) { p.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano) },
		func(p *rollbackPlan) { p.ObservedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano) },
		func(p *rollbackPlan) {
			p.ArtifactExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		},
		func(p *rollbackPlan) { p.Intent.Revision = strings.Repeat("f", 40) },
		func(p *rollbackPlan) { p.ConfigurationDigest = "" },
		func(p *rollbackPlan) { p.ParentID = "memql-id:" + strings.Repeat("a", 64) },
	} {
		bad := plan
		mutate(&bad)
		_, err := j.reserveRollback(ctx, parent.Plan.InstallationID, parent.ID, bad)
		require.Error(t, err)
	}
	for _, actor := range []context.Context{context.Background(), captureOperator(auth.RoleReader, "reader"), captureOperator(auth.RoleOwner, "other")} {
		_, err := j.reserveRollback(actor, parent.Plan.InstallationID, parent.ID, plan)
		require.Error(t, err)
	}
	_, err := j.reserveRollback(ctx, parent.Plan.InstallationID, parent.ID, plan)
	require.NoError(t, err)
	changed := plan
	changed.Intent.RequestID = "other-reversal"
	_, err = j.reserveRollback(ctx, parent.Plan.InstallationID, parent.ID, changed)
	require.ErrorIs(t, err, errChanged)
	_, err = j.beginRollback(ctx, parent.Plan.InstallationID, parent.ID, parent.Plan.WorkflowDigest)
	require.Error(t, err)
	_, err = j.observeRollback(ctx, parent.Plan.InstallationID, parent.ID, 0, argocd.Facts{})
	require.ErrorIs(t, err, errChanged, "observation cannot precede durable start")
	require.Equal(t, 1, api.writes)
}

func TestRollbackJournalRevalidatesStoredBindingsAndRetainsHistory(t *testing.T) {
	ctx, j, _, parent, plan, api := rollbackJournalFixture(t)
	r, err := j.reserveRollback(ctx, parent.Plan.InstallationID, parent.ID, plan)
	require.NoError(t, err)
	down, err := os.ReadFile(rollbackMigrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = api.db.Exec(string(down))
	require.ErrorContains(t, err, "rollback history must be retained")
	body, _, err := plan.canonical(parent)
	require.NoError(t, err)
	for _, edit := range []string{
		`plan || '{"unknown":true}'::jsonb`,
		`jsonb_set(plan,'{intent,revision}','"` + strings.Repeat("f", 40) + `"'::jsonb)`,
		`jsonb_set(plan,'{intent,unknown}','true'::jsonb)`,
	} {
		_, err = api.db.Exec(`UPDATE installation_revision_rollbacks SET plan=$2::jsonb WHERE parent_plan_id=$1`, parent.ID, string(body))
		require.NoError(t, err)
		_, err = api.db.Exec(`UPDATE installation_revision_rollbacks SET plan=`+edit+` WHERE parent_plan_id=$1`, parent.ID)
		require.NoError(t, err)
		_, err = j.get(ctx, parent.Plan.InstallationID, parent.ID)
		require.Error(t, err)
	}
	_, err = api.db.Exec(`UPDATE installation_revision_rollbacks SET plan=$2::jsonb WHERE parent_plan_id=$1`, parent.ID, string(body))
	require.NoError(t, err)
	_, err = j.beginRollback(ctx, parent.Plan.InstallationID, parent.ID, plan.WorkflowDigest)
	require.NoError(t, err)
	// A failed reversal keeps the owned history/head for explicit recovery.
	r, err = j.observeRollback(ctx, parent.Plan.InstallationID, parent.ID, 0, argocd.Facts{IntentObserved: true, OperationPhase: "Failed"})
	require.NoError(t, err)
	require.Equal(t, "Failed", r.Rollback.Observation.OperationPhase)
	var saved map[string]any
	require.NoError(t, json.Unmarshal(body, &saved))
	require.NotEmpty(t, saved["intent"])
	var active string
	require.NoError(t, api.db.QueryRow(`SELECT active_plan_id FROM installation_revision_heads WHERE installation_id=$1`, parent.Plan.InstallationID).Scan(&active))
	require.Equal(t, parent.ID, active)
}
