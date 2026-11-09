package installation

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// closeFixture promotes and starts one plan through the real journals. The JSON
// Patch controller double stands in for its Application, and the public host
// uses a second database connection, as another replica would.
func closeFixture(t *testing.T) (context.Context, *Preparer, *revisionJournal, revisionRecord, *revisionControllerFixture) {
	t.Helper()
	db, peerDB := journalDB(t)
	preparations := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, preparations)
	r, err := preparations.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	j := &revisionJournal{db: func() *sql.DB { return db }}
	r, err = j.begin(ctx, plan.InstallationID, r.ID, plan.ExecutionWorkflowDigest)
	require.NoError(t, err)
	api := newRevisionControllerFixture(t, db, r)
	return ctx, &Preparer{host: &preparationHost{api: api, journal: preparationConnection(peerDB)}}, j, r, api
}

func activePlan(t *testing.T, db *sql.DB, installation string) string {
	t.Helper()
	var active sql.NullString
	require.NoError(t, db.QueryRow(`SELECT active_plan_id FROM installation_revision_heads WHERE installation_id=$1`, installation).Scan(&active))
	return active.String
}

const closeReasonFixture = "Protected-resource evidence was saved by an older engine and cannot be reproduced"

// The production case of 2026-11-09: the plan's write landed and its sync
// finished, but its saved evidence can never match again. While its own sync
// is still queued the close refuses; once that sync is over, the original
// operator closes it, which frees the installation for the next preparation
// and leaves no way back into the plan's effects.
func TestInstallationCloseSelectedWaitsForItsOwnOperationThenReleasesTheInstallation(t *testing.T) {
	ctx, public, j, r, api := closeFixture(t)
	installation, db := r.Plan.InstallationID, api.db
	owner := operator(auth.RoleDeveloper, r.Plan.RequestedBy)
	client, err := argocd.New(api)
	require.NoError(t, err)
	_, err = client.Apply(ctx, r.Plan.Intent)
	require.NoError(t, err)

	_, err = public.CloseSelected(owner, installation, r.ID, closeReasonFixture)
	require.ErrorContains(t, err, "still running", "a queued sync of this plan's own would finish with no owner")
	require.Equal(t, r.ID, activePlan(t, db, installation))
	api.succeed(r.Plan.Intent.Revision)

	for name, actor := range map[string]context.Context{
		"another operator": operator(auth.RoleOwner, "another-operator"),
		"reader":           operator(auth.RoleReader, r.Plan.RequestedBy),
		"unidentified":     context.Background(),
	} {
		_, err = public.CloseSelected(actor, installation, r.ID, closeReasonFixture)
		require.Error(t, err, name)
	}
	for _, reason := range []string{"", " padded", "two\nlines", "tab\tinside", strings.Repeat("x", maxCloseReasonRunes+1)} {
		_, err = public.CloseSelected(owner, installation, r.ID, reason)
		require.Error(t, err, "reason %q", reason)
	}
	require.Equal(t, r.ID, activePlan(t, db, installation), "no refusal released the installation")

	closed, err := public.CloseSelected(owner, installation, r.ID, closeReasonFixture)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.State)
	require.Equal(t, closeReasonFixture, closed.CloseReason)
	require.NotEmpty(t, closed.ClosedAt)
	require.True(t, closed.CanManage)
	require.False(t, closed.CanRollback, "a close is never a route back into Restore previous release")
	require.False(t, closed.CanClose)
	require.Empty(t, activePlan(t, db, installation))
	var claims int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_application_claims WHERE preparation_id=$1`, r.Plan.Preparation.ID).Scan(&claims))
	require.Zero(t, claims)
	var closedBy, reason string
	var completed sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT closed_by,close_reason,completed_at FROM installation_revision_attempts WHERE plan_id=$1`, r.ID).Scan(&closedBy, &reason, &completed))
	require.Equal(t, r.Plan.RequestedBy, closedBy)
	require.Equal(t, closeReasonFixture, reason)
	require.False(t, completed.Valid, "closing claims no completion")

	again, err := public.CloseSelected(owner, installation, r.ID, "A different reason after a lost reply")
	require.NoError(t, err)
	require.Equal(t, closed, again, "a lost reply recovers the stored close and its first reason")

	stored, err := j.get(ctx, installation, r.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", stored.State)
	require.Nil(t, stored.Completion)
	require.False(t, stored.RollbackEligible)
	_, err = j.begin(ctx, installation, r.ID, r.Plan.ExecutionWorkflowDigest)
	require.Error(t, err, "a closed plan cannot start again")
	_, err = j.observe(ctx, installation, r.ID, stored.ObservationVersion, argocd.Facts{Healthy: true})
	require.ErrorIs(t, err, errChanged)
	_, err = j.cancel(ctx, installation, r.ID)
	require.Error(t, err)
	_, err = public.RollbackSelected(owner, installation, r.ID)
	require.ErrorContains(t, err, "cannot be restored")
	observed, err := public.ObserveSelected(owner, installation, r.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", observed.State)
	require.Equal(t, 1, api.writes, "closing wrote nothing to the Application")

	// The next preparation can take the installation and its Application.
	next := successorPreparation(t)
	_, err = preparationConnection(db).reserve(captureOperator(auth.RoleDeveloper, next.RequestedBy), next)
	require.NoError(t, err)
}

// A close does not need the Application at the plan's intent. One the plan's
// write never reached, and one a hand deploy has since moved on and is syncing,
// both hold no operation of this plan's.
func TestInstallationCloseSelectedAcceptsAnUntouchedOrHandDeployedApplication(t *testing.T) {
	t.Run("write never landed", func(t *testing.T) {
		_, public, _, r, api := closeFixture(t)
		closed, err := public.CloseSelected(operator(auth.RoleDeveloper, r.Plan.RequestedBy), r.Plan.InstallationID, r.ID, closeReasonFixture)
		require.NoError(t, err)
		require.Equal(t, "closed", closed.State)
		require.Zero(t, api.writes)
	})
	t.Run("moved on by hand", func(t *testing.T) {
		ctx, public, _, r, api := closeFixture(t)
		client, err := argocd.New(api)
		require.NoError(t, err)
		_, err = client.Apply(ctx, r.Plan.Intent)
		require.NoError(t, err)
		api.succeed(r.Plan.Intent.Revision)
		api.mu.Lock()
		resourceMap(resourceMap(api.object, "spec"), "source")["targetRevision"] = strings.Repeat("9", 40)
		api.object["operation"] = map[string]any{"initiatedBy": map[string]any{"username": "instance.argocd-sync"}, "sync": map[string]any{"revision": strings.Repeat("9", 40)}}
		resourceMap(api.object, "status")["operationState"] = map[string]any{"phase": "Running", "operation": api.object["operation"]}
		api.mu.Unlock()
		_, err = client.Observe(ctx, r.Plan.Intent)
		require.ErrorIs(t, err, argocd.ErrChanged, "the Application no longer sits at the plan's intent")
		closed, err := public.CloseSelected(operator(auth.RoleDeveloper, r.Plan.RequestedBy), r.Plan.InstallationID, r.ID, closeReasonFixture)
		require.NoError(t, err)
		require.Equal(t, "closed", closed.State)
	})
}

// An Application that cannot be read cannot show the plan's operation is over.
type closeUnreadableAPI struct{}

func (closeUnreadableAPI) Do(context.Context, string, string, string, []byte) ([]byte, error) {
	return nil, errors.New("Application read timed out")
}

func TestInstallationCloseSelectedRefusesAnUnreadableApplication(t *testing.T) {
	_, public, _, r, api := closeFixture(t)
	public.host.api = closeUnreadableAPI{}
	_, err := public.CloseSelected(operator(auth.RoleDeveloper, r.Plan.RequestedBy), r.Plan.InstallationID, r.ID, closeReasonFixture)
	require.ErrorContains(t, err, "could not read Application")
	require.Equal(t, r.ID, activePlan(t, api.db, r.Plan.InstallationID))
}

// A recorded restoration is one of the plan's own effects. While its reverse
// sync runs the close refuses; once it is over the plan closes, and the
// reversal stays readable as history while every further reverse step is fenced.
func TestInstallationCloseSelectedCoversARecordedRestoration(t *testing.T) {
	ctx, j, _, parent, reversal, api := rollbackJournalFixture(t)
	public := &Preparer{host: &preparationHost{api: api, journal: &preparationJournal{db: j.db}}}
	owner := operator(auth.RoleDeveloper, parent.Plan.RequestedBy)
	_, err := j.reserveRollback(ctx, parent.Plan.InstallationID, parent.ID, reversal)
	require.NoError(t, err)
	_, err = j.beginRollback(ctx, parent.Plan.InstallationID, parent.ID, reversal.WorkflowDigest)
	require.NoError(t, err)
	// A history-backed single-Application reversal records its own start
	// immediately before the write (perApplyStarts), as the rollback apply does.
	admission := revisionWriteAdmission{planID: parent.ID, workflow: parent.Plan.ExecutionWorkflowDigest, freshUntil: time.Now().Add(time.Minute)}
	_, err = j.beginRollbackApplication(ctx, parent.Plan.InstallationID, parent.ID, "", reversal.WorkflowDigest, admission)
	require.NoError(t, err)
	client, err := argocd.New(api)
	require.NoError(t, err)
	_, err = applyReversalIntent(ctx, client, reversal.Intent)
	require.NoError(t, err)
	_, err = public.CloseSelected(owner, parent.Plan.InstallationID, parent.ID, closeReasonFixture)
	require.ErrorContains(t, err, "still running")
	api.succeed(reversal.Intent.Revision)
	closed, err := public.CloseSelected(owner, parent.Plan.InstallationID, parent.ID, closeReasonFixture)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.State)
	require.Equal(t, "rollback", closed.Direction)
	stored, err := j.get(ctx, parent.Plan.InstallationID, parent.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Rollback, "the reversal remains in the closed plan's history")
	_, err = j.beginRollback(ctx, parent.Plan.InstallationID, parent.ID, reversal.WorkflowDigest)
	require.Error(t, err)
	_, err = j.observeRollback(ctx, parent.Plan.InstallationID, parent.ID, stored.Rollback.ObservationVersion, argocd.Facts{})
	require.ErrorIs(t, err, errChanged)
	require.Empty(t, activePlan(t, api.db, parent.Plan.InstallationID))
}

// closeGroupAPI routes each group member's Application to its own controller
// double. A path with no double is an Application the close must not read.
type closeGroupAPI map[string]*revisionControllerFixture

func (a closeGroupAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if c := a[path]; c != nil {
		return c.Do(ctx, method, path, contentType, body)
	}
	return nil, errors.New("close read an Application the plan never wrote")
}

// In a group only started members were ever written. The close reads each of
// them and nothing else, and names the member whose own sync is still going.
func TestInstallationCloseSelectedReadsEveryStartedGroupMemberOnly(t *testing.T) {
	db, peerDB := journalDB(t)
	preparations := preparationConnection(db)
	ctx, prepared, plan, evidence := groupPromotionFixture(t, preparations)
	r, err := preparations.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	j := &revisionJournal{db: func() *sql.DB { return db }}
	admission := revisionWriteAdmission{planID: r.ID, workflow: r.Plan.ExecutionWorkflowDigest, freshUntil: time.Now().Add(time.Minute)}
	r, err = j.beginAdmitted(ctx, r.Plan.InstallationID, r.ID, r.Plan.ExecutionWorkflowDigest, admission)
	require.NoError(t, err)
	r, err = j.beginApplication(ctx, r.Plan.InstallationID, r.ID, "engine", admission)
	require.NoError(t, err)
	engine, err := r.Plan.application("engine")
	require.NoError(t, err)
	member := r
	member.Plan.Intent = engine.Intent
	controller := newRevisionControllerFixture(t, db, member)
	api := closeGroupAPI{controller.path: controller}
	client, err := argocd.New(api)
	require.NoError(t, err)
	_, err = client.Apply(ctx, engine.Intent)
	require.NoError(t, err)
	public := &Preparer{host: &preparationHost{api: api, journal: preparationConnection(peerDB)}}
	owner := operator(auth.RoleDeveloper, r.Plan.RequestedBy)
	_, err = public.CloseSelected(owner, r.Plan.InstallationID, r.ID, closeReasonFixture)
	require.ErrorContains(t, err, "Application engine is still running")
	controller.succeed(engine.Intent.Revision)
	closed, err := public.CloseSelected(owner, r.Plan.InstallationID, r.ID, closeReasonFixture)
	require.NoError(t, err, "the unstarted product member was never written and is not read")
	require.Equal(t, "closed", closed.State)
	var claims int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_application_claims WHERE preparation_id=$1`, r.Plan.Preparation.ID).Scan(&claims))
	require.Zero(t, claims, "both members' claims are released")
}

// The journal transition alone: only applying, only its requester, only for
// the exact set of intents the caller classified, and never erased by a schema
// rollback. The table refuses a close marker the transition could not write.
func TestInstallationCloseJournalAdmitsOnlyTheClassifiedApplyingAttempt(t *testing.T) {
	db, _ := journalDB(t)
	preparations := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, preparations)
	r, err := preparations.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	j := &revisionJournal{db: func() *sql.DB { return db }}
	intentDigest, err := r.Plan.Intent.Digest()
	require.NoError(t, err)
	_, err = j.close(ctx, plan.InstallationID, r.ID, closeReasonFixture, nil)
	require.ErrorContains(t, err, "still being applied", "a prepared plan is cancelled, not closed")

	r, err = j.begin(ctx, plan.InstallationID, r.ID, plan.ExecutionWorkflowDigest)
	require.NoError(t, err)
	dispatched, err := r.dispatchedIntents()
	require.NoError(t, err)
	require.Len(t, dispatched, 1)
	_, err = j.close(captureOperator(auth.RoleOwner, "another-operator"), plan.InstallationID, r.ID, closeReasonFixture, []string{intentDigest})
	require.ErrorContains(t, err, "only the operator who started")
	_, err = j.close(ctx, plan.InstallationID, r.ID, "", []string{intentDigest})
	require.Error(t, err)
	_, err = j.close(ctx, plan.InstallationID, r.ID, closeReasonFixture, nil)
	require.ErrorIs(t, err, errChanged, "an intent the caller did not classify cannot be closed unexamined")
	_, err = j.close(ctx, plan.InstallationID, r.ID, closeReasonFixture, []string{intentDigest, "memql-id:" + strings.Repeat("0", 64)})
	require.ErrorIs(t, err, errChanged)
	require.Equal(t, r.ID, activePlan(t, db, plan.InstallationID))

	closed, err := j.close(ctx, plan.InstallationID, r.ID, closeReasonFixture, []string{intentDigest})
	require.NoError(t, err)
	require.Equal(t, "closed", closed.State)
	require.NotNil(t, closed.Closure)
	require.Equal(t, plan.RequestedBy, closed.Closure.By)
	require.Empty(t, activePlan(t, db, plan.InstallationID))

	for _, edit := range []string{
		`closed_by='another-operator'`,
		`close_reason=''`,
		`closed_at=NULL`,
		`state='applying'`,
		`completed_at=clock_timestamp(),completion_receipt='{}'::jsonb`,
	} {
		_, err = db.Exec(`UPDATE installation_revision_attempts SET `+edit+` WHERE plan_id=$1`, r.ID)
		require.Error(t, err, "the table refuses %s", edit)
	}
	down, err := os.ReadFile(closeMigrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(down))
	require.ErrorContains(t, err, "closed installation history cannot be discarded")
	_, err = j.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
}
