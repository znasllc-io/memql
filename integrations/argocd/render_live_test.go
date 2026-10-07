package argocd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestRenderThroughInstalledRepoServer is a read-only protocol qualification.
// Its explicitly selected loopback port must be forwarded to the local
// repo-server using the operator's authenticated Kubernetes connection. Supply
// a public certificate pinned through that connection; never skip TLS checks.
// It does not change Application spec, install resources or grant approval.
func TestRenderThroughInstalledRepoServer(t *testing.T) {
	address := os.Getenv("MEMQL_ARGOCD_RENDER_TEST_ADDRESS")
	if address == "" {
		t.Skip("set MEMQL_ARGOCD_RENDER_TEST_ADDRESS for the explicit local repo-server render rehearsal")
	}
	host, port, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)
	require.NotEmpty(t, port)
	serverName := os.Getenv("MEMQL_ARGOCD_RENDER_TEST_SERVER_NAME")
	require.NotEmpty(t, serverName, "name from the explicitly pinned repo-server certificate")
	ca, err := os.ReadFile(os.Getenv("MEMQL_ARGOCD_RENDER_TEST_CA"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))
	probe, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: serverName,
	})
	require.NoError(t, err, "verify the explicitly pinned repo-server TLS identity before gRPC")
	require.NoError(t, probe.Close())
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: serverName,
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	body, err := os.ReadFile(os.Getenv("MEMQL_ARGOCD_RENDER_TEST_SPEC"))
	require.NoError(t, err)
	var spec RenderSpec
	require.NoError(t, json.Unmarshal(body, &spec))
	source, err := decodeObject(spec.Source)
	require.NoError(t, err)
	require.Equal(t, "https://github.com/znasllc-io/memql.git", stringAt(source, "repoURL"))
	require.Equal(t, "deploy/k8s/overlays/local", stringAt(source, "path"))
	require.True(t, commitSHA.MatchString(stringAt(source, "targetRevision")))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := RenderRevision(ctx, connection, spec, RepositoryCredentials{})
	require.NoError(t, err)
	require.Greater(t, len(result.Resources()), 40)
	deployments := map[string]bool{}
	for _, body := range result.Resources() {
		resource, err := decodeObject(body)
		require.NoError(t, err)
		if stringAt(resource, "kind") == "Deployment" && stringAt(object(resource, "metadata"), "namespace") == spec.Namespace {
			deployments[stringAt(object(resource, "metadata"), "name")] = true
		}
	}
	for _, name := range []string{"identity", "bff", "agent", "planner", "workbench", "mcp", "edge"} {
		require.True(t, deployments[name], "actual overlay must contain each engine node")
	}
	// Another client/process can reproduce this observation without the first
	// result's maps. No resource bodies or credentials are printed as evidence.
	second, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: serverName,
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })
	again, err := RenderRevision(ctx, second, spec, RepositoryCredentials{})
	require.NoError(t, err)
	require.Equal(t, result.Digest(), again.Digest())
	t.Logf("repo-server rendered commit=%s resources=%d digest=%s; independent clients agree", stringAt(source, "targetRevision"), len(result.Resources()), result.Digest())
}
