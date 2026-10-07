//go:build clustere2e

package clustere2e

// An external observer stays alive while the operator replaces the serving
// engine through Argo. This test never performs that rollout. The readiness
// file is a rendezvous, not an installation journal proof or approval.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

const installationContinuityTimeout = 20 * time.Minute

type installationContinuityAsset struct {
	URL, SHA256, ContentType string
	Bytes                    int
}

type installationContinuityReport struct {
	RunID, ExpectedCommit, StartedAt, FinishedAt string
	Before, After                                []memqlclient.ConnectionServerInfo
	Assets                                       []installationContinuityAsset
	ReconnectCycles                              []uint64
	Passed                                       bool
}

func continuityWriteReport(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(body, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func continuityAsset(ctx context.Context, client *http.Client, address string) (installationContinuityAsset, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return installationContinuityAsset{}, err
	}
	// The site's ordinary public asset path is intentional. Never forward the
	// engine's user bearer to a site or attach it to a report.
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return installationContinuityAsset{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return installationContinuityAsset{}, fmt.Errorf("asset returned HTTP %d", resp.StatusCode)
	}
	const maxBytes = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxBytes {
		return installationContinuityAsset{}, fmt.Errorf("asset body is empty, unreadable or exceeds 4 MiB")
	}
	digest := sha256.Sum256(body)
	return installationContinuityAsset{address, hex.EncodeToString(digest[:]), resp.Header.Get("Content-Type"), len(body)}, nil
}

func continuityReadRow(ctx context.Context, conn *memqlclient.Connection, row string) ([]memqlclient.Row, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := memqlclient.NewQueryClient(conn.Dispatcher()).MissingCapabilityByKindAndName(ctx,
		memqlclient.MissingCapabilityByKindAndNameArgs{Kind: "tool", Capability: "clustere2e-" + row})
	if err != nil {
		return nil, err
	}
	return result.Rows(), nil
}

func continuityAccess(ctx context.Context, conn *memqlclient.Connection) (*memqlclient.AccessSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	access, err := memqlclient.NewQueryClient(conn.Dispatcher()).GetMyAccess(ctx)
	if access != nil {
		access.RequestId = ""
	}
	return access, err
}

func continuityCreateRow(ctx context.Context, t *testing.T, conn *memqlclient.Connection, scope, row, actor string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	createProbeRow(ctx, t, conn, scope, row, "installation continuity probe", actor)
}

func TestInstallationServingContinuityAcrossRollout(t *testing.T) {
	expected := os.Getenv("MEMQL_E2E_INSTALLATION_EXPECT_COMMIT")
	if expected == "" {
		t.Skip("explicit installation rollout rehearsal not requested")
	}
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{40}$`), expected, "require the exact replacement engine source commit")
	ready := os.Getenv("MEMQL_E2E_INSTALLATION_READY_FILE")
	require.True(t, filepath.IsAbs(ready), "require a fresh absolute readiness path shared with the rollout operator")
	for _, path := range []string{ready, ready + ".result.json"} {
		_, err := os.Lstat(path)
		require.True(t, os.IsNotExist(err), "refuse an existing rendezvous/result file")
	}
	address := os.Getenv("MEMQL_E2E_ENDPOINT")
	u, err := url.Parse(address)
	require.NoError(t, err)
	require.True(t, u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "", "use the normal HTTPS gRPC front door")
	tok := os.Getenv("MEMQL_E2E_TOKEN")
	require.NotEmpty(t, tok, "an explicitly requested rehearsal cannot skip missing authentication")
	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims struct {
		Exp int64 `json:"exp"`
	}
	require.NoError(t, json.Unmarshal(payload, &claims))
	require.True(t, time.Unix(claims.Exp, 0).After(time.Now().Add(installationContinuityTimeout+time.Minute)), "use an existing session token valid for the full rehearsal; the server verifies its signature")
	var roots *x509.CertPool
	if path := os.Getenv("MEMQL_E2E_ROOT_CA_FILE"); path != "" {
		pem, err := os.ReadFile(path)
		require.NoError(t, err)
		roots = x509.NewCertPool()
		require.True(t, roots.AppendCertsFromPEM(pem))
	}
	var assetURLs []string
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("MEMQL_E2E_INSTALLATION_ASSET_URLS")), &assetURLs))
	require.NotEmpty(t, assetURLs, "include existing persisted site assets")
	require.LessOrEqual(t, len(assetURLs), 16)
	seenAssets := map[string]bool{}
	for _, address := range assetURLs {
		u, err := url.Parse(address)
		require.NoError(t, err)
		require.True(t, u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !seenAssets[address], "assets must be distinct public HTTPS URLs without credentials")
		seenAssets[address] = true
	}

	ctx, cancel := context.WithTimeout(t.Context(), installationContinuityTimeout)
	defer cancel()
	report := installationContinuityReport{RunID: newProbeScope(), ExpectedCommit: expected, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	defer func() {
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		report.Passed = !t.Failed() && report.Passed
		if err := continuityWriteReport(ready+".result.json", report); err != nil {
			t.Errorf("write continuity result: %v", err)
		}
	}()
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, address := range assetURLs {
		asset, err := continuityAsset(ctx, httpClient, address)
		require.NoError(t, err, "read existing site asset before rollout")
		report.Assets = append(report.Assets, asset)
	}

	conns := make([]*memqlclient.Connection, 0, connCount)
	defer func() {
		for _, conn := range conns {
			conn.Close()
		}
	}()
	cycles := make([]atomic.Uint64, connCount)
	events := make([]<-chan memqlclient.Event, connCount)
	beforeNodes := map[string]bool{}
	beforeCommits := map[string]bool{}
	accessBefore := make([]memqlclient.AccessSummary, connCount)
	for n := 0; n < connCount; n++ {
		dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
		conn, err := memqlclient.Connect(dialCtx, memqlclient.ConnectConfig{Endpoint: address, Token: tok, RootCAs: roots,
			Reconnect: &memqlclient.ReconnectConfig{InitialDelay: time.Second, MaxDelay: 5 * time.Second}})
		dialCancel()
		require.NoError(t, err)
		conns = append(conns, conn)
		info := conn.ServerInfo()
		require.NotEmpty(t, info.NodeID)
		require.NotEmpty(t, info.EngineCommit)
		require.NotEqual(t, expected[:12], info.EngineCommit, "replacement must not be installed before the baseline")
		report.Before = append(report.Before, info)
		beforeNodes[info.NodeID], beforeCommits[info.EngineCommit] = true, true
		conn.OnReconnect(func(cycle uint64) { cycles[n].Store(cycle) })
		_, events[n], err = conn.Subscriptions().SubscribeGraph(ctx, memqlclient.GraphSubscribeOptions{Concept: "v1:platform:missingCapability", Actions: []memqlclient.GraphAction{memqlclient.GraphActionCreated}})
		require.NoError(t, err)
		access, err := continuityAccess(ctx, conn)
		require.NoError(t, err)
		require.NotNil(t, access)
		require.NotEmpty(t, access.UserId)
		accessBefore[n] = *access
	}
	require.GreaterOrEqual(t, len(beforeNodes), 2, "baseline must reach two distinct BFF replicas through the front door")
	require.Len(t, beforeCommits, 1, "baseline must be a converged engine build")
	actor := userIDFromToken(t, tok)
	baselineID, afterID := id.NewShortId(), id.NewShortId()
	continuityCreateRow(ctx, t, conns[0], report.RunID, baselineID, actor)
	continuityExpectEvents(ctx, t, events, baselineID)
	baseline, err := continuityReadRow(ctx, conns[0], baselineID)
	require.NoError(t, err)
	require.Len(t, baseline, 1)
	require.Equal(t, baselineID, rowID(baseline[0]))
	baselineCycles := make([]uint64, connCount)
	for n := range conns {
		baselineCycles[n] = cycles[n].Load()
	}
	require.NoError(t, continuityWriteReport(ready, report))
	t.Logf("baseline ready: %s; awaiting Argo rollout to %.12s with the same session and subscriptions", ready, expected)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		complete := true
		for n, conn := range conns {
			if conn.Status() != memqlclient.StatusConnected || cycles[n].Load() <= baselineCycles[n] || conn.ServerInfo().EngineCommit != expected[:12] {
				complete = false
			}
		}
		if complete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("rollout did not reconnect every existing session to the exact replacement before the deadline")
		case <-ticker.C:
		}
	}
	afterNodes := map[string]bool{}
	for n, conn := range conns {
		info := conn.ServerInfo()
		require.False(t, beforeNodes[info.NodeID], "replacement must use a new serving process identity")
		afterNodes[info.NodeID] = true
		report.After = append(report.After, info)
		report.ReconnectCycles = append(report.ReconnectCycles, cycles[n].Load())
		access, err := continuityAccess(ctx, conn)
		require.NoError(t, err, "the original session must remain authenticated")
		require.NotNil(t, access)
		require.Equal(t, accessBefore[n], *access, "identity and access must survive replacement")
		rows, err := continuityReadRow(ctx, conn, baselineID)
		require.NoError(t, err)
		require.Equal(t, baseline, rows, "persisted row must survive and be readable from every reconnected session")
	}
	require.GreaterOrEqual(t, len(afterNodes), 2, "replacement must reach two distinct BFF replicas")
	continuityCreateRow(ctx, t, conns[0], report.RunID, afterID, actor)
	continuityExpectEvents(ctx, t, events, afterID)
	for _, before := range report.Assets {
		after, err := continuityAsset(ctx, httpClient, before.URL)
		require.NoError(t, err)
		require.Equal(t, before, after, "existing site asset must retain exact bytes and media type")
	}
	report.Passed = true
	t.Log("same-session authentication, persisted data, replayed subscriptions, cross-replica delivery and existing site bytes survived the rollout")
}

func continuityExpectEvents(ctx context.Context, t *testing.T, channels []<-chan memqlclient.Event, expected string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for n, events := range channels {
		found := false
		for !found {
			select {
			case event, open := <-events:
				require.True(t, open, "original subscription %d closed", n)
				found = planIDFor(event) == expected
			case <-ctx.Done():
				t.Fatalf("original subscription %d missed row %s", n, expected)
			}
		}
	}
}
