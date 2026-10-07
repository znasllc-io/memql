package installation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type preparationCatalogFixture map[string]pipelines.VerifiedPublishedRelease

func (c preparationCatalogFixture) Get(_ context.Context, _ string, candidate, digest string) (pipelines.VerifiedPublishedRelease, error) {
	proof, ok := c[digest]
	if !ok {
		return pipelines.VerifiedPublishedRelease{}, errors.New("unknown publication")
	}
	release, err := proof.Release()
	if err != nil || release.CandidateID != candidate {
		return pipelines.VerifiedPublishedRelease{}, errors.New("wrong publication")
	}
	return proof, nil
}

type preparationTokenFixture struct{}

func (preparationTokenFixture) CloneToken(context.Context, int64, string, string) (string, error) {
	return "fixture-clone-token", nil
}

var _ pipelinesteps.TokenMinter = preparationTokenFixture{}

func TestPreparationWorkflowBindsInstalledRecipesAndEngine(t *testing.T) {
	h := &preparationHost{}
	require.NoError(t, h.bindWorkflow(strings.Repeat("a", 40)))
	require.Equal(t, installationPrepareWorkflow, h.definition.Name)
	first := h.digest
	other := &preparationHost{}
	require.NoError(t, other.bindWorkflow(strings.Repeat("b", 40)))
	require.NotEqual(t, first, other.digest)
	require.Error(t, h.bindWorkflow("main"))
}

func TestPreparationWorkflowFreshHostRecoversSameStartedCapture(t *testing.T) {
	db, peerDB := journalDB(t)
	receiver := newReceiverFixture(t)
	artifacts := newAdmissionFixture(t)
	before, err := artifacts.old.Release()
	require.NoError(t, err)
	after, err := artifacts.next.Release()
	require.NoError(t, err)
	receiver.config.Rollback = receiverSelection{CandidateID: before.CandidateID, CatalogDigest: artifacts.old.Digest()}
	receiver.saveConfig()
	publications := preparationCatalogFixture{artifacts.old.Digest(): artifacts.old, artifacts.next.Digest(): artifacts.next}
	factory := func(context.Context, []byte, string) (Catalog, error) { return publications, nil }
	executor := &captureExecutorFixture{executeError: errors.New("lost dispatch response")}
	files := &captureFilesFixture{}
	first := &preparationHost{api: receiver, journal: preparationConnection(db), executor: executor, files: files, tokens: preparationTokenFixture{}, catalog: factory, namespace: "memql", configurationName: "receiver"}
	require.NoError(t, first.bindWorkflow(strings.Repeat("a", 40)))
	request := preparationRequest{InstallationID: receiver.config.InstallationID, RequestID: "request", CandidateID: after.CandidateID, CatalogDigest: artifacts.next.Digest(), OverlayRevision: strings.Repeat("b", 40)}
	ctx := captureOperator(auth.RoleOwner, "operator")
	_, err = first.prepare(ctx, request)
	require.ErrorContains(t, err, "no confirmed outcome")
	require.Len(t, executor.requests, 1)
	saved, err := first.journal.getByRequest(ctx, request.InstallationID, request.RequestID)
	require.NoError(t, err)
	require.True(t, saved.Captures["rollback"].Started)
	require.False(t, saved.Captures["candidate"].Started)
	require.Equal(t, first.digest, saved.Scope.WorkflowDigest)
	fresh := &preparationHost{api: receiver, journal: preparationConnection(peerDB), executor: executor, files: files, tokens: preparationTokenFixture{}, catalog: factory, namespace: "memql", configurationName: "receiver"}
	require.NoError(t, fresh.bindWorkflow(strings.Repeat("a", 40)))
	_, err = fresh.prepare(ctx, request)
	require.ErrorContains(t, err, "no confirmed outcome")
	require.Len(t, executor.requests, 2)
	require.False(t, executor.requests[0].RecoverOnly)
	require.True(t, executor.requests[1].RecoverOnly)
	executor.requests[0].RecoverOnly = true
	require.Equal(t, executor.requests[0], executor.requests[1], "fresh replica must not invent another source attempt or start time")
	again, err := fresh.journal.getByRequest(ctx, request.InstallationID, request.RequestID)
	require.NoError(t, err)
	require.Equal(t, saved, again)
	request.OverlayRevision = strings.Repeat("c", 40)
	_, err = fresh.prepare(ctx, request)
	require.ErrorContains(t, err, "changed inputs")
	require.Len(t, executor.requests, 2)
	request.OverlayRevision = strings.Repeat("b", 40)
	resourceMap(receiver.objects["api/v1/namespaces/memql/secrets/catalog"], "metadata")["resourceVersion"] = "rotated"
	_, err = fresh.prepare(ctx, request)
	require.ErrorContains(t, err, "configuration changed")
	require.Len(t, executor.requests, 2)
}

func TestPreparationRecipeCannotAssertOrIgnoreMissingEvidence(t *testing.T) {
	for name, body := range map[string]string{
		"assertion":        `return {prepared: true, evidence: "claimed"}`,
		"missing evidence": `builtin installationPromotePreparation()`,
		"ignored failure":  `builtin installationPromotePreparation() on error continue`,
		"unbound child":    `automation unboundPreparationChild()`,
	} {
		t.Run(name, func(t *testing.T) {
			host := &preparationHost{}
			require.NoError(t, host.bindWorkflow(strings.Repeat("a", 40)))
			definition, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation alternativePrepareWorkflow {\n"+body+"\n}", "preparation-test.memql")
			require.NoError(t, err)
			definition.Trusted = true // test-only installed provenance
			host.definition = definition
			scope := &preparationWorkflowScope{host: host, operator: "operator"}
			result, err := scope.run(captureOperator(auth.RoleOwner, "operator"))
			require.Error(t, err)
			require.Empty(t, result.ID)
		})
	}
}

func TestPreparationRechecksAndRefreshesResourceObservation(t *testing.T) {
	f := newAdmissionFixture(t)
	api := resourceAPIFixture()
	initial, err := verifyImagesAndDiff(context.Background(), api, f.next, f.before, f.after, f.config.Platform, f.config.Bindings)
	require.NoError(t, err)
	time.Sleep(time.Millisecond)
	observed := time.Now().UTC()
	scope := &preparationWorkflowScope{
		host:      &preparationHost{api: api},
		operator:  "operator",
		candidate: f.next,
		rollback:  f.old,
		configuration: &receiverSnapshot{
			configuration: receiverConfiguration{Platform: f.config.Platform},
			digest:        f.config.ConfigurationDigest,
			bindings:      f.config.Bindings,
		},
		renders:   map[string]argocd.RenderedRevision{"rollback": f.before, "candidate": f.after},
		resources: initial,
		artifacts: &artifactWorkflowScope{result: &artifactEvidence{observed: observed, expires: observed.Add(time.Minute)}},
	}

	_, err = scope.operations()["installationRecheckResources"](captureOperator(auth.RoleOwner, "operator"), nil)
	require.NoError(t, err)
	require.Equal(t, initial.digest, scope.resources.digest)
	require.Equal(t, initial.publication, scope.resources.publication)
	require.Equal(t, initial.before, scope.resources.before)
	require.Equal(t, initial.after, scope.resources.after)
	require.True(t, scope.resources.observed.After(initial.observed), "recheck must replace the older resource observation")
	require.True(t, scope.resources.observed.After(observed), "resource evidence must be refreshed after artifact verification")
}

// A recipe may fan out the same admitted operation. The native scope must not
// race its start time or let those calls create distinct durable reservations.
func TestPreparationPortsConcurrentReservationsKeepOneScope(t *testing.T) {
	db, _ := journalDB(t)
	receiver := newReceiverFixture(t)
	images := newAdmissionFixture(t)
	before, err := images.old.Release()
	require.NoError(t, err)
	after, err := images.next.Release()
	require.NoError(t, err)
	receiver.config.Rollback = receiverSelection{CandidateID: before.CandidateID, CatalogDigest: images.old.Digest()}
	receiver.saveConfig()
	publications := preparationCatalogFixture{images.old.Digest(): images.old, images.next.Digest(): images.next}
	factory := func(context.Context, []byte, string) (Catalog, error) { return publications, nil }
	host := &preparationHost{api: receiver, journal: preparationConnection(db), executor: &captureExecutorFixture{}, files: &captureFilesFixture{}, tokens: preparationTokenFixture{}, catalog: factory, namespace: "memql", configurationName: "receiver"}
	require.NoError(t, host.bindWorkflow(strings.Repeat("a", 40)))
	ctx := captureOperator(auth.RoleOwner, "operator")
	config, err := readReceiver(ctx, receiver, "memql", "receiver", factory)
	require.NoError(t, err)
	request := preparationRequest{InstallationID: receiver.config.InstallationID, RequestID: "concurrent-reserve", CandidateID: after.CandidateID, CatalogDigest: images.next.Digest(), OverlayRevision: strings.Repeat("b", 40)}
	scope := &preparationWorkflowScope{host: host, request: request, operator: "operator", configuration: config}
	_, err = scope.resolve(ctx, nil)
	require.NoError(t, err)
	operation := scope.operations()["installationReservePreparation"]
	failures := make([]error, 8)
	var group sync.WaitGroup
	for n := range failures {
		group.Add(1)
		go func() { defer group.Done(); _, failures[n] = operation(ctx, nil) }()
	}
	group.Wait()
	for _, err := range failures {
		require.NoError(t, err)
	}
	saved, err := host.journal.getByRequest(ctx, request.InstallationID, request.RequestID)
	require.NoError(t, err)
	require.Equal(t, saved, scope.record)
}
