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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/integrations/argocd"
)

type groupProtocolAPI struct {
	receiver    *receiverFixture
	controllers map[string]*revisionControllerFixture
	members     map[string]string
	interrupt   map[string]bool
	delayed     map[string][]byte
	race        map[string]bool
}

func (a groupProtocolAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if c := a.controllers[path]; c != nil && (method != http.MethodGet || c.hasWritten()) {
		if method == http.MethodPatch {
			r, err := (&revisionJournal{db: func() *sql.DB { return c.db }}).get(ctx, a.receiver.config.InstallationID, c.key)
			if err != nil {
				return nil, err
			}
			started := r.Applications[a.members[path]].Started
			if r.Rollback != nil {
				started = r.Rollback.Applications[a.members[path]].Started
			}
			if !started {
				return nil, errors.New("group controller effect preceded member start")
			}
			if a.interrupt[path] && r.Rollback == nil {
				a.interrupt[path] = false
				a.delayed[path] = append([]byte(nil), body...)
				return nil, errors.New("interrupted after durable member start")
			}
			if a.race[path] && r.Rollback != nil && len(a.delayed[path]) > 0 {
				a.race[path] = false
				if _, err = c.Do(ctx, method, path, contentType, a.delayed[path]); err != nil {
					return nil, err
				}
				c.succeed(r.Plan.Intent.Revision)
			}
		}
		return c.Do(ctx, method, path, contentType, body)
	}
	return a.receiver.Do(ctx, method, path, contentType, body)
}
func groupRuntimeWorkloads(t *testing.T, receiver *receiverFixture, manifests []string) {
	t.Helper()
	pods, sets := []any{}, []any{}
	for _, manifest := range manifests {
		if resourceText(workloadObject(t, manifest), "kind") != "Deployment" {
			continue
		}
		name := resourceText(resourceMap(workloadObject(t, manifest), "metadata"), "name")
		// Reuse the qualified Deployment protocol fixture, then substitute every
		// controller/owner/label name consistently before combining complete lists.
		canonical := strings.ReplaceAll(manifest, `"`+name+`"`, `"bff"`)
		protocolCompletionWorkload(t, receiver, canonical)
		rewrite := func(value map[string]any) map[string]any {
			b, _ := json.Marshal(value)
			b = []byte(strings.ReplaceAll(string(b), "bff", name))
			var out map[string]any
			require.NoError(t, json.Unmarshal(b, &out))
			return out
		}
		receiver.objects["apis/apps/v1/namespaces/memql/deployments/"+name] = rewrite(receiver.objects["apis/apps/v1/namespaces/memql/deployments/bff"])
		for _, v := range receiver.objects["api/v1/namespaces/memql/pods?limit=256"]["items"].([]any) {
			pods = append(pods, rewrite(v.(map[string]any)))
		}
		for _, v := range receiver.objects["apis/apps/v1/namespaces/memql/replicasets?limit=256"]["items"].([]any) {
			sets = append(sets, rewrite(v.(map[string]any)))
		}
	}
	receiver.objects["api/v1/namespaces/memql/pods?limit=256"] = workloadListFixture("v1", "Pod", pods)
	receiver.objects["apis/apps/v1/namespaces/memql/replicasets?limit=256"] = workloadListFixture("apps/v1", "ReplicaSet", sets)
}

func TestInstallationGroupActualWorkflowRecoversEngineReplacementAndRollback(t *testing.T) {
	for _, mode := range []string{"both applied", "start before effect", "delayed forward wins fence", "completed", "completed reservation race", "completed successor cancelled", "completed competing preparation", "completed target claimed elsewhere", "empty reversal before begin"} {
		t.Run(mode, func(t *testing.T) { testGroupProtocolRecovery(t, mode) })
	}
}
func testGroupProtocolRecovery(t *testing.T, mode string) {
	db, peerDB := journalDB(t)
	receiver := newReceiverFixture(t)
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
	receiver.config.FormatVersion = 2
	receiver.config.Application = receiverNamed{}
	receiver.config.Applications = []receiverApplication{{Key: "engine", Namespace: "argocd", Name: "memql"}, {Key: "product", Namespace: "argocd", Name: "product"}}
	receiver.config.ImageBindings[0].Application = "engine"
	receiver.config.ImageBindings[0].Container = "engine"
	productBinding := receiver.config.ImageBindings[0]
	productBinding.Application = "product"
	productBinding.Name = "product"
	receiver.config.ImageBindings = append(receiver.config.ImageBindings, productBinding)
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
	enginePath := "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"
	app := receiver.objects[enginePath]
	resourceMap(app, "spec")["source"] = rollbackSource
	resourceMap(resourceMap(app, "status"), "sync")["revision"] = rollbackSource["targetRevision"]
	resourceMap(receiver.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects/default"], "spec")["sourceRepos"] = []any{rollbackSource["repoURL"]}
	raw, _ := json.Marshal(app)
	var productApp map[string]any
	require.NoError(t, json.Unmarshal(raw, &productApp))
	productApp["metadata"] = fixtureMeta("product", "argocd")
	resourceMap(resourceMap(productApp, "spec"), "source")["path"] = "deploy/product"
	productPath := "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/product"
	receiver.objects[productPath] = productApp
	publications := preparationCatalogFixture{images.old.Digest(): images.old, images.next.Digest(): images.next}
	factory := func(context.Context, []byte, string) (Catalog, error) { return publications, nil }
	ctx := captureOperator(auth.RoleOwner, "operator")
	snapshot, err := readReceiver(ctx, receiver, "memql", "receiver", factory)
	require.NoError(t, err)
	require.Equal(t, "memql", snapshot.bindings[0].Application, "operator key resolves to the verified renderer Application name")
	require.Equal(t, "product", snapshot.bindings[1].Application)
	archives := map[string][]byte{}
	for _, member := range snapshot.applicationMembers() {
		for _, role := range []string{"rollback", "candidate"} {
			_, archive := captureRevisionWithRenderFixture(t, role, &member.render)
			archives[applicationCaptureKey(member.key, role)] = archive
		}
	}
	rollbackSHA, candidateSHA := rollbackSource["targetRevision"].(string), candidateSource["targetRevision"].(string)
	manifests := func(digest string, product bool) []string {
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
	rpc := &preparationRendererFixture{applicationManifests: map[string]map[string][]string{"memql": {rollbackSHA: manifests(before.Components[0].Artifacts[0].ImageDigest, false), candidateSHA: manifests(after.Components[0].Artifacts[0].ImageDigest, false)}, "product": {rollbackSHA: manifests(before.Components[0].Artifacts[0].ImageDigest, true), candidateSHA: manifests(after.Components[0].Artifacts[0].ImageDigest, true)}}, reads: map[string]int{}}
	address := preparationRendererServer(t, receiver.certificate, rpc)
	request := preparationRequest{InstallationID: receiver.config.InstallationID, RequestID: "group-recovery", CandidateID: after.CandidateID, CatalogDigest: images.next.Digest(), OverlayRevision: candidateSHA}
	external := &preparationProtocolFiles{t: t, journal: preparationConnection(db), request: request, archives: archives, files: map[string]*captureFilesFixture{}}
	host := &preparationHost{api: receiver, journal: external.journal, executor: external, files: external, tokens: preparationTokenFixture{}, catalog: factory, namespace: "memql", configurationName: "receiver", rendererAddressOverride: address}
	serving, migrated := before.Components[0].Commit, false
	host.health = func(_ context.Context, _, _, _, nonce string) (HealthObservation, error) {
		migration := "sha256:" + strings.Repeat("c", 64)
		if migrated {
			migration = "sha256:" + strings.Repeat("d", 64)
		}
		return HealthObservation{UserID: "operator", IdentityID: "identity", EngineCommit: serving, MigrationsDigest: migration, Nonce: nonce}, nil
	}
	require.NoError(t, host.bindWorkflow(strings.Repeat("a", 40)))
	record, err := host.prepare(ctx, request)
	require.NoError(t, err)
	require.Len(t, record.Plan.Applications, 2)
	require.Len(t, external.requests, 4)
	require.NotNil(t, record.Plan.RevisionWorkflow)
	controllers := map[string]*revisionControllerFixture{}
	for _, member := range record.Plan.Applications {
		one := record
		one.Plan.Intent = member.Intent
		c := newRevisionControllerFixture(t, db, one)
		b, _ := json.Marshal(receiver.objects[c.path])
		require.NoError(t, json.Unmarshal(b, &c.object))
		controllers[c.path] = c
	}
	bridge := groupProtocolAPI{receiver: receiver, controllers: controllers, members: map[string]string{enginePath: "engine", productPath: "product"}, interrupt: map[string]bool{}, delayed: map[string][]byte{}, race: map[string]bool{}}
	host.api = bridge
	if mode == "empty reversal before begin" {
		journal := &revisionJournal{db: func() *sql.DB { return db }}
		admission := revisionWriteAdmission{planID: record.ID, workflow: record.Plan.ExecutionWorkflowDigest, freshUntil: time.Now().Add(time.Minute)}
		_, err = journal.beginAdmitted(ctx, request.InstallationID, record.ID, record.Plan.ExecutionWorkflowDigest, admission)
		require.NoError(t, err)
		client, err := argocd.New(bridge)
		require.NoError(t, err)
		revision := &revisionWorkflowScope{workflow: &revisionWorkflow{digest: record.Plan.ExecutionWorkflowDigest, artifacts: &artifactWorkflow{digest: record.Plan.Preparation.ArtifactWorkflowDigest}}, host: host, journal: journal, client: client, installation: request.InstallationID, key: record.ID, operator: "operator", mode: revisionStart, rollbackExecution: true, renders: map[string]argocd.RenderedRevision{}, sources: map[string]verifiedSourceCapture{}}
		reversal := &rollbackWorkflowScope{revision: revision, snapshot: record.Plan.RollbackWorkflow}
		operations := reversal.operations()
		operations["installationRollbackBegin"] = func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("interrupted before rollback start")
		}
		_, err = workflowhost.RunSnapshot(ctx, reversal.snapshot, rollbackContract, nil, workflowhost.Options{Operations: operations})
		require.ErrorContains(t, err, "interrupted before rollback start")
		reserved, err := journal.get(ctx, request.InstallationID, record.ID)
		require.NoError(t, err)
		require.NotNil(t, reserved.Rollback)
		require.False(t, reserved.Rollback.Started)
		require.Empty(t, reserved.Rollback.Plan.Applications)
		fresh := *host
		fresh.journal = preparationConnection(peerDB)
		require.NoError(t, fresh.bindWorkflow(strings.Repeat("f", 40)))
		public := &Preparer{host: &fresh}
		_, err = public.RollbackSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
		require.NoError(t, err)
		all := append(manifests(before.Components[0].Artifacts[0].ImageDigest, false), manifests(before.Components[0].Artifacts[0].ImageDigest, true)...)
		groupRuntimeWorkloads(t, receiver, all)
		result, err := public.ObserveSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
		require.NoError(t, err)
		require.Equal(t, "rolled_back", result.State)
		for _, c := range controllers {
			require.Zero(t, c.writes, "an untouched member requires no controller effect")
		}
		var claims int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_application_claims`).Scan(&claims))
		require.Zero(t, claims)
		return
	}
	controllers[enginePath].loseReply = true
	public := &Preparer{host: host}
	_, err = public.StartSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.ErrorContains(t, err, "unconfirmed")
	require.Equal(t, 1, controllers[enginePath].writes)
	require.Zero(t, controllers[productPath].writes)
	fresh := *host
	fresh.journal = preparationConnection(peerDB)
	require.NoError(t, fresh.bindWorkflow(strings.Repeat("f", 40)))
	public = &Preparer{host: &fresh}
	_, err = public.StartSelected(operator(auth.RoleOwner, "another-owner"), request.InstallationID, record.ID)
	require.Error(t, err)
	require.Zero(t, controllers[productPath].writes)
	if mode != "both applied" && !strings.HasPrefix(mode, "completed") {
		bridge.interrupt[productPath] = true
	}
	result, err := public.StartSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	productBefore := 1
	if mode != "both applied" && !strings.HasPrefix(mode, "completed") {
		require.ErrorContains(t, err, "unconfirmed")
		productBefore = 0
	} else {
		require.NoError(t, err)
		require.Equal(t, "applying", result.State)
	}
	require.Equal(t, 1, controllers[enginePath].writes)
	require.Equal(t, productBefore, controllers[productPath].writes)
	for _, c := range controllers {
		if c.hasWritten() {
			c.succeed(candidateSHA)
		}
	}
	serving = after.Components[0].Commit
	_, err = public.ObserveSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.Error(t, err, "green Applications cannot replace runtime image evidence")
	var originalCompletion []byte
	var originalCompletedAt time.Time
	if strings.HasPrefix(mode, "completed") {
		all := append(manifests(after.Components[0].Artifacts[0].ImageDigest, false), manifests(after.Components[0].Artifacts[0].ImageDigest, true)...)
		groupRuntimeWorkloads(t, receiver, all)
		result, err = public.ObserveSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
		require.NoError(t, err)
		require.Equal(t, "complete", result.State)
		require.True(t, result.CanRollback)
		require.NoError(t, db.QueryRow(`SELECT completion_receipt,completed_at FROM installation_revision_attempts WHERE plan_id=$1`, record.ID).Scan(&originalCompletion, &originalCompletedAt))
		j := &revisionJournal{db: func() *sql.DB { return db }}
		peer := &revisionJournal{db: func() *sql.DB { return peerDB }}
		foreign, e := peer.get(captureOperator(auth.RoleOwner, "another-owner"), request.InstallationID, record.ID)
		require.NoError(t, e)
		require.False(t, foreign.RollbackEligible)
		_, e = peer.reacquireRollback(captureOperator(auth.RoleOwner, "another-owner"), request.InstallationID, record.ID)
		require.Error(t, e)
		if mode == "completed reservation race" {
			var wg sync.WaitGroup
			errors := make(chan error, 2)
			start := make(chan struct{})
			for _, journal := range []*revisionJournal{j, peer} {
				wg.Add(1)
				go func(journal *revisionJournal) {
					defer wg.Done()
					<-start
					_, e := journal.reacquireRollback(ctx, request.InstallationID, record.ID)
					errors <- e
				}(journal)
			}
			close(start)
			wg.Wait()
			close(errors)
			for e := range errors {
				require.NoError(t, e)
			}
			var claims int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_application_claims`).Scan(&claims))
			require.Equal(t, 2, claims)
			_, e = public.ObserveSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
			require.ErrorContains(t, e, "resume the selected rollback")
		}
		if mode == "completed successor cancelled" || mode == "completed competing preparation" || mode == "completed target claimed elsewhere" {
			prior, e := external.journal.get(ctx, request.InstallationID, record.Plan.Preparation.ID)
			require.NoError(t, e)
			successor := prior.Scope
			successor.RequestID = "next-selection"
			if mode == "completed target claimed elsewhere" {
				successor.InstallationID = "another-installation"
			}
			for k, capture := range successor.Captures {
				capture.RunID = preparationSourceRun(successor.InstallationID, successor.RequestID, successor.RequestedBy)
				capture.WorkRunID = capture.RunID
				successor.Captures[k] = capture
			}
			if mode == "completed target claimed elsewhere" {
				_, e = external.journal.reserve(ctx, successor)
				require.NoError(t, e)
				_, e = public.RollbackSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
				require.ErrorContains(t, e, "latest unchanged")
				old, e := j.get(ctx, request.InstallationID, record.ID)
				require.NoError(t, e)
				require.False(t, old.RollbackEligible)
				require.Equal(t, "complete", old.State)
				return
			}
			if mode == "completed successor cancelled" {
				reserved, e := external.journal.reserve(ctx, successor)
				require.NoError(t, e)
				_, e = external.journal.cancelBeforeCapture(ctx, successor.InstallationID, reserved.ID, successor.WorkflowDigest)
				require.NoError(t, e)
				_, e = public.RollbackSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
				require.ErrorContains(t, e, "latest unchanged")
				old, e := j.get(ctx, request.InstallationID, record.ID)
				require.NoError(t, e)
				require.False(t, old.RollbackEligible)
				require.Equal(t, "complete", old.State)
				return
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			var rollbackErr, preparationErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, rollbackErr = peer.reacquireRollback(ctx, request.InstallationID, record.ID)
			}()
			go func() { defer wg.Done(); <-start; _, preparationErr = external.journal.reserve(ctx, successor) }()
			close(start)
			wg.Wait()
			require.NotEqual(t, rollbackErr == nil, preparationErr == nil, "only one competing action may own the installation")
			if rollbackErr != nil {
				return
			}
		}
	}
	migrated = true
	_, err = public.RollbackSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.ErrorContains(t, err, "unchanged migrations")
	require.Equal(t, 1, controllers[enginePath].writes)
	require.Equal(t, productBefore, controllers[productPath].writes)
	migrated = false
	controllers[enginePath].loseReply = true
	_, err = public.RollbackSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.ErrorContains(t, err, "unconfirmed")
	require.Equal(t, 2, controllers[enginePath].writes)
	require.Equal(t, productBefore, controllers[productPath].writes)
	_, err = public.StartSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.Error(t, err, "recorded rollback permanently fences forward writes")
	next := fresh
	next.journal = preparationConnection(db)
	require.NoError(t, next.bindWorkflow(strings.Repeat("e", 40)))
	public = &Preparer{host: &next}
	if mode == "delayed forward wins fence" {
		bridge.race[productPath] = true
	}
	_, err = public.RollbackSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	if mode == "delayed forward wins fence" {
		require.ErrorContains(t, err, "unconfirmed")
		_, err = public.RollbackSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	}
	require.NoError(t, err)
	productFinal := productBefore + 1
	if mode == "delayed forward wins fence" {
		productFinal = 2
	}
	require.Equal(t, 2, controllers[enginePath].writes)
	require.Equal(t, productFinal, controllers[productPath].writes)
	for _, c := range controllers {
		c.succeed(rollbackSHA)
	}
	serving = before.Components[0].Commit
	all := append(manifests(before.Components[0].Artifacts[0].ImageDigest, false), manifests(before.Components[0].Artifacts[0].ImageDigest, true)...)
	groupRuntimeWorkloads(t, receiver, all)
	result, err = public.ObserveSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.NoError(t, err)
	require.Equal(t, "rolled_back", result.State)
	require.False(t, result.CanRollback)
	terminal, err := (&revisionJournal{db: func() *sql.DB { return peerDB }}).get(ctx, request.InstallationID, record.ID)
	require.NoError(t, err)
	require.NotNil(t, terminal.RollbackCompletion)
	require.Equal(t, "rollback", terminal.RollbackCompletion.Direction)
	if len(originalCompletion) > 0 {
		var retained []byte
		var retainedAt time.Time
		require.NoError(t, db.QueryRow(`SELECT completion_receipt,completed_at FROM installation_revision_attempts WHERE plan_id=$1`, record.ID).Scan(&retained, &retainedAt))
		require.JSONEq(t, string(originalCompletion), string(retained))
		require.Equal(t, originalCompletedAt, retainedAt)
	} else {
		require.Nil(t, terminal.Completion)
	}
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_application_claims`).Scan(&count))
	require.Zero(t, count)
	again, err := public.ObserveSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.NoError(t, err)
	require.Equal(t, result, again)
	require.Equal(t, 2, controllers[enginePath].writes)
	require.Equal(t, productFinal, controllers[productPath].writes)
}
