package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// This fixture uses real archive, render, signed release and preservation
// verifiers. Only the Kubernetes/renderer transports and artifact receipt are
// protocol fixtures; the competing connections share a real PostgreSQL journal.
func groupPreparationFixture(t *testing.T) (preparationScope, map[string][]byte) {
	t.Helper()
	scope, _ := preparationFixture(t)
	base := scope.Captures
	scope.FormatVersion = 2
	scope.Captures = map[string]sourceCaptureSpec{}
	scope.Applications = nil
	archives := map[string][]byte{}
	for _, key := range []string{"engine", "product"} {
		var intent argocd.Intent
		for _, role := range []string{"rollback", "candidate"} {
			render := base[role].Render
			render.AppName = key
			var source map[string]any
			require.NoError(t, json.Unmarshal(render.Source, &source))
			source["path"] = "deploy/" + key
			render.Source, _ = json.Marshal(source)
			spec, archive := captureRevisionWithRenderFixture(t, role, &render)
			spec.OwnerUserID = scope.RequestedBy
			spec.RunID = preparationSourceRun(scope.InstallationID, scope.RequestID, scope.RequestedBy)
			spec.WorkRunID = spec.RunID
			spec.RunStartedAt = base[role].RunStartedAt
			spec.StepKey = "source-" + applicationCaptureKey(key, role)
			scope.Captures[applicationCaptureKey(key, role)] = spec
			archives[applicationCaptureKey(key, role)] = archive
		}
		member := preparationScope{Captures: map[string]sourceCaptureSpec{"rollback": scope.Captures[key+"-rollback"], "candidate": scope.Captures[key+"-candidate"]}}
		bindPreparationIntent(t, &member)
		intent = member.Intent
		intent.Target.Name = key
		intent.Target.UID = "uid-" + key
		scope.Applications = append(scope.Applications, preparationApplication{key, intent})
	}
	scope.Intent = scope.Applications[0].Intent
	_, _, err := scope.canonical()
	require.NoError(t, err)
	return scope, archives
}
func groupPromotionFixture(t *testing.T, j *preparationJournal) (context.Context, preparationRecord, preparedPlan, promotionEvidence) {
	t.Helper()
	scope, archives := groupPreparationFixture(t)
	evidence := promotionEvidence{published: publishedFixture(t), sources: map[string]verifiedSourceCapture{}, renders: map[string]argocd.RenderedRevision{}}
	release, err := evidence.published.Release()
	require.NoError(t, err)
	scope.CandidateID, scope.PublicationDigest = release.CandidateID, evidence.published.Digest()
	ctx := captureOperator(auth.RoleDeveloper, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	api, before, after, protected := sensitiveFixture(t)
	for key, spec := range scope.Captures {
		capture, err := newSourceCapture(spec)
		require.NoError(t, err)
		executor, files := capturePorts(capture, archives[key])
		_, err = j.capture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, key, executor, "")
		require.NoError(t, err)
		proof, err := j.verifyCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, key, files)
		require.NoError(t, err)
		r, err = j.acknowledgeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, key, executor)
		require.NoError(t, err)
		evidence.sources[key] = proof
		rendered := before
		if strings.HasSuffix(key, "candidate") {
			rendered = after
		}
		manifests := []string{}
		for _, body := range rendered.Resources() {
			if strings.HasPrefix(key, "engine-") {
				manifests = append(manifests, string(body))
			} else if resourceText(workloadObject(t, string(body)), "kind") == "Deployment" {
				manifests = append(manifests, strings.ReplaceAll(string(body), `"bff"`, `"product"`))
			}
		}
		evidence.renders[key], err = argocd.RenderRevision(ctx, inventoryRenderer{manifests}, spec.Render, argocd.RepositoryCredentials{})
		require.NoError(t, err)
	}
	evidence.rollbackRender = renderInventory(scope, evidence.renders, "rollback")
	evidence.candidateRender = renderInventory(scope, evidence.renders, "candidate")
	bindings := resourceBindings()
	bindings[0].Application = "engine"
	product := bindings[0]
	product.Application = "product"
	product.Slot.Resource.Name = "product"
	bindings = append(bindings, product)
	evidence.resources, err = verifyImagesAndDiff(ctx, api, evidence.published, evidence.rollbackRender, evidence.candidateRender, "linux/arm64", bindings)
	require.NoError(t, err)
	evidence.storage, err = verifyStoragePreservation(ctx, api, evidence.rollbackRender, evidence.candidateRender)
	require.NoError(t, err)
	evidence.sensitive, err = verifySensitivePreservation(ctx, api, evidence.rollbackRender, evidence.candidateRender, protected)
	require.NoError(t, err)
	plan := testPlan()
	plan.FormatVersion = 2
	plan.InstallationID = scope.InstallationID
	plan.RequestedBy = scope.RequestedBy
	plan.WorkflowDigest = scope.WorkflowDigest
	plan.ExecutionWorkflowDigest = scope.ExecutionWorkflowDigest
	plan.CandidateID = scope.CandidateID
	plan.CandidateApprovalID = release.ApprovalID
	plan.PublicationDigest = scope.PublicationDigest
	plan.Intent = scope.Intent
	plan.RollbackRevision = beforeRevision(scope.Intent.BeforeSpec)
	plan.RenderDigest = evidence.candidateRender.Digest()
	plan.RollbackRenderDigest = evidence.rollbackRender.Digest()
	plan.ResourceDiffDigest = evidence.resources.digest
	for _, app := range scope.Applications {
		a, b := app.Key+"-candidate", app.Key+"-rollback"
		plan.Applications = append(plan.Applications, preparedApplication{Key: app.Key, Intent: app.Intent, CandidateCapture: a, RollbackCapture: b, RenderDigest: evidence.renders[a].Digest(), RollbackRenderDigest: evidence.renders[b].Digest(), CandidateSourceDigest: r.Captures[a].SourceDigest, RollbackSourceDigest: r.Captures[b].SourceDigest, CandidateReceiptDigest: r.Captures[a].ReceiptDigest, RollbackReceiptDigest: r.Captures[b].ReceiptDigest})
	}
	evidence.configuration = &receiverSnapshot{digest: scope.ConfigurationDigest, invariantDigest: scope.ConfigurationInvariantDigest, observed: time.Now().UTC()}
	evidence.artifacts = artifactEvidence{digest: scope.WorkflowDigest, workflow: scope.WorkflowDigest, operator: scope.RequestedBy, configuration: scope.ConfigurationDigest, candidate: plan.PublicationDigest, rollback: plan.PublicationDigest, resources: plan.ResourceDiffDigest, before: plan.RollbackRenderDigest, after: plan.RenderDigest, observed: time.Now().UTC(), expires: time.Now().Add(30 * time.Minute)}
	return ctx, r, plan, evidence
}
func TestInstallationGroupTargetsSerializeAcrossInstallationIDs(t *testing.T) {
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := groupPreparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	other := scope
	other.InstallationID = "other-installation"
	other.Captures = map[string]sourceCaptureSpec{}
	for key, spec := range scope.Captures {
		spec.RunID = preparationSourceRun(other.InstallationID, other.RequestID, other.RequestedBy)
		spec.WorkRunID = spec.RunID
		other.Captures[key] = spec
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	success := make(chan preparationRecord, 2)
	failure := make(chan error, 2)
	for i, s := range []preparationScope{scope, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			host := j
			if i == 1 {
				host = peer
			}
			r, e := host.reserve(ctx, s)
			if e != nil {
				failure <- e
			} else {
				success <- r
			}
		}()
	}
	close(start)
	wg.Wait()
	require.Len(t, success, 1)
	require.Len(t, failure, 1)
	winner := <-success
	require.ErrorContains(t, <-failure, "already claimed")
	retry, err := peer.reserve(ctx, winner.Scope)
	require.NoError(t, err)
	require.Equal(t, winner.ID, retry.ID)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_application_claims`).Scan(&count))
	require.Equal(t, 2, count)
	_, err = peer.cancelBeforeCapture(ctx, winner.Scope.InstallationID, winner.ID, winner.Scope.WorkflowDigest)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_application_claims`).Scan(&count))
	require.Zero(t, count)
	loser := scope
	if winner.Scope.InstallationID == scope.InstallationID {
		loser = other
	}
	_, err = j.reserve(ctx, loser)
	require.NoError(t, err)
}
func TestInstallationGroupPromotionAndPerApplicationJournal(t *testing.T) {
	db, peerDB := journalDB(t)
	p := preparationConnection(db)
	ctx, prepared, plan, evidence := groupPromotionFixture(t, p)
	forged := plan
	forged.Applications = append([]preparedApplication(nil), plan.Applications...)
	forged.Applications[1].RenderDigest = plan.RenderDigest
	_, err := p.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, forged, evidence)
	require.ErrorContains(t, err, "member differs")
	r, err := p.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	j, peer := &revisionJournal{db: func() *sql.DB { return db }}, &revisionJournal{db: func() *sql.DB { return peerDB }}
	admission := revisionWriteAdmission{planID: r.ID, workflow: r.Plan.ExecutionWorkflowDigest, freshUntil: time.Now().Add(time.Minute)}
	r, err = j.beginAdmitted(ctx, r.Plan.InstallationID, r.ID, r.Plan.ExecutionWorkflowDigest, admission)
	require.NoError(t, err)
	facts := argocd.Facts{IntentObserved: true, OperationSucceeded: true, RevisionObserved: true, Healthy: true, Synced: true, OperationPhase: "Succeeded"}
	for n, key := range []string{"engine", "product"} {
		r, err = peer.beginApplication(ctx, r.Plan.InstallationID, r.ID, key, admission)
		require.NoError(t, err)
		r, err = j.observeApplication(ctx, r.Plan.InstallationID, r.ID, key, 0, facts)
		require.NoError(t, err)
		if n == 0 {
			require.Nil(t, r.Observation, "one healthy Application cannot complete a group")
		}
	}
	require.True(t, completedController(*r.Observation))
	require.Equal(t, "applying", r.State)
	_, err = peer.observeApplication(ctx, r.Plan.InstallationID, r.ID, "engine", 0, facts)
	require.ErrorIs(t, err, errChanged)
	changedActor := captureOperator(auth.RoleOwner, "another-owner")
	_, err = peer.beginApplication(changedActor, r.Plan.InstallationID, r.ID, "engine", admission)
	require.Error(t, err)
	var active string
	require.NoError(t, db.QueryRow(`SELECT active_plan_id FROM installation_revision_heads WHERE installation_id=$1`, r.Plan.InstallationID).Scan(&active))
	require.Equal(t, r.ID, active)
}
