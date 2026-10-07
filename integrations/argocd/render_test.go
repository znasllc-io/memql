package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type manifestService interface {
	Generate(context.Context, *gen.ManifestRequest) (*gen.ManifestResponse, error)
}

type manifestServiceFunc func(context.Context, *gen.ManifestRequest) (*gen.ManifestResponse, error)

func (f manifestServiceFunc) Generate(ctx context.Context, request *gen.ManifestRequest) (*gen.ManifestResponse, error) {
	return f(ctx, request)
}

func renderFixture(t *testing.T, callback manifestServiceFunc) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "repository.RepoServerService",
		HandlerType: (*manifestService)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "GenerateManifest", Handler: func(service any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			request := &gen.ManifestRequest{}
			if err := decode(request); err != nil {
				return nil, err
			}
			return service.(manifestService).Generate(ctx, request)
		}}},
	}, callback)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	connection, err := grpc.NewClient("passthrough:///argocd-fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func renderSpecFixture() RenderSpec {
	return RenderSpec{
		Source:  json.RawMessage(`{"repoURL":"https://github.com/example/installation.git","path":"deploy/instance","targetRevision":"` + testRevision + `"}`),
		AppName: "installation", AppLabelKey: "app.kubernetes.io/instance", Namespace: "memql", ProjectName: "installation",
		ProjectSourceRepos: []string{"https://github.com/example/installation.git"}, TrackingMethod: "annotation+label",
		InstallationID: "installation", KubeVersion: "1.35.7", APIVersions: []string{"apps/v1/Deployment", "v1/Secret"},
	}
}

func manifestResponseFixture() *gen.ManifestResponse {
	return &gen.ManifestResponse{Revision: testRevision, SourceType: "Kustomize", Manifests: []string{
		`{"apiVersion":"v1","kind":"Secret","metadata":{"namespace":"memql","name":"preserve"},"data":{"opaque":"c2VjcmV0"}}`,
		`{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"values","namespace":"memql"},"data":{"numeric":9007199254740993}}`,
	}}
}

func TestRenderRevisionCarriesExactSourceAndOwnsUnredactedEvidence(t *testing.T) {
	spec := renderSpecFixture()
	credentials := RepositoryCredentials{Username: "x-access-token", Password: "fixture-private-token"}
	var seen *gen.ManifestRequest
	rpc := renderFixture(t, func(ctx context.Context, request *gen.ManifestRequest) (*gen.ManifestResponse, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), time.Minute)
		seen = proto.Clone(request).(*gen.ManifestRequest)
		return manifestResponseFixture(), nil
	})
	result, err := RenderRevision(context.Background(), rpc, spec, credentials)
	require.NoError(t, err)
	require.Equal(t, testRevision, seen.Revision)
	require.Equal(t, testRevision, seen.ApplicationSource.TargetRevision)
	require.Equal(t, "deploy/instance", seen.ApplicationSource.Path)
	require.Equal(t, "https://github.com/example/installation.git", seen.Repo.Repo)
	require.Equal(t, seen.Repo.Repo, seen.ApplicationSource.RepoURL)
	require.Equal(t, credentials.Username, seen.Repo.Username)
	require.Equal(t, credentials.Password, seen.Repo.Password)
	require.True(t, seen.NoCache && seen.NoRevisionCache)
	require.Equal(t, spec.AppName, seen.AppName)
	require.Equal(t, spec.Namespace, seen.Namespace)
	require.Equal(t, spec.ProjectName, seen.ProjectName)
	require.Empty(t, seen.Repo.Project, "a global repository must not inherit the Application project")
	require.Equal(t, spec.ProjectSourceRepos, seen.ProjectSourceRepos)
	require.Equal(t, spec.TrackingMethod, seen.TrackingMethod)
	require.Equal(t, spec.InstallationID, seen.InstallationID)
	require.Equal(t, spec.APIVersions, seen.ApiVersions)
	require.Equal(t, map[string]bool{"Kustomize": true, "Helm": false, "Plugin": false, "Directory": false}, seen.EnabledSourceTypes)
	require.Len(t, result.Resources(), 2)
	require.Contains(t, string(result.Resources()[0]), "9007199254740993")
	require.Contains(t, string(result.Resources()[1]), "c2VjcmV0", "the native verifier needs the actual Secret bytes")
	require.Regexp(t, `^memql-id:[a-f0-9]{64}$`, result.Digest())
	require.NotContains(t, string(result.spec), credentials.Password)
	require.NotContains(t, fmt.Sprintf("%+v %#v", credentials, credentials), credentials.Password)
	require.Equal(t, "ArgoCD render resources=2 digest="+result.Digest(), fmt.Sprintf("%+v", result))
	require.Equal(t, fmt.Sprintf("%+v", result), fmt.Sprintf("%#v", result))

	// A later caller mutation must not change the evidence used by another host.
	spec.APIVersions[0] = "mutated"
	spec.Source[0] = 'x'
	copySpec := result.Spec()
	copySpec.Source[0] = 'x'
	copySpec.ProjectSourceRepos[0] = "elsewhere"
	copyResources := result.Resources()
	copyResources[0][0] = 'x'
	require.JSONEq(t, string(renderSpecFixture().Source), string(result.Spec().Source))
	require.Equal(t, renderSpecFixture().APIVersions, result.Spec().APIVersions)
	require.Equal(t, byte('{'), result.Resources()[0][0])
}

type manifestRPCFunc func(context.Context, string, any, any, ...grpc.CallOption) error

func (f manifestRPCFunc) Invoke(ctx context.Context, method string, in, out any, options ...grpc.CallOption) error {
	return f(ctx, method, in, out, options...)
}

func responseRPC(response *gen.ManifestResponse) ManifestRPC {
	return manifestRPCFunc(func(_ context.Context, _ string, _, out any, _ ...grpc.CallOption) error {
		proto.Merge(out.(*gen.ManifestResponse), response)
		return nil
	})
}

func TestRenderDigestBindsCompleteInputAndResourceBytes(t *testing.T) {
	spec, response := renderSpecFixture(), manifestResponseFixture()
	first, err := RenderRevision(context.Background(), responseRPC(response), spec, RepositoryCredentials{})
	require.NoError(t, err)
	reordered := proto.Clone(response).(*gen.ManifestResponse)
	reordered.Manifests[0], reordered.Manifests[1] = reordered.Manifests[1], reordered.Manifests[0]
	reordered.Manifests[0] = `{"data":{"numeric":9007199254740993},"apiVersion":"v1","metadata":{"namespace":"memql","name":"values"},"kind":"ConfigMap"}`
	second, err := RenderRevision(context.Background(), responseRPC(reordered), spec, RepositoryCredentials{Username: "rotated", Password: "credential"})
	require.NoError(t, err)
	require.Equal(t, first.Digest(), second.Digest(), "map/manifest ordering and credential rotation do not change resources")
	for name, change := range map[string]func(*RenderSpec, *gen.ManifestResponse){
		"path": func(s *RenderSpec, _ *gen.ManifestResponse) {
			s.Source = json.RawMessage(strings.Replace(string(s.Source), "deploy/instance", "deploy/other", 1))
		},
		"revision": func(s *RenderSpec, r *gen.ManifestResponse) {
			r.Revision = strings.Repeat("a", 40)
			s.Source = json.RawMessage(strings.Replace(string(s.Source), testRevision, r.Revision, 1))
		},
		"tracking":     func(s *RenderSpec, _ *gen.ManifestResponse) { s.TrackingMethod = "label" },
		"installation": func(s *RenderSpec, _ *gen.ManifestResponse) { s.InstallationID = "other" },
		"renderer options": func(s *RenderSpec, _ *gen.ManifestResponse) {
			s.KustomizeBuildOptions = "--load-restrictor LoadRestrictionsNone"
		},
		"secret bytes": func(_ *RenderSpec, r *gen.ManifestResponse) {
			r.Manifests[0] = strings.Replace(r.Manifests[0], "c2VjcmV0", "bmV3", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, r := renderSpecFixture(), manifestResponseFixture()
			change(&s, r)
			result, err := RenderRevision(context.Background(), responseRPC(r), s, RepositoryCredentials{})
			require.NoError(t, err)
			require.NotEqual(t, first.Digest(), result.Digest())
		})
	}
}

func TestRenderRejectsUnrepresentableSourceBeforeRPC(t *testing.T) {
	for name, change := range map[string]func(*RenderSpec){
		"branch": func(s *RenderSpec) {
			s.Source = json.RawMessage(strings.Replace(string(s.Source), testRevision, "main", 1))
		},
		"duplicate field": func(s *RenderSpec) { s.Source = append([]byte(`{"repoURL":"other",`), s.Source[1:]...) },
		"unknown source":  func(s *RenderSpec) { s.Source = append([]byte(`{"chart":"substitute",`), s.Source[1:]...) },
		"image override": func(s *RenderSpec) {
			s.Source = append([]byte(`{"kustomize":{"images":["other:latest"]},`), s.Source[1:]...)
		},
		"Kustomize type":  func(s *RenderSpec) { s.Source = append([]byte(`{"kustomize":true,`), s.Source[1:]...) },
		"unpinned binary": func(s *RenderSpec) { s.Source = append([]byte(`{"kustomize":{"version":"v5.4.3"},`), s.Source[1:]...) },
		"repository credentials in URL": func(s *RenderSpec) {
			s.Source = json.RawMessage(strings.Replace(string(s.Source), "https://github.com", "https://secret@github.com", 1))
		},
		"path escape": func(s *RenderSpec) {
			s.Source = json.RawMessage(strings.Replace(string(s.Source), "deploy/instance", "../other", 1))
		},
		"exec plugins":    func(s *RenderSpec) { s.KustomizeBuildOptions = "--enable-alpha-plugins --enable-exec" },
		"binary path":     func(s *RenderSpec) { s.KustomizeBinaryPath = "../other" },
		"excessive input": func(s *RenderSpec) { s.APIVersions = []string{strings.Repeat("a", 512<<10)} },
	} {
		t.Run(name, func(t *testing.T) {
			spec := renderSpecFixture()
			change(&spec)
			rpc := manifestRPCFunc(func(context.Context, string, any, any, ...grpc.CallOption) error {
				t.Fatal("invalid input reached repo-server")
				return nil
			})
			result, err := RenderRevision(context.Background(), rpc, spec, RepositoryCredentials{})
			require.Error(t, err)
			require.Empty(t, result.Digest())
		})
	}
}

func TestRenderRefusesSubstitutionAmbiguityAndUnboundedOutput(t *testing.T) {
	for name, change := range map[string]func(*gen.ManifestResponse){
		"wrong revision":       func(r *gen.ManifestResponse) { r.Revision = strings.Repeat("a", 40) },
		"missing revision":     func(r *gen.ManifestResponse) { r.Revision = "" },
		"wrong renderer":       func(r *gen.ManifestResponse) { r.SourceType = "Helm" },
		"destination metadata": func(r *gen.ManifestResponse) { r.Namespace = "other" },
		"empty inventory":      func(r *gen.ManifestResponse) { r.Manifests = nil },
		"too many objects":     func(r *gen.ManifestResponse) { r.Manifests = make([]string, maxRenderResources+1) },
		"large object":         func(r *gen.ManifestResponse) { r.Manifests[0] = strings.Repeat(" ", maxApplicationBytes+1) },
		"large total":          func(r *gen.ManifestResponse) { r.Manifests = []string{strings.Repeat(" ", maxRenderBytes+1)} },
		"duplicate resource":   func(r *gen.ManifestResponse) { r.Manifests = append(r.Manifests, r.Manifests[0]) },
		"implicit namespace alias": func(r *gen.ManifestResponse) {
			r.Manifests = append(r.Manifests, strings.Replace(r.Manifests[0], `"namespace":"memql",`, "", 1))
		},
		"same resource different version": func(r *gen.ManifestResponse) {
			r.Manifests[0] = strings.Replace(r.Manifests[0], `"v1"`, `"example.io/v1"`, 1)
			r.Manifests = append(r.Manifests, strings.Replace(r.Manifests[0], `"example.io/v1"`, `"example.io/v2"`, 1))
		},
		"duplicate field": func(r *gen.ManifestResponse) { r.Manifests[0] = `{"kind":"ConfigMap",` + r.Manifests[0][1:] },
		"array":           func(r *gen.ManifestResponse) { r.Manifests[0] = `[]` },
		"list": func(r *gen.ManifestResponse) {
			r.Manifests[0] = `{"apiVersion":"v1","kind":"List","metadata":{"name":"list"},"items":[]}`
		},
		"generated identity": func(r *gen.ManifestResponse) {
			r.Manifests[0] = `{"apiVersion":"v1","kind":"Pod","metadata":{"generateName":"pod-"}}`
		},
		"trailing JSON": func(r *gen.ManifestResponse) { r.Manifests[0] += "{}" },
	} {
		t.Run(name, func(t *testing.T) {
			response := manifestResponseFixture()
			change(response)
			result, err := RenderRevision(context.Background(), responseRPC(response), renderSpecFixture(), RepositoryCredentials{})
			require.Error(t, err)
			require.Empty(t, result.Digest())
			require.Empty(t, result.Resources())
		})
	}
}

func TestRenderRPCBoundsAndRedactsUpstreamFailures(t *testing.T) {
	t.Run("error text", func(t *testing.T) {
		rpc := renderFixture(t, func(context.Context, *gen.ManifestRequest) (*gen.ManifestResponse, error) {
			return nil, status.Error(codes.PermissionDenied, "fixture-private-token; rendered Secret data")
		})
		_, err := RenderRevision(context.Background(), rpc, renderSpecFixture(), RepositoryCredentials{})
		require.EqualError(t, err, "ArgoCD repository render failed (PermissionDenied)")
	})
	t.Run("response frame", func(t *testing.T) {
		rpc := renderFixture(t, func(context.Context, *gen.ManifestRequest) (*gen.ManifestResponse, error) {
			response := manifestResponseFixture()
			response.Manifests = []string{strings.Repeat("x", maxRenderBytes+1)}
			return response, nil
		})
		_, err := RenderRevision(context.Background(), rpc, renderSpecFixture(), RepositoryCredentials{})
		require.EqualError(t, err, "ArgoCD repository render failed (ResourceExhausted)")
	})
	t.Run("caller cancellation", func(t *testing.T) {
		rpc := renderFixture(t, func(ctx context.Context, _ *gen.ManifestRequest) (*gen.ManifestResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := RenderRevision(ctx, rpc, renderSpecFixture(), RepositoryCredentials{})
		require.EqualError(t, err, "ArgoCD repository render failed (Canceled)")
	})
}
