package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/core/id"
)

type lostRevisionReply struct {
	API
	writes atomic.Int32
}

func (api *lostRevisionReply) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	response, err := api.API.Do(ctx, method, path, contentType, body)
	if method == http.MethodPatch && err == nil && api.writes.Add(1) == 1 {
		return nil, io.ErrUnexpectedEOF // the real API committed; caller lost the reply
	}
	return response, err
}

type localProxyTransport struct{ http.RoundTripper }

func (transport localProxyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	// kubectl proxy holds the explicit local kubeconfig identity. Do not replace
	// it with ClusterAPIWith's empty bearer in this optional test-only seam.
	request.Header.Del("Authorization")
	return transport.RoundTripper.RoundTrip(request)
}

// TestRevisionThroughInstalledArgo requires an explicitly created, disposable
// Application and an AppProject limited to one namespace's ConfigMaps. It never
// creates or selects an Application by discovery, and refuses real mesh targets.
// Baseline source: test/fixtures/argocd-revision/revision.yaml at an exact commit
// whose data.revision is baseline. Target commit must render candidate instead.
func TestRevisionThroughInstalledArgo(t *testing.T) {
	base := os.Getenv("MEMQL_ARGOCD_TEST_PROXY")
	if base == "" {
		t.Skip("set MEMQL_ARGOCD_TEST_PROXY for the explicitly isolated ConfigMap controller rehearsal")
	}
	u, err := url.Parse(base)
	require.NoError(t, err)
	require.Equal(t, "http", u.Scheme)
	require.Equal(t, "127.0.0.1", u.Hostname())
	require.NotEmpty(t, u.Port())
	require.Empty(t, u.Path)
	require.Empty(t, u.RawQuery)
	require.Nil(t, u.User)
	require.Equal(t, "isolated-configmap-only", os.Getenv("MEMQL_ARGOCD_TEST_CONFIRM"))
	name, uid := os.Getenv("MEMQL_ARGOCD_TEST_APPLICATION"), os.Getenv("MEMQL_ARGOCD_TEST_UID")
	require.True(t, strings.HasPrefix(name, "memql-delivery-argocd-"))
	target := Target{Namespace: "argocd", Name: name, UID: uid}
	revision := os.Getenv("MEMQL_ARGOCD_TEST_REVISION")
	require.True(t, commitSHA.MatchString(revision))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	api := deploycontrol.NewClusterAPIWith(base, "", &http.Client{
		Timeout: 30 * time.Second, Transport: localProxyTransport{transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
	c, err := New(api)
	require.NoError(t, err)
	snapshot, err := c.Read(ctx, target)
	require.NoError(t, err)
	spec := object(snapshot.object, "spec")
	source := object(spec, "source")
	require.Equal(t, "https://github.com/znasllc-io/memql.git", stringAt(source, "repoURL"))
	require.Equal(t, "test/fixtures/argocd-revision", stringAt(source, "path"))
	require.True(t, commitSHA.MatchString(stringAt(source, "targetRevision")))
	require.NotEqual(t, revision, stringAt(source, "targetRevision"))
	require.Equal(t, name, stringAt(spec, "project"))
	require.True(t, equalJSON(object(spec, "destination"), map[string]any{"server": "https://kubernetes.default.svc", "namespace": name}))
	require.Nil(t, object(spec, "syncPolicy")["automated"], "the fixture must not auto-sync")
	require.Equal(t, "argocd-revision", stringAt(object(object(snapshot.object, "metadata"), "labels"), "memql.io/delivery-proof"))

	projectBody, err := api.Do(ctx, http.MethodGet, "apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects/"+name, "", nil)
	require.NoError(t, err)
	project, err := decodeObject(projectBody)
	require.NoError(t, err)
	projectSpec := object(project, "spec")
	require.True(t, equalJSON(projectSpec["sourceRepos"], []any{stringAt(source, "repoURL")}))
	require.True(t, equalJSON(projectSpec["destinations"], []any{map[string]any{"server": "https://kubernetes.default.svc", "namespace": name}}))
	require.True(t, equalJSON(projectSpec["namespaceResourceWhitelist"], []any{map[string]any{"group": "", "kind": "ConfigMap"}}))
	require.True(t, equalJSON(projectSpec["clusterResourceBlacklist"], []any{map[string]any{"group": "*", "kind": "*"}}))
	require.True(t, emptyArray(projectSpec["clusterResourceWhitelist"]))

	configMapPath := "api/v1/namespaces/" + name + "/configmaps/revision-proof"
	readRevision := func() string {
		body, err := api.Do(ctx, http.MethodGet, configMapPath, "", nil)
		require.NoError(t, err)
		cm, err := decodeObject(body)
		require.NoError(t, err)
		require.Equal(t, name, stringAt(object(cm, "metadata"), "namespace"))
		require.Equal(t, "argocd-revision", stringAt(object(object(cm, "metadata"), "labels"), "memql.io/delivery-proof"))
		return stringAt(object(cm, "data"), "revision")
	}
	require.Equal(t, "baseline", readRevision())
	intent, err := PlanRevision(snapshot, "proof-"+id.NewShortId(), revision, false)
	require.NoError(t, err)
	body, err := json.Marshal(intent)
	require.NoError(t, err)
	// Only the protocol value crosses the host boundary, not the first client's
	// maps or process state. This is not a substitute for the production journal.
	recovered, err := DecodeIntent(body)
	require.NoError(t, err)
	fault := &lostRevisionReply{API: api}
	first, err := New(fault)
	require.NoError(t, err)
	_, err = first.Apply(ctx, intent)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, int32(1), fault.writes.Load())
	second, err := New(api)
	require.NoError(t, err)
	awaitControllerRevision(t, ctx, second, recovered)
	require.Equal(t, "candidate", readRevision())
	// An explicit same-intent reconciliation must not issue a second patch.
	probe := &lostRevisionReply{API: api}
	third, err := New(probe)
	require.NoError(t, err)
	facts, err := third.Apply(ctx, recovered)
	require.NoError(t, err)
	require.True(t, facts.OperationSucceeded && facts.RevisionObserved && facts.Synced)
	require.Zero(t, probe.writes.Load())

	// Rollback is a NEW explicit intent whose target is the observed immutable
	// starting commit. It never clears or replays the failed/old request.
	current, err := second.Read(ctx, target)
	require.NoError(t, err)
	rollback, err := PlanRevision(current, "proof-rollback-"+id.NewShortId(), stringAt(source, "targetRevision"), false)
	require.NoError(t, err)
	_, err = second.Apply(ctx, rollback)
	require.NoError(t, err)
	fourth, err := New(api)
	require.NoError(t, err)
	awaitControllerRevision(t, ctx, fourth, rollback)
	require.Equal(t, "baseline", readRevision())
	_, err = third.Apply(ctx, recovered)
	require.ErrorIs(t, err, ErrChanged, "an old update must not undo a completed rollback")
	require.Zero(t, probe.writes.Load())
}

func awaitControllerRevision(t *testing.T, ctx context.Context, client *Client, intent Intent) {
	t.Helper()
	last := Facts{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		facts, err := client.Observe(ctx, intent)
		require.NoError(t, err)
		if facts != last {
			t.Logf("controller facts for %s: %+v", intent.Revision, facts)
			last = facts
		}
		if facts.OperationPhase == "Failed" || facts.OperationPhase == "Error" {
			t.Fatalf("owned ConfigMap revision failed: %+v", facts)
		}
		if facts.OperationSucceeded && facts.RevisionObserved && facts.Synced && facts.Healthy {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(fmt.Errorf("Argo revision observation did not complete: %w; last facts=%+v", ctx.Err(), last))
		case <-ticker.C:
		}
	}
}
