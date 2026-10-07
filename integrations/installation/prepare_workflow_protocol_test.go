package installation

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/argocd/gen"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Only external executor/storage records survive replacement of the host. The
// journal and every native verifier below are the production implementations.
type preparationProtocolFiles struct {
	t               *testing.T
	journal         *preparationJournal
	request         preparationRequest
	archives        map[string][]byte
	files           map[string]*captureFilesFixture
	requests        []pipelines.StepRequest
	acknowledgments int
}

func (f *preparationProtocolFiles) Execute(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
	f.requests = append(f.requests, req)
	record, err := f.journal.getByRequest(ctx, f.request.InstallationID, f.request.RequestID)
	if err != nil {
		return pipelines.StepResult{}, err
	}
	role := strings.TrimPrefix(req.StepKey, "source-")
	capture, err := newSourceCapture(record.Scope.Captures[role])
	if err != nil {
		return pipelines.StepResult{}, err
	}
	executor, file := capturePorts(capture, f.archives[role])
	intent := strings.Repeat("d", 64)
	if role == "rollback" {
		intent = strings.Repeat("e", 64)
	}
	executor.result.ArtifactIntentIDs = []string{intent}
	file.row.IntentID = intent
	f.files[req.StepKey] = file
	return executor.result, nil
}
func (f *preparationProtocolFiles) AcknowledgeReceipt(context.Context, pipelines.StepRequest) error {
	f.acknowledgments++
	return nil
}
func (f *preparationProtocolFiles) Cancel(context.Context, string) error { return nil }
func (f *preparationProtocolFiles) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	file := f.files[ref.Scope.StepKey]
	if file == nil {
		return nil, errors.New("missing external receipt")
	}
	return file.PinRunFileReceipts(ctx, ref)
}
func (f *preparationProtocolFiles) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, intent string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	file := f.files[scope.StepKey]
	if file == nil {
		return pipelinesteps.StoredFileReceipt{}, nil, errors.New("missing external bytes")
	}
	return file.OpenRunFileReceipt(ctx, scope, intent)
}
func (f *preparationProtocolFiles) ReleaseRunFileReference(context.Context, pipelinesteps.RunFileReference) error {
	return nil
}
func (f *preparationProtocolFiles) RetireRunFile(context.Context, pipelinesteps.RunFileReceiptScope, string) (pipelinesteps.RetiredFileReceipt, error) {
	return pipelinesteps.RetiredFileReceipt{}, nil
}

type preparationRendererRPC interface {
	Generate(context.Context, *gen.ManifestRequest) (*gen.ManifestResponse, error)
}
type preparationRendererFixture struct {
	mu           sync.Mutex
	manifests    map[string][]string
	failRevision string
	reads        map[string]int
}

func (f *preparationRendererFixture) Generate(_ context.Context, req *gen.ManifestRequest) (*gen.ManifestResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[req.Revision]++
	if req.Revision == f.failRevision {
		return nil, errors.New("interrupted renderer")
	}
	manifests, ok := f.manifests[req.Revision]
	if !ok || req.Repo.Password != "fixture-clone-token" || !req.NoCache || !req.NoRevisionCache {
		return nil, errors.New("unscoped render")
	}
	return &gen.ManifestResponse{Revision: req.Revision, SourceType: "Kustomize", Manifests: manifests}, nil
}
func preparationRendererServer(t *testing.T, certificate tls.Certificate, implementation *preparationRendererFixture) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})))
	server.RegisterService(&grpc.ServiceDesc{ServiceName: "repository.RepoServerService", HandlerType: (*preparationRendererRPC)(nil), Methods: []grpc.MethodDesc{{MethodName: "GenerateManifest", Handler: func(service any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		req := &gen.ManifestRequest{}
		if err := decode(req); err != nil {
			return nil, err
		}
		return service.(preparationRendererRPC).Generate(ctx, req)
	}}}}, implementation)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return listener.Addr().String()
}

func TestPreparationWorkflowProtocolRecoveryPromotesWithFreshNativeEvidence(t *testing.T) {
	db, peerDB := journalDB(t)
	receiver := newReceiverFixture(t)
	images := newAdmissionFixture(t)
	tlsRegistry := httptest.NewTLSServer(images.server.Config.Handler)
	t.Cleanup(tlsRegistry.Close)
	images.server.Close()
	before, err := images.old.Release()
	require.NoError(t, err)
	after, err := images.next.Release()
	require.NoError(t, err)
	before.Components[0].Artifacts[0].Locations[0].Origin = tlsRegistry.URL
	after.Components[0].Artifacts[0].Locations[0].Origin = tlsRegistry.URL
	images.old, images.next = signAdmissionRelease(t, before), signAdmissionRelease(t, after)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsRegistry.Certificate().Raw})
	receiver.put("v1", "Secret", "secrets", "memql", "registry-ca", map[string]any{"data": map[string]any{"ca": base64.StdEncoding.EncodeToString(ca)}})
	receiver.config.Registries = []receiverRegistry{{Origin: tlsRegistry.URL, Repository: "memql/engine", RootCA: &receiverSecret{Name: "registry-ca", Key: "ca"}}}
	receiver.config.Rollback = receiverSelection{CandidateID: before.CandidateID, CatalogDigest: images.old.Digest()}
	receiver.config.ImageBindings[0].Container = "engine"
	receiver.config.Protected = []receiverProtected{{Namespace: "memql", Name: "keys", Kind: "Secret"}}
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
	app := receiver.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"]
	resourceMap(app, "spec")["source"] = rollbackSource
	resourceMap(resourceMap(app, "status"), "sync")["revision"] = rollbackSource["targetRevision"]
	resourceMap(receiver.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects/default"], "spec")["sourceRepos"] = []any{rollbackSource["repoURL"]}
	publications := preparationCatalogFixture{images.old.Digest(): images.old, images.next.Digest(): images.next}
	factory := func(context.Context, []byte, string) (Catalog, error) { return publications, nil }
	ctx := captureOperator(auth.RoleOwner, "operator")
	snapshot, err := readReceiver(ctx, receiver, "memql", "receiver", factory)
	require.NoError(t, err)
	_, rollbackArchive := captureRevisionWithRenderFixture(t, "rollback", &snapshot.render)
	_, candidateArchive := captureRevisionWithRenderFixture(t, "candidate", &snapshot.render)
	rollbackSHA, candidateSHA := rollbackSource["targetRevision"].(string), candidateSource["targetRevision"].(string)
	manifests := func(digest string) []string {
		result := []string{}
		for _, body := range sensitiveBefore.Resources() {
			result = append(result, strings.ReplaceAll(string(body), oldImage, strings.TrimPrefix(tlsRegistry.URL, "https://")+"/memql/engine@"+digest))
		}
		return result
	}
	rpc := &preparationRendererFixture{manifests: map[string][]string{rollbackSHA: manifests(before.Components[0].Artifacts[0].ImageDigest), candidateSHA: manifests(after.Components[0].Artifacts[0].ImageDigest)}, failRevision: candidateSHA, reads: map[string]int{}}
	address := preparationRendererServer(t, receiver.certificate, rpc)
	request := preparationRequest{InstallationID: receiver.config.InstallationID, RequestID: "protocol-recovery", CandidateID: after.CandidateID, CatalogDigest: images.next.Digest(), OverlayRevision: candidateSHA}
	external := &preparationProtocolFiles{t: t, journal: preparationConnection(db), request: request, archives: map[string][]byte{"rollback": rollbackArchive, "candidate": candidateArchive}, files: map[string]*captureFilesFixture{}}
	host := &preparationHost{api: receiver, journal: external.journal, executor: external, files: external, tokens: preparationTokenFixture{}, catalog: factory, namespace: "memql", configurationName: "receiver"}
	require.NoError(t, host.bindWorkflow(strings.Repeat("a", 40)))
	// The fixture replaces only DNS routing to a loopback listener. It still
	// uses the observed service's exact certificate, trust and server name.
	snapshot.rendererAddress = address
	scope := &preparationWorkflowScope{host: host, request: request, operator: "operator", configuration: snapshot, sources: map[string]verifiedSourceCapture{}, renders: map[string]argocd.RenderedRevision{}}
	_, err = scope.run(ctx)
	require.Error(t, err)
	require.Len(t, external.requests, 2)
	saved, err := host.journal.getByRequest(ctx, request.InstallationID, request.RequestID)
	require.NoError(t, err)
	require.Equal(t, "preparing", saved.State)
	require.Equal(t, 2, external.acknowledgments)
	rpc.mu.Lock()
	rpc.failRevision = ""
	rpc.mu.Unlock()
	fresh := *host
	fresh.journal = preparationConnection(peerDB)
	freshSnapshot, err := readReceiver(ctx, receiver, "memql", "receiver", factory)
	require.NoError(t, err)
	freshSnapshot.rendererAddress = address
	recovered := &preparationWorkflowScope{host: &fresh, request: request, operator: "operator", configuration: freshSnapshot, record: saved, sources: map[string]verifiedSourceCapture{}, renders: map[string]argocd.RenderedRevision{}}
	result, err := recovered.run(ctx)
	require.NoError(t, err)
	require.Equal(t, "prepared", result.State)
	require.NotEmpty(t, result.Plan.Preparation.ArtifactDigest)
	require.Equal(t, freshSnapshot.digest, result.Plan.Preparation.ConfigurationDigest)
	require.Len(t, external.requests, 2, "recovery must not launch duplicate source jobs")
	require.Equal(t, 2, external.acknowledgments, "durable acknowledgments are reused")
	for _, file := range external.files {
		require.Equal(t, 2, file.opens, "replacement host reopens private source bytes")
		require.Equal(t, file.opens, file.closed)
	}
	rpc.mu.Lock()
	require.Equal(t, 2, rpc.reads[rollbackSHA])
	require.Equal(t, 2, rpc.reads[candidateSHA])
	rpc.mu.Unlock()
	images.mu.Lock()
	require.Equal(t, 1, images.reads[images.oldLayer])
	require.Equal(t, 1, images.reads[images.newLayer])
	images.mu.Unlock()
	historical, err := fresh.prepare(ctx, request)
	require.NoError(t, err)
	require.Equal(t, result, historical)
	require.Len(t, external.requests, 2)
}
