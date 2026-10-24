package installation

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

func TestPreparationSourceGraphPersistsDependenciesAcrossReplicas(t *testing.T) {
	db, peerDB := journalDB(t)
	first, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	root, rootArchive, depArchive := graphCaptureFixture(t)
	root.CloneInstallationID = 41 // Private product root; dependencies remain public.
	original := scope.Captures["candidate"]
	root.RunID, root.WorkRunID, root.StepKey, root.RunStartedAt = original.RunID, original.WorkRunID, original.StepKey, original.RunStartedAt
	scope.Captures["candidate"] = root
	rollback := scope.Captures["rollback"]
	rollback.CloneInstallationID = root.CloneInstallationID
	rollback.Fragment = &argocd.SourceFragmentSpec{Render: rollback.Render, AllowedDependencies: root.Fragment.AllowedDependencies}
	scope.Captures["rollback"] = rollback
	bindPreparationIntent(t, &scope)
	immutable, scopeKey, err := scope.canonical()
	require.NoError(t, err)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	record, err := first.reserve(ctx, scope)
	require.NoError(t, err)
	capture, err := newSourceCapture(root)
	require.NoError(t, err)
	rootExecutor, rootFiles := capturePorts(capture, rootArchive)
	files := graphCaptureFiles{root.StepKey: rootFiles}
	_, err = first.capture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", rootExecutor, "")
	require.NoError(t, err)
	requirements, err := peer.sourceRequirements(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", files)
	require.NoError(t, err)
	require.Len(t, requirements, 1)
	record, err = first.get(ctx, scope.InstallationID, record.ID)
	require.NoError(t, err)
	body, key, err := record.Scope.canonical()
	require.NoError(t, err)
	require.Equal(t, immutable, body)
	require.Equal(t, scopeKey, key)
	require.Empty(t, record.Captures["candidate"].SourceDigest)
	require.NotEmpty(t, record.Captures["candidate"].FragmentDigest)
	rootProof, err := first.verifyCapture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", files)
	require.NoError(t, err)
	tokenCalls := 0
	tokens := graphRenderTokenFixture(func(_ context.Context, installation int64, owner, repository string) (string, error) {
		tokenCalls++
		require.Equal(t, int64(41), installation)
		require.Equal(t, root.Repository.Owner, owner)
		require.Equal(t, root.Repository.Name, repository)
		return "fixture-clone-token", nil
	})
	workflow := &preparationWorkflowScope{
		host: &preparationHost{tokens: tokens}, operator: scope.RequestedBy, record: record,
		sources: map[string]verifiedSourceCapture{"candidate": rootProof}, renders: map[string]argocd.RenderedRevision{},
	}
	render := workflow.operations()["installationRenderSource"]
	_, err = render(ctx, map[string]any{"role": "candidate"})
	require.ErrorContains(t, err, "fresh private source verification", "a verified root fragment is not a complete graph")
	require.Zero(t, tokenCalls, "partial graph proof must not mint a renderer credential")
	_, err = first.acknowledgeCapture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", rootExecutor)
	require.Error(t, err)
	_, err = first.verifySourceGraph(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", files)
	require.Error(t, err, "missing dependency cannot close a root")
	depKey := requirements[0].Key
	dep, err := preparationCaptureSpec(record, depKey)
	require.NoError(t, err)
	dependency, err := newSourceCapture(dep)
	require.NoError(t, err)
	depExecutor, depFiles := capturePorts(dependency, depArchive)
	files[dep.StepKey] = depFiles
	_, err = peer.capture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, depKey, depExecutor, "")
	require.NoError(t, err)
	require.Zero(t, depExecutor.requests[0].InstallationID, "the separate dependency collector must be anonymous")
	partial, err := first.verifyCapture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, depKey, files)
	require.NoError(t, err)
	require.Empty(t, partial.source.Digest())
	_, err = peer.acknowledgeCapture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, depKey, depExecutor)
	require.NoError(t, err)
	complete, err := peer.verifySourceGraph(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", files)
	require.NoError(t, err)
	require.NotEmpty(t, complete.source.Digest())
	require.Empty(t, complete.fragment.Digest())
	_, err = first.acknowledgeCapture(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", rootExecutor)
	require.NoError(t, err)
	again, err := first.verifySourceGraph(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", files)
	require.NoError(t, err)
	require.Equal(t, complete.source.Digest(), again.source.Digest())

	// This TLS protocol fixture observes the native handoff, not GitHub's
	// transport or Argo's child-process credential behavior. The app token
	// minter regression separately checks the one-repository contents:read mint.
	receiver := newReceiverFixture(t)
	snapshot, err := readReceiver(ctx, receiver, "memql", "receiver", receiver.factory(t))
	require.NoError(t, err)
	sha := sourceRevision(root.Render.Source)
	rpc := &preparationRendererFixture{manifests: map[string][]string{sha: {`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"qualified-source","namespace":"memql"}}`}}, reads: map[string]int{}}
	snapshot.rendererAddress = preparationRendererServer(t, receiver.certificate, rpc)
	workflow.configuration = snapshot
	workflow.sources["candidate"] = again
	for _, field := range []string{"credentials", "token", "installationId", "repository"} {
		_, err = render(ctx, map[string]any{"role": "candidate", field: "caller-supplied"})
		require.ErrorContains(t, err, "accepts no caller evidence", field)
	}
	_, err = render(captureOperator(auth.RoleOwner, "another-operator"), map[string]any{"role": "candidate"})
	require.ErrorContains(t, err, "outside its admitted scope")
	require.Zero(t, tokenCalls)
	_, err = render(ctx, map[string]any{"role": "candidate"})
	require.NoError(t, err)
	require.Equal(t, 1, tokenCalls)
	require.NotEmpty(t, workflow.renders["candidate"].Digest())
	rpc.mu.Lock()
	require.Equal(t, 1, rpc.reads[sha])
	rpc.mu.Unlock()
	cleanup := "memql-id:" + strings.Repeat("9", 64)
	retired, err := peer.beginRetirement(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	require.Len(t, retired.Retirement.Captures, 3)
	require.Contains(t, retired.Retirement.Captures, depKey)
	_, err = first.sourceRequirements(ctx, scope.InstallationID, record.ID, scope.WorkflowDigest, "candidate", files)
	require.Error(t, err, "retirement fences graph discovery as well as dispatch")
}

type graphRenderTokenFixture func(context.Context, int64, string, string) (string, error)

func (f graphRenderTokenFixture) CloneToken(ctx context.Context, installation int64, owner, repository string) (string, error) {
	return f(ctx, installation, owner, repository)
}
