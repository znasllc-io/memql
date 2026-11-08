package argocd

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This opt-in transport permits only reads of the explicitly named Application.
// The operator's connection qualifies Kubernetes history, not installer RBAC.
type installedHistoryAPI struct {
	kubeconfig string
	target     Target
}

func (a installedHistoryAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	u, err := url.Parse(path)
	collection, _ := a.target.collectionPath()
	if err != nil || method != http.MethodGet || contentType != "" || len(body) != 0 || u.Path != collection || u.Query().Get("fieldSelector") != "metadata.name="+a.target.Name {
		return nil, errors.New("installed history test permits only the selected Application's reads")
	}
	return exec.CommandContext(ctx, "kubectl", "--kubeconfig="+a.kubeconfig,
		"get", "--raw=/"+path, "--request-timeout=10s").Output()
}

func TestApplicationHistoryThroughInstalledKubernetes(t *testing.T) {
	kubeconfig := os.Getenv("MEMQL_ARGOCD_HISTORY_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set MEMQL_ARGOCD_HISTORY_TEST_KUBECONFIG, APPLICATION and UID for the explicit read-only history check")
	}
	require.True(t, filepath.IsAbs(kubeconfig))
	target := Target{Namespace: "argocd", Name: os.Getenv("MEMQL_ARGOCD_HISTORY_TEST_APPLICATION"), UID: os.Getenv("MEMQL_ARGOCD_HISTORY_TEST_UID")}
	_, err := target.path()
	require.NoError(t, err)
	c, err := New(installedHistoryAPI{kubeconfig: kubeconfig, target: target})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	snapshot, err := c.ReadPlanning(ctx, target)
	require.NoError(t, err)
	intent, err := PlanRevision(snapshot, "read-only-history-check", stringAt(object(object(snapshot.object, "spec"), "source"), "targetRevision"), false)
	require.NoError(t, err)
	state, err := NewContinuityState(intent)
	require.NoError(t, err)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("no status-driven generation change observed within the read-only check")
		case <-ticker.C:
			var proof *ContinuityProof
			state, proof, err = c.CheckContinuity(ctx, intent, state)
			require.NoError(t, err)
			require.NotNil(t, proof)
			if state.Generation > intent.BeforeGeneration {
				t.Logf("authenticated status-only generation %d -> %d with retained history; no write attempted", intent.BeforeGeneration, state.Generation)
				return
			}
		}
	}
}
