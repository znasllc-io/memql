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

func workloadRecipeFixture(t *testing.T, body string) *workloadWorkflow {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation alternateWorkloadRecipe {\n"+body+"\n}", "workload-test.memql")
	require.NoError(t, err)
	a.Trusted = true
	w, err := newWorkloadWorkflow(a, strings.Repeat("d", 40))
	require.NoError(t, err)
	return w
}

func TestWorkloadWorkflowInstalledRecipeAndFreshHost(t *testing.T) {
	f := newWorkloadFixture(t, "Deployment", "Deployment")
	ctx := captureOperator(auth.RoleOwner, "operator")
	w, err := loadWorkloadWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	first, err := w.run(ctx, f.scope(t), "operator")
	require.NoError(t, err)
	require.Equal(t, w.digest, first.workflow)
	f.api.reads = nil
	other, err := loadWorkloadWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	second, err := other.run(ctx, f.scope(t), "operator")
	require.NoError(t, err)
	require.Equal(t, first.workflow, second.workflow)
	require.NotEqual(t, first.digest, second.digest)
	require.Contains(t, f.api.reads, "api/v1/namespaces/memql/pods?limit=256")
	alternate := workloadRecipeFixture(t, `requirements := builtin installationWorkloadRequirements()
for item in requirements.where(entry => entry.kind == "apps/Deployment") parallel(2) {
  if item.kind == "apps/Deployment" { builtin installationWorkloadCheck(key: item.key) }
}
builtin installationWorkloadsComplete()`)
	third, err := alternate.run(ctx, f.scope(t), "operator")
	require.NoError(t, err)
	require.NotEqual(t, first.workflow, third.workflow)
}

func TestWorkloadWorkflowCannotSkipNativeEvidence(t *testing.T) {
	for name, body := range map[string]string{
		"fabricated":     `return { complete: true, proof: "claimed" }`,
		"no reads":       `builtin installationWorkloadsComplete()`,
		"ignore failure": `builtin installationWorkloadsComplete() on error continue`,
		"one missing": `requirements := builtin installationWorkloadRequirements()
builtin installationWorkloadCheck(key: requirements.first().key)
builtin installationWorkloadsComplete()`,
		"no seal": `requirements := builtin installationWorkloadRequirements()
for item in requirements { builtin installationWorkloadCheck(key: item.key) }`,
		"duplicate": `requirements := builtin installationWorkloadRequirements()
for item in requirements { builtin installationWorkloadCheck(key: item.key) }
builtin installationWorkloadCheck(key: requirements.first().key)
builtin installationWorkloadsComplete()`,
		"unbound operation": `builtin unboundExternalEffect()`,
		"unbound child":     `automation unboundWorkloadChild()`,
		"after completion": `requirements := builtin installationWorkloadRequirements()
for item in requirements { builtin installationWorkloadCheck(key: item.key) }
builtin installationWorkloadsComplete()
builtin installationWorkloadRequirements()`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkloadFixture(t, "Deployment", "Deployment")
			scope := f.scope(t)
			f.api.reads = nil
			result, err := workloadRecipeFixture(t, body).run(captureOperator(auth.RoleOwner, "operator"), scope, "operator")
			require.Error(t, err)
			require.Empty(t, result.digest)
			if strings.HasPrefix(name, "unbound") {
				require.Empty(t, f.api.reads)
			}
		})
	}
}

func TestWorkloadWorkflowReadFailureRecoveryAndNativeAuthority(t *testing.T) {
	f := newWorkloadFixture(t, "Deployment", "Deployment")
	w := workloadRecipeFixture(t, `requirements := builtin installationWorkloadRequirements()
for item in requirements { builtin installationWorkloadCheck(key: item.key) on error continue }
builtin installationWorkloadsComplete()`)
	ctx := captureOperator(auth.RoleOwner, "operator")
	p := "api/v1/namespaces/memql/pods?limit=256"
	saved := f.api.response[p]
	delete(f.api.response, p)
	result, err := w.run(ctx, f.scope(t), "operator")
	require.Error(t, err)
	require.Empty(t, result.digest)
	f.api.response[p] = saved
	f.api.reads = nil
	result, err = w.run(ctx, f.scope(t), "operator")
	require.NoError(t, err)
	require.NotEmpty(t, result.digest)
	require.Contains(t, f.api.reads, p)
	for name, ctx := range map[string]context.Context{"external": operator(auth.RoleOwner, "operator"), "missing actor": auth.ContextWithInternalOrigin(context.Background()), "other actor": captureOperator(auth.RoleOwner, "other")} {
		t.Run(name, func(t *testing.T) {
			result, err := w.run(ctx, f.scope(t), "operator")
			require.Error(t, err)
			require.Empty(t, result.digest)
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = w.run(canceled, f.scope(t), "operator")
	require.Error(t, err)
	s := &workloadWorkflowScope{admission: f.scope(t), operator: "operator", observations: map[string]workloadObservation{}}
	for name, op := range s.operations() {
		_, err := op(captureOperator(auth.RoleOwner, "other"), nil)
		require.Error(t, err, name)
		_, err = op(ctx, map[string]any{"proof": true})
		require.Error(t, err, name)
	}
	for _, cap := range workflowhost.ScopedCapabilities(s.operations()) {
		_, err := cap.Handler(ctx, nil, 0)
		require.Error(t, err)
	}
}

func TestWorkloadWorkflowOwnsDefinitionAndEngineBinding(t *testing.T) {
	a, err := workflowhost.Load(installationWorkloadWorkflow)
	require.NoError(t, err)
	owned, err := automations.NewLoader(automations.LoaderOptions{}).Snapshot(a)
	require.NoError(t, err)
	w, err := newWorkloadWorkflow(owned, strings.Repeat("d", 40))
	require.NoError(t, err)
	owned.Steps = nil
	f := newWorkloadFixture(t, "Deployment")
	_, err = w.run(captureOperator(auth.RoleOwner, "operator"), f.scope(t), "operator")
	require.NoError(t, err)
	other, err := newWorkloadWorkflow(a, strings.Repeat("e", 40))
	require.NoError(t, err)
	require.NotEqual(t, w.digest, other.digest)
	owned.Trusted = false
	_, err = newWorkloadWorkflow(owned, strings.Repeat("d", 40))
	require.Error(t, err)
	_, err = newWorkloadWorkflow(a, "unbound")
	require.Error(t, err)
}
