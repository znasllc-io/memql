package installation

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// lifecycleHarness is a two-Application installation (engine and product), or
// with newSingleLifecycleHarness a single Application, brought to a completed
// update through the installed preparation, revision and completion recipes,
// with real PostgreSQL journals on two connections, real source, render,
// publication, image and preservation verification, and history-capable
// controller doubles for every Application. It follows the group protocol
// regression's setup (group_protocol_test.go); the scenarios here are the ways
// a healthy cluster stranded a plan (fixes 1, 4 and 5 of the 2026-11-09 audit,
// and the review's S1-S4 that followed). Installed Argo and serving continuity
// are not qualified.
type lifecycleHarness struct {
	t                         *testing.T
	ctx                       context.Context
	db, peerDB                *sql.DB
	receiver                  *receiverFixture
	host                      *preparationHost
	external                  *preparationProtocolFiles
	request                   preparationRequest
	record                    revisionRecord
	controllers               map[string]*historyController
	enginePath, productPath   string
	rollbackSHA, candidateSHA string
	beforeDigest, afterDigest string
	beforeCommit, afterCommit string
	manifests                 func(digest string, product bool) []string

	mu          sync.Mutex
	serving     string
	migrated    bool
	healthCalls int
	// onHealth runs on every authenticated serving probe with its 1-based call
	// number; a test uses it to change the cluster between a restoration's
	// planning and its writes, at the probe its apply step makes.
	onHealth      func(call int)
	faultReserved []bool
}

type lifecycleAPI struct {
	receiver    *receiverFixture
	controllers map[string]*historyController
	members     map[string]string
}

func (a lifecycleAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if u, err := url.Parse(path); err == nil && strings.HasSuffix(u.Path, "/applications") {
		name := strings.TrimPrefix(u.Query().Get("fieldSelector"), "metadata.name=")
		if c := a.controllers[u.Path+"/"+name]; c != nil && (c.hasWritten() || u.Query().Get("watch") == "true") {
			return c.Do(ctx, method, path, contentType, body)
		}
	}
	if c := a.controllers[path]; c != nil && (method != http.MethodGet || c.hasWritten()) {
		if method == http.MethodPatch {
			r, err := (&revisionJournal{db: func() *sql.DB { return c.db }}).get(ctx, a.receiver.config.InstallationID, c.key)
			if err != nil {
				return nil, err
			}
			// Every write needs the start that covers it: a group member's own
			// start, the applying state of a single Application, and for a
			// reversal its member's reverse start.
			member := a.members[path]
			started := r.State == "applying"
			if r.Plan.FormatVersion == 2 {
				started = r.Applications[member].Started
			}
			if r.Rollback != nil {
				reverse, _ := r.Rollback.application(member)
				started = reverse.Started
			}
			if !started {
				return nil, errors.New("controller effect preceded its durable start")
			}
		}
		return c.Do(ctx, method, path, contentType, body)
	}
	return a.receiver.Do(ctx, method, path, contentType, body)
}

// reacquireRollback is the restoration admission engines up to 0.31.28 made:
// it takes the installation head back for a completed attempt on its own,
// leaving it reverting with no reversal, before any restoration check ran. No
// production path makes it any more (RollbackSelected takes the head only
// together with the reversal, in reserveCompletedRollback); tests use it to
// reproduce the state such an engine left, and the group protocol's races use
// it to exercise the eligibility and transfer rules the reservation shares
// (reacquireCompleted).
func (j *revisionJournal) reacquireRollback(ctx context.Context, installation, key string) (revisionRecord, error) {
	return j.withRecord(ctx, installation, key, func(tx *sql.Tx, r *revisionRecord, actor string) error {
		allowed, err := rollbackEligibility(ctx, tx, *r, actor)
		if err != nil {
			return err
		}
		if !allowed {
			return errRestoreIneligible
		}
		if r.rollbackActive() {
			return nil // Another replica already admitted this exact request.
		}
		return reacquireCompleted(ctx, tx, installation, key, *r, actor)
	})
}

// newLifecycleHarness is the two-Application group.
func newLifecycleHarness(t *testing.T) *lifecycleHarness {
	t.Helper()
	return newLifecycleHarnessOf(t, true)
}

// newSingleLifecycleHarness is a single-Application installation (format 1),
// whose reversal has no member starts of its own (perApplyStarts).
func newSingleLifecycleHarness(t *testing.T) *lifecycleHarness {
	t.Helper()
	return newLifecycleHarnessOf(t, false)
}

func newLifecycleHarnessOf(t *testing.T, group bool) *lifecycleHarness {
	t.Helper()
	h := &lifecycleHarness{t: t}
	h.db, h.peerDB = journalDB(t)
	receiver := newReceiverFixture(t)
	h.receiver = receiver
	images := newAdmissionFixture(t)
	registry := httptest.NewTLSServer(images.server.Config.Handler)
	t.Cleanup(registry.Close)
	images.server.Close()
	before, err := images.old.Release()
	require.NoError(t, err)
	after, err := images.next.Release()
	require.NoError(t, err)
	before.Components[0].Artifacts[0].Locations[0].Origin = registry.URL
	after.Components[0].Artifacts[0].Locations[0].Origin = registry.URL
	images.old, images.next = signAdmissionRelease(t, before), signAdmissionRelease(t, after)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: registry.Certificate().Raw})
	receiver.put("v1", "Secret", "secrets", "memql", "registry-ca", map[string]any{"data": map[string]any{"ca": base64.StdEncoding.EncodeToString(ca)}})
	receiver.config.Registries = []receiverRegistry{{Origin: registry.URL, Repository: "memql/engine", RootCA: &receiverSecret{Name: "registry-ca", Key: "ca"}}}
	receiver.config.Rollback = receiverSelection{before.CandidateID, images.old.Digest()}
	receiver.config.Health = &receiverHealth{Endpoint: "https://api.installation.example", Component: "engine", Credential: receiverSecret{Name: "catalog", Key: "token"}}
	receiver.config.Protected = []receiverProtected{{Namespace: "memql", Name: "keys", Kind: "Secret"}}
	receiver.config.ImageBindings[0].Container = "engine"
	if group {
		receiver.config.FormatVersion = 2
		receiver.config.Application = receiverNamed{}
		receiver.config.Applications = []receiverApplication{{Key: "engine", Namespace: "argocd", Name: "memql"}, {Key: "product", Namespace: "argocd", Name: "product"}}
		receiver.config.ImageBindings[0].Application = "engine"
		productBinding := receiver.config.ImageBindings[0]
		productBinding.Application = "product"
		productBinding.Name = "product"
		receiver.config.ImageBindings = append(receiver.config.ImageBindings, productBinding)
	}
	receiver.saveConfig()
	api, sensitiveBefore, _, _ := sensitiveFixture(t)
	for path, body := range api.response {
		var object map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &object))
		receiver.objects[path] = object
	}
	rollbackSpec, _ := captureRevisionFixture(t, "rollback")
	candidateSpec, _ := captureRevisionFixture(t, "candidate")
	var rollbackSource, candidateSource map[string]any
	require.NoError(t, json.Unmarshal(rollbackSpec.Render.Source, &rollbackSource))
	require.NoError(t, json.Unmarshal(candidateSpec.Render.Source, &candidateSource))
	h.enginePath = "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"
	app := receiver.objects[h.enginePath]
	resourceMap(app, "spec")["source"] = rollbackSource
	resourceMap(resourceMap(app, "status"), "sync")["revision"] = rollbackSource["targetRevision"]
	resourceMap(receiver.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects/default"], "spec")["sourceRepos"] = []any{rollbackSource["repoURL"]}
	if group {
		raw, _ := json.Marshal(app)
		var productApp map[string]any
		require.NoError(t, json.Unmarshal(raw, &productApp))
		productApp["metadata"] = fixtureMeta("product", "argocd")
		resourceMap(resourceMap(productApp, "spec"), "source")["path"] = "deploy/product"
		h.productPath = "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/product"
		receiver.objects[h.productPath] = productApp
	}
	publications := preparationCatalogFixture{images.old.Digest(): images.old, images.next.Digest(): images.next}
	factory := func(context.Context, []byte, string) (Catalog, error) { return publications, nil }
	h.ctx = captureOperator(auth.RoleOwner, "operator")
	snapshot, err := readReceiver(h.ctx, receiver, "memql", "receiver", factory)
	require.NoError(t, err)
	archives := map[string][]byte{}
	for _, member := range snapshot.applicationMembers() {
		for _, role := range []string{"rollback", "candidate"} {
			_, archive := captureRevisionWithRenderFixture(t, role, &member.render)
			archives[applicationCaptureKey(member.key, role)] = archive
		}
	}
	h.rollbackSHA, h.candidateSHA = rollbackSource["targetRevision"].(string), candidateSource["targetRevision"].(string)
	h.beforeDigest, h.afterDigest = before.Components[0].Artifacts[0].ImageDigest, after.Components[0].Artifacts[0].ImageDigest
	h.beforeCommit, h.afterCommit = before.Components[0].Commit, after.Components[0].Commit
	h.manifests = func(digest string, product bool) []string {
		result := []string{}
		for _, body := range sensitiveBefore.Resources() {
			text := strings.ReplaceAll(string(body), oldImage, strings.TrimPrefix(registry.URL, "https://")+"/memql/engine@"+digest)
			text = strings.Replace(text, `"spec":{"template"`, `"spec":{"selector":{"matchLabels":{"app":"bff"}},"template"`, 1)
			if product {
				if resourceText(workloadObject(t, text), "kind") != "Deployment" {
					continue
				}
				text = strings.ReplaceAll(text, `"bff"`, `"product"`)
			}
			result = append(result, text)
		}
		return result
	}
	rpc := &preparationRendererFixture{applicationManifests: map[string]map[string][]string{
		"memql":   {h.rollbackSHA: h.manifests(h.beforeDigest, false), h.candidateSHA: h.manifests(h.afterDigest, false)},
		"product": {h.rollbackSHA: h.manifests(h.beforeDigest, true), h.candidateSHA: h.manifests(h.afterDigest, true)},
	}, reads: map[string]int{}}
	address := preparationRendererServer(t, receiver.certificate, rpc)
	h.request = preparationRequest{InstallationID: receiver.config.InstallationID, RequestID: "restore-lifecycle", CandidateID: after.CandidateID, CatalogDigest: images.next.Digest(), OverlayRevision: h.candidateSHA}
	h.external = &preparationProtocolFiles{t: t, journal: preparationConnection(h.db), request: h.request, archives: archives, files: map[string]*captureFilesFixture{}}
	h.host = &preparationHost{api: receiver, journal: h.external.journal, executor: h.external, files: h.external, tokens: preparationTokenFixture{}, catalog: factory, namespace: "memql", configurationName: "receiver", rendererAddressOverride: address}
	h.serving = h.beforeCommit
	h.host.health = func(_ context.Context, _, _, _, nonce string) (HealthObservation, error) {
		h.mu.Lock()
		h.healthCalls++
		call, hook := h.healthCalls, h.onHealth
		h.mu.Unlock()
		if hook != nil {
			hook(call)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		migration := "sha256:" + strings.Repeat("c", 64)
		if h.migrated {
			migration = "sha256:" + strings.Repeat("d", 64)
		}
		return HealthObservation{UserID: "operator", IdentityID: "identity", EngineCommit: h.serving, MigrationsDigest: migration, Nonce: nonce}, nil
	}
	require.NoError(t, h.host.bindWorkflow(strings.Repeat("a", 40)))
	h.record, err = h.host.prepare(h.ctx, h.request)
	require.NoError(t, err)
	if group {
		require.Len(t, h.record.Plan.Applications, 2)
	} else {
		require.Equal(t, 1, h.record.Plan.FormatVersion)
	}
	h.controllers = map[string]*historyController{}
	members := map[string]string{}
	for _, member := range h.record.Plan.applicationMembers() {
		one := h.record
		one.Plan.Intent = member.Intent
		c := newHistoryController(t, h.db, one)
		b, _ := json.Marshal(receiver.objects[c.path])
		require.NoError(t, json.Unmarshal(b, &c.object))
		h.controllers[c.path] = c
		members[c.path] = member.Key
	}
	h.host.api = lifecycleAPI{receiver: receiver, controllers: h.controllers, members: members}
	return h
}

func (h *lifecycleHarness) owner() context.Context { return operator(auth.RoleOwner, "operator") }

func (h *lifecycleHarness) public() *Preparer { return &Preparer{host: h.host} }

// replica is another engine on the second database connection, as a
// replacement serving engine would be.
func (h *lifecycleHarness) replica() *Preparer {
	fresh := *h.host
	fresh.journal = preparationConnection(h.peerDB)
	require.NoError(h.t, fresh.bindWorkflow(strings.Repeat("f", 40)))
	return &Preparer{host: &fresh}
}

func (h *lifecycleHarness) setServing(commit string, migrated bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serving, h.migrated = commit, migrated
}

func (h *lifecycleHarness) setHealthHook(hook func(call int)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onHealth, h.healthCalls = hook, 0
}

// atFirstApply runs fault at the first member's apply: the restoration's second
// authenticated probe, after the reservation's. It also records whether the
// reversal was reserved and its Begin step committed by then, so
// requireFaultAfterReservation can prove the fault landed between those and
// the writes even if probes move.
func (h *lifecycleHarness) atFirstApply(fault func()) {
	h.setHealthHook(func(call int) {
		if call != 2 {
			return
		}
		var reserved int
		_ = h.db.QueryRow(`SELECT count(*) FROM installation_revision_rollbacks WHERE parent_plan_id=$1 AND started_at IS NOT NULL`, h.record.ID).Scan(&reserved)
		h.mu.Lock()
		h.faultReserved = append(h.faultReserved, reserved == 1)
		h.mu.Unlock()
		fault()
	})
}

func (h *lifecycleHarness) requireFaultAfterReservation() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Equal(h.t, []bool{true}, h.faultReserved, "the fault ran once, after the reversal was reserved and begun")
	h.faultReserved = nil
}

// reversal is the attempt's recorded reversal as the journal reads it.
func (h *lifecycleHarness) reversal() *rollbackRecord {
	h.t.Helper()
	r, err := (&revisionJournal{db: func() *sql.DB { return h.db }}).get(h.ctx, h.request.InstallationID, h.record.ID)
	require.NoError(h.t, err)
	require.NotNil(h.t, r.Rollback)
	return r.Rollback
}

// readApplications reads every member Application as it stands now.
func (h *lifecycleHarness) readApplications() map[string]argocd.Snapshot {
	h.t.Helper()
	client, err := argocd.New(h.host.api)
	require.NoError(h.t, err)
	out := map[string]argocd.Snapshot{}
	for _, app := range h.record.Plan.applicationMembers() {
		snapshot, err := client.Read(h.ctx, app.Intent.Target)
		require.NoError(h.t, err)
		out[app.Key] = snapshot
	}
	return out
}

// asLegacyReversal stores the reserved reversal as an engine up to 0.31.28
// planned it: each member's intent format 1, whose patch tests the exact
// generation of the Application as read in planned, under the request ids the
// reservation chose. With unstart it also clears every start, as that engine
// left a reservation whose Begin step never ran.
func (h *lifecycleHarness) asLegacyReversal(planned map[string]argocd.Snapshot, unstart bool) {
	t := h.t
	t.Helper()
	parent, err := (&revisionJournal{db: func() *sql.DB { return h.db }}).get(h.ctx, h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.NotNil(t, parent.Rollback)
	p := parent.Rollback.Plan
	require.Equal(t, 2, p.FormatVersion)
	for i, app := range p.Applications {
		legacy, err := argocd.PlanRevision(planned[app.Key], app.Intent.RequestID, h.rollbackSHA, app.Intent.Prune)
		require.NoError(t, err)
		require.Equal(t, 1, legacy.FormatVersion)
		p.Applications[i].Intent = legacy
	}
	p.Intent = p.Applications[0].Intent
	body, rollbackID, err := p.canonical(parent)
	require.NoError(t, err)
	statement := `UPDATE installation_revision_rollbacks SET plan=$2::jsonb,rollback_id=$3 WHERE parent_plan_id=$1`
	if unstart {
		statement = `UPDATE installation_revision_rollbacks SET plan=$2::jsonb,rollback_id=$3,started_at=NULL,application_observations=NULL WHERE parent_plan_id=$1`
	}
	_, err = h.db.Exec(statement, h.record.ID, string(body), rollbackID)
	require.NoError(t, err)
}

func (h *lifecycleHarness) engine() *historyController  { return h.controllers[h.enginePath] }
func (h *lifecycleHarness) product() *historyController { return h.controllers[h.productPath] }

// writes is every controller write the plan's Applications have accepted.
func (h *lifecycleHarness) writes() int {
	total := 0
	for _, c := range h.controllers {
		c.mu.Lock()
		total += c.writes
		c.mu.Unlock()
	}
	return total
}

// apply starts the selected plan and has every member's own sync succeed.
func (h *lifecycleHarness) apply() {
	h.t.Helper()
	result, err := h.public().StartSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(h.t, err)
	require.Equal(h.t, "applying", result.State)
	require.Equal(h.t, len(h.controllers), h.writes())
	for _, c := range h.controllers {
		c.succeed(h.candidateSHA)
	}
	h.setServing(h.afterCommit, false)
}

// running makes one release's workloads the running ones.
func (h *lifecycleHarness) running(digest string) {
	all := h.manifests(digest, false)
	if h.productPath != "" {
		all = append(all, h.manifests(digest, true)...)
	}
	groupRuntimeWorkloads(h.t, h.receiver, all)
}

// runCandidate makes the candidate's workloads the running ones.
func (h *lifecycleHarness) runCandidate() { h.running(h.afterDigest) }

// complete applies the plan and records its verified completion.
func (h *lifecycleHarness) complete() {
	h.t.Helper()
	h.apply()
	h.runCandidate()
	result, err := h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(h.t, err)
	require.Equal(h.t, "complete", result.State)
	require.True(h.t, result.CanRollback)
}

// restored has both reversals succeed and the previous release run.
func (h *lifecycleHarness) restored() {
	for _, c := range h.controllers {
		c.succeed(h.rollbackSHA)
	}
	h.runPrevious()
}

// runPrevious makes the previous release's engine and workloads the running ones.
func (h *lifecycleHarness) runPrevious() {
	h.setServing(h.beforeCommit, false)
	h.running(h.beforeDigest)
}

type attemptRow struct {
	state       string
	active      sql.NullString
	claims      int
	rollbacks   int
	updatedAt   time.Time
	withdrawals []withdrawnRestoration
	owned       map[string]ownedSuccessRecord
}

func (h *lifecycleHarness) row() attemptRow {
	h.t.Helper()
	var out attemptRow
	var withdrawn, owned []byte
	require.NoError(h.t, h.db.QueryRow(`SELECT state,updated_at,withdrawn_restorations,owned_successes FROM installation_revision_attempts WHERE plan_id=$1`, h.record.ID).Scan(&out.state, &out.updatedAt, &withdrawn, &owned))
	require.NoError(h.t, h.db.QueryRow(`SELECT active_plan_id FROM installation_revision_heads WHERE installation_id=$1`, h.request.InstallationID).Scan(&out.active))
	require.NoError(h.t, h.db.QueryRow(`SELECT count(*) FROM installation_application_claims WHERE installation_id=$1`, h.request.InstallationID).Scan(&out.claims))
	require.NoError(h.t, h.db.QueryRow(`SELECT count(*) FROM installation_revision_rollbacks WHERE parent_plan_id=$1`, h.record.ID).Scan(&out.rollbacks))
	if len(withdrawn) != 0 {
		require.NoError(h.t, json.Unmarshal(withdrawn, &out.withdrawals))
	}
	if len(owned) != 0 {
		require.NoError(h.t, json.Unmarshal(owned, &out.owned))
	}
	return out
}

// requireReleased asserts the completed update holds nothing, exactly as it
// stood before Restore previous release was pressed, and that a new
// preparation is admitted.
func (h *lifecycleHarness) requireReleased(row attemptRow) {
	h.t.Helper()
	require.Equal(h.t, "complete", row.state)
	require.False(h.t, row.active.Valid, "the installation head is free")
	require.Zero(h.t, row.claims, "no Application stays claimed")
	require.Zero(h.t, row.rollbacks, "a completed attempt carries no reversal")
	status, err := h.public().SelectionStatus(h.owner())
	require.NoError(h.t, err)
	require.Nil(h.t, status.Active)
	require.NotNil(h.t, status.Latest)
	require.Equal(h.t, "complete", status.Latest.State)
	require.True(h.t, status.Latest.CanRollback, "it can be restored again")
}

// requirePreparationAdmitted reserves a successor preparation, which only a
// free installation head admits.
func (h *lifecycleHarness) requirePreparationAdmitted() {
	h.t.Helper()
	prior, err := h.external.journal.get(h.ctx, h.request.InstallationID, h.record.Plan.Preparation.ID)
	require.NoError(h.t, err)
	successor := prior.Scope
	successor.RequestID = "next-selection"
	for k, capture := range successor.Captures {
		capture.RunID = preparationSourceRun(successor.InstallationID, successor.RequestID, successor.RequestedBy)
		capture.WorkRunID = capture.RunID
		successor.Captures[k] = capture
	}
	_, err = h.external.journal.reserve(h.ctx, successor)
	require.NoError(h.t, err, "a new preparation is admitted")
}

// Fix 1: Restore previous release used to take the installation head back and
// mark the plan reverting before any check ran, so a check that refused left an
// installation nothing could release. Every check now runs first. A refusal at
// the first of them (the receiving configuration and its invariant, or an
// Application that moved on) or at the last (the migration continuity proof
// the reservation makes) leaves the attempt exactly as it was: complete, its
// head free, nothing claimed, no reversal, nothing withdrawn and the row
// unchanged.
func TestInstallationRestorationRefusedByAnyCheckLeavesTheUpdateComplete(t *testing.T) {
	for _, fault := range []string{"migrations changed", "configuration changed", "Application moved to another revision"} {
		t.Run(fault, func(t *testing.T) {
			h := newLifecycleHarness(t)
			h.complete()
			before := h.row()
			writes := h.writes()
			var want string
			switch fault {
			case "migrations changed":
				h.setServing(h.afterCommit, true)
				want = "unchanged migrations"
			case "configuration changed":
				h.receiver.config.Protected = append(h.receiver.config.Protected, receiverProtected{Namespace: "memql", Name: "registry-ca", Kind: "Secret"})
				h.receiver.saveConfig()
				resourceMap(h.receiver.objects["api/v1/namespaces/memql/configmaps/receiver"], "metadata")["resourceVersion"] = "12"
				want = "configuration"
			case "Application moved to another revision":
				h.engine().change(true, func(o map[string]any) {
					resourceMap(resourceMap(o, "spec"), "source")["targetRevision"] = strings.Repeat("9", 40)
				})
				want = "Application"
			}
			_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
			require.ErrorContains(t, err, want)
			after := h.row()
			h.requireReleased(after)
			require.Empty(t, after.withdrawals, "nothing was taken back, so nothing was withdrawn")
			require.Equal(t, before.updatedAt, after.updatedAt, "the refused request never wrote the attempt")
			require.Equal(t, writes, h.writes(), "no reverse write")
			h.requirePreparationAdmitted()
		})
	}
}

// Fix 1: a check that refuses after the reversal is reserved and the head
// taken back, but before any reverse write has a durable start, returns the
// attempt to complete in the same request. Here the migration proof each
// member's apply makes fails at the first member. The record keeps the
// automatic withdrawal and the reserved reversal's identity; a later
// restoration then goes through.
func TestInstallationRestorationRefusedBeforeItsFirstWriteIsWithdrawn(t *testing.T) {
	h := newLifecycleHarness(t)
	h.complete()
	writes := h.writes()
	// The reservation makes the first probe and the first member's apply the
	// second; only the second sees changed migrations.
	h.atFirstApply(func() { h.setServing(h.afterCommit, true) })
	_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.ErrorContains(t, err, "unchanged migrations")
	require.ErrorContains(t, err, "changed no application")
	h.requireFaultAfterReservation()
	after := h.row()
	h.requireReleased(after)
	require.Len(t, after.withdrawals, 1)
	require.Equal(t, withdrawalRefused, after.withdrawals[0].Kind)
	require.Equal(t, "operator", after.withdrawals[0].By)
	require.NotEmpty(t, after.withdrawals[0].RollbackID, "the reserved reversal is named in the record")
	require.Equal(t, writes, h.writes())

	h.setHealthHook(nil)
	h.setServing(h.afterCommit, false)
	result, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "rollback", result.Direction)
	require.Equal(t, writes+2, h.writes(), "each member's reversal was written once")
	h.restored()
	result, err = h.replica().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "rolled_back", result.State)
}

// Fix 1: an attempt an earlier engine left reverting with no reversal written
// (it took the head first) can be closed by its operator. The close returns it
// to the verified completed state with who, when and why recorded; a lost
// reply recovers the same close. Restore stays available afterwards.
func TestInstallationRestorationStrandedBeforeAnyWriteCanBeClosed(t *testing.T) {
	h := newLifecycleHarness(t)
	h.complete()
	journal := &revisionJournal{db: func() *sql.DB { return h.db }}
	stranded, err := journal.reacquireRollback(h.ctx, h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "reverting", stranded.State)
	require.Nil(t, stranded.Rollback)
	// Cluster > Updates offers Close for it, to its operator only.
	status, err := h.public().SelectionStatus(h.owner())
	require.NoError(t, err)
	require.NotNil(t, status.Active)
	require.Equal(t, "rollback", status.Active.Direction)
	require.True(t, status.Active.CanClose, "a restoration that wrote nothing is offered Close")
	other, err := h.public().SelectionStatus(operator(auth.RoleOwner, "another-operator"))
	require.NoError(t, err)
	require.NotNil(t, other.Active)
	require.False(t, other.Active.CanClose, "only the plan's operator is offered Close")
	_, err = h.public().CloseSelected(operator(auth.RoleOwner, "another-operator"), h.request.InstallationID, h.record.ID, closeReasonFixture)
	require.Error(t, err, "only the plan's operator may end its restoration")
	_, err = h.public().CloseSelected(h.owner(), h.request.InstallationID, h.record.ID, "")
	require.Error(t, err, "a close needs a reason")
	require.Equal(t, "reverting", h.row().state)

	closed, err := h.public().CloseSelected(h.owner(), h.request.InstallationID, h.record.ID, closeReasonFixture)
	require.NoError(t, err)
	require.Equal(t, "complete", closed.State)
	require.True(t, closed.CanRollback)
	after := h.row()
	h.requireReleased(after)
	require.Len(t, after.withdrawals, 1)
	require.Equal(t, withdrawnRestoration{Kind: withdrawalClosed, By: "operator", At: after.withdrawals[0].At, Reason: closeReasonFixture}, after.withdrawals[0])
	again, err := h.replica().CloseSelected(h.owner(), h.request.InstallationID, h.record.ID, "A different reason after a lost reply")
	require.NoError(t, err)
	require.Equal(t, closed, again, "a lost reply recovers the stored close and its first reason")
	require.Len(t, h.row().withdrawals, 1)
	h.requirePreparationAdmitted()
}

// Fix 1: a restoration that has recorded any reverse start may have reached
// the cluster, so it is neither closed nor withdrawn, as before: it keeps the
// installation for its operator to resume.
func TestInstallationRestorationWithAWrittenReversalCannotBeClosed(t *testing.T) {
	h := newLifecycleHarness(t)
	h.complete()
	h.engine().loseReply = true
	_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.ErrorContains(t, err, "unconfirmed")
	row := h.row()
	require.Equal(t, "reverting", row.state, "an uncertain reverse write is never withdrawn")
	require.True(t, row.active.Valid)
	require.Empty(t, row.withdrawals)
	status, err := h.public().SelectionStatus(h.owner())
	require.NoError(t, err)
	require.NotNil(t, status.Active)
	require.False(t, status.Active.CanClose, "Close is not offered once a reverse start is recorded")
	_, err = h.public().CloseSelected(h.owner(), h.request.InstallationID, h.record.ID, closeReasonFixture)
	require.ErrorContains(t, err, "started a reverse sync")
	row = h.row()
	require.Equal(t, "reverting", row.state)
	require.True(t, row.active.Valid)
	require.Equal(t, 1, row.rollbacks)
}

// Fix 4: Argo keeps only the last operation. After the plan's own success was
// observed and recorded, a hand sync of the same revision replaced it in
// operationState; completion now still records the verified receipt. Before,
// it refused forever.
func TestInstallationCompletionSurvivesALaterSyncOfTheSameRevision(t *testing.T) {
	h := newLifecycleHarness(t)
	h.apply()
	// The first check sees both members' own operations succeed and records
	// them, then stops because the candidate's workloads are not running yet.
	_, err := h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.Error(t, err, "green Applications cannot replace runtime image evidence")
	row := h.row()
	require.Equal(t, "applying", row.state)
	require.Len(t, row.owned, 2, "each member's own success is recorded")
	for _, record := range row.owned {
		require.Equal(t, h.candidateSHA, record.Revision)
	}
	h.engine().laterSync(h.candidateSHA)
	h.product().laterSync(h.candidateSHA)
	h.runCandidate()
	result, err := h.replica().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "complete", result.State)
	require.True(t, result.CanRollback)
}

// Fix 4: the record never vouches for a different state. A later sync to
// another revision, or a plan whose own success was replaced before any check
// saw it, still does not complete.
func TestInstallationCompletionStillRefusesWithoutItsOwnRecordedSuccess(t *testing.T) {
	t.Run("later sync to another revision", func(t *testing.T) {
		h := newLifecycleHarness(t)
		h.apply()
		_, err := h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.Error(t, err)
		require.Len(t, h.row().owned, 2)
		h.engine().change(true, func(o map[string]any) {
			resourceMap(resourceMap(o, "spec"), "source")["targetRevision"] = strings.Repeat("9", 40)
		})
		h.runCandidate()
		_, err = h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.Error(t, err, "the engine Application no longer runs the plan's revision")
		require.Equal(t, "applying", h.row().state)
	})
	t.Run("own success never observed", func(t *testing.T) {
		h := newLifecycleHarness(t)
		h.apply()
		h.engine().laterSync(h.candidateSHA)
		h.runCandidate()
		result, err := h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.NoError(t, err)
		require.Equal(t, "applying", result.State, "no record, no completion; the operator can close it")
		row := h.row()
		require.Len(t, row.owned, 1, "only the product member's own success was seen")
		require.True(t, result.CanClose)
	})
}

// Fix 4 for restoration: a recorded success, or for an update completed
// before successes were recorded its forward completion receipt (completion
// required each member's own operation to succeed), lets Restore plan the
// exact reversal after a later hand sync of the same revision; a later sync to
// another revision still refuses, before anything is taken back.
func TestInstallationRestorationAfterALaterSyncOfTheSameRevision(t *testing.T) {
	for _, mode := range []string{"same revision", "same revision, completed by an earlier engine"} {
		t.Run(mode, func(t *testing.T) {
			h := newLifecycleHarness(t)
			h.complete()
			if mode != "same revision" {
				// An engine up to 0.31.28 completed updates without recording
				// owned successes; its completion receipt is the only record.
				_, err := h.db.Exec(`UPDATE installation_revision_attempts SET owned_successes=NULL WHERE plan_id=$1`, h.record.ID)
				require.NoError(t, err)
			}
			h.engine().laterSync(h.candidateSHA)
			h.product().laterSync(h.candidateSHA)
			writes := h.writes()
			result, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
			require.NoError(t, err)
			require.Equal(t, "rollback", result.Direction)
			require.Equal(t, writes+2, h.writes())
			h.restored()
			result, err = h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
			require.NoError(t, err)
			require.Equal(t, "rolled_back", result.State)
		})
	}
	t.Run("another revision", func(t *testing.T) {
		h := newLifecycleHarness(t)
		h.complete()
		h.engine().laterSync(strings.Repeat("9", 40))
		writes := h.writes()
		_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.Error(t, err)
		h.requireReleased(h.row())
		require.Equal(t, writes, h.writes())
	})
}

// Fix 4 for a restoration's own completion: the reversal's observed success is
// recorded too, so a hand sync of the restored revision before the restored
// workloads are verified does not strand it.
func TestInstallationRestorationCompletesAfterALaterSyncOfTheRestoredRevision(t *testing.T) {
	h := newLifecycleHarness(t)
	h.complete()
	_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	for _, c := range h.controllers {
		c.succeed(h.rollbackSHA)
	}
	h.setServing(h.beforeCommit, false)
	_, err = h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.Error(t, err, "the previous release's workloads are not running yet")
	row := h.row()
	require.Equal(t, "reverting", row.state)
	require.Len(t, row.owned, 4, "both members' forward and reverse successes are recorded")
	h.engine().laterSync(h.rollbackSHA)
	h.product().laterSync(h.rollbackSHA)
	h.runPrevious()
	result, err := h.replica().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "rolled_back", result.State)
}

// Fix 5: Argo's status writes advance the generation of an Application CRD
// without a status subresource. One such write between a reversal's planning
// and its patch used to refuse that patch on every retry. A history-backed
// reversal accepts it; another writer's spec change still refuses, before the
// member's reverse start, so the restoration is withdrawn.
func TestInstallationRestorationSurvivesAStatusWriteBeforeItsPatch(t *testing.T) {
	t.Run("status write", func(t *testing.T) {
		h := newLifecycleHarness(t)
		h.complete()
		writes := h.writes()
		h.atFirstApply(func() { // the engine member's apply, after planning
			h.engine().statusWrite()
			h.product().statusWrite()
		})
		result, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.NoError(t, err)
		require.Equal(t, "rollback", result.Direction)
		h.requireFaultAfterReservation()
		require.Equal(t, writes+2, h.writes())
		h.setHealthHook(nil)
		h.restored()
		result, err = h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.NoError(t, err)
		require.Equal(t, "rolled_back", result.State)
	})
	t.Run("spec edited by someone else", func(t *testing.T) {
		h := newLifecycleHarness(t)
		h.complete()
		writes := h.writes()
		h.atFirstApply(func() { h.engine().specEdit() })
		_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.Error(t, err)
		h.requireFaultAfterReservation()
		row := h.row()
		h.requireReleased(row)
		require.Len(t, row.withdrawals, 1, "refused before the member's reverse start, so withdrawn")
		require.Equal(t, writes, h.writes())
	})
}

// S1: a single-Application reversal has one member and no group starts. Its
// start is now also taken at its apply, after every check that apply makes
// (perApplyStarts). Its only start used to be the reversal's own, which the
// Begin step commits ahead of those checks, so a refusal there left a
// restoration that had written nothing but could be neither withdrawn nor
// closed. Here the apply's migration proof refuses; the restoration is
// withdrawn as a group's is, and a later one goes through.
func TestInstallationSingleApplicationRestorationRefusedAtItsApplyIsWithdrawn(t *testing.T) {
	h := newSingleLifecycleHarness(t)
	h.complete()
	writes := h.writes()
	h.atFirstApply(func() { h.setServing(h.afterCommit, true) })
	_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.ErrorContains(t, err, "unchanged migrations")
	require.ErrorContains(t, err, "changed no application")
	h.requireFaultAfterReservation()
	after := h.row()
	h.requireReleased(after)
	require.Len(t, after.withdrawals, 1)
	require.Equal(t, withdrawalRefused, after.withdrawals[0].Kind)
	require.Equal(t, writes, h.writes())

	h.setHealthHook(nil)
	h.setServing(h.afterCommit, false)
	result, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "rollback", result.Direction)
	require.False(t, result.CanClose, "its reverse start is recorded, so Close is not offered")
	require.Equal(t, writes+1, h.writes())
	h.restored()
	result, err = h.replica().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "rolled_back", result.State)
}

// S1 and S2 for a restoration an engine up to 0.31.28 left: its reversal
// intents are format 1, whose patch tests the exact generation they were
// planned at, so one status write by Argo refused that patch on every retry.
// Here the engine member's patch meets such a write and nothing lands. A
// resume now plans again only what provably never reached the cluster: a
// reservation that never began is replaced at reservation; a member that
// began, but whose Application still holds the reversal's exact baseline at a
// later generation (no request formed for it can ever pass its fence, and
// none landed), is planned again at its apply. A member whose fence still
// holds keeps its intent.
func TestInstallationRestorationResumesALegacyReversal(t *testing.T) {
	for _, mode := range []string{"reserved, never begun", "member begun, its fence passed by a status write"} {
		t.Run(mode, func(t *testing.T) {
			h := newLifecycleHarness(t)
			h.complete()
			planned := h.readApplications()
			writes := h.writes()
			h.engine().mu.Lock()
			h.engine().beforeWrite = h.engine().statusWrite
			h.engine().mu.Unlock()
			_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
			require.ErrorContains(t, err, "unconfirmed")
			require.Equal(t, writes, h.writes(), "the engine member's patch was refused, so nothing landed")
			require.Equal(t, "reverting", h.row().state, "a recorded member start is never withdrawn")

			unbegun := mode == "reserved, never begun"
			h.asLegacyReversal(planned, unbegun)
			legacy := h.reversal()
			result, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
			require.NoError(t, err)
			require.Equal(t, "rollback", result.Direction)
			require.Equal(t, writes+2, h.writes(), "each member's reversal was written once")
			resumed := h.reversal()
			if unbegun {
				require.NotEqual(t, legacy.ID, resumed.ID, "the unbegun reservation was replaced")
				for _, app := range resumed.Plan.Applications {
					require.Equal(t, 2, app.Intent.FormatVersion)
				}
				require.Empty(t, resumed.Replanned)
			} else {
				require.Equal(t, legacy.ID, resumed.ID, "a begun reversal is kept")
				require.Len(t, resumed.Replanned, 1, "only the superseded member is planned again")
				require.Equal(t, 2, resumed.Replanned["engine"].FormatVersion)
			}
			h.restored()
			result, err = h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
			require.NoError(t, err)
			require.Equal(t, "rolled_back", result.State)
		})
	}
}

// S2: a group restoration resumed after Kubernetes compacted the watch history
// its unwritten member's reversal was planned against. With the other member
// written the restoration can be neither withdrawn nor closed, and the saved
// reversal can never be proved again; since nothing of it was sent, it is
// planned again from the Application as it is now.
func TestInstallationRestorationResumedAfterItsHistoryIsCompacted(t *testing.T) {
	h := newLifecycleHarness(t)
	h.complete()
	writes := h.writes()
	h.engine().loseReply = true
	_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.ErrorContains(t, err, "unconfirmed")
	require.Equal(t, writes+1, h.writes(), "the engine member's reversal landed; the product member's was never sent")
	var saved argocd.Intent
	for _, app := range h.reversal().Plan.Applications {
		if app.Key == "product" {
			saved = app.Intent
		}
	}
	h.product().statusWrite()
	h.product().mu.Lock()
	h.product().watchError = true
	h.product().mu.Unlock()

	result, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "rollback", result.Direction)
	require.Equal(t, writes+2, h.writes())
	replanned := h.reversal().Replanned
	require.Len(t, replanned, 1)
	require.NotEqual(t, saved.WatchResourceVersion, replanned["product"].WatchResourceVersion, "planned again against a current boundary")

	// The replanned record is a native journal fact: an entry for another
	// member, in a member's place that it does not reverse, or with a field an
	// intent does not have makes the attempt unreadable, and the table refuses
	// the record in a shape it cannot hold or on a reversal that never began.
	journal := &revisionJournal{db: func() *sql.DB { return h.db }}
	var stored []byte
	require.NoError(t, h.db.QueryRow(`SELECT replanned_intents FROM installation_revision_rollbacks WHERE parent_plan_id=$1`, h.record.ID).Scan(&stored))
	for name, edit := range map[string]string{
		"another member":       `jsonb_build_object('elsewhere', replanned_intents->'product')`,
		"the engine's place":   `jsonb_build_object('engine', replanned_intents->'product')`,
		"a field of no intent": `jsonb_set(replanned_intents,'{product,approved}','true')`,
		"another revision":     `jsonb_set(replanned_intents,'{product,revision}','"` + strings.Repeat("9", 40) + `"')`,
	} {
		_, err = h.db.Exec(`UPDATE installation_revision_rollbacks SET replanned_intents=`+edit+` WHERE parent_plan_id=$1`, h.record.ID)
		require.NoError(t, err, name)
		_, err = journal.get(h.ctx, h.request.InstallationID, h.record.ID)
		require.Error(t, err, "a replanned reversal naming %s is refused", name)
		_, err = h.db.Exec(`UPDATE installation_revision_rollbacks SET replanned_intents=$2::jsonb WHERE parent_plan_id=$1`, h.record.ID, string(stored))
		require.NoError(t, err)
	}
	for _, edit := range []string{`replanned_intents='[]'::jsonb`, `started_at=NULL`} {
		_, err = h.db.Exec(`UPDATE installation_revision_rollbacks SET `+edit+` WHERE parent_plan_id=$1`, h.record.ID)
		require.Error(t, err, "the table refuses %s", edit)
	}
	_, err = journal.get(h.ctx, h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	h.restored()
	result, err = h.public().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	require.Equal(t, "rolled_back", result.State)
}

// S3: a restoration's own reverse syncs succeeded, and before any check saw
// them someone synced both Applications once more, replacing them in Argo's
// operationState. Every member has started, so the restoration cannot be
// closed; it now completes from Argo's deployment history, whose newest entry
// is the restored revision and source, deployed after the reverse start. A
// newest deployment from before that start, or of another revision, still
// refuses, and a read that sees a reversal's own success keeps it even when
// the request then refuses.
func TestInstallationRestorationProvedByItsDeploymentHistory(t *testing.T) {
	restore := func(t *testing.T) *lifecycleHarness {
		h := newLifecycleHarness(t)
		h.complete()
		_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.NoError(t, err)
		return h
	}
	for _, mode := range []string{"deployed after the reverse start", "deployed before the reverse start", "another revision deployed since"} {
		t.Run(mode, func(t *testing.T) {
			h := restore(t)
			for _, c := range h.controllers {
				c.succeed(h.rollbackSHA)
				switch mode {
				case "deployed before the reverse start":
					c.deployHistory(h.rollbackSHA, time.Hour)
				case "another revision deployed since":
					c.deployHistory(h.rollbackSHA, 0)
					c.deployHistory(strings.Repeat("9", 40), 0)
				default:
					c.deployHistory(h.rollbackSHA, 0)
				}
				c.laterSync(h.rollbackSHA)
			}
			h.runPrevious()
			require.Len(t, h.row().owned, 2, "no check saw a reversal's own success")
			result, err := h.replica().ObserveSelected(h.owner(), h.request.InstallationID, h.record.ID)
			if mode == "deployed after the reverse start" {
				require.NoError(t, err)
				require.Equal(t, "rolled_back", result.State)
				require.Len(t, h.row().owned, 2, "a deployment is proof that is read again, never recorded")
				return
			}
			require.Error(t, err)
			require.Equal(t, "reverting", h.row().state)
		})
	}
	t.Run("a refused resume still records the success it read", func(t *testing.T) {
		h := restore(t)
		for _, c := range h.controllers {
			c.succeed(h.rollbackSHA)
		}
		h.receiver.config.Protected = append(h.receiver.config.Protected, receiverProtected{Namespace: "memql", Name: "registry-ca", Kind: "Secret"})
		h.receiver.saveConfig()
		resourceMap(h.receiver.objects["api/v1/namespaces/memql/configmaps/receiver"], "metadata")["resourceVersion"] = "12"
		_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
		require.ErrorContains(t, err, "configuration")
		require.Len(t, h.row().owned, 4, "both members' forward and reverse successes are recorded")
	})
}

// Both new records are native journal facts. A stored entry that names an
// intent this attempt never started, states another revision or source, or a
// withdrawal by anyone but the requester makes the attempt unreadable, the
// table refuses either record in a shape it cannot hold, and the migration
// cannot be rolled back while either holds history.
func TestInstallationRestoreLifecycleRecordsAreValidatedAndRetained(t *testing.T) {
	h := newLifecycleHarness(t)
	h.complete()
	h.atFirstApply(func() { h.setServing(h.afterCommit, true) })
	_, err := h.public().RollbackSelected(h.owner(), h.request.InstallationID, h.record.ID)
	require.ErrorContains(t, err, "changed no application")
	h.requireFaultAfterReservation()
	h.setHealthHook(nil)
	row := h.row()
	require.Len(t, row.owned, 2)
	require.Len(t, row.withdrawals, 1)
	journal := &revisionJournal{db: func() *sql.DB { return h.db }}
	_, err = journal.get(h.ctx, h.request.InstallationID, h.record.ID)
	require.NoError(t, err)

	var owned, withdrawn []byte
	require.NoError(t, h.db.QueryRow(`SELECT owned_successes,withdrawn_restorations FROM installation_revision_attempts WHERE plan_id=$1`, h.record.ID).Scan(&owned, &withdrawn))
	digest := ""
	for key := range row.owned {
		digest = key
	}
	for name, edit := range map[string]string{
		"another revision":    `jsonb_set(owned_successes,'{` + digest + `,revision}','"` + strings.Repeat("9", 40) + `"')`,
		"another source":      `jsonb_set(owned_successes,'{` + digest + `,source,path}','"elsewhere"')`,
		"another member":      `jsonb_set(owned_successes,'{` + digest + `,application}','"elsewhere"')`,
		"an unstarted intent": `owned_successes || jsonb_build_object('memql-id:` + strings.Repeat("0", 64) + `', owned_successes->'` + digest + `')`,
		"an unknown field":    `jsonb_set(owned_successes,'{` + digest + `,approved}','true')`,
	} {
		_, err = h.db.Exec(`UPDATE installation_revision_attempts SET owned_successes=`+edit+` WHERE plan_id=$1`, h.record.ID)
		require.NoError(t, err, name)
		_, err = journal.get(h.ctx, h.request.InstallationID, h.record.ID)
		require.Error(t, err, "an owned success naming %s is refused", name)
		_, err = h.db.Exec(`UPDATE installation_revision_attempts SET owned_successes=$2::jsonb WHERE plan_id=$1`, h.record.ID, string(owned))
		require.NoError(t, err)
	}
	for name, edit := range map[string]string{
		"another operator":         `jsonb_set(withdrawn_restorations,'{0,by}','"another-operator"')`,
		"an unknown kind":          `jsonb_set(withdrawn_restorations,'{0,kind}','"forgotten"')`,
		"a reason on an automatic": `jsonb_set(withdrawn_restorations,'{0,reason}','"not the operator words"')`,
	} {
		_, err = h.db.Exec(`UPDATE installation_revision_attempts SET withdrawn_restorations=`+edit+` WHERE plan_id=$1`, h.record.ID)
		require.NoError(t, err, name)
		_, err = journal.get(h.ctx, h.request.InstallationID, h.record.ID)
		require.Error(t, err, "a withdrawal with %s is refused", name)
		_, err = h.db.Exec(`UPDATE installation_revision_attempts SET withdrawn_restorations=$2::jsonb WHERE plan_id=$1`, h.record.ID, string(withdrawn))
		require.NoError(t, err)
	}
	_, err = journal.get(h.ctx, h.request.InstallationID, h.record.ID)
	require.NoError(t, err)
	for _, edit := range []string{`owned_successes='[]'::jsonb`, `withdrawn_restorations='{}'::jsonb`} {
		_, err = h.db.Exec(`UPDATE installation_revision_attempts SET `+edit+` WHERE plan_id=$1`, h.record.ID)
		require.Error(t, err, "the table refuses %s", edit)
	}
	down, err := os.ReadFile(restoreLifecycleMigrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = h.db.Exec(string(down))
	require.ErrorContains(t, err, "cannot be discarded")
}
