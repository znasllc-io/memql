package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func preparationFixture(t *testing.T) (preparationScope, []byte) {
	t.Helper()
	spec, body := captureFixture(t)
	scope := preparationScope{FormatVersion: 1, InstallationID: "installation-one", RequestID: "request-one", RequestedBy: spec.OwnerUserID,
		WorkflowDigest: "memql-id:" + strings.Repeat("a", 64), ExecutionWorkflowDigest: "memql-id:" + strings.Repeat("e", 64), ConfigurationDigest: "memql-id:" + strings.Repeat("b", 64), ConfigurationInvariantDigest: "memql-id:" + strings.Repeat("f", 64), CandidateID: "sha256:" + strings.Repeat("c", 64), PublicationDigest: "sha256:" + strings.Repeat("d", 64)}
	spec.RunID = preparationSourceRun(scope.InstallationID, scope.RequestID, scope.RequestedBy)
	spec.WorkRunID = spec.RunID
	spec.StepKey = "source-candidate"
	rollback := spec
	rollback.StepKey = "source-rollback"
	var source map[string]any
	require.NoError(t, json.Unmarshal(spec.Render.Source, &source))
	source["targetRevision"] = strings.Repeat("a", 40)
	rollback.Render.Source, _ = json.Marshal(source)
	scope.Captures = map[string]sourceCaptureSpec{"candidate": spec, "rollback": rollback}
	bindPreparationIntent(t, &scope)
	return scope, body
}

func bindPreparationIntent(t *testing.T, scope *preparationScope) {
	t.Helper()
	scope.Intent = testPlan().Intent
	before, after := scope.Captures["rollback"].Render, scope.Captures["candidate"].Render
	var source struct{ TargetRevision string }
	require.NoError(t, json.Unmarshal(after.Source, &source))
	scope.Intent.Revision, scope.Intent.Target.Name = source.TargetRevision, after.AppName
	var err error
	scope.Intent.BeforeSpec, err = json.Marshal(map[string]any{"source": json.RawMessage(before.Source), "project": before.ProjectName,
		"destination": map[string]string{"server": "https://kubernetes.default.svc", "namespace": before.Namespace}})
	require.NoError(t, err)
}

func preparationConnection(db *sql.DB) *preparationJournal {
	return &preparationJournal{db: func() *sql.DB { return db }}
}

func TestPreparationRejectsUnadmittedActorsAndInvalidScopesBeforeStorage(t *testing.T) {
	scope, _ := preparationFixture(t)
	j := preparationJournal{db: func() *sql.DB { t.Fatal("unauthorized preparation accessed storage"); return nil }}
	for _, ctx := range []context.Context{context.Background(), operator(auth.RoleOwner, scope.RequestedBy), captureOperator(auth.RoleReader, scope.RequestedBy), captureOperator(auth.RoleOwner, "other")} {
		_, err := j.reserve(ctx, scope)
		require.Error(t, err)
	}
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	for _, fault := range []string{"workflow", "role", "foreign-run", "missing-start", "same-revision"} {
		t.Run(fault, func(t *testing.T) {
			changed, _ := preparationFixture(t)
			switch fault {
			case "workflow":
				changed.WorkflowDigest = ""
			case "role":
				delete(changed.Captures, "rollback")
			case "foreign-run":
				s := changed.Captures["candidate"]
				s.RunID = "another-run"
				changed.Captures["candidate"] = s
			case "missing-start":
				s := changed.Captures["candidate"]
				s.RunStartedAt = ""
				changed.Captures["candidate"] = s
			case "same-revision":
				s := changed.Captures["rollback"]
				s.Render = changed.Captures["candidate"].Render
				changed.Captures["rollback"] = s
			}
			_, err := j.reserve(ctx, changed)
			require.Error(t, err)
		})
	}
}

func TestPreparationConcurrentReplicasCommitOneDispatchIntent(t *testing.T) {
	db, peerDB := journalDB(t)
	first, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleDeveloper, scope.RequestedBy)
	record, err := first.reserve(ctx, scope)
	require.NoError(t, err)
	again, err := peer.reserve(ctx, scope)
	require.NoError(t, err)
	require.Equal(t, record, again)
	var mu sync.Mutex
	created, recovered := 0, 0
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j := first
			if n%2 != 0 {
				j = peer
			}
			r, recoverOnly, err := j.beginCapture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate")
			require.NoError(t, err)
			require.True(t, r.Captures["candidate"].Started)
			mu.Lock()
			defer mu.Unlock()
			if recoverOnly {
				recovered++
			} else {
				created++
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, created)
	require.Equal(t, 7, recovered)
	_, err = peer.cancelBeforeCapture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest)
	require.Error(t, err)
	_, _, err = peer.beginCapture(ctx, scope.InstallationID, record.ID, "memql-id:"+strings.Repeat("f", 64), "rollback")
	require.Error(t, err)
}

func TestPreparationRequestLookupRecoversOriginalScopeWithoutNewConfiguration(t *testing.T) {
	db, peerDB := journalDB(t)
	first, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	_, err := peer.getByRequest(ctx, scope.InstallationID, scope.RequestID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	r, err := first.reserve(ctx, scope)
	require.NoError(t, err)
	recovered, err := peer.getByRequest(ctx, scope.InstallationID, scope.RequestID)
	require.NoError(t, err)
	require.Equal(t, r, recovered)
	_, err = peer.getByRequest(ctx, "other-installation", scope.RequestID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = first.cancelBeforeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest)
	require.NoError(t, err)
	recovered, err = peer.getByRequest(ctx, scope.InstallationID, scope.RequestID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", recovered.State)
	want, wantID, err := scope.canonical()
	require.NoError(t, err)
	got, gotID, err := recovered.Scope.canonical()
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, wantID, gotID)
	_, err = peer.reserve(ctx, recovered.Scope)
	require.Error(t, err, "a retry cannot reopen the cancelled request")
}

func TestPreparationRechecksEffectAuthorityOnReplacementHost(t *testing.T) {
	db, peerDB := journalDB(t)
	first, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, body := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	r, err := first.reserve(ctx, scope)
	require.NoError(t, err)
	capture, err := newSourceCapture(scope.Captures["candidate"])
	require.NoError(t, err)
	executor, files := capturePorts(capture, body)
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		workflow string
	}{
		{"different-operator", captureOperator(auth.RoleAdmin, "another"), scope.WorkflowDigest},
		{"revoked-role", captureOperator(auth.RoleReader, scope.RequestedBy), scope.WorkflowDigest},
		{"client-origin", operator(auth.RoleOwner, scope.RequestedBy), scope.WorkflowDigest},
		{"changed-workflow", ctx, "memql-id:" + strings.Repeat("f", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := peer.capture(tc.ctx, scope.InstallationID, r.ID, tc.workflow, "candidate", executor, "")
			require.Error(t, err)
			_, err = peer.verifyCapture(tc.ctx, scope.InstallationID, r.ID, tc.workflow, "candidate", files)
			require.Error(t, err)
			_, err = peer.acknowledgeCapture(tc.ctx, scope.InstallationID, r.ID, tc.workflow, "candidate", executor)
			require.Error(t, err)
			_, err = peer.cancelBeforeCapture(tc.ctx, scope.InstallationID, r.ID, tc.workflow)
			require.Error(t, err)
		})
	}
	require.Empty(t, executor.requests)
	require.Zero(t, executor.acked)
	require.Zero(t, files.opens)
	unchanged, err := first.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, r, unchanged)
}

func TestPreparationOwnsSharedInstallationHeadAndPermanentlyFencesCancellation(t *testing.T) {
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	plan := testPlan()
	plan.RequestedBy = scope.RequestedBy
	_, err = (&revisionJournal{db: func() *sql.DB { return peerDB }}).reserve(ctx, plan)
	require.ErrorIs(t, err, errBusy)
	_, err = j.cancelBeforeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest)
	require.NoError(t, err)
	_, err = peer.reserve(ctx, scope)
	require.Error(t, err)
	_, _, err = peer.beginCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate")
	require.Error(t, err)
	_, err = (&revisionJournal{db: func() *sql.DB { return peerDB }}).reserve(ctx, plan)
	require.NoError(t, err)
	_, err = j.cancelBeforeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest)
	require.Error(t, err, "late cancel cannot release the revision slot")
	other, _ := preparationFixture(t)
	other.RequestID = "next-request"
	for role, s := range other.Captures {
		s.RunID = preparationSourceRun(other.InstallationID, other.RequestID, other.RequestedBy)
		s.WorkRunID = s.RunID
		other.Captures[role] = s
	}
	_, err = peer.reserve(ctx, other)
	require.ErrorIs(t, err, errBusy)
}

func TestPreparationRecoversCaptureBeforeAcknowledgingItsJob(t *testing.T) {
	db, peerDB := journalDB(t)
	first, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, body := preparationFixture(t)
	ctx := captureOperator(auth.RoleAdmin, scope.RequestedBy)
	r, err := first.reserve(ctx, scope)
	require.NoError(t, err)
	capture, err := newSourceCapture(scope.Captures["candidate"])
	require.NoError(t, err)
	executor, files := capturePorts(capture, body)
	_, err = first.acknowledgeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", executor)
	require.Error(t, err)
	require.Zero(t, executor.acked)
	// Dispatch committed, then the original driver vanished before saving its
	// external reply. A different DB connection must request recovery only.
	_, recoverOnly, err := first.beginCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate")
	require.NoError(t, err)
	require.False(t, recoverOnly)
	_, err = capture.run(ctx, executor, false, "")
	require.NoError(t, err)
	// The receiver completed its work, but that reply was not journaled.
	r, err = peer.capture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", executor, "")
	require.NoError(t, err)
	require.Len(t, executor.requests, 2)
	require.False(t, executor.requests[0].RecoverOnly)
	require.True(t, executor.requests[1].RecoverOnly)
	require.NotNil(t, r.Captures["candidate"].Receipt)
	require.Empty(t, r.Captures["candidate"].SourceDigest)
	require.Zero(t, executor.acked)
	_, err = first.capture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", executor, "")
	require.NoError(t, err)
	require.Len(t, executor.requests, 2)
	_, err = first.acknowledgeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", executor)
	require.Error(t, err)
	require.Zero(t, executor.acked)
	proof, err := peer.verifyCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", files)
	require.NoError(t, err)
	r, err = first.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, proof.source.Digest(), r.Captures["candidate"].SourceDigest)
	executor.ackError = errors.New("lost cleanup reply")
	_, err = first.acknowledgeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", executor)
	require.Error(t, err)
	r, err = peer.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.False(t, r.Captures["candidate"].Acknowledged)
	executor.ackError = nil
	r, err = peer.acknowledgeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", executor)
	require.NoError(t, err)
	require.True(t, r.Captures["candidate"].Acknowledged)
	_, err = first.acknowledgeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", executor)
	require.NoError(t, err)
	require.Equal(t, 2, executor.acked)
	_, err = first.verifyCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", files)
	require.NoError(t, err)
	require.Equal(t, 2, files.opens, "persisted hashes cannot replace fresh archive verification")
	files.body[0] ^= 1
	_, err = peer.verifyCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", files)
	require.Error(t, err)
	require.NotContains(t, r.String(), "private-source")
}

func TestPreparationRejectsChangedRequestInputsAndStoredAuthority(t *testing.T) {
	db, _ := journalDB(t)
	j := preparationConnection(db)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	changed := scope
	changed.ConfigurationDigest = "memql-id:" + strings.Repeat("e", 64)
	_, err = j.reserve(ctx, changed)
	require.ErrorContains(t, err, "reused")
	_, err = db.Exec(`UPDATE installation_preparations SET scope=jsonb_set(scope,'{requestedBy}','"another"') WHERE preparation_id=$1`, r.ID)
	require.NoError(t, err)
	_, err = j.get(ctx, scope.InstallationID, r.ID)
	require.Error(t, err)
}

func TestPreparationMigrationNeverErasesDispatchHistory(t *testing.T) {
	db, _ := journalDB(t)
	j := preparationConnection(db)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	_, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	down, err := os.ReadFile(preparationMigrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(down))
	require.ErrorContains(t, err, "history is not empty")
}
