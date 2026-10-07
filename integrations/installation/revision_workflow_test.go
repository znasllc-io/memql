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

func TestRevisionWorkflowLostApplyReplyRecoversWithoutSecondWrite(t *testing.T) {
	db, peerDB := journalDB(t)
	w, err := loadRevisionWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	preparation := preparationConnection(db)
	ctx, prepared, plan, evidence := promotionFixture(t, preparation, w.digest)
	r, err := preparation.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	api := newRevisionControllerFixture(t, db, r)
	api.loseReply = true
	j := &revisionJournal{db: func() *sql.DB { return db }}
	_, err = w.run(ctx, j, api, plan.InstallationID, r.ID, revisionStart, evidence.configuration)
	require.ErrorContains(t, err, "unconfirmed")
	require.NotContains(t, err.Error(), "private response")
	current, err := j.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "applying", current.State)
	require.Zero(t, current.ObservationVersion)
	peer := &revisionJournal{db: func() *sql.DB { return peerDB }}
	fresh, err := loadRevisionWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	current, err = fresh.run(ctx, peer, api, plan.InstallationID, r.ID, revisionStart, nil)
	require.NoError(t, err)
	require.Equal(t, 1, api.writes)
	require.True(t, current.Observation.IntentObserved)
	api.succeed(plan.Intent.Revision)
	changed, err := loadRevisionWorkflow(strings.Repeat("e", 40))
	require.NoError(t, err)
	_, err = changed.run(ctx, peer, api, plan.InstallationID, r.ID, revisionStart, nil)
	require.ErrorContains(t, err, "recipe changed")
	current, err = changed.run(ctx, peer, api, plan.InstallationID, r.ID, revisionObserve, nil)
	require.NoError(t, err)
	require.True(t, current.Observation.OperationSucceeded)
	require.True(t, current.Observation.Healthy)
	require.True(t, current.Observation.Synced)
	require.Equal(t, "applying", current.State, "Argo success alone must not complete or release an installation")
	require.Equal(t, 1, api.writes)
	competing := testPlan()
	competing.RequestedBy = plan.RequestedBy
	_, err = peer.reserve(ctx, competing)
	require.ErrorIs(t, err, errBusy)
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
		_, err = w.run(ctx, revisions, api, plan.InstallationID, r.ID, revisionStart, cfg)
		require.Error(t, err)
	}
	for _, actor := range []context.Context{context.Background(), captureOperator(auth.RoleOwner, "other"), operator(auth.RoleOwner, plan.RequestedBy)} {
		_, err = w.run(actor, revisions, api, plan.InstallationID, r.ID, revisionStart, evidence.configuration)
		require.Error(t, err)
	}
	require.Zero(t, api.writes)
	r, err = revisions.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "prepared", r.State)
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
			_, err = w.run(ctx, &revisionJournal{db: func() *sql.DB { return db }}, api, plan.InstallationID, r.ID, revisionStart, evidence.configuration)
			require.Error(t, err)
			require.Zero(t, api.writes)
		})
	}
}

func TestRevisionWorkflowCrashBeforeDispatchDoesNotReuseStaleAdmission(t *testing.T) {
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
	_, err = w.run(ctx, peer, api, plan.InstallationID, r.ID, revisionStart, nil)
	require.ErrorContains(t, err, "freshly authenticated")
	require.Zero(t, api.writes)
	current, err := peer.get(ctx, plan.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "applying", current.State, "uncertain start must retain ownership")
	require.Zero(t, current.ObservationVersion)
	_, err = w.run(ctx, peer, api, plan.InstallationID, r.ID, revisionStart, evidence.configuration)
	require.NoError(t, err)
	require.Equal(t, 1, api.writes)
}

var _ argocd.API = (*revisionControllerFixture)(nil)
