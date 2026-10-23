package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/installation"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// Status must never reach these effect ports. An unexpected call fails the test
// immediately rather than returning fabricated preparation or apply evidence.
type unusedSelectionExecutor struct {
	pipelines.Executor
	pipelines.ReceiptAcknowledger
}
type unusedSelectionFiles struct {
	pipelinesteps.LibraryReceiptOpener
	pipelinesteps.LibraryArtifactLifecycle
}
type unusedSelectionTokens struct{ pipelinesteps.TokenMinter }
type selectionStatusAPI struct{ t *testing.T }

func (a selectionStatusAPI) Do(_ context.Context, method, path, _ string, _ []byte) ([]byte, error) {
	a.t.Helper()
	require.Equal(a.t, http.MethodGet, method)
	var body map[string]any
	switch path {
	case "api/v1/namespaces/memql/configmaps/installation":
		body = map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "installation", "namespace": "memql", "uid": "config-id", "resourceVersion": "1"}, "data": map[string]any{"installation.json": `{"formatVersion":1,"installationId":"installation","application":{"namespace":"argocd","name":"memql"}}`}}
	case "apis/argoproj.io/v1alpha1/namespaces/argocd/applications/memql":
		body = map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": map[string]any{"name": "memql", "namespace": "argocd", "uid": "application-id", "resourceVersion": "1"}, "spec": map[string]any{"source": map[string]any{"targetRevision": strings.Repeat("a", 40)}}}
	default:
		a.t.Fatalf("status attempted an unexpected cluster operation: %s", path)
	}
	return json.Marshal(body)
}
func nativeSelectionStatusHost(t *testing.T, db *sql.DB) *installation.Preparer {
	t.Helper()
	p, err := installation.NewPreparer(installation.PreparationDependencies{API: selectionStatusAPI{t}, Database: func() *sql.DB { return db }, Executor: unusedSelectionExecutor{}, Files: unusedSelectionFiles{}, Tokens: unusedSelectionTokens{}, Catalog: func(context.Context, []byte, string) (installation.Catalog, error) {
		t.Fatal("status read the publication catalog")
		return nil, errors.New("unexpected catalog read")
	}, Namespace: "memql", ConfigurationName: "installation", EngineRevision: strings.Repeat("f", 40)})
	require.NoError(t, err)
	return p
}
func TestInstallationStatusKeepsNativeRecoveryAvailableDuringRepositoryOutage(t *testing.T) {
	s, hub := overlayFixture(t)
	i := s.integration
	i.installation.Store(nativeSelectionStatusHost(t, s.db))
	configuration, err := json.Marshal(s.configuration)
	require.NoError(t, err)
	choice := installationChoice{s.request.CandidateID, s.request.CatalogDigest, strings.Repeat("9", 40), "Reviewed release"}
	inventory, err := json.Marshal(installationChoices{1, "installation", []installationChoice{choice}})
	require.NoError(t, err)
	i.resolver.systemVariable = func(_ context.Context, name string) (string, error) {
		if name == InstallationChoicesVariable {
			return string(inventory), nil
		}
		return string(configuration), nil
	}
	_, err = s.build(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = s.reserve(ownerCtx(), nil)
	require.NoError(t, err)
	for _, outage := range []string{"token unavailable", "base unavailable"} {
		t.Run(outage, func(t *testing.T) {
			hub.revoked = outage == "token unavailable"
			hub.base = ""
			nodes, err := i.handleInstallationStatus(ownerCtx(), nil, 0)
			require.NoError(t, err)
			require.Len(t, nodes, 1)
			var payload map[string]any
			require.NoError(t, json.Unmarshal(nodes[0].Payload, &payload))
			require.Equal(t, "installation", payload["installationId"])
			require.Equal(t, strings.Repeat("a", 40), payload["currentRevision"])
			require.NotEmpty(t, payload["proposalWarning"])
			require.NotContains(t, payload, "proposalBaseRevision")
			require.NotContains(t, payload, "choicesWarning")
			require.Len(t, payload["choices"], 1)
			require.Len(t, payload["proposals"], 1, "saved proposals remain readable during an outage")
			// Projecting a saved native reservation preserves its exact identity;
			// optional discovery never substitutes a new request or drops it.
			native := installation.SelectionStatus{InstallationID: "installation", CurrentRevision: strings.Repeat("a", 40), Preparation: &installation.SelectionPreparation{PrepareSelection: installation.PrepareSelection{RequestID: "durable-original-request"}, PreparationID: "saved", CanResume: true}}
			view := i.installationStatusView(ownerCtx(), native)
			require.Equal(t, native.Preparation, view["preparation"])
			native.Preparation = nil
			native.Active = &installation.SelectionResult{PlanID: "saved-plan", State: "prepared", CanManage: true}
			view = i.installationStatusView(ownerCtx(), native)
			require.Equal(t, native.Active, view["active"])
		})
	}
	// Invalid or unreadable optional inventory cannot block recovery either.
	reads := 0
	i.resolver.systemVariable = func(context.Context, string) (string, error) {
		reads++
		return "", errors.New("optional configuration unavailable")
	}
	nodes, err := i.handleInstallationStatus(ownerCtx(), nil, 0)
	require.NoError(t, err)
	require.Contains(t, string(nodes[0].Payload), "choicesWarning")
	require.Contains(t, string(nodes[0].Payload), "proposalWarning")
	reads = 0
	p, err := i.receivingSelection(ownerCtx(), "installation")
	require.NoError(t, err)
	require.NotNil(t, p)
	_, err = i.receivingSelection(ownerCtx(), "different-installation")
	require.ErrorContains(t, err, "another receiving installation")
	_, err = i.receivingSelection(actorContext(auth.RoleReader), "installation")
	require.Error(t, err)
	type handler func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	for name, call := range map[string]handler{"apply": i.handleInstallationApply, "observe": i.handleInstallationObserve, "cancel": i.handleInstallationCancel, "resume": i.handleInstallationResume} {
		t.Run(name+" reaches native validation", func(t *testing.T) {
			_, err := call(ownerCtx(), map[string]any{"installationId": "installation", "planId": strings.Repeat("0", 64), "preparationId": strings.Repeat("0", 64)}, 0)
			require.Error(t, err, "a missing native plan must still be refused")
			require.NotContains(t, err.Error(), "optional configuration unavailable")
			require.Zero(t, reads, "saved plan controls must not consult optional inventory or GitHub")
		})
	}
}
