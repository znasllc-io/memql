package installation

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

func artifactRecipeFixture(t *testing.T, body string) *artifactWorkflow {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation alternateArtifactRecipe {\n"+body+"\n}", "artifact-test.memql")
	require.NoError(t, err)
	// Test-only installed-source provenance, never a DSL argument.
	a.Trusted = true
	w, err := newArtifactWorkflow(a, strings.Repeat("d", 40))
	require.NoError(t, err)
	return w
}

func TestArtifactWorkflowInstalledRecipeAndFreshReceiver(t *testing.T) {
	f := newAdmissionFixture(t)
	ctx := captureOperator(auth.RoleOwner, "operator")
	w, err := loadArtifactWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	first, err := w.run(ctx, f.scope(t), "operator")
	require.NoError(t, err)
	require.Equal(t, w.digest, first.workflow)
	require.Equal(t, "operator", first.operator)
	// Reload and execute in another receiver, without any original observation
	// map. Shared configuration names work; only new registry reads prove it.
	other, err := loadArtifactWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	second, err := other.run(ctx, f.scope(t), "operator")
	require.NoError(t, err)
	require.Equal(t, first.workflow, second.workflow)
	require.Equal(t, first.scope, second.scope)
	require.NotEqual(t, first.digest, second.digest, "fresh observation times are part of the evidence")
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 2, f.reads[f.oldLayer])
	require.Equal(t, 2, f.reads[f.newLayer])
}

func TestArtifactWorkflowSecondRecipeComposesSameNativeOperations(t *testing.T) {
	f := newAdmissionFixture(t)
	w := artifactRecipeFixture(t, `requirements := builtin installationArtifactRequirements()
for item in requirements.where(entry => entry.platform == "linux/arm64") parallel(2) {
  if item.platform == "linux/arm64" {
    builtin installationArtifactCheck(key: item.key)
  }
}
builtin installationArtifactsComplete()`)
	result, err := w.run(captureOperator(auth.RoleDeveloper, "operator"), f.scope(t), "operator")
	require.NoError(t, err)
	require.NotEmpty(t, result.digest)
	installed, err := loadArtifactWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	require.NotEqual(t, installed.digest, w.digest, "different composition must not reuse a workflow binding")
}

func TestArtifactWorkflowCannotManufactureCompletionOrInvokeUnboundWork(t *testing.T) {
	for name, body := range map[string]string{
		"serialized assertion": `return { complete: true, proof: "claimed" }`,
		"no reads":             `builtin installationArtifactsComplete()`,
		"ignore incomplete":    `builtin installationArtifactsComplete() on error continue`,
		"missing one": `requirements := builtin installationArtifactRequirements()
builtin installationArtifactCheck(key: requirements.first().key)
builtin installationArtifactsComplete()`,
		"no completion": `requirements := builtin installationArtifactRequirements()
for item in requirements { builtin installationArtifactCheck(key: item.key) }`,
		"unbound operation": `builtin installationArtifactRequirements()
builtin externalUnboundEffect()`,
		"unbound child": `builtin installationArtifactRequirements()
automation unboundArtifactChild()`,
		"work after completion": `requirements := builtin installationArtifactRequirements()
for item in requirements { builtin installationArtifactCheck(key: item.key) }
builtin installationArtifactsComplete()
builtin installationArtifactRequirements()`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t)
			w := artifactRecipeFixture(t, body)
			result, err := w.run(captureOperator(auth.RoleOwner, "operator"), f.scope(t), "operator")
			require.Error(t, err)
			require.Empty(t, result.digest)
			if strings.HasPrefix(name, "unbound") {
				f.mu.Lock()
				defer f.mu.Unlock()
				require.Empty(t, f.reads, "preflight must refuse before any registry access")
			}
		})
	}
}

func TestArtifactWorkflowFailedReadCannotBeIgnoredAndRecoveryRereads(t *testing.T) {
	f := newAdmissionFixture(t)
	w := artifactRecipeFixture(t, `requirements := builtin installationArtifactRequirements()
for item in requirements {
  builtin installationArtifactCheck(key: item.key) on error continue
}
builtin installationArtifactsComplete()`)
	f.mu.Lock()
	bytes := f.bodies[f.oldLayer]
	delete(f.bodies, f.oldLayer)
	f.mu.Unlock()
	ctx := captureOperator(auth.RoleAdmin, "operator")
	result, err := w.run(ctx, f.scope(t), "operator")
	require.Error(t, err)
	require.Empty(t, result.digest)
	f.mu.Lock()
	f.bodies[f.oldLayer] = bytes
	f.mu.Unlock()
	result, err = w.run(ctx, f.scope(t), "operator")
	require.NoError(t, err)
	require.NotEmpty(t, result.digest)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 1, f.reads[f.oldLayer])
	require.Equal(t, 2, f.reads[f.newLayer], "a recovered recipe must not inherit the first run's successful read")
}

func TestArtifactWorkflowRetainsNativeAdmissionAndRefusesProofArguments(t *testing.T) {
	f := newAdmissionFixture(t)
	w, err := loadArtifactWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	for name, ctx := range map[string]context.Context{
		"external":         operator(auth.RoleOwner, "operator"),
		"no actor":         auth.ContextWithInternalOrigin(context.Background()),
		"changed operator": captureOperator(auth.RoleOwner, "other"),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := w.run(ctx, f.scope(t), "operator")
			require.Error(t, err)
			require.Empty(t, result.digest)
		})
	}
	ctx, cancel := context.WithCancel(captureOperator(auth.RoleOwner, "operator"))
	cancel()
	_, err = w.run(ctx, f.scope(t), "operator")
	require.Error(t, err)
	s := &artifactWorkflowScope{admission: f.scope(t), operator: "operator", observations: map[string]artifactObservation{}}
	for _, op := range s.operations() {
		_, err := op(captureOperator(auth.RoleOwner, "other"), nil)
		require.Error(t, err, "each callback must independently enforce its operator")
	}
	for name, op := range s.operations() {
		_, err := op(captureOperator(auth.RoleOwner, "operator"), map[string]any{"proof": true})
		require.Error(t, err, name)
	}
	for _, cap := range workflowhost.ScopedCapabilities(s.operations()) {
		_, err := cap.Handler(captureOperator(auth.RoleOwner, "operator"), nil, 0)
		require.Error(t, err, "even an internal direct call cannot create scope")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Empty(t, f.reads)
}

func TestArtifactWorkflowOwnsDefinitionAndBindsNativeRevision(t *testing.T) {
	a, err := workflowhost.Load(installationArtifactWorkflow)
	require.NoError(t, err)
	owned, err := automations.NewLoader(automations.LoaderOptions{}).Snapshot(a)
	require.NoError(t, err)
	w, err := newArtifactWorkflow(owned, strings.Repeat("d", 40))
	require.NoError(t, err)
	owned.Steps = nil
	f := newAdmissionFixture(t)
	_, err = w.run(captureOperator(auth.RoleOwner, "operator"), f.scope(t), "operator")
	require.NoError(t, err, "changing a loader's definition cannot mutate the executing recipe")
	other, err := newArtifactWorkflow(a, strings.Repeat("e", 40))
	require.NoError(t, err)
	require.NotEqual(t, w.digest, other.digest)
	owned.Trusted = false
	_, err = newArtifactWorkflow(owned, strings.Repeat("d", 40))
	require.Error(t, err)
	_, err = newArtifactWorkflow(a, "unbound")
	require.Error(t, err)
}
