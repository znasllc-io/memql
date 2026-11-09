package installation

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// receiverChurn is routine cluster activity between two calls of one started
// update. None of it changes what the receiver relies on.
type receiverChurn struct {
	t         *testing.T
	receiver  *receiverFixture
	authority rendererAuthority
	host      *preparationHost
	rpc       *preparationRendererFixture
	round     int
}

func (c *receiverChurn) apply(restartRepoServer bool) {
	c.t.Helper()
	c.round++
	f := c.receiver
	// The repo-server is restarted or rescheduled (Fix 2).
	restartRenderer(c.t, f, c.round, c.round%2 == 1)
	// cert-manager renews the repo-server's leaf under the same CA (Fix 6). Argo
	// keeps serving the leaf it loaded until the repo-server restarts.
	renewed := c.authority.issue(c.t, int64(100+c.round))
	useRendererCertificate(f, renewed, c.authority.pem())
	if restartRepoServer {
		c.host.rendererAddressOverride = preparationRendererServer(c.t, renewed, c.rpc)
	}
	// An AKS patch release, its API server address, and a newly installed CRD
	// (Fix 3).
	f.objects["version"]["gitVersion"] = "v1.35." + string(rune('0'+c.round))
	f.objects["api"]["serverAddressByClientCIDRs"] = []any{map[string]any{"clientCIDR": "0.0.0.0/0", "serverAddress": "10.0.0." + string(rune('0'+c.round)) + ":443"}}
	group := "churn" + string(rune('a'+c.round)) + ".example.com"
	f.objects["apis"]["groups"] = append(f.objects["apis"]["groups"].([]any), map[string]any{"name": group, "versions": []any{map[string]any{"groupVersion": group + "/v1", "version": "v1"}}})
	f.objects["apis/"+group+"/v1"] = map[string]any{"groupVersion": group + "/v1", "resources": []any{map[string]any{"kind": "Widget", "name": "widgets", "namespaced": true}}}
	// Annotation-only writes to every kind of watched configuration (Fix 6):
	// the receiver's own ConfigMap (re-applied by its Application), a credential
	// with an expiry annotation, Argo's configuration and its project.
	for _, path := range []string{receiverConfigPath, receiverCatalogPath, argoConfigPath, argoProjectPath, "api/v1/namespaces/memql/secrets/registry-ca"} {
		meta := resourceMap(f.objects[path], "metadata")
		meta["resourceVersion"] = "churn-" + string(rune('a'+c.round))
		meta["annotations"] = map[string]any{"memql.io/credential-expires-at": "2027-0" + string(rune('1'+c.round)) + "-01T00:00:00Z"}
	}
	f.objects[argoProjectPath]["status"] = map[string]any{"jwtTokensByRole": map[string]any{}}
}

// The whole Cluster > Updates path on a fresh two-Application installation:
// prepare, Apply, Check progress to completion, then Restore previous release
// to its own completion, with a repo-server restart, a certificate renewal, a
// Kubernetes patch, a new CRD and annotation-only writes between every call.
// Before receiving-configuration-v2 the first Apply after any of these refused,
// and a started update could neither complete nor be restored. What still
// refuses Apply, before any Argo write: another CA for the repo-server, and a
// CRD the render applies that is no longer served, which the configuration
// invariant does not bind and resource evidence catches.
func TestInstallationUpdateSurvivesReceiverChurnThroughCompletionAndRestore(t *testing.T) {
	for _, mode := range []string{"routine churn", "different CA", "rendered API removed"} {
		t.Run(mode, func(t *testing.T) { testInstallationUpdateSurvivesReceiverChurn(t, mode) })
	}
}

func testInstallationUpdateSurvivesReceiverChurn(t *testing.T, mode string) {
	db, peerDB := journalDB(t)
	receiver := newReceiverFixture(t)
	authority := newRendererAuthority(t, "memql-installation-renderer")
	useRendererCertificate(receiver, authority.issue(t, 1), authority.pem())
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
	resourceMap(receiver.objects[argoProjectPath], "spec")["sourceRepos"] = []any{rollbackSource["repoURL"]}
	raw, _ := json.Marshal(app)
	var productApp map[string]any
	require.NoError(t, json.Unmarshal(raw, &productApp))
	productApp["metadata"] = fixtureMeta("product", "argocd")
	resourceMap(resourceMap(productApp, "spec"), "source")["path"] = "deploy/product"
	productPath := "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/product"
	receiver.objects[productPath] = productApp
	// The engine Application also renders a cert-manager Certificate, an API
	// outside the receiver's own contract.
	receiver.objects["apis"]["groups"] = append(receiver.objects["apis"]["groups"].([]any), map[string]any{"name": "cert-manager.io", "versions": []any{map[string]any{"groupVersion": "cert-manager.io/v1", "version": "v1"}}})
	receiver.objects["apis/cert-manager.io/v1"] = map[string]any{"groupVersion": "cert-manager.io/v1", "resources": []any{map[string]any{"name": "certificates", "kind": "Certificate", "namespaced": true}}}
	certificate := `{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"identity","namespace":"memql"},"spec":{"secretName":"identity-tls","dnsNames":["identity.memql.example"]}}`
	publications := preparationCatalogFixture{images.old.Digest(): images.old, images.next.Digest(): images.next}
	factory := func(context.Context, []byte, string) (Catalog, error) { return publications, nil }
	ctx := captureOperator(auth.RoleOwner, "operator")
	snapshot, err := readReceiver(ctx, receiver, "memql", "receiver", factory)
	require.NoError(t, err)
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
		if !product {
			result = append(result, certificate)
		}
		return result
	}
	rpc := &preparationRendererFixture{applicationManifests: map[string]map[string][]string{"memql": {rollbackSHA: manifests(before.Components[0].Artifacts[0].ImageDigest, false), candidateSHA: manifests(after.Components[0].Artifacts[0].ImageDigest, false)}, "product": {rollbackSHA: manifests(before.Components[0].Artifacts[0].ImageDigest, true), candidateSHA: manifests(after.Components[0].Artifacts[0].ImageDigest, true)}}, reads: map[string]int{}}
	address := preparationRendererServer(t, receiver.certificate, rpc)
	request := preparationRequest{InstallationID: receiver.config.InstallationID, RequestID: "churn", CandidateID: after.CandidateID, CatalogDigest: images.next.Digest(), OverlayRevision: candidateSHA}
	external := &preparationProtocolFiles{t: t, journal: preparationConnection(db), request: request, archives: archives, files: map[string]*captureFilesFixture{}}
	host := &preparationHost{api: receiver, journal: external.journal, executor: external, files: external, tokens: preparationTokenFixture{}, catalog: factory, namespace: "memql", configurationName: "receiver", rendererAddressOverride: address}
	serving := before.Components[0].Commit
	host.health = func(_ context.Context, _, _, _, nonce string) (HealthObservation, error) {
		return HealthObservation{UserID: "operator", IdentityID: "identity", EngineCommit: serving, MigrationsDigest: "sha256:" + strings.Repeat("c", 64), Nonce: nonce}, nil
	}
	require.NoError(t, host.bindWorkflow(strings.Repeat("a", 40)))
	record, err := host.prepare(ctx, request)
	require.NoError(t, err)
	require.Len(t, record.Plan.Applications, 2)
	controllers := map[string]*revisionControllerFixture{}
	for _, member := range record.Plan.Applications {
		one := record
		one.Plan.Intent = member.Intent
		c := newRevisionControllerFixture(t, db, one)
		b, _ := json.Marshal(receiver.objects[c.path])
		require.NoError(t, json.Unmarshal(b, &c.object))
		controllers[c.path] = c
	}
	host.api = groupProtocolAPI{receiver: receiver, controllers: controllers, members: map[string]string{enginePath: "engine", productPath: "product"}, interrupt: map[string]bool{}, delayed: map[string][]byte{}, race: map[string]bool{}}
	churn := &receiverChurn{t: t, receiver: receiver, authority: authority, host: host, rpc: rpc}
	public := &Preparer{host: host}

	if mode == "rendered API removed" {
		// A CRD upgrade drops an API the plan's render applies. The receiver's
		// configuration invariant still matches (it binds only the receiver's own
		// APIs); resource evidence resolves every rendered kind from fresh
		// discovery and refuses before the durable start or any write.
		receiver.objects["apis/cert-manager.io/v1"]["resources"] = []any{map[string]any{"name": "certificaterequests", "kind": "CertificateRequest", "namespaced": true}}
		fresh, err := readSelectedReceiver(captureOperator(auth.RoleOwner, "operator"), host, record)
		require.NoError(t, err, "the configuration invariant does not bind the render's APIs")
		require.True(t, fresh.matchesInvariant(record.Plan.Preparation.ConfigurationInvariantDigest))
		_, err = public.StartSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
		require.ErrorContains(t, err, "rendered resource kind is absent from cluster discovery")
		for _, c := range controllers {
			require.Zero(t, c.writes, "no Argo write after refused resource evidence")
		}
		started, err := (&revisionJournal{db: func() *sql.DB { return db }}).get(ctx, request.InstallationID, record.ID)
		require.NoError(t, err)
		require.Equal(t, "prepared", started.State)
		return
	}
	if mode == "different CA" {
		// What still refuses: the repo-server's TLS trust replaced by another CA,
		// even with every other input unchanged and the new leaf served.
		other := newRendererAuthority(t, "memql-installation-renderer")
		replaced := other.issue(t, 7)
		useRendererCertificate(receiver, replaced, other.pem())
		host.rendererAddressOverride = preparationRendererServer(t, replaced, rpc)
		_, err = public.StartSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
		require.ErrorContains(t, err, "configuration or membership changed")
		for _, c := range controllers {
			require.Zero(t, c.writes, "no Argo write after a refused configuration")
		}
		return
	}

	churn.apply(false)
	result, err := public.StartSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.NoError(t, err, "Apply after routine churn")
	require.Equal(t, "applying", result.State)
	for _, c := range controllers {
		require.Equal(t, 1, c.writes)
		c.succeed(candidateSHA)
	}
	serving = after.Components[0].Commit
	groupRuntimeWorkloads(t, receiver, append(manifests(after.Components[0].Artifacts[0].ImageDigest, false), manifests(after.Components[0].Artifacts[0].ImageDigest, true)...))

	churn.apply(true)
	// Check progress runs on a replacement engine with its own journal connection.
	fresh := *host
	fresh.journal = preparationConnection(peerDB)
	require.NoError(t, fresh.bindWorkflow(strings.Repeat("f", 40)))
	public = &Preparer{host: &fresh}
	result, err = public.ObserveSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.NoError(t, err, "completion after routine churn")
	require.Equal(t, "complete", result.State)
	require.True(t, result.CanRollback)

	churn.apply(false)
	fresh.rendererAddressOverride = host.rendererAddressOverride
	_, err = public.RollbackSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.NoError(t, err, "restoration after routine churn")
	for _, c := range controllers {
		require.Equal(t, 2, c.writes)
		c.succeed(rollbackSHA)
	}
	serving = before.Components[0].Commit
	groupRuntimeWorkloads(t, receiver, append(manifests(before.Components[0].Artifacts[0].ImageDigest, false), manifests(before.Components[0].Artifacts[0].ImageDigest, true)...))

	churn.apply(true)
	fresh.rendererAddressOverride = host.rendererAddressOverride
	result, err = public.ObserveSelected(operator(auth.RoleOwner, "operator"), request.InstallationID, record.ID)
	require.NoError(t, err, "restoration's completion after routine churn")
	require.Equal(t, "rolled_back", result.State)
	terminal, err := (&revisionJournal{db: func() *sql.DB { return peerDB }}).get(ctx, request.InstallationID, record.ID)
	require.NoError(t, err)
	require.NotNil(t, terminal.Completion)
	require.NotNil(t, terminal.RollbackCompletion)
	var claims int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM installation_application_claims`).Scan(&claims))
	require.Zero(t, claims)
}
