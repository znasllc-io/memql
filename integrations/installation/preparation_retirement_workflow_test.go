package installation

import (
	"context"
	"strings"
	"sync"
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
	require.Len(t, files.purged, 4, "a cancelled preparation leaves no Library entry pointing at a tombstone")
	require.Equal(t, 1, executor.calls, "durable producer stop must survive replacement")
	again, err := w.run(ctx, j, scope.InstallationID, r.ID, executor, files)
	require.NoError(t, err)
	require.Equal(t, r, again)
}

// The installed recipe removes each retired capture's Library entries before
// the page may advance. A removal whose reply was lost is repeated by a fresh
// host from the journal; completion is impossible while one is unconfirmed.
func TestRetirementWorkflowInstalledPassPurgesRetiredCapturesOnFreshHost(t *testing.T) {
	db, otherDB := journalDB(t)
	j, other := preparationConnection(db), preparationConnection(otherDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	w, err := loadRetirementWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	files, executor := retirementFiles(scope), &retirementStopFixture{}
	files.fail = "purge"
	_, err = w.run(ctx, j, scope.InstallationID, r.ID, executor, files)
	require.Error(t, err)
	r, err = other.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	first := r.Retirement.Captures["candidate"].Page[0]
	require.NotEmpty(t, first.ReceiptDigest)
	require.False(t, first.Purged, "a lost removal reply is not a removal")
	_, err = other.finishRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, w.digest)
	require.Error(t, err, "completion requires every Library removal")
	for range 3 {
		r, err = w.run(ctx, other, scope.InstallationID, r.ID, executor, files)
		require.NoError(t, err)
		if r.State == "cancelled" {
			break
		}
	}
	require.Equal(t, "cancelled", r.State)
	require.Len(t, files.purged, 4)
	require.Equal(t, 2, files.purged[first.IntentID], "the fresh host repeated the unconfirmed removal")
}

// A page written before removals were recorded keeps its exact encoding, and
// a removal can never be recorded for an artifact without its retirement.
func TestPreparationRetirementRemovalNeedsItsRetirementReceipt(t *testing.T) {
	intent := strings.Repeat("a", 64)
	receipt := "memql-id:" + strings.Repeat("b", 64)
	page := func(entry string) []byte {
		return []byte(`{"workflowDigest":"memql-id:` + strings.Repeat("c", 64) + `","producerStopped":true,"captures":{` +
			`"candidate":{"fenced":true,"pinsReleased":true,"cursor":"","page":[` + entry + `],"complete":false},` +
			`"rollback":{"fenced":true,"pinsReleased":true,"cursor":"","page":[],"complete":false}}}`)
	}
	for name, entry := range map[string]string{
		"pending":      `{"intentId":"` + intent + `","receiptDigest":""}`,
		"retired":      `{"intentId":"` + intent + `","receiptDigest":"` + receipt + `"}`,
		"retired+gone": `{"intentId":"` + intent + `","receiptDigest":"` + receipt + `","purged":true}`,
	} {
		_, err := decodePreparationRetirement(page(entry), "retiring")
		require.NoError(t, err, name)
	}
	for name, entry := range map[string]string{
		"gone before retirement": `{"intentId":"` + intent + `","receiptDigest":"","purged":true}`,
		"noncanonical false":     `{"intentId":"` + intent + `","receiptDigest":"` + receipt + `","purged":false}`,
	} {
		_, err := decodePreparationRetirement(page(entry), "retiring")
		require.Error(t, err, name)
	}
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
    builtin installationRetirementPurge(role: role, intentId: artifact.intentId)
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
	require.Len(t, files.purged, 4)
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

// The generic interpreter serializes callbacks today. The native operation
// closures also keep their completion gate safe when a host dispatches them
// concurrently; durable receipts remain the authority in either case.
func TestRetirementWorkflowConcurrentObservationAndCompletion(t *testing.T) {
	db, _ := journalDB(t)
	j := preparationConnection(db)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	w, err := loadRetirementWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	files, executor := retirementFiles(scope), &retirementStopFixture{}
	for role := range files.ids {
		files.ids[role] = nil
	}
	s := &retirementWorkflowScope{journal: j, installation: scope.InstallationID, key: r.ID, workflow: scope.WorkflowDigest, cleanup: w.digest, operator: scope.RequestedBy, executor: executor, files: files}
	ops := s.operations()
	for _, name := range []string{"installationRetirementBegin", "installationRetirementStop"} {
		_, err = ops[name](ctx, nil)
		require.NoError(t, err)
	}
	for _, role := range []string{"candidate", "rollback"} {
		for _, name := range []string{"installationRetirementFence", "installationRetirementRelease", "installationRetirementPage"} {
			_, err = ops[name](ctx, map[string]any{"role": role})
			require.NoError(t, err)
		}
	}
	// Observation writers and completion contend across real journal reads and
	// transactions. Calls losing the completion race may correctly refuse.
	start := make(chan struct{})
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			<-start
			_, _ = ops["installationRetirementObserve"](ctx, nil)
			_, _ = ops["installationRetirementComplete"](ctx, nil)
		})
	}
	close(start)
	group.Wait()
	require.True(t, s.observed.Load())
	require.True(t, s.completed.Load())
	r, err = j.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", r.State)
	for name, operation := range ops {
		_, err = operation(ctx, nil)
		require.Error(t, err, name+" must reject work after completion")
	}
}
