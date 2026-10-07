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

func retirementRecipeFixture(t *testing.T, body string) *retirementWorkflow {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation alternateRetirement {\n"+body+"\n}", "retirement-test.memql")
	require.NoError(t, err)
	a.Trusted = true // Test-only installed-source provenance, never an input port.
	w, err := newRetirementWorkflow(a, strings.Repeat("d", 40))
	require.NoError(t, err)
	return w
}

func TestRetirementWorkflowInstalledPassRecoversPendingPagesOnFreshHost(t *testing.T) {
	db, otherDB := journalDB(t)
	j, other := preparationConnection(db), preparationConnection(otherDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	_, _, err = j.beginCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate")
	require.NoError(t, err)
	w, err := loadRetirementWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	files, executor := retirementFiles(scope), &retirementStopFixture{}
	files.fail = "retire"
	_, err = w.run(ctx, j, scope.InstallationID, r.ID, executor, files)
	require.Error(t, err)
	r, err = other.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "retiring", r.State)
	require.Len(t, r.Retirement.Captures["candidate"].Page, 2)
	require.Empty(t, r.Retirement.Captures["candidate"].Page[0].ReceiptDigest)
	// A changed installed recipe cannot inherit the started cleanup's grants.
	changed, err := loadRetirementWorkflow(strings.Repeat("e", 40))
	require.NoError(t, err)
	calls := files.calls
	_, err = changed.run(ctx, other, scope.InstallationID, r.ID, executor, files)
	require.Error(t, err)
	require.Equal(t, calls, files.calls)
	replacement, err := loadRetirementWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	r, err = replacement.run(ctx, other, scope.InstallationID, r.ID, executor, files)
	require.NoError(t, err)
	require.Equal(t, "retiring", r.State, "one bounded pass cannot claim unseen pages are done")
	require.Len(t, r.Retirement.Captures["candidate"].Page, 1)
	r, err = replacement.run(ctx, other, scope.InstallationID, r.ID, executor, files)
	require.NoError(t, err)
	require.Equal(t, "cancelled", r.State)
	require.Len(t, files.retired, 4)
	require.Equal(t, 1, executor.calls, "durable producer stop must survive replacement")
	again, err := w.run(ctx, j, scope.InstallationID, r.ID, executor, files)
	require.NoError(t, err)
	require.Equal(t, r, again)
}

func TestRetirementWorkflowSecondRecipeComposesNativeOperations(t *testing.T) {
	db, _ := journalDB(t)
	j := preparationConnection(db)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleDeveloper, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	w := retirementRecipeFixture(t, `builtin installationRetirementBegin()
builtin installationRetirementStop()
for role in ["rollback", "candidate"] parallel(2) {
  builtin installationRetirementFence(role: role)
  builtin installationRetirementRelease(role: role)
  pending := builtin installationRetirementPage(role: role)
  for artifact in pending.items parallel(2) {
    builtin installationRetirementArtifact(role: role, intentId: artifact.intentId)
  }
  builtin installationRetirementPage(role: role)
}
observed := builtin installationRetirementObserve()
if observed.ready { builtin installationRetirementComplete() }`)
	files, executor := retirementFiles(scope), &retirementStopFixture{}
	for range 2 {
		r, err = w.run(ctx, j, scope.InstallationID, r.ID, executor, files)
		require.NoError(t, err)
	}
	require.Equal(t, "cancelled", r.State)
	installed, err := loadRetirementWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	require.NotEqual(t, installed.digest, w.digest)
}

func TestRetirementWorkflowRefusesFabricatedCompletionAndUnboundCalls(t *testing.T) {
	for name, body := range map[string]string{
		"fake output": `return { complete: true }`,
		"no cleanup": `builtin installationRetirementBegin()
builtin installationRetirementComplete()`,
		"ignored failure": `builtin installationRetirementBegin()
builtin installationRetirementComplete() on error continue
return { complete: true }`,
		"unknown operation": `builtin installationRetirementBegin()
builtin unboundExternalEffect()`,
		"unknown child": `builtin installationRetirementBegin()
automation unboundRetirementChild()`,
		"forged proof": `builtin installationRetirementBegin(proof: true)`,
	} {
		t.Run(name, func(t *testing.T) {
			db, _ := journalDB(t)
			j := preparationConnection(db)
			scope, _ := preparationFixture(t)
			ctx := captureOperator(auth.RoleAdmin, scope.RequestedBy)
			r, err := j.reserve(ctx, scope)
			require.NoError(t, err)
			files, executor := retirementFiles(scope), &retirementStopFixture{}
			_, err = retirementRecipeFixture(t, body).run(ctx, j, scope.InstallationID, r.ID, executor, files)
			require.Error(t, err)
			current, err := j.get(ctx, scope.InstallationID, r.ID)
			require.NoError(t, err)
			require.NotEqual(t, "cancelled", current.State)
			require.Zero(t, files.calls)
			require.Zero(t, executor.calls)
			if strings.HasPrefix(name, "unknown") {
				require.Equal(t, "preparing", current.State, "preflight must refuse before begin commits")
			}
		})
	}
}

func TestRetirementWorkflowPortsKeepNativeActorAndScope(t *testing.T) {
	w, err := loadRetirementWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	db, _ := journalDB(t)
	j := preparationConnection(db)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	files, executor := retirementFiles(scope), &retirementStopFixture{}
	for _, bad := range []context.Context{context.Background(), operator(auth.RoleOwner, scope.RequestedBy), captureOperator(auth.RoleReader, scope.RequestedBy), captureOperator(auth.RoleOwner, "other")} {
		_, err = w.run(bad, j, scope.InstallationID, r.ID, executor, files)
		require.Error(t, err)
	}
	s := &retirementWorkflowScope{journal: j, installation: scope.InstallationID, key: r.ID, workflow: scope.WorkflowDigest, cleanup: w.digest, operator: scope.RequestedBy, executor: executor, files: files}
	for name, operation := range s.operations() {
		_, err = operation(captureOperator(auth.RoleOwner, "other"), nil)
		require.Error(t, err, name)
		_, err = operation(ctx, map[string]any{"proof": true})
		require.Error(t, err, name)
	}
	for _, capability := range workflowhost.ScopedCapabilities(s.operations()) {
		_, err = capability.Handler(ctx, nil, 0)
		require.Error(t, err)
	}
	require.Zero(t, files.calls)
	require.Zero(t, executor.calls)
}
