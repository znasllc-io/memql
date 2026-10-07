package installation

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	snapshot := &receiverSnapshot{configuration: receiverConfiguration{
		Application: receiverNamed{Namespace: "argocd"},
		Renderer: receiverRenderer{Profile: "argocd-2.13.3-kustomize-5.4.3",
			Deployment: "argocd-repo-server", Container: "argocd-repo-server",
			Service: "argocd-repo-server", TLSSecret: "argocd-repo-server-tls", ConfigMap: "argocd-cm", Image: image},
	}}
	require.NoError(t, snapshot.observeRenderer(ctx, reads))
	require.Equal(t, "argocd-repo-server.argocd.svc:8081", snapshot.rendererAddress)
	probe, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: snapshot.rendererTLS}).DialContext(ctx, "tcp", address)
	require.NoError(t, err, "verify the live TLS leaf against the authenticated named Secret")
	require.NoError(t, probe.Close())
	digest, err := reads.finish(ctx)
	require.NoError(t, err, "all authenticated renderer inputs must remain unchanged during qualification")
	t.Logf("installed renderer inputs=%d digest=%s; pinned runtime image and TLS identity verified", len(reads.reads), digest)
}
