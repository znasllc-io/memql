package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

const migrationPath = "../../component/database/memory-nodes/migrations/20261007040000_installation_revisions"

func journalDB(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "installation revision journal", dbtest.DSN(), err)
	}
	schema := "installation_" + strings.ReplaceAll(id.NewShortId(), "-", "")
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); require.NoError(t, err) })
	open := func() *sql.DB {
		db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema})))
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		return db
	}
	first, second := open(), open()
	body, err := os.ReadFile(migrationPath + ".up.sql")
	require.NoError(t, err)
	_, err = first.ExecContext(ctx, string(body))
	require.NoError(t, err)
	return first, second
}

func operator(role auth.Role, user string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: role, UserId: "v1:identity:user:" + user})
}

func testPlan() preparedPlan {
	d := "memql-id:" + strings.Repeat("a", 64)
	before := strings.Repeat("b", 40)
	return preparedPlan{
		FormatVersion: 1, InstallationID: "installation-one", RequestedBy: "developer-one", WorkflowDigest: d,
		CandidateID: "sha256:" + strings.Repeat("c", 64), CandidateApprovalID: "owner-approval", PublicationDigest: "sha256:" + strings.Repeat("e", 64),
		RenderDigest: d, ResourceDiffDigest: d, RollbackRevision: before, RollbackRenderDigest: d,
		Intent: argocd.Intent{FormatVersion: 1, RequestID: id.NewShortId(), Target: argocd.Target{Namespace: "argocd", Name: "installation-one", UID: "application-uid"}, BeforeGeneration: 42,
			BeforeSpec: json.RawMessage(`{"source":{"repoURL":"https://github.com/acme/install.git","path":"deploy/overlay","targetRevision":"` + before + `"},"project":"installation","destination":{"server":"https://kubernetes.default.svc","namespace":"memql"}}`),
			Revision:   strings.Repeat("d", 40)},
	}
}

func TestJournalRejectsUnresolvedAndNonOperatorActorsBeforeDatabase(t *testing.T) {
	j := revisionJournal{db: func() *sql.DB { t.Fatal("denied actor reached database"); return nil }}
	contexts := []context.Context{context.Background(), operator(auth.RoleReader, "reader"), operator(auth.RoleWriter, "writer"), operator(auth.Role(""), "none"), operator(auth.RoleOwner, "")}
	for _, flag := range []string{"synthetic", "anonymous", "stand-in"} {
		actor := &auth.AccessContext{Role: auth.RoleOwner, UserId: "v1:identity:user:owner"}
		actor.Synthetic = flag == "synthetic"
		actor.IsAnonymous = flag == "anonymous"
		actor.RoleStandIn = flag == "stand-in"
		contexts = append(contexts, auth.ContextWithAccess(context.Background(), actor))
	}
	for _, ctx := range contexts {
		_, err := j.reserve(ctx, testPlan())
		require.Error(t, err)
		_, err = j.get(ctx, "installation", "key")
		require.Error(t, err)
		_, err = j.begin(ctx, "installation", "key", "workflow")
		require.Error(t, err)
		_, err = j.cancel(ctx, "installation", "key")
		require.Error(t, err)
		_, err = j.observe(ctx, "installation", "key", 0, argocd.Facts{})
		require.Error(t, err)
	}
}

func TestJournalConcurrentReplicasReserveAndStartOneImmutableIntent(t *testing.T) {
	db, peerDB := journalDB(t)
	first := revisionJournal{db: func() *sql.DB { return db }}
	peer := revisionJournal{db: func() *sql.DB { return peerDB }}
	ctx := operator(auth.RoleDeveloper, "developer-one")
	plan := testPlan()
	results := make(chan revisionRecord, 12)
	failures := make(chan error, 12)
	var wg sync.WaitGroup
	for n := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j := &first
			if n%2 == 0 {
				j = &peer
			}
			r, err := j.reserve(ctx, plan)
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
	key := ""
	for r := range results {
		if key == "" {
			key = r.ID
		}
		require.Equal(t, key, r.ID)
		require.Equal(t, "prepared", r.State)
		require.EqualValues(t, 1, r.SlotEpoch)
	}
	require.NotEmpty(t, key)
	started, err := first.begin(ctx, plan.InstallationID, key, plan.WorkflowDigest)
	require.NoError(t, err)
	// Drop the first host's return value and all native objects. A different
	// connection and newly constructed journal recover the same external intent.
	peer = revisionJournal{db: func() *sql.DB { return peerDB }}
	recovered, err := peer.begin(ctx, plan.InstallationID, key, plan.WorkflowDigest)
	require.NoError(t, err)
	require.Equal(t, started, recovered)
	require.Equal(t, plan.Intent.RequestID, recovered.Plan.Intent.RequestID)
	require.Equal(t, "applying", recovered.State)
	changed := testPlan()
	_, err = peer.reserve(ctx, changed)
	require.ErrorIs(t, err, errBusy)
	_, err = peer.cancel(ctx, plan.InstallationID, key)
	require.Error(t, err)
	// Read-only recovery does not require loading the original workflow. A new
	// workflow may NOT turn an old approval into fresh write authority.
	_, err = peer.begin(ctx, plan.InstallationID, key, "memql-id:"+strings.Repeat("e", 64))
	require.Error(t, err)
	_, err = peer.get(operator(auth.RoleAdmin, "replacement"), plan.InstallationID, key)
	require.NoError(t, err)
}

func TestJournalDistinctConcurrentRequestsCannotBothAcquireInstallation(t *testing.T) {
	db, peerDB := journalDB(t)
	journals := []revisionJournal{{db: func() *sql.DB { return db }}, {db: func() *sql.DB { return peerDB }}}
	ctx := operator(auth.RoleDeveloper, "developer-one")
	gate := make(chan struct{})
	errs := make(chan error, 2)
	for n := range 2 {
		go func() { <-gate; _, err := journals[n].reserve(ctx, testPlan()); errs <- err }()
	}
	close(gate)
	wins := 0
	for range 2 {
		if err := <-errs; err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, errBusy)
		}
	}
	require.Equal(t, 1, wins)
}

func TestJournalCancellationFencesLateStartAndCannotReleaseSuccessor(t *testing.T) {
	db, peerDB := journalDB(t)
	first := revisionJournal{db: func() *sql.DB { return db }}
	peer := revisionJournal{db: func() *sql.DB { return peerDB }}
	ctx := operator(auth.RoleOwner, "developer-one")
	plan := testPlan()
	old, err := first.reserve(ctx, plan)
	require.NoError(t, err)
	_, err = peer.cancel(operator(auth.RoleDeveloper, "somebody-else"), plan.InstallationID, old.ID)
	require.Error(t, err)
	cancelled, err := peer.cancel(ctx, plan.InstallationID, old.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", cancelled.State)
	_, err = first.begin(ctx, plan.InstallationID, old.ID, plan.WorkflowDigest)
	require.Error(t, err)
	_, err = first.reserve(ctx, plan)
	require.ErrorContains(t, err, "permanently cancelled")
	next, err := peer.reserve(ctx, testPlan())
	require.NoError(t, err)
	require.EqualValues(t, 2, next.SlotEpoch)
	_, err = first.cancel(ctx, plan.InstallationID, old.ID)
	require.NoError(t, err)
	_, err = peer.get(ctx, plan.InstallationID, next.ID)
	require.NoError(t, err)
	_, err = first.begin(ctx, plan.InstallationID, old.ID, plan.WorkflowDigest)
	require.Error(t, err)
	_, err = peer.begin(ctx, plan.InstallationID, next.ID, plan.WorkflowDigest)
	require.NoError(t, err)
}

func TestJournalConcurrentStartAndCancellationHaveOnlyOneWinner(t *testing.T) {
	db, peerDB := journalDB(t)
	first := revisionJournal{db: func() *sql.DB { return db }}
	peer := revisionJournal{db: func() *sql.DB { return peerDB }}
	ctx := operator(auth.RoleDeveloper, "developer-one")
	for range 8 {
		plan := testPlan()
		plan.InstallationID = "race-" + id.NewShortId()
		r, err := first.reserve(ctx, plan)
		require.NoError(t, err)
		gate := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-gate
			_, err := first.begin(ctx, plan.InstallationID, r.ID, plan.WorkflowDigest)
			results <- err
		}()
		go func() {
			<-gate
			_, err := peer.cancel(ctx, plan.InstallationID, r.ID)
			results <- err
		}()
		close(gate)
		wins := 0
		for range 2 {
			if err := <-results; err == nil {
				wins++
			}
		}
		require.Equal(t, 1, wins)
		stored, err := peer.get(ctx, plan.InstallationID, r.ID)
		require.NoError(t, err)
		require.Contains(t, []string{"applying", "cancelled"}, stored.State)
		successor := testPlan()
		successor.InstallationID = plan.InstallationID
		_, err = first.reserve(ctx, successor)
		if stored.State == "applying" {
			require.ErrorIs(t, err, errBusy)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestJournalLateObservationCannotOverwriteNewerFactsOrCompleteInstallation(t *testing.T) {
	db, peerDB := journalDB(t)
	first := revisionJournal{db: func() *sql.DB { return db }}
	peer := revisionJournal{db: func() *sql.DB { return peerDB }}
	ctx := operator(auth.RoleDeveloper, "developer-one")
	plan := testPlan()
	r, err := first.reserve(ctx, plan)
	require.NoError(t, err)
	_, err = first.observe(ctx, plan.InstallationID, r.ID, 0, argocd.Facts{})
	require.ErrorIs(t, err, errChanged)
	_, err = first.begin(ctx, plan.InstallationID, r.ID, plan.WorkflowDigest)
	require.NoError(t, err)
	good := argocd.Facts{IntentObserved: true, OperationPhase: "Succeeded", OperationSucceeded: true, RevisionObserved: true, Healthy: true, Synced: true}
	observed, err := peer.observe(operator(auth.RoleAdmin, "replacement"), plan.InstallationID, r.ID, 0, good)
	require.NoError(t, err)
	require.EqualValues(t, 1, observed.ObservationVersion)
	require.Equal(t, "applying", observed.State, "Argo success alone is not installation success")
	_, err = first.observe(ctx, plan.InstallationID, r.ID, 0, argocd.Facts{OperationPhase: "Running"})
	require.ErrorIs(t, err, errChanged)
	recovered, err := first.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, &good, recovered.Observation)
	_, err = peer.reserve(ctx, testPlan())
	require.ErrorIs(t, err, errBusy)
	_, err = peer.cancel(ctx, plan.InstallationID, r.ID)
	require.Error(t, err)
}

func TestJournalRevalidatesStoredAuthorityAndRetainsHistoryOnMigrationRollback(t *testing.T) {
	db, _ := journalDB(t)
	j := revisionJournal{db: func() *sql.DB { return db }}
	ctx := operator(auth.RoleDeveloper, "developer-one")
	plan := testPlan()
	r, err := j.reserve(ctx, plan)
	require.NoError(t, err)
	down, err := os.ReadFile(migrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(down))
	require.ErrorContains(t, err, "preserve its effect and cancellation records")
	_, err = j.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	for _, edit := range []string{
		`jsonb_set(plan,'{intent,revision}','"` + strings.Repeat("f", 40) + `"'::jsonb)`,
		`plan || '{"unknown":true}'::jsonb`,
		`plan || '{"RequestedBy":"developer-one"}'::jsonb`,
		`jsonb_set(plan,'{intent,unknown}','true'::jsonb)`,
	} {
		body, _, err := plan.canonical()
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE installation_revision_attempts SET plan=$2::jsonb WHERE plan_id=$1`, r.ID, string(body))
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE installation_revision_attempts SET plan=`+edit+` WHERE plan_id=$1`, r.ID)
		require.NoError(t, err)
		_, err = j.begin(ctx, plan.InstallationID, r.ID, plan.WorkflowDigest)
		require.Error(t, err)
	}
}

func TestInstallationPlanRequiresImmutableRollbackAndExactEvidenceBindings(t *testing.T) {
	plan := testPlan()
	_, key, err := plan.canonical()
	require.NoError(t, err)
	for _, mutate := range []func(*preparedPlan){
		func(p *preparedPlan) { p.RollbackRevision = "main" },
		func(p *preparedPlan) { p.RollbackRevision = strings.Repeat("f", 40) },
		func(p *preparedPlan) { p.Intent.Revision = p.RollbackRevision },
		func(p *preparedPlan) { p.PublicationDigest = "" },
		func(p *preparedPlan) { p.PublicationDigest = "memql-id:" + strings.Repeat("e", 64) },
		func(p *preparedPlan) { p.RenderDigest = "passed" },
		func(p *preparedPlan) { p.ResourceDiffDigest = "" },
		func(p *preparedPlan) { p.CandidateApprovalID = "" },
	} {
		changed := plan
		mutate(&changed)
		_, _, err := changed.canonical()
		require.Error(t, err)
	}
	changed := plan
	changed.RenderDigest = "memql-id:" + strings.Repeat("e", 64)
	_, other, err := changed.canonical()
	require.NoError(t, err)
	require.NotEqual(t, key, other)
}

func TestInstallationJournalMigrationCanRollBackOnlyWhileEmpty(t *testing.T) {
	db, _ := journalDB(t)
	down, err := os.ReadFile(migrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(down))
	require.NoError(t, err)
	var absent bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('installation_revision_attempts') IS NULL AND to_regclass('installation_revision_heads') IS NULL`).Scan(&absent))
	require.True(t, absent)
}
