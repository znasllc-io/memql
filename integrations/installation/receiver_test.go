package installation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/pipelines"
)

type receiverCatalogFixture struct{}

func (receiverCatalogFixture) Get(context.Context, string, string, string) (pipelines.VerifiedPublishedRelease, error) {
	return pipelines.VerifiedPublishedRelease{}, errors.New("unselected fixture publication")
}

type receiverFixture struct {
	certificate tls.Certificate
	objects     map[string]map[string]any
	config      receiverConfiguration
	reads       map[string]int
	before      func(string, int)
}

func (f *receiverFixture) Do(_ context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if method != http.MethodGet || contentType != "" || len(body) != 0 {
		return nil, errors.New("configuration observation attempted a write")
	}
	f.reads[path]++
	if f.before != nil {
		f.before(path, f.reads[path])
	}
	value, ok := f.objects[path]
	if !ok {
		return nil, errors.New("unknown fixture path")
	}
	return json.Marshal(value)
}
func fixtureMeta(name, namespace string) map[string]any {
	return map[string]any{"name": name, "namespace": namespace, "uid": "uid-" + name, "resourceVersion": "11", "generation": 1}
}
func (f *receiverFixture) put(version, kind, plural, namespace, name string, fields map[string]any) map[string]any {
	fields["apiVersion"], fields["kind"], fields["metadata"] = version, kind, fixtureMeta(name, namespace)
	path, _ := receiverPath(version, plural, namespace, name)
	f.objects[path] = fields
	return fields
}
func fixtureOwner(version, kind, name string) []any {
	return []any{map[string]any{"apiVersion": version, "kind": kind, "name": name, "uid": "uid-" + name, "controller": true}}
}
func (f *receiverFixture) saveConfig() {
	body, _ := json.Marshal(f.config)
	f.objects["api/v1/namespaces/memql/configmaps/receiver"]["data"] = map[string]any{"installation.json": string(body)}
}
func (f *receiverFixture) factory(t *testing.T) CatalogFactory {
	return func(_ context.Context, body []byte, token string) (Catalog, error) {
		require.Equal(t, "private-catalog-token", token)
		require.Contains(t, string(body), "https://publisher.example")
		return receiverCatalogFixture{}, nil
	}
}
func newReceiverFixture(t *testing.T) *receiverFixture {
	t.Helper()
	f := &receiverFixture{objects: map[string]map[string]any{}, reads: map[string]int{}}
	f.config = receiverConfiguration{FormatVersion: 1, InstallationID: "receiving-installation", Platform: "linux/arm64", Application: receiverNamed{"argocd", "memql"}, Renderer: receiverRenderer{Deployment: "renderer", Service: "renderer", Container: "repo-server", Image: oldImage, Profile: "argocd-2.13.3-kustomize-5.4.3", ConfigMap: "argocd-cm", TLSSecret: "renderer-tls"}, Collector: receiverCollector{Image: oldImage, CloneInstallationID: 123}, Catalog: receiverCatalog{ID: "publisher", Publisher: "memql", Endpoint: "https://publisher.example", PublicKeys: map[string]string{"key": base64.StdEncoding.EncodeToString(make([]byte, 32))}, Credential: receiverSecret{"catalog", "token"}}, Rollback: receiverSelection{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)}, Registries: []receiverRegistry{{Origin: "https://registry.example", Repository: "memql/engine"}}, ImageBindings: []receiverImageBinding{{Group: "apps", Kind: "Deployment", Namespace: "memql", Name: "bff", Field: "containers", Container: "bff", Component: "engine", Artifact: "bff"}}, Protected: []receiverProtected{{Namespace: "memql", Name: "catalog", Kind: "Secret"}}}
	f.put("v1", "ConfigMap", "configmaps", "memql", "receiver", map[string]any{})
	f.saveConfig()
	f.put("v1", "Secret", "secrets", "memql", "catalog", map[string]any{"data": map[string]any{"token": base64.StdEncoding.EncodeToString([]byte("private-catalog-token"))}})
	f.put("argoproj.io/v1alpha1", "Application", "applications", "argocd", "memql", map[string]any{"spec": map[string]any{"project": "default", "source": map[string]any{"repoURL": "https://github.com/acme/platform.git", "path": "deploy", "targetRevision": strings.Repeat("a", 40)}, "destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "memql"}}, "status": map[string]any{"sync": map[string]any{"status": "Synced", "revision": strings.Repeat("a", 40)}, "health": map[string]any{"status": "Healthy"}}})
	f.put("argoproj.io/v1alpha1", "AppProject", "appprojects", "argocd", "default", map[string]any{"spec": map[string]any{"sourceRepos": []any{"https://github.com/acme/platform.git"}, "destinations": []any{map[string]any{"namespace": "memql", "server": "https://kubernetes.default.svc"}}}})
	f.put("v1", "ConfigMap", "configmaps", "argocd", "argocd-cm", map[string]any{"data": map[string]any{"application.resourceTrackingMethod": "annotation"}})
	f.objects["api/v1/namespaces/argocd/secrets?labelSelector=argocd.argoproj.io%2Fsecret-type%3Drepository&limit=257"] = map[string]any{"apiVersion": "v1", "kind": "SecretList", "metadata": map[string]any{"resourceVersion": "10"}, "items": []any{}}
	f.objects["version"] = map[string]any{"gitVersion": "v1.35.0"}
	f.objects["api"] = map[string]any{"versions": []any{"v1"}}
	f.objects["apis"] = map[string]any{"groups": []any{map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}}}}
	f.objects["api/v1"] = map[string]any{"groupVersion": "v1", "resources": []any{map[string]any{"kind": "Secret", "name": "secrets", "namespaced": true}}}
	f.objects["apis/apps/v1"] = map[string]any{"groupVersion": "apps/v1", "resources": []any{map[string]any{"kind": "Deployment", "name": "deployments", "namespaced": true}}}
	labels := map[string]any{"app": "renderer"}
	podSpec := map[string]any{"containers": []any{map[string]any{"name": "repo-server", "image": oldImage, "args": []any{"/usr/local/bin/argocd-repo-server"}, "volumeMounts": []any{map[string]any{"name": "tls", "mountPath": "/app/config/reposerver/tls"}}}}, "volumes": []any{map[string]any{"name": "tls", "secret": map[string]any{"secretName": "renderer-tls"}}}}
	f.put("apps/v1", "Deployment", "deployments", "argocd", "renderer", map[string]any{"spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec}}, "status": map[string]any{"observedGeneration": 1, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1}})
	rs := f.put("apps/v1", "ReplicaSet", "replicasets", "argocd", "renderer-rs", map[string]any{"spec": map[string]any{"template": map[string]any{"spec": podSpec}}})
	resourceMap(rs, "metadata")["ownerReferences"] = fixtureOwner("apps/v1", "Deployment", "renderer")
	pod := f.put("v1", "Pod", "pods", "argocd", "renderer-pod", map[string]any{"spec": podSpec, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "podIPs": []any{map[string]any{"ip": "10.42.0.7"}}, "containerStatuses": []any{map[string]any{"name": "repo-server", "ready": true, "imageID": "docker-pullable://" + oldImage, "state": map[string]any{"running": map[string]any{"startedAt": "2026-10-01T00:00:00Z"}}}}}})
	resourceMap(pod, "metadata")["ownerReferences"] = fixtureOwner("apps/v1", "ReplicaSet", "renderer-rs")
	resourceMap(pod, "metadata")["labels"] = labels
	f.put("v1", "Service", "services", "argocd", "renderer", map[string]any{"spec": map[string]any{"type": "ClusterIP", "clusterIP": "10.43.0.11", "selector": labels, "ports": []any{map[string]any{"name": "grpc", "protocol": "TCP", "port": 8081, "targetPort": 8081}}}})
	slice := map[string]any{"metadata": fixtureMeta("renderer-slice", "argocd"), "ports": []any{map[string]any{"name": "grpc", "protocol": "TCP", "port": 8081}}, "endpoints": []any{map[string]any{"addresses": []any{"10.42.0.7"}, "conditions": map[string]any{"ready": true}, "targetRef": map[string]any{"kind": "Pod", "namespace": "argocd", "name": "renderer-pod", "uid": "uid-renderer-pod"}}}}
	resourceMap(slice, "metadata")["ownerReferences"] = fixtureOwner("v1", "Service", "renderer")
	resourceMap(slice, "metadata")["labels"] = map[string]any{"kubernetes.io/service-name": "renderer"}
	f.objects["apis/discovery.k8s.io/v1/namespaces/argocd/endpointslices?labelSelector=kubernetes.io%2Fservice-name%3Drenderer&limit=129"] = map[string]any{"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSliceList", "metadata": map[string]any{"resourceVersion": "10"}, "items": []any{slice}}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "renderer"}, DNSNames: []string{"renderer.argocd.svc"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	require.NoError(t, err)
	f.certificate = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	f.put("v1", "Secret", "secrets", "argocd", "renderer-tls", map[string]any{"data": map[string]any{"tls.crt": base64.StdEncoding.EncodeToString(certPEM), "tls.key": base64.StdEncoding.EncodeToString([]byte("private-key-never-output"))}})
	return f
}
func TestReceiverIndependentAuthenticatedReadsBindRotationWithoutSecrets(t *testing.T) {
	f := newReceiverFixture(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer projected-fixture-token", r.Header.Get("Authorization"))
		body, err := f.Do(r.Context(), r.Method, strings.TrimPrefix(r.URL.RequestURI(), "/"), "", nil)
		if err != nil {
			http.Error(w, "unavailable", 404)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	api := deploycontrol.NewClusterAPIWith(server.URL, "projected-fixture-token", server.Client())
	ctx := captureOperator(auth.RoleOwner, "operator")
	a, err := readReceiver(ctx, api, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	b, err := readReceiver(ctx, api, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Equal(t, a.digest, b.digest)
	require.NotSame(t, a.rendererTLS, b.rendererTLS)
	require.Equal(t, []string{"apps/v1", "apps/v1/Deployment", "v1", "v1/Secret"}, a.render.APIVersions)
	for _, path := range []string{"apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql", "api/v1/namespaces/argocd/pods/renderer-pod"} {
		resourceMap(f.objects[path], "metadata")["resourceVersion"] = "12"
	}
	c, err := readReceiver(ctx, api, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Equal(t, a.digest, c.digest, "status-only RV churn must not invalidate recovery")
	resourceMap(f.objects["api/v1/namespaces/memql/secrets/catalog"], "metadata")["resourceVersion"] = "12"
	d, err := readReceiver(ctx, api, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.NotEqual(t, a.digest, d.digest, "rotation invalidates prior configuration even if credential bytes match")
	formatted := fmt.Sprintf("%+v %#v", a, a)
	require.NotContains(t, formatted, "private-catalog-token")
	require.NotContains(t, formatted, "private-key-never-output")
	require.NotContains(t, formatted, "publisher.example")
	body, err := json.Marshal(a)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(body))
}

func TestReceiverRefusesChangedOrUnqualifiedConfiguration(t *testing.T) {
	for _, fault := range []string{"unknown field", "duplicate field", "case duplicate", "secret rotation during read", "application ABA", "renderer not ready", "renderer pod replaced", "renderer runtime image differs", "endpoint addresses another pod", "service selector differs", "mutable renderer", "plaintext registry", "unqualified profile", "TLS secret missing", "wrong TLS identity", "extra renderer args", "external destination", "mutable baseline", "unhealthy baseline", "discovery changes", "endpoint truncated", "endpoint replaced", "unknown renderer env", "literal credential", "repository truncated"} {
		t.Run(fault, func(t *testing.T) {
			f := newReceiverFixture(t)
			app := f.objects["apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql"]
			pod := f.objects["api/v1/namespaces/argocd/pods/renderer-pod"]
			deployment := f.objects["apis/apps/v1/namespaces/argocd/deployments/renderer"]
			endpointPath := "apis/discovery.k8s.io/v1/namespaces/argocd/endpointslices?labelSelector=kubernetes.io%2Fservice-name%3Drenderer&limit=129"
			endpoints := f.objects[endpointPath]
			slice := endpoints["items"].([]any)[0].(map[string]any)
			endpoint := slice["endpoints"].([]any)[0].(map[string]any)
			switch fault {
			case "unknown field", "duplicate field", "case duplicate":
				key := "bogus"
				if fault == "duplicate field" {
					key = "formatVersion"
				}
				if fault == "case duplicate" {
					key = "FormatVersion"
				}
				data := resourceMap(f.objects["api/v1/namespaces/memql/configmaps/receiver"], "data")
				data["installation.json"] = `{"` + key + `":1,` + resourceText(data, "installation.json")[1:]
			case "secret rotation during read":
				f.before = func(path string, n int) {
					if path == "api/v1/namespaces/memql/secrets/catalog" && n == 2 {
						resourceMap(f.objects[path], "metadata")["resourceVersion"] = "12"
					}
				}
			case "application ABA":
				f.before = func(path string, n int) {
					if strings.HasSuffix(path, "/applications/memql") && n == 2 {
						resourceMap(f.objects[path], "metadata")["resourceVersion"] = "13"
					}
				}
			case "renderer not ready":
				resourceMap(deployment, "status")["readyReplicas"] = 0
			case "renderer pod replaced":
				resourceMap(pod, "metadata")["uid"] = "replacement"
			case "renderer runtime image differs":
				resourceMap(pod, "status")["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = nextImage
			case "endpoint addresses another pod":
				endpoint["addresses"] = []any{"10.42.0.99"}
			case "service selector differs":
				resourceMap(f.objects["api/v1/namespaces/argocd/services/renderer"], "spec")["selector"] = map[string]any{"app": "other"}
			case "mutable renderer":
				f.config.Renderer.Image = "quay.io/argoproj/argocd:v2.13.3"
				f.saveConfig()
			case "plaintext registry":
				f.config.Registries[0].Origin = "http://registry.example"
				f.saveConfig()
			case "unqualified profile":
				f.config.Renderer.Profile = "other"
				f.saveConfig()
			case "TLS secret missing":
				delete(f.objects, "api/v1/namespaces/argocd/secrets/renderer-tls")
			case "wrong TLS identity":
				server := httptest.NewTLSServer(http.NotFoundHandler())
				defer server.Close()
				certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
				resourceMap(f.objects["api/v1/namespaces/argocd/secrets/renderer-tls"], "data")["tls.crt"] = base64.StdEncoding.EncodeToString(certificate)
			case "unknown renderer env", "literal credential":
				name := "ARGOCD_FUTURE_EXECUTABLE"
				if fault == "literal credential" {
					name = "REDIS_PASSWORD"
				}
				resourceMap(deployment, "spec")["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"] = []any{map[string]any{"name": name, "value": "private-inline-credential"}}
			case "repository truncated":
				resourceMap(f.objects["api/v1/namespaces/argocd/secrets?labelSelector=argocd.argoproj.io%2Fsecret-type%3Drepository&limit=257"], "metadata")["continue"] = "more"
			case "extra renderer args":
				resourceMap(pod, "spec")["containers"].([]any)[0].(map[string]any)["args"] = []any{"/usr/local/bin/argocd-repo-server", "--disable-tls"}
			case "external destination":
				resourceMap(resourceMap(app, "spec"), "destination")["server"] = "https://another-cluster.invalid"
			case "mutable baseline":
				resourceMap(resourceMap(app, "spec"), "source")["targetRevision"] = "main"
			case "unhealthy baseline":
				resourceMap(resourceMap(app, "status"), "health")["status"] = "Progressing"
			case "discovery changes":
				f.before = func(path string, n int) {
					if path == "version" && n == 2 {
						f.objects[path]["gitVersion"] = "v1.36.0"
					}
				}
			case "endpoint truncated":
				resourceMap(endpoints, "metadata")["continue"] = "page2"
			case "endpoint replaced":
				resourceMap(slice, "metadata")["ownerReferences"] = fixtureOwner("v1", "Service", "other")
			}
			_, err := readReceiver(captureOperator(auth.RoleOwner, "operator"), f, "memql", "receiver", f.factory(t))
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-")
		})
	}
}
func TestReceiverCopyInitPermitsOnlyInheritedLiteralTimeout(t *testing.T) {
	for _, fault := range []string{"", "other variable", "different timeout", "indirect timeout", "extra field", "extra variable", "envFrom", "different command"} {
		t.Run("fault="+fault, func(t *testing.T) {
			f := newReceiverFixture(t)
			pod := resourceMap(f.objects["api/v1/namespaces/argocd/pods/renderer-pod"], "spec")
			container := pod["containers"].([]any)[0].(map[string]any)
			container["env"] = []any{map[string]any{"name": "ARGOCD_EXEC_TIMEOUT", "value": "1200s"}}
			env := map[string]any{"name": "ARGOCD_EXEC_TIMEOUT", "value": "1200s"}
			init := map[string]any{"name": "copyutil", "image": oldImage, "command": []any{"/bin/cp", "-n", "/usr/local/bin/argocd", "/var/run/argocd/argocd-cmp-server"}, "env": []any{env}}
			pod["initContainers"] = []any{init}
			switch fault {
			case "other variable":
				env["name"] = "LD_PRELOAD"
			case "different timeout":
				env["value"] = "30s"
			case "indirect timeout":
				delete(env, "value")
				env["valueFrom"] = map[string]any{"secretKeyRef": map[string]any{"name": "private", "key": "timeout"}}
			case "extra field":
				env["unexpected"] = "private"
			case "extra variable":
				init["env"] = append(init["env"].([]any), map[string]any{"name": "LD_PRELOAD", "value": "private"})
			case "envFrom":
				init["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "private"}}}
			case "different command":
				init["command"] = []any{"/bin/sh", "-c", "private"}
			}
			_, err := readReceiver(captureOperator(auth.RoleOwner, "operator"), f, "memql", "receiver", f.factory(t))
			if fault == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "init container requires qualification")
				require.NotContains(t, err.Error(), "private")
			}
		})
	}
}

func TestReceiverRequiresCurrentNativeOperator(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), operator(auth.RoleOwner, "operator"), captureOperator(auth.RoleReader, "operator")} {
		f := newReceiverFixture(t)
		_, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
		require.Error(t, err)
		require.Empty(t, f.reads)
	}
}

func TestReceiverRepositoryScopeAndCollectionVersions(t *testing.T) {
	f := newReceiverFixture(t)
	path := "api/v1/namespaces/argocd/secrets?labelSelector=argocd.argoproj.io%2Fsecret-type%3Drepository&limit=257"
	list := f.objects[path]
	secret := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": fixtureMeta("repository", "argocd"), "data": map[string]any{"url": base64.StdEncoding.EncodeToString([]byte(" HTTPS://GITHUB.COM/ACME/PLATFORM.GIT ")), "project": base64.StdEncoding.EncodeToString([]byte("default")), "password": base64.StdEncoding.EncodeToString([]byte("private-repository-password"))}}
	resourceMap(secret, "metadata")["labels"] = map[string]any{"argocd.argoproj.io/secret-type": "repository"}
	// Kubernetes omits TypeMeta on items inside a typed SecretList.
	delete(secret, "kind")
	delete(secret, "apiVersion")
	list["items"] = []any{secret}
	ctx := captureOperator(auth.RoleOwner, "operator")
	first, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Equal(t, "default", first.render.RepositoryProject)
	f.before = func(path string, n int) {
		if strings.Contains(path, "?labelSelector=") {
			resourceMap(f.objects[path], "metadata")["resourceVersion"] = fmt.Sprint(n)
		}
	}
	again, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.Equal(t, first.digest, again.digest, "unrelated collection writes do not change repository or endpoint identities")
	resourceMap(secret, "metadata")["resourceVersion"] = "rotated"
	changed, err := readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.NoError(t, err)
	require.NotEqual(t, first.digest, changed.digest)
	r := newReceiverReads(f)
	_, err = r.repositoryProject(ctx, "argocd", "default", "https://github.com/acme/platform.git")
	require.NoError(t, err)
	encoded, err := json.Marshal(r.reads[path].stable)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-repository-password")
	require.NotContains(t, string(encoded), resourceMap(secret, "data")["password"])
	list["items"] = []any{secret, secret}
	_, err = readReceiver(ctx, f, "memql", "receiver", f.factory(t))
	require.ErrorContains(t, err, "ambiguous")
}
