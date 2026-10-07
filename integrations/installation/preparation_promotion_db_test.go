package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// This fixture runs the real native source/archive, render, publication and
// preservation verifiers over protocol doubles; PostgreSQL is real. It does
// not qualify the installed renderer or configuration provider.
func promotionFixture(t *testing.T, j *preparationJournal) (context.Context, preparationRecord, preparedPlan, promotionEvidence) {
	t.Helper()
	scope, candidateArchive := preparationFixture(t)
	rollback, rollbackArchive := captureRevisionFixture(t, "rollback-source-commit")
	spec := scope.Captures["candidate"]
	rollback.RunID, rollback.WorkRunID = spec.RunID, spec.WorkRunID
	rollback.RunStartedAt, rollback.StepKey = spec.RunStartedAt, "source-rollback"
	scope.Captures["rollback"] = rollback
	evidence := promotionEvidence{published: publishedFixture(t)}
	release, err := evidence.published.Release()
	require.NoError(t, err)
	scope.CandidateID, scope.PublicationDigest = release.CandidateID, evidence.published.Digest()
	ctx := captureOperator(auth.RoleDeveloper, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	for role, archive := range map[string][]byte{"candidate": candidateArchive, "rollback": rollbackArchive} {
		capture, err := newSourceCapture(scope.Captures[role])
		require.NoError(t, err)
		executor, files := capturePorts(capture, archive)
		_, err = j.capture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, role, executor, "")
		require.NoError(t, err)
		proof, err := j.verifyCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, role, files)
		require.NoError(t, err)
		r, err = j.acknowledgeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, role, executor)
		require.NoError(t, err)
		if role == "candidate" {
			evidence.candidateSource = proof
		} else {
			evidence.rollbackSource = proof
		}
	}
	api, before, after, protected := sensitiveFixture(t)
	manifests := func(render argocd.RenderedRevision) []string {
		out := []string{}
		for _, body := range render.Resources() {
			out = append(out, string(body))
		}
		return out
	}
	evidence.candidateRender, err = argocd.RenderRevision(ctx, inventoryRenderer{manifests(after)}, spec.Render, argocd.RepositoryCredentials{})
	require.NoError(t, err)
	evidence.rollbackRender, err = argocd.RenderRevision(ctx, inventoryRenderer{manifests(before)}, rollback.Render, argocd.RepositoryCredentials{})
	require.NoError(t, err)
	before, after = evidence.rollbackRender, evidence.candidateRender
	evidence.resources, err = verifyImagesAndDiff(ctx, api, evidence.published, before, after, spec.Platform, resourceBindings())
	require.NoError(t, err)
	evidence.storage, err = verifyStoragePreservation(ctx, api, before, after)
	require.NoError(t, err)
	evidence.sensitive, err = verifySensitivePreservation(ctx, api, before, after, protected)
	require.NoError(t, err)
	plan := testPlan()
	plan.RequestedBy, plan.WorkflowDigest = scope.RequestedBy, scope.WorkflowDigest
	plan.CandidateID, plan.CandidateApprovalID, plan.PublicationDigest = release.CandidateID, release.ApprovalID, evidence.published.Digest()
	plan.RenderDigest, plan.RollbackRenderDigest, plan.ResourceDiffDigest = after.Digest(), before.Digest(), evidence.resources.digest
	var source struct{ TargetRevision string }
	require.NoError(t, json.Unmarshal(spec.Render.Source, &source))
	plan.Intent.Revision = source.TargetRevision
	require.NoError(t, json.Unmarshal(rollback.Render.Source, &source))
	plan.RollbackRevision = source.TargetRevision
	plan.Intent.Target.Name = spec.Render.AppName
	plan.Intent.BeforeSpec, err = json.Marshal(map[string]any{"source": json.RawMessage(rollback.Render.Source), "project": spec.Render.ProjectName,
		"destination": map[string]string{"server": "https://kubernetes.default.svc", "namespace": spec.Render.Namespace}})
	require.NoError(t, err)
	return ctx, r, plan, evidence
}

func TestPreparationPromotionTransfersOneHeadAcrossReplicas(t *testing.T) {
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	ctx, prepared, plan, evidence := promotionFixture(t, j)
	scope := prepared.Scope
	results := make(chan revisionRecord, 8)
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := j
			if n%2 != 0 {
				host = peer
			}
			r, err := host.promote(ctx, scope.InstallationID, prepared.ID, scope.WorkflowDigest, scope.ConfigurationDigest, plan, evidence)
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
	var promoted revisionRecord
	for r := range results {
		if promoted.ID == "" {
			promoted = r
		}
		require.Equal(t, promoted, r)
	}
	require.NotEmpty(t, promoted.ID)
	require.Equal(t, prepared.SlotEpoch+1, promoted.SlotEpoch)
	require.Equal(t, "prepared", promoted.State)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_revision_attempts`).Scan(&count))
	require.Equal(t, 1, count)
	// Discard the original host's response and recover via only the request ID.
	recovered, err := peer.getByRequest(ctx, scope.InstallationID, scope.RequestID)
	require.NoError(t, err)
	require.Equal(t, promoted.ID, recovered.PromotedPlanID)
	require.Equal(t, prepared.Captures, recovered.Captures)
	_, _, err = j.beginCapture(ctx, scope.InstallationID, prepared.ID, scope.WorkflowDigest, "candidate")
	require.Error(t, err, "a late source callback cannot reopen promoted preparation")
	_, err = j.cancelBeforeCapture(ctx, scope.InstallationID, prepared.ID, scope.WorkflowDigest)
	require.Error(t, err)
	changed := plan
	changed.Intent.RequestID = "changed-intent"
	_, err = peer.promote(ctx, scope.InstallationID, prepared.ID, scope.WorkflowDigest, scope.ConfigurationDigest, changed, evidence)
	require.ErrorIs(t, err, errChanged)
	// The source binding cannot be copied into the direct reservation path.
	revisions := revisionJournal{db: func() *sql.DB { return peerDB }}
	_, err = revisions.reserve(ctx, promoted.Plan)
	require.ErrorContains(t, err, "atomic preparation")
	_, err = revisions.cancel(ctx, scope.InstallationID, promoted.ID)
	require.NoError(t, err)
	next := scope
	next.RequestID = "successor"
	next.Captures = maps.Clone(scope.Captures)
	for role, spec := range next.Captures {
		spec.RunID = preparationSourceRun(next.InstallationID, next.RequestID, next.RequestedBy)
		spec.WorkRunID = spec.RunID
		next.Captures[role] = spec
	}
	successor, err := j.reserve(ctx, next)
	require.NoError(t, err)
	old, err := peer.promote(ctx, scope.InstallationID, prepared.ID, scope.WorkflowDigest, scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	require.Equal(t, "cancelled", old.State)
	current, err := j.get(ctx, next.InstallationID, successor.ID)
	require.NoError(t, err)
	require.Equal(t, successor, current, "historical replay must not change a successor's head")
	_, err = revisions.begin(ctx, scope.InstallationID, promoted.ID, scope.WorkflowDigest)
	require.Error(t, err)
	// Migration rollback must not erase the source/revision relationship.
	down, err := os.ReadFile(promotionMigrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(down))
	require.ErrorContains(t, err, "promotion history is not empty")
}

func TestPreparationPromotionRollsBackEveryJournalWriteOnFailure(t *testing.T) {
	db, peerDB := journalDB(t)
	j := preparationConnection(db)
	ctx, r, plan, evidence := promotionFixture(t, j)
	_, err := db.Exec(`CREATE FUNCTION refuse_promotion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.active_plan_id IS NOT NULL THEN RAISE EXCEPTION 'fixture head write failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER refuse_promotion BEFORE UPDATE ON installation_revision_heads FOR EACH ROW EXECUTE FUNCTION refuse_promotion()`)
	require.NoError(t, err)
	_, err = j.promote(ctx, r.Scope.InstallationID, r.ID, r.Scope.WorkflowDigest, r.Scope.ConfigurationDigest, plan, evidence)
	require.ErrorContains(t, err, "fixture head write failure")
	peer := preparationConnection(peerDB)
	unchanged, err := peer.get(ctx, r.Scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, r, unchanged)
	var count int
	require.NoError(t, peerDB.QueryRow(`SELECT count(*) FROM installation_revision_attempts`).Scan(&count))
	require.Zero(t, count)
	_, err = db.Exec(`DROP TRIGGER refuse_promotion ON installation_revision_heads`)
	require.NoError(t, err)
	_, err = peer.promote(ctx, r.Scope.InstallationID, r.ID, r.Scope.WorkflowDigest, r.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
}

func TestPreparationPromotionRejectsChangedAuthorityAndEvidence(t *testing.T) {
	db, _ := journalDB(t)
	j := preparationConnection(db)
	ctx, r, plan, evidence := promotionFixture(t, j)
	for _, fault := range []string{"operator", "role", "origin", "workflow", "configuration", "candidate", "approval", "source", "receipt", "render", "resource-diff", "storage", "sensitive", "destination", "source-path", "application"} {
		t.Run(fault, func(t *testing.T) {
			caller, p, e := ctx, plan, evidence
			workflow, configuration := r.Scope.WorkflowDigest, r.Scope.ConfigurationDigest
			switch fault {
			case "operator":
				caller = captureOperator(auth.RoleOwner, "another")
			case "role":
				caller = captureOperator(auth.RoleReader, r.Scope.RequestedBy)
			case "origin":
				caller = operator(auth.RoleOwner, r.Scope.RequestedBy)
			case "workflow":
				workflow = "memql-id:" + strings.Repeat("f", 64)
			case "configuration":
				configuration = "memql-id:" + strings.Repeat("f", 64)
			case "candidate":
				p.CandidateID = "sha256:" + strings.Repeat("f", 64)
			case "approval":
				p.CandidateApprovalID = "another-approval"
			case "source":
				e.candidateSource = e.rollbackSource
			case "receipt":
				e.candidateSource.receipt.IntentID = "different-receipt"
			case "render":
				e.candidateRender = e.rollbackRender
			case "resource-diff":
				e.resources.before = e.resources.after
			case "storage":
				e.storage = storageEvidence{}
			case "sensitive":
				e.sensitive = sensitiveEvidence{}
			case "destination":
				p.Intent.BeforeSpec = []byte(strings.ReplaceAll(string(p.Intent.BeforeSpec), `"namespace":"memql"`, `"namespace":"other"`))
			case "source-path":
				p.Intent.BeforeSpec = []byte(strings.ReplaceAll(string(p.Intent.BeforeSpec), `"path":"overlay"`, `"path":"other"`))
			case "application":
				p.Intent.Target.Name = "another"
			}
			_, err := j.promote(caller, r.Scope.InstallationID, r.ID, workflow, configuration, p, e)
			require.Error(t, err)
			current, err := j.get(ctx, r.Scope.InstallationID, r.ID)
			require.NoError(t, err)
			require.Equal(t, r, current)
		})
	}
}
