package installation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Test transport only: normal installation wiring uses the native cluster API.
// No subprocess is a production capability and no write verb is accepted.
type storageLiveAPI struct{ kubeconfig string }

func (a storageLiveAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if method != http.MethodGet || contentType != "" || len(body) != 0 || strings.HasPrefix(path, "/") {
		return nil, errors.New("storage rehearsal accepts cluster reads only")
	}
	result, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+a.kubeconfig, "--request-timeout=15s", "get", "--raw=/"+path).Output()
	if err != nil || len(result) > 4<<20 {
		return nil, errors.New("storage rehearsal read failed or exceeded its bound")
	}
	return result, nil
}

func TestStorageAgainstInstalledLocalCluster(t *testing.T) {
	kubeconfig := os.Getenv("MEMQL_INSTALLATION_STORAGE_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set MEMQL_INSTALLATION_STORAGE_TEST_KUBECONFIG for explicit read-only local storage qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+kubeconfig, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}").Output()
	require.NoError(t, err)
	u, err := url.Parse(string(server))
	require.NoError(t, err)
	require.Equal(t, "https", u.Scheme)
	ip := net.ParseIP(u.Hostname())
	require.NotNil(t, ip)
	require.True(t, ip.IsLoopback(), "this qualification may read only the explicitly selected local cluster")
	address := os.Getenv("MEMQL_ARGOCD_RENDER_TEST_ADDRESS")
	host, _, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)
	serverName := os.Getenv("MEMQL_ARGOCD_RENDER_TEST_SERVER_NAME")
	require.NotEmpty(t, serverName)
	ca, err := os.ReadFile(os.Getenv("MEMQL_ARGOCD_RENDER_TEST_CA"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: serverName})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	body, err := os.ReadFile(os.Getenv("MEMQL_ARGOCD_RENDER_TEST_SPEC"))
	require.NoError(t, err)
	spec, err := argocd.DecodeRenderSpec(body)
	require.NoError(t, err)
	source, err := resourceJSON(spec.Source)
	require.NoError(t, err)
	require.Equal(t, "https://github.com/znasllc-io/memql.git", resourceText(source, "repoURL"))
	require.Equal(t, "deploy/k8s/overlays/local", resourceText(source, "path"))
	require.Equal(t, "memql", spec.Namespace)
	before, err := argocd.RenderRevision(ctx, conn, spec, argocd.RepositoryCredentials{})
	require.NoError(t, err)
	revision := os.Getenv("MEMQL_INSTALLATION_STORAGE_TEST_REVISION")
	require.Regexp(t, commitDigest, revision)
	source["targetRevision"] = revision
	spec.Source, err = json.Marshal(source)
	require.NoError(t, err)
	after, err := argocd.RenderRevision(ctx, conn, spec, argocd.RepositoryCredentials{})
	require.NoError(t, err)
	evidence, err := verifyStoragePreservation(ctx, storageLiveAPI{kubeconfig}, before, after)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(evidence.volumes), 3, "database data/WAL and installation blob storage must be observed")
	again, err := verifyStoragePreservation(ctx, storageLiveAPI{kubeconfig}, before, after)
	require.NoError(t, err)
	require.Equal(t, evidence.digest, again.digest, "independent reads bind the same current PVC/PV identities")
	require.Equal(t, evidence.before, again.before)
	require.Equal(t, evidence.after, again.after)
	require.Equal(t, evidence.volumes, again.volumes)
	require.True(t, freshPreservationObservation(evidence.observed, time.Now()))
	require.True(t, freshPreservationObservation(again.observed, time.Now()))
	t.Logf("read-only installed storage proof: volumes=%d digest=%s", len(evidence.volumes), evidence.digest)
}
