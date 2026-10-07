package installation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// This double performs the adapter's JSON Patch compare-and-swap against an
// Application. The real PostgreSQL journal must show started before any PATCH.
// Installed Argo and serving continuity have separate qualification.
type revisionControllerFixture struct {
	mu        sync.Mutex
	db        *sql.DB
	key, path string
	object    map[string]any
	writes    int
	loseReply bool
	readDelay time.Duration
}

func newRevisionControllerFixture(t *testing.T, db *sql.DB, r revisionRecord) *revisionControllerFixture {
	t.Helper()
	var spec map[string]any
	require.NoError(t, json.Unmarshal(r.Plan.Intent.BeforeSpec, &spec))
	target := r.Plan.Intent.Target
	return &revisionControllerFixture{db: db, key: r.ID,
		path: "apis/argoproj.io/v1alpha1/namespaces/" + target.Namespace + "/applications/" + target.Name,
		object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": map[string]any{"namespace": target.Namespace, "name": target.Name, "uid": target.UID, "resourceVersion": "1", "generation": r.Plan.Intent.BeforeGeneration},
			"spec":     spec, "status": map[string]any{}},
	}
}

func (f *revisionControllerFixture) Do(ctx context.Context, method, path, _ string, body []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path != f.path {
		return nil, errors.New("unexpected Application path")
	}
	if method == http.MethodGet {
		if f.readDelay > 0 {
			timer := time.NewTimer(f.readDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		return json.Marshal(f.object)
	}
	if method != http.MethodPatch {
		return nil, errors.New("unexpected controller operation")
	}
	var state string
	if err := f.db.QueryRowContext(ctx, "SELECT state FROM installation_revision_attempts WHERE plan_id=$1", f.key).Scan(&state); err != nil || state != "applying" {
		return nil, errors.New("controller write preceded durable start")
	}
	var patch []struct {
		Op, Path string
		Value    any
	}
	if json.Unmarshal(body, &patch) != nil {
		return nil, errors.New("bad JSON patch")
	}
	encoded, _ := json.Marshal(f.object)
	var next map[string]any
	_ = json.Unmarshal(encoded, &next)
	for _, operation := range patch {
		parts := strings.Split(strings.TrimPrefix(operation.Path, "/"), "/")
		parent := next
		for _, part := range parts[:len(parts)-1] {
			parent, _ = parent[part].(map[string]any)
			if parent == nil {
				return nil, errors.New("missing patch parent")
			}
		}
		key := parts[len(parts)-1]
		switch operation.Op {
		case "test":
			if !sameJSON(parent[key], operation.Value) {
				return nil, errors.New("patch conflict")
			}
		case "add", "replace":
			parent[key] = operation.Value
		default:
			return nil, errors.New("unsupported patch")
		}
	}
	f.writes++
	resourceMap(next, "metadata")["resourceVersion"] = strconv.Itoa(f.writes + 1)
	f.object = next
	if f.loseReply {
		f.loseReply = false
		return nil, errors.New("lost private response after commit")
	}
	return json.Marshal(f.object)
}

func (f *revisionControllerFixture) succeed(revision string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	source := resourceMap(resourceMap(f.object, "spec"), "source")
	operation := f.object["operation"]
	f.object["operation"] = nil
	f.object["status"] = map[string]any{
		"health":         map[string]any{"status": "Healthy"},
		"sync":           map[string]any{"status": "Synced", "revision": revision, "comparedTo": map[string]any{"source": source}},
		"operationState": map[string]any{"phase": "Succeeded", "operation": operation, "syncResult": map[string]any{"revision": revision, "source": source}},
	}
}

func (f *revisionControllerFixture) hasWritten() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes > 0
}

func TestRevisionWorkflowCannotApplyFromCachedConfigurationOnly(t *testing.T) {
	db, _ := journalDB(t)
	w, err := loadRevisionWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	preparation := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, preparation, w.digest)
	r, err := preparation.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	api := newRevisionControllerFixture(t, db, r)
	j := &revisionJournal{db: func() *sql.DB { return db }}
	_, err = w.run(ctx, j, plan.InstallationID, r.ID, revisionStart, evidence.configuration, nil)
	require.ErrorContains(t, err, "complete native preparation host")
	require.Zero(t, api.writes)
	current, err := j.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "prepared", current.State)
	require.Zero(t, current.ObservationVersion)
}

func TestRevisionWorkflowRequiresFreshConfigurationAndExactExecutionBinding(t *testing.T) {
	db, _ := journalDB(t)
	w, err := loadRevisionWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	j := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, j, w.digest)
	changed := plan
	changed.ExecutionWorkflowDigest = plan.WorkflowDigest
	_, err = j.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, changed, evidence)
	require.Error(t, err, "preparation recipe is not execution authority")
	r, err := j.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	api := newRevisionControllerFixture(t, db, r)
	revisions := &revisionJournal{db: func() *sql.DB { return db }}
	for _, cfg := range []*receiverSnapshot{nil, {digest: evidence.configuration.digest, observed: time.Now().Add(-2 * time.Minute)}, {digest: "changed", observed: time.Now()}} {
		_, err = w.run(ctx, revisions, plan.InstallationID, r.ID, revisionStart, cfg, nil)
		require.Error(t, err)
	}
	for _, actor := range []context.Context{context.Background(), captureOperator(auth.RoleOwner, "other"), operator(auth.RoleOwner, plan.RequestedBy)} {
		_, err = w.run(actor, revisions, plan.InstallationID, r.ID, revisionStart, evidence.configuration, nil)
		require.Error(t, err)
	}
	require.Zero(t, api.writes)
	r, err = revisions.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "prepared", r.State)
	_, err = revisions.begin(ctx, plan.InstallationID, r.ID, w.digest)
	require.NoError(t, err)
	client, err := argocd.New(api)
	require.NoError(t, err)
	_, err = client.Apply(ctx, plan.Intent)
	require.NoError(t, err)
	api.succeed(plan.Intent.Revision)
	changedWorkflow, err := loadRevisionWorkflow(strings.Repeat("e", 40))
	require.NoError(t, err)
	_, err = changedWorkflow.run(ctx, revisions, plan.InstallationID, r.ID, revisionStart, nil, nil)
	require.ErrorContains(t, err, "recipe changed")
	observed, err := changedWorkflow.run(ctx, revisions, plan.InstallationID, r.ID, revisionObserve, nil, &preparationHost{api: api})
	require.NoError(t, err)
	require.True(t, observed.Observation.OperationSucceeded)
	require.True(t, observed.Observation.Healthy)
	require.True(t, observed.Observation.Synced)
	require.Equal(t, "applying", observed.State, "Argo success alone must not complete or release an installation")
}

func TestRevisionWorkflowRecipeCannotForgeNativeProgressOrOmitStart(t *testing.T) {
	for name, body := range map[string]string{
		"forged return":   `return {complete:true}`,
		"missing begin":   `builtin installationRevisionApply()`,
		"ignored refusal": `builtin installationRevisionApply() on error continue`,
		"unbound child":   `automation unboundRevision()`,
	} {
		t.Run(name, func(t *testing.T) {
			db, _ := journalDB(t)
			definition, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation alternateRevision {\n"+body+"\n}", "revision-test.memql")
			require.NoError(t, err)
			definition.Trusted = true // installed-source fixture only
			w, err := newRevisionWorkflow(definition, strings.Repeat("d", 40))
			require.NoError(t, err)
			j := preparationConnection(db)
			ctx, prepared, plan, evidence := promotionFixture(t, j, w.digest)
			r, err := j.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
			require.NoError(t, err)
			api := newRevisionControllerFixture(t, db, r)
			_, err = w.run(ctx, &revisionJournal{db: func() *sql.DB { return db }}, plan.InstallationID, r.ID, revisionObserve, evidence.configuration, &preparationHost{api: api})
			require.Error(t, err)
			require.Zero(t, api.writes)
		})
	}
}

func TestRevisionWriteAdmissionRequalifiesExpiredPromotedArtifacts(t *testing.T) {
	now := time.Now().UTC()
	digest := func(label string) string { return artifactHash("revision-admission-test", label) }
	configurationDigest, invariantDigest := digest("configuration"), digest("invariant")
	resourceDigest, storageDigest, sensitiveDigest := digest("resources"), digest("storage"), digest("sensitive")
	beforeDigest, afterDigest := digest("before"), digest("after")
	publication, rollbackPublication := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	workflowDigest, artifactWorkflowDigest := digest("revision-workflow"), digest("artifact-workflow")
	scope := &revisionWorkflowScope{
		workflow: &revisionWorkflow{digest: workflowDigest, artifacts: &artifactWorkflow{digest: artifactWorkflowDigest}},
		operator: "operator", configuration: &receiverSnapshot{digest: configurationDigest, invariantDigest: invariantDigest, observed: now},
		preflightComplete: true, reobserved: true,
		resources: resourceEvidence{digest: resourceDigest, publication: publication, before: beforeDigest, after: afterDigest, observed: now},
		artifacts: &artifactEvidence{workflow: artifactWorkflowDigest, operator: "operator", configuration: configurationDigest, candidate: publication, rollback: rollbackPublication,
			resources: resourceDigest, before: beforeDigest, after: afterDigest, observed: now, expires: now.Add(20 * time.Minute)},
		storage:   storageEvidence{digest: storageDigest, before: beforeDigest, after: afterDigest, observed: now},
		sensitive: sensitiveEvidence{digest: sensitiveDigest, before: beforeDigest, after: afterDigest, observed: now},
	}
	record := revisionRecord{ID: digest("plan"), Plan: preparedPlan{PublicationDigest: publication, ResourceDiffDigest: resourceDigest, RenderDigest: afterDigest, RollbackRenderDigest: beforeDigest,
		Preparation: &preparationBinding{ConfigurationDigest: configurationDigest, ConfigurationInvariantDigest: invariantDigest, ArtifactWorkflowDigest: artifactWorkflowDigest,
			ArtifactDigest: digest("old-artifacts"), ArtifactExpiresAt: now.Add(-time.Hour).Format(time.RFC3339Nano), RollbackPublicationDigest: rollbackPublication,
			StorageDigest: storageDigest, SensitiveDigest: sensitiveDigest}}}

	admission, err := scope.freshWriteEvidence(record)
	require.NoError(t, err, "fresh exact-plan OCI reads and preservation observations replace an expired historical receipt")
	require.Equal(t, record.ID, admission.planID)
	require.Equal(t, workflowDigest, admission.workflow)
	require.True(t, admission.freshUntil.After(now))
	require.LessOrEqual(t, admission.freshUntil.Sub(now), time.Minute)

	stale := *scope
	stale.storage.observed = now.Add(-2 * time.Minute)
	_, err = stale.freshWriteEvidence(record)
	require.ErrorContains(t, err, "fresh storage")

	staleResources := *scope
	staleResources.resources.observed = now.Add(-2 * time.Minute)
	_, err = staleResources.freshWriteEvidence(record)
	require.ErrorContains(t, err, "fresh resource")

	changed := *scope
	changed.artifacts = &artifactEvidence{workflow: artifactWorkflowDigest, operator: "other", configuration: configurationDigest, candidate: publication, rollback: rollbackPublication,
		resources: resourceDigest, before: beforeDigest, after: afterDigest, observed: now, expires: now.Add(20 * time.Minute)}
	_, err = changed.freshWriteEvidence(record)
	require.ErrorContains(t, err, "immutable artifact")
}

func TestRevisionApplyDoesNotWriteAfterPreservationEvidenceExpiresDuringObserve(t *testing.T) {
	db, _ := journalDB(t)
	w, err := loadRevisionWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	preparation := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, preparation, w.digest)
	r, err := preparation.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	journal := &revisionJournal{db: func() *sql.DB { return db }}
	_, err = journal.begin(ctx, plan.InstallationID, r.ID, w.digest)
	require.NoError(t, err)
	controller := newRevisionControllerFixture(t, db, r)
	controller.readDelay = 1500 * time.Millisecond
	client, err := argocd.New(controller)
	require.NoError(t, err)
	now := time.Now().UTC()
	binding := r.Plan.Preparation
	scope := &revisionWorkflowScope{workflow: w, journal: journal, client: client, installation: plan.InstallationID, key: r.ID,
		operator: plan.RequestedBy, mode: revisionStart, needsWrite: true, preflightComplete: true, reobserved: true,
		configuration: &receiverSnapshot{digest: binding.ConfigurationDigest, invariantDigest: binding.ConfigurationInvariantDigest, observed: now.Add(-59 * time.Second)},
		resources:     resourceEvidence{digest: r.Plan.ResourceDiffDigest, publication: r.Plan.PublicationDigest, before: r.Plan.RollbackRenderDigest, after: r.Plan.RenderDigest, observed: now.Add(-59 * time.Second)},
		artifacts: &artifactEvidence{workflow: w.artifacts.digest, operator: plan.RequestedBy, configuration: binding.ConfigurationDigest, candidate: r.Plan.PublicationDigest,
			rollback: binding.RollbackPublicationDigest, resources: r.Plan.ResourceDiffDigest, before: r.Plan.RollbackRenderDigest, after: r.Plan.RenderDigest, observed: now, expires: now.Add(20 * time.Minute)},
		storage:   storageEvidence{digest: binding.StorageDigest, before: r.Plan.RollbackRenderDigest, after: r.Plan.RenderDigest, observed: now.Add(-59 * time.Second)},
		sensitive: sensitiveEvidence{digest: binding.SensitiveDigest, before: r.Plan.RollbackRenderDigest, after: r.Plan.RenderDigest, observed: now.Add(-59 * time.Second)}}
	_, err = scope.apply(ctx, nil)
	require.Error(t, err)
	require.Zero(t, controller.writes, "the preflight read consumed the remaining preservation-evidence window")
	current, err := journal.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "applying", current.State, "an uncertain started attempt retains its slot for fresh reconciliation")
}

func TestRevisionWorkflowCrashBeforeDispatchRequiresNativeRequalification(t *testing.T) {
	db, peerDB := journalDB(t)
	w, err := loadRevisionWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	j := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, j, w.digest)
	r, err := j.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	original := &revisionJournal{db: func() *sql.DB { return db }}
	_, err = original.begin(ctx, plan.InstallationID, r.ID, w.digest)
	require.NoError(t, err) // Crash before the first external request.
	api := newRevisionControllerFixture(t, db, r)
	peer := &revisionJournal{db: func() *sql.DB { return peerDB }}
	_, err = w.run(ctx, peer, plan.InstallationID, r.ID, revisionStart, nil, nil)
	require.ErrorContains(t, err, "complete native preparation host")
	require.Zero(t, api.writes)
	current, err := peer.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "applying", current.State, "uncertain start must retain ownership")
	require.Zero(t, current.ObservationVersion)
	_, err = w.run(ctx, peer, plan.InstallationID, r.ID, revisionStart, evidence.configuration, nil)
	require.ErrorContains(t, err, "complete native preparation host")
	require.Zero(t, api.writes)
}

var _ argocd.API = (*revisionControllerFixture)(nil)
