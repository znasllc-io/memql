package installation

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This transport is test-only and read-only. It uses the explicitly selected
// operator connection, not ambient kubectl context or fabricated API objects.
type installedRendererAPI struct{ kubeconfig string }

func (a installedRendererAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if method != http.MethodGet || contentType != "" || len(body) != 0 ||
		(!strings.HasPrefix(path, "api/v1/namespaces/argocd/") &&
			!strings.HasPrefix(path, "apis/apps/v1/namespaces/argocd/") &&
			!strings.HasPrefix(path, "apis/discovery.k8s.io/v1/namespaces/argocd/")) {
		return nil, errors.New("installed renderer test permits only scoped API reads")
	}
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+a.kubeconfig,
		"get", "--raw=/"+path, "--request-timeout=10s")
	result, err := cmd.Output()
	if err != nil {
		return nil, errors.New("installed renderer API read failed")
	}
	return result, nil
}

// TestReceivingRendererThroughInstalledArgo qualifies only renderer identity and
// configuration. The loopback address must be an operator-authenticated forward
// to argocd/argocd-repo-server:8081. No Application, approval or installation
// update is created, and this does not qualify serving-node RBAC or networking.
func TestReceivingRendererThroughInstalledArgo(t *testing.T) {
	kubeconfig := os.Getenv("MEMQL_INSTALLATION_RENDERER_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set MEMQL_INSTALLATION_RENDERER_TEST_KUBECONFIG for the explicit read-only installed renderer check")
	}
	require.True(t, filepath.IsAbs(kubeconfig), "select the operator connection explicitly")
	_, err := os.Stat(kubeconfig)
	require.NoError(t, err)
	address := os.Getenv("MEMQL_INSTALLATION_RENDERER_TEST_ADDRESS")
	host, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)
	require.NotEmpty(t, port)
	image := os.Getenv("MEMQL_INSTALLATION_RENDERER_TEST_IMAGE")
	_, err = immutableImage(image)
	require.NoError(t, err, "supply the reviewed platform image digest")

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	reads := newReceiverReads(installedRendererAPI{kubeconfig: kubeconfig})
	// This check saves nothing, so a per-run key stands in for the catalog
	// credential that keys a real receiver's content digests.
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	reads.useBindingKey(hex.EncodeToString(key))
	snapshot := &receiverSnapshot{configuration: receiverConfiguration{
		Application: receiverNamed{Namespace: "argocd"},
		Renderer: receiverRenderer{Profile: "argocd-2.13.3-kustomize-5.4.3",
			Deployment: "argocd-repo-server", Container: "argocd-repo-server",
			Service: "argocd-repo-server", TLSSecret: "argocd-repo-server-tls", ConfigMap: "argocd-cm", Image: image},
	}}
	require.NoError(t, snapshot.observeRenderer(ctx, reads))
	require.Equal(t, "argocd-repo-server.argocd.svc:8081", snapshot.rendererAddress)
	// The qualified repo-server consumes its TLS Secret only as its serving
	// certificate, so its trust anchors stand in for the Secret's content and a
	// renewal under the same anchor keeps a started update verifiable.
	tlsPath, err := receiverPath("v1", "secrets", "argocd", "argocd-repo-server-tls")
	require.NoError(t, err)
	require.Equal(t, receiverBindValue, reads.reads[tlsPath].binding.kind, "the renderer TLS Secret must be bound by its trust anchors, not its content")
	probe, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: snapshot.rendererTLS}).DialContext(ctx, "tcp", address)
	require.NoError(t, err, "verify the served certificate chains to the named Secret's trust anchors")
	require.NoError(t, probe.Close())
	digests, err := reads.configuration(ctx)
	require.NoError(t, err, "all authenticated renderer inputs must remain unchanged during qualification")
	t.Logf("installed renderer inputs=%d digest=%s; pinned runtime image and self-signed TLS trust anchors verified; TLS Secret bound by its trust anchors", len(reads.reads), digests.current)
}

type installedDiscoveryAPI struct{ kubeconfig string }

// Discovery only: this probe cannot read namespaced objects or credentials.
var installedDiscoveryPath = regexp.MustCompile(`^(version|api|apis|api/v1|apis/[a-z0-9.-]+/[a-z0-9.-]+)$`)

func (a installedDiscoveryAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if method != http.MethodGet || contentType != "" || len(body) != 0 || !installedDiscoveryPath.MatchString(path) {
		return nil, errors.New("installed discovery test permits only API catalog reads")
	}
	result, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+a.kubeconfig,
		"get", "--raw=/"+path, "--request-timeout=10s").Output()
	if err != nil {
		return nil, errors.New("installed discovery API read failed")
	}
	return result, nil
}

func TestReceivingDiscoveryThroughInstalledKubernetes(t *testing.T) {
	kubeconfig := os.Getenv("MEMQL_INSTALLATION_DISCOVERY_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set MEMQL_INSTALLATION_DISCOVERY_TEST_KUBECONFIG for the explicit read-only discovery check")
	}
	require.True(t, filepath.IsAbs(kubeconfig), "select the operator connection explicitly")
	_, err := os.Stat(kubeconfig)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	reads := newReceiverReads(installedDiscoveryAPI{kubeconfig: kubeconfig})
	version, resources, err := reads.discovery(ctx)
	require.NoError(t, err)
	_, err = reads.configuration(ctx)
	require.NoError(t, err, "discovery must retain every resource contract while tolerating reordered inventories")
	contract, err := json.Marshal(reads.contract)
	require.NoError(t, err)
	t.Logf("installed Kubernetes %s: %d authenticated discovery inputs and %d renderer API entries verified; saved contract %s", version, len(reads.reads), len(resources), contract)
}
