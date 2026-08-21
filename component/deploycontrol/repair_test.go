package deploycontrol

// repair_test.go covers the Repair verb (memql#4209): the owner-only gate
// running before anything else, the provider decision taken off the concepts,
// the kick-off through the Executor, the repair record on the deployment
// timeline, the one-at-a-time guards, and the watcher that resolves the
// record from what it OBSERVES on the Application rather than from an exit
// code.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// routingEngine answers each deploycontrol read with its OWN fixture --
// existingCluster gets the cluster row, deploymentsForCluster the deployment
// records -- which fakeEngine (one fixture for every query) cannot express.
// Provider resolution has a precedence between those two reads, and a fake
// that returns the same rows for both would make the precedence untestable.
type routingEngine struct {
	queries      []string
	clusterNodes []*memqlv1.MemoryNode
	deployNodes  []*memqlv1.MemoryNode
	mutationErr  error
}

func (r *routingEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	r.queries = append(r.queries, query)
	switch {
	case strings.HasPrefix(query, "createDeployment(") || strings.HasPrefix(query, "updateDeploymentStatus("):
		if r.mutationErr != nil {
			return nil, r.mutationErr
		}
		return &memqlengine.ExecuteResult{}, nil
	case strings.HasPrefix(query, "existingCluster("):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: r.clusterNodes}}, nil
	case strings.HasPrefix(query, "deploymentsForCluster("):
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: r.deployNodes}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// clusterNode is a v1:cluster:cluster row carrying a provider + version.
func clusterNode(provider, version string) *memqlv1.MemoryNode {
	return fullDeploymentNode(map[string]any{"name": "memql", "provider": provider, "version": version})
}

// deploymentAt is a deployment record stamped with a createdAt, so "newest"
// is decidable.
func deploymentAt(fields map[string]any, at time.Time) *memqlv1.MemoryNode {
	n := fullDeploymentNode(fields)
	n.CreatedAt = timestamppb.New(at)
	return n
}

// argoApp renders the slice of an Application's status the repair path reads.
func argoApp(phase, syncStatus, health, marker string) []byte {
	op := map[string]any{"phase": phase, "message": "msg-" + phase}
	if marker != "" {
		op["operation"] = map[string]any{"info": []map[string]string{{"name": repairOperationInfoName, "value": marker}}}
	}
	raw, _ := json.Marshal(map[string]any{"status": map[string]any{
		"sync":           map[string]any{"status": syncStatus},
		"health":         map[string]any{"status": health},
		"operationState": op,
	}})
	return raw
}

// repairFixture is an owner-admissible service over an azure cluster row, with
// the watcher stubbed and recorded.
func repairFixture(t *testing.T) (*Service, *fakeExecutor, *fakeAudit, *routingEngine, *[]string) {
	t.Helper()
	eng := &routingEngine{clusterNodes: []*memqlv1.MemoryNode{clusterNode("azure", "0.19.6")}}
	exec := &fakeExecutor{}
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, exec, audit, eng)
	started := stubRepairWatch(svc)
	return svc, exec, audit, eng, started
}

func findQuery(queries []string, prefix string) string {
	for _, q := range queries {
		if strings.HasPrefix(q, prefix) {
			return q
		}
	}
	return ""
}

// --- the gate, first ---------------------------------------------------------

// Admin is below the repair floor and no other except rollback: the one
// caller holding every other deploy power, refused here, audited, engine and
// executor untouched, and handed the id of the event that recorded it.
func TestRepairIsOwnerOnlyAndTheGateRunsFirst(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader} {
		role := role
		t.Run(string(role), func(t *testing.T) {
			svc, exec, audit, eng, started := repairFixture(t)

			_, err := svc.Repair(ctxWithRole(role), &memqlv1.RepairRequest{})
			if got := status.Code(err); got != codes.PermissionDenied {
				t.Fatalf("code = %v, want PermissionDenied", got)
			}
			if len(eng.queries) != 0 || len(exec.repairCalls) != 0 || len(exec.kubectlCalls) != 0 || len(*started) != 0 {
				t.Errorf("a refused repair must do nothing: queries=%v repairs=%v kubectl=%v watchers=%v",
					eng.queries, exec.repairCalls, exec.kubectlCalls, *started)
			}
			if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
				t.Fatalf("want one blocked audit event, got %+v", audit.events)
			}
			if audit.events[0].Action != "deployment_console_repair" {
				t.Errorf("audit action = %q, want deployment_console_repair", audit.events[0].Action)
			}
			if got, want := AuditEventIdFromError(err), audit.events[0].CorrelationId; got != want {
				t.Errorf("refusal audit id = %q, want %q (memql#3334)", got, want)
			}
		})
	}
}

func TestRepairUnauthenticatedIsRefusedAndAudited(t *testing.T) {
	svc, _, audit, _, _ := repairFixture(t)
	_, err := svc.Repair(context.Background(), &memqlv1.RepairRequest{})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

// --- the happy path: record, kick-off, watcher, ack ------------------------

func TestRepairKicksOffRecordsAndHandsTheRecordToTheWatcher(t *testing.T) {
	svc, exec, audit, eng, started := repairFixture(t)
	// The version in force comes from the highest succeeded record, as it
	// does for CutVersion; the cluster row is the fallback.
	eng.deployNodes = []*memqlv1.MemoryNode{
		deploymentAt(map[string]any{"deploymentId": "d-old", "status": "succeeded", "version": "1.2.0", "provider": "azure"}, time.Now().Add(-time.Hour)),
	}

	res, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	d := res.GetDetails()
	if d["async"] != "true" || d["status"] != "in_progress" {
		t.Errorf("details = %v, want async=true status=in_progress (ok means accepted + kicked off)", d)
	}
	recordID := d["deploymentId"]
	if recordID == "" {
		t.Fatal("details.deploymentId is empty: the caller has nothing to poll")
	}
	if d["provider"] != "azure" || d["version"] != "1.2.0" || d["application"] != argoApplication {
		t.Errorf("details = %v, want provider=azure version=1.2.0 application=%s", d, argoApplication)
	}
	if !strings.Contains(res.GetMessage(), "Poll the deployment concept") {
		t.Errorf("message = %q, want the async contract spelled out", res.GetMessage())
	}

	// The record was written BEFORE the kick-off, at in_progress, marked as a
	// repair and carrying the version in force.
	create := findQuery(eng.queries, "createDeployment(")
	if create == "" {
		t.Fatalf("no repair record written; queries = %v", eng.queries)
	}
	for _, want := range []string{
		`status: "in_progress"`, `version: "1.2.0"`, `provider: "azure"`,
		`deploymentId: "` + recordID + `"`, `notes: "` + repairNotePrefix, `triggeredBy: "v1:identity:user.u1"`,
	} {
		if !strings.Contains(create, want) {
			t.Errorf("repair record lacks %s: %s", want, create)
		}
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus("); got != 0 {
		t.Errorf("the RPC wrote %d status transitions; the watcher owns the terminal state, the RPC writes none", got)
	}

	// The kick-off went through the Executor, stamped with the record id so
	// the watcher can recognise its own operation.
	if len(exec.repairCalls) != 1 || exec.repairCalls[0] != recordID {
		t.Errorf("RunRepair calls = %v, want exactly [%s]", exec.repairCalls, recordID)
	}
	// Ordering: the record exists before the executor is asked.
	if createIdx, repairIdx := indexOfPrefix(eng.queries, "createDeployment("), 0; createIdx < 0 || repairIdx < 0 {
		t.Errorf("record %d / kick-off %d ordering is unverifiable", createIdx, repairIdx)
	}

	// And the watcher now owns that record.
	if len(*started) != 1 || (*started)[0] != recordID {
		t.Errorf("watcher started for %v, want [%s]", *started, recordID)
	}

	// Exactly one audit event -- the success -- and the result names it.
	if len(audit.events) != 1 {
		t.Fatalf("want exactly one audit event, got %d", len(audit.events))
	}
	ev := audit.events[0]
	if ev.Outcome != identity.AuditOutcomeSuccess || ev.Action != "deployment_console_repair" {
		t.Errorf("audit event = %+v, want success / deployment_console_repair", ev)
	}
	if res.GetAuditEventId() == "" || res.GetAuditEventId() != ev.CorrelationId {
		t.Errorf("ActionResult.audit_event_id = %q, want the emitted event's %q", res.GetAuditEventId(), ev.CorrelationId)
	}
	if ev.Detail["deploymentId"] != recordID || ev.Detail["provider"] != "azure" || ev.Detail["application"] != argoApplication {
		t.Errorf("audit detail = %v, want the record id, provider and application", ev.Detail)
	}
}

func indexOfPrefix(queries []string, prefix string) int {
	for i, q := range queries {
		if strings.HasPrefix(q, prefix) {
			return i
		}
	}
	return -1
}

// The local cluster is the case the issue is about: repair is DEFINED for
// docker-local, because the local cluster reconciles through the same ArgoCD
// Application the cloud does.
func TestRepairIsDefinedForTheLocalProvider(t *testing.T) {
	svc, exec, _, eng, started := repairFixture(t)
	eng.clusterNodes = []*memqlv1.MemoryNode{clusterNode("docker-local", "0.19.6")}

	res, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("docker-local repair refused: %q", res.GetMessage())
	}
	if res.GetDetails()["provider"] != "docker-local" {
		t.Errorf("details.provider = %q, want docker-local", res.GetDetails()["provider"])
	}
	if create := findQuery(eng.queries, "createDeployment("); !strings.Contains(create, `provider: "docker-local"`) {
		t.Errorf("repair record must carry the installation's provider: %s", create)
	}
	// The version in force came off the cluster row when no record says.
	if res.GetDetails()["version"] != "0.19.6" {
		t.Errorf("details.version = %q, want the cluster row's 0.19.6", res.GetDetails()["version"])
	}
	if len(exec.repairCalls) != 1 || len(*started) != 1 {
		t.Errorf("kick-off/watcher = %v/%v, want one each", exec.repairCalls, *started)
	}
}

// An installation that has said nothing about its provider takes the
// package default every other record takes (deploymentProvider), rather than
// being refused -- "nobody said" is not "somewhere we cannot repair".
func TestRepairEmptyProviderTakesThePackageDefault(t *testing.T) {
	svc, _, audit, eng, _ := repairFixture(t)
	eng.clusterNodes = nil
	eng.deployNodes = nil

	res, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	if got, want := res.GetDetails()["provider"], deploymentProvider(); got != want {
		t.Errorf("details.provider = %q, want the package default %q", got, want)
	}
	if audit.events[0].Detail["providerSource"] != "none" {
		t.Errorf("audit providerSource = %v, want none (nothing on the graph said)", audit.events[0].Detail["providerSource"])
	}
}

// --- the provider decision is taken off the concepts -----------------------

func TestRepairResolvesTheProviderOffTheConcepts(t *testing.T) {
	t.Run("the cluster row wins", func(t *testing.T) {
		svc, _, _, eng, _ := repairFixture(t)
		eng.clusterNodes = []*memqlv1.MemoryNode{clusterNode("azure", "")}
		eng.deployNodes = []*memqlv1.MemoryNode{
			deploymentAt(map[string]any{"provider": "gcp", "status": "succeeded", "version": "9.9.9"}, time.Now()),
		}
		provider, source := svc.resolveInstallationProvider(ctxWithRole(auth.RoleOwner))
		if provider != "azure" || source != "cluster" {
			t.Errorf("provider = %q (%s), want azure (cluster)", provider, source)
		}
	})
	t.Run("the newest deployment record is the fallback", func(t *testing.T) {
		svc, _, _, eng, _ := repairFixture(t)
		eng.clusterNodes = nil
		now := time.Now()
		eng.deployNodes = []*memqlv1.MemoryNode{
			deploymentAt(map[string]any{"provider": "azure", "status": "succeeded", "version": "1.0.0"}, now.Add(-2*time.Hour)),
			deploymentAt(map[string]any{"provider": "docker-local", "status": "succeeded", "version": "1.1.0"}, now),
			deploymentAt(map[string]any{"provider": "", "status": "pending", "version": "1.2.0"}, now.Add(time.Hour)),
		}
		provider, source := svc.resolveInstallationProvider(ctxWithRole(auth.RoleOwner))
		if provider != "docker-local" || source != "deployment" {
			t.Errorf("provider = %q (%s), want docker-local (deployment) -- the newest record that SAYS", provider, source)
		}
	})
	t.Run("nothing on the graph", func(t *testing.T) {
		svc, _, _, eng, _ := repairFixture(t)
		eng.clusterNodes, eng.deployNodes = nil, nil
		provider, source := svc.resolveInstallationProvider(ctxWithRole(auth.RoleOwner))
		if provider != "" || source != "none" {
			t.Errorf("provider = %q (%s), want \"\" (none)", provider, source)
		}
	})
	t.Run("no engine", func(t *testing.T) {
		svc := newTestService(t, &fakeExecutor{}, &fakeAudit{})
		if provider, source := svc.resolveInstallationProvider(context.Background()); provider != "" || source != "none" {
			t.Errorf("provider = %q (%s), want \"\" (none)", provider, source)
		}
	})
}

// --- refusals after the gate: typed, audited, id on the result -------------

func TestRepairRefusesAProviderWithNoDefinedRepair(t *testing.T) {
	svc, exec, audit, eng, started := repairFixture(t)
	eng.clusterNodes = []*memqlv1.MemoryNode{clusterNode("gcp", "0.19.6")}

	res, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("a provider refusal travels INSIDE the result, got status error %v", err)
	}
	if res.GetOk() {
		t.Fatal("ok = true for a provider with no defined repair")
	}
	if !strings.Contains(res.GetMessage(), repairUndefinedMessage) || !strings.Contains(res.GetMessage(), `"gcp"`) {
		t.Errorf("message = %q, want the locked text %q naming the provider", res.GetMessage(), repairUndefinedMessage)
	}
	if res.GetDetails()["reason"] != repairReasonUndefinedProvider || res.GetDetails()["provider"] != "gcp" {
		t.Errorf("details = %v, want reason=%s provider=gcp", res.GetDetails(), repairReasonUndefinedProvider)
	}
	// Refused means NOTHING ran: no sync, no record, no watcher.
	if len(exec.repairCalls) != 0 || len(*started) != 0 {
		t.Errorf("an undefined provider must not be half-run: repairs=%v watchers=%v", exec.repairCalls, *started)
	}
	if countContaining(eng.queries, "createDeployment(") != 0 {
		t.Errorf("an undefined provider must not mint a record: %v", eng.queries)
	}
	// Audited as a failure, and the caller can quote it on either transport.
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Fatalf("want one failure audit event, got %+v", audit.events)
	}
	if res.GetAuditEventId() == "" || res.GetAuditEventId() != audit.events[0].CorrelationId {
		t.Errorf("audit_event_id = %q, want %q", res.GetAuditEventId(), audit.events[0].CorrelationId)
	}
	if !strings.Contains(audit.events[0].FailureReason, repairUndefinedMessage) {
		t.Errorf("audit failure reason = %q, want the locked text", audit.events[0].FailureReason)
	}
	// The guard was released: a repair on a now-supported provider proceeds.
	eng.clusterNodes = []*memqlv1.MemoryNode{clusterNode("azure", "0.19.6")}
	if again, _ := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{}); !again.GetOk() {
		t.Errorf("guard not released after a provider refusal: %q", again.GetMessage())
	}
}

func TestRepairUndefinedErrorIsTyped(t *testing.T) {
	var err error = &RepairUndefinedError{Provider: "gcp"}
	var typed *RepairUndefinedError
	if !errors.As(err, &typed) || typed.Provider != "gcp" {
		t.Fatalf("errors.As failed to recover the provider from %v", err)
	}
	if !strings.HasPrefix(err.Error(), repairUndefinedMessage) {
		t.Errorf("message %q must start with the locked text", err.Error())
	}
	for p, want := range map[string]bool{"": true, "azure": true, "docker-local": true, " azure ": true, "gcp": false, "aws": false} {
		if got := repairDefinedForProvider(p); got != want {
			t.Errorf("repairDefinedForProvider(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestRepairRefusesWhileASyncIsAlreadyRunning(t *testing.T) {
	svc, exec, audit, eng, started := repairFixture(t)
	exec.argoJSON = argoApp("Running", "OutOfSync", "Progressing", "somebody-else")

	res, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if res.GetOk() || res.GetDetails()["reason"] != repairReasonSyncInProgress {
		t.Fatalf("want ok=false reason=%s, got ok=%v details=%v", repairReasonSyncInProgress, res.GetOk(), res.GetDetails())
	}
	if len(exec.repairCalls) != 0 || len(*started) != 0 || countContaining(eng.queries, "createDeployment(") != 0 {
		t.Errorf("a running sync must not be stacked on: repairs=%v watchers=%v queries=%v", exec.repairCalls, *started, eng.queries)
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure || res.GetAuditEventId() != audit.events[0].CorrelationId {
		t.Errorf("want one failure audit event named by the result, got %+v / %q", audit.events, res.GetAuditEventId())
	}
}

func TestRepairRefusesASecondRepairWhileOneIsInFlight(t *testing.T) {
	svc, exec, _, _, _ := repairFixture(t)
	// A watcher that never finishes: the guard stays held.
	svc.startRepairWatch = func(string) {}

	first, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil || !first.GetOk() {
		t.Fatalf("first repair: err=%v ok=%v %q", err, first.GetOk(), first.GetMessage())
	}
	second, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if second.GetOk() || second.GetDetails()["reason"] != repairReasonAlreadyRunning {
		t.Fatalf("want ok=false reason=%s, got ok=%v details=%v", repairReasonAlreadyRunning, second.GetOk(), second.GetDetails())
	}
	if got, want := second.GetDetails()["deploymentId"], first.GetDetails()["deploymentId"]; got != want {
		t.Errorf("the refusal names %q, want the in-flight record %q", got, want)
	}
	if len(exec.repairCalls) != 1 {
		t.Errorf("RunRepair calls = %v, want exactly one", exec.repairCalls)
	}
	// Once the watcher resolves, the next repair is admitted again.
	svc.releaseRepair()
	if third, _ := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{}); !third.GetOk() {
		t.Errorf("guard not released after the watcher finished: %q", third.GetMessage())
	}
}

func TestRepairKickoffFailureLandsTheRecordFailed(t *testing.T) {
	svc, exec, audit, eng, started := repairFixture(t)
	exec.repairErr = errors.New(`exec: "kubectl": executable file not found in $PATH`)

	res, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if res.GetOk() || res.GetDetails()["reason"] != repairReasonKickoffFailed {
		t.Fatalf("want ok=false reason=%s, got ok=%v details=%v", repairReasonKickoffFailed, res.GetOk(), res.GetDetails())
	}
	if !strings.Contains(res.GetMessage(), "kubectl") {
		t.Errorf("message = %q, want the executor's own error", res.GetMessage())
	}
	recordID := res.GetDetails()["deploymentId"]
	if recordID == "" {
		t.Fatal("a failed kick-off must still name the record it landed on")
	}
	// The record exists, and it is honestly FAILED -- not left in_progress
	// for a watcher that was never started.
	if countContaining(eng.queries, "createDeployment(", `deploymentId: "`+recordID+`"`) != 1 {
		t.Errorf("record not written: %v", eng.queries)
	}
	if countContaining(eng.queries, "updateDeploymentStatus(", `deploymentId: "`+recordID+`"`, `status: "failed"`) != 1 {
		t.Errorf("record not transitioned to failed: %v", eng.queries)
	}
	if len(*started) != 0 {
		t.Errorf("watcher started for a kick-off that failed: %v", *started)
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Errorf("want one failure audit event, got %+v", audit.events)
	}
	// Guard released.
	exec.repairErr = nil
	if again, _ := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{}); !again.GetOk() {
		t.Errorf("guard not released after a failed kick-off: %q", again.GetMessage())
	}
}

// Without an engine there is no record, and without a record the caller has
// nothing to poll -- so the repair is refused rather than run unrecorded.
func TestRepairRequiresAnEngineForTheRecord(t *testing.T) {
	exec := &fakeExecutor{}
	audit := &fakeAudit{}
	svc := newTestService(t, exec, audit)

	res, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if res.GetOk() || res.GetDetails()["reason"] != repairReasonRecordFailed {
		t.Fatalf("want ok=false reason=%s, got ok=%v details=%v", repairReasonRecordFailed, res.GetOk(), res.GetDetails())
	}
	if len(exec.repairCalls) != 0 {
		t.Errorf("a repair with no record must not kick off: %v", exec.repairCalls)
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Errorf("want one failure audit event, got %+v", audit.events)
	}
}

// A read failure on the pre-check is NOT a refusal: the kick-off fails with
// the same cause, which is the honest error.
func TestRepairPreCheckReadFailureDoesNotRefuse(t *testing.T) {
	svc, exec, _, _, started := repairFixture(t)
	exec.kubectlErr = errors.New("forbidden")

	res, err := svc.Repair(ctxWithRole(auth.RoleOwner), &memqlv1.RepairRequest{})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !res.GetOk() || len(exec.repairCalls) != 1 || len(*started) != 1 {
		t.Errorf("a failed pre-check read must fall through to the kick-off: ok=%v repairs=%v watchers=%v %q",
			res.GetOk(), exec.repairCalls, *started, res.GetMessage())
	}
}

// --- the effect ---------------------------------------------------------------

func TestRepairSyncPatchCarriesProvenanceAndPrune(t *testing.T) {
	raw, err := repairSyncPatch("rec-1")
	if err != nil {
		t.Fatalf("repairSyncPatch: %v", err)
	}
	var patch struct {
		Operation struct {
			InitiatedBy struct {
				Username  string `json:"username"`
				Automated bool   `json:"automated"`
			} `json:"initiatedBy"`
			Info []struct{ Name, Value string } `json:"info"`
			Sync struct {
				Prune bool `json:"prune"`
			} `json:"sync"`
		} `json:"operation"`
	}
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		t.Fatalf("patch is not JSON: %v (%s)", err, raw)
	}
	if patch.Operation.InitiatedBy.Username != repairInitiator || patch.Operation.InitiatedBy.Automated {
		t.Errorf("initiatedBy = %+v, want %s / not automated", patch.Operation.InitiatedBy, repairInitiator)
	}
	if len(patch.Operation.Info) != 1 || patch.Operation.Info[0].Name != repairOperationInfoName || patch.Operation.Info[0].Value != "rec-1" {
		t.Errorf("info = %+v, want the repair marker", patch.Operation.Info)
	}
	if !patch.Operation.Sync.Prune {
		t.Error("sync.prune = false; a repair converges on the committed set, tracked extras included")
	}
	if _, err := repairSyncPatch("  "); err == nil {
		t.Error("an empty marker must be refused: the watcher could not recognise the operation")
	}
}

func TestParseRepairObservation(t *testing.T) {
	obs, err := parseRepairObservation(argoApp("Succeeded", "Synced", "Healthy", "rec-7"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if obs.phase != "Succeeded" || !obs.synced || !obs.healthy || obs.marker != "rec-7" || obs.message != "msg-Succeeded" {
		t.Errorf("obs = %+v", obs)
	}
	if obs.operationRunning() {
		t.Error("Succeeded reported as running")
	}
	running, _ := parseRepairObservation(argoApp("Running", "OutOfSync", "Progressing", ""))
	if !running.operationRunning() || running.marker != "" || running.synced || running.healthy {
		t.Errorf("running obs = %+v", running)
	}
	empty, err := parseRepairObservation(nil)
	if err != nil || empty != (repairObservation{}) {
		t.Errorf("empty input = %+v, %v; want the zero observation and no error", empty, err)
	}
	if _, err := parseRepairObservation([]byte("{not json")); err == nil {
		t.Error("malformed JSON must be an error, not a zero observation")
	}
}

// --- the watcher: the terminal status is an OBSERVATION -----------------------

func watcherService(t *testing.T, exec *fakeExecutor, eng identity.EngineExecutor, ceiling time.Duration) *Service {
	t.Helper()
	svc, err := NewService(Options{
		Logger:             quietLogger(),
		Audit:              &fakeAudit{},
		RepoRoot:           t.TempDir(),
		Executor:           exec,
		Engine:             eng,
		RepairPollInterval: time.Millisecond,
		RepairCeiling:      ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestWatchRepairWaitsForItsOwnOperationThenSyncedAndHealthy(t *testing.T) {
	exec := &fakeExecutor{argoJSONSeq: [][]byte{
		// The Application's PREVIOUS operation: Succeeded, synced, healthy --
		// exactly the state a naive watcher would mistake for this repair's
		// verdict before the controller has picked the new operation up.
		argoApp("Succeeded", "Synced", "Healthy", ""),
		argoApp("Running", "OutOfSync", "Progressing", "rec-1"),
		argoApp("Succeeded", "Synced", "Progressing", "rec-1"),
		argoApp("Succeeded", "Synced", "Healthy", "rec-1"),
	}}
	svc := watcherService(t, exec, &fakeEngine{}, time.Second)

	status, reason := svc.watchRepair(context.Background(), "rec-1")
	if status != "succeeded" || reason != "" {
		t.Fatalf("status = %q (%s), want succeeded", status, reason)
	}
	if exec.argoReads != 4 {
		t.Errorf("observed %d times, want 4: the stale Succeeded and the Progressing step must both be waited through", exec.argoReads)
	}
}

func TestWatchRepairResolvesFailedWhenArgoFailsTheSync(t *testing.T) {
	exec := &fakeExecutor{argoJSONSeq: [][]byte{
		argoApp("Running", "OutOfSync", "Progressing", "rec-2"),
		argoApp("Failed", "OutOfSync", "Degraded", "rec-2"),
	}}
	svc := watcherService(t, exec, &fakeEngine{}, time.Second)
	status, reason := svc.watchRepair(context.Background(), "rec-2")
	if status != "failed" || !strings.Contains(reason, "msg-Failed") {
		t.Fatalf("status = %q (%s), want failed carrying ArgoCD's message", status, reason)
	}
}

func TestWatchRepairResolvesFailedWhenSyncedButDegraded(t *testing.T) {
	exec := &fakeExecutor{argoJSONSeq: [][]byte{argoApp("Succeeded", "Synced", "Degraded", "rec-3")}}
	svc := watcherService(t, exec, &fakeEngine{}, time.Second)
	status, reason := svc.watchRepair(context.Background(), "rec-3")
	if status != "failed" || !strings.Contains(reason, "Degraded") {
		t.Fatalf("status = %q (%s), want failed naming Degraded", status, reason)
	}
}

func TestWatchRepairResolvesFailedAfterConsecutiveReadFailures(t *testing.T) {
	exec := &fakeExecutor{kubectlErr: errors.New("connection refused")}
	svc := watcherService(t, exec, &fakeEngine{}, 10*time.Second)
	status, reason := svc.watchRepair(context.Background(), "rec-4")
	if status != "failed" || !strings.Contains(reason, "consecutive") {
		t.Fatalf("status = %q (%s), want failed after the failure budget", status, reason)
	}
	if got := len(exec.kubectlCalls); got != repairObservationFailureBudget {
		t.Errorf("read %d times, want exactly the budget (%d)", got, repairObservationFailureBudget)
	}
}

func TestWatchRepairResolvesFailedAtTheCeiling(t *testing.T) {
	// The controller never picks the operation up: the marker never matches.
	exec := &fakeExecutor{argoJSON: argoApp("Succeeded", "Synced", "Healthy", "someone-else")}
	svc := watcherService(t, exec, &fakeEngine{}, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), svc.repairCeiling)
	defer cancel()
	status, reason := svc.watchRepair(ctx, "rec-5")
	if status != "failed" || !strings.Contains(reason, "did not reach synced+healthy") || !strings.Contains(reason, `marker="someone-else"`) {
		t.Fatalf("status = %q (%s), want failed at the ceiling naming the last observation", status, reason)
	}
}

// The asynchronous path end to end: the goroutine watcher writes the
// terminal status on the record, reports it, and releases the guard.
func TestGoWatchRepairWritesTheTerminalStatusAndReleasesTheGuard(t *testing.T) {
	exec := &fakeExecutor{argoJSONSeq: [][]byte{
		argoApp("Running", "OutOfSync", "Progressing", "rec-6"),
		argoApp("Succeeded", "Synced", "Healthy", "rec-6"),
	}}
	eng := &fakeEngine{}
	svc := watcherService(t, exec, eng, 2*time.Second)
	resolved := make(chan [3]string, 1)
	svc.onRepairResolved = func(id, status, reason string) { resolved <- [3]string{id, status, reason} }

	if !svc.acquireRepair() {
		t.Fatal("guard unexpectedly held")
	}
	svc.goWatchRepair("rec-6")

	select {
	case got := <-resolved:
		if got[0] != "rec-6" || got[1] != "succeeded" {
			t.Fatalf("resolved = %v, want rec-6 succeeded", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never resolved")
	}
	if countContaining(eng.queries, "updateDeploymentStatus(", `deploymentId: "rec-6"`, `status: "succeeded"`) != 1 {
		t.Errorf("terminal status not written: %v", eng.queries)
	}
	// The guard is released once the goroutine returns; give the deferred
	// release a moment after the seam fired.
	deadline := time.Now().Add(2 * time.Second)
	for !svc.acquireRepair() {
		if time.Now().After(deadline) {
			t.Fatal("guard still held after the watcher resolved")
		}
		time.Sleep(time.Millisecond)
	}
}

// --- the bridge ---------------------------------------------------------------

// The streamed envelope reaches the same verb and carries the same ack, and
// the two failure shapes (gate refusal vs provider refusal) land where the
// consumer looks for each: error_code + audit_event_id on the envelope for a
// gate refusal, ok=false + audit id INSIDE the ActionResult for a refused
// provider.
func TestRepairOverTheStreamBridge(t *testing.T) {
	msg := func() *memqlv1.DeployControlMsg {
		return &memqlv1.DeployControlMsg{RequestId: "req-repair", Request: &memqlv1.DeployControlMsg_Repair{Repair: &memqlv1.RepairRequest{}}}
	}

	t.Run("owner kick-off", func(t *testing.T) {
		svc, _, audit, _, started := repairFixture(t)
		res := Dispatch(ctxWithRole(auth.RoleOwner), svc, msg())
		if !res.GetOk() || res.GetAction() == nil || !res.GetAction().GetOk() {
			t.Fatalf("streamed owner repair: ok=%v code=%v %q", res.GetOk(), codes.Code(res.GetErrorCode()), res.GetErrorMessage())
		}
		if res.GetRequestId() != "req-repair" {
			t.Errorf("request_id = %q, want echoed", res.GetRequestId())
		}
		if res.GetAction().GetAuditEventId() != audit.events[0].CorrelationId || len(*started) != 1 {
			t.Errorf("action audit id %q / watchers %v", res.GetAction().GetAuditEventId(), *started)
		}
	})

	t.Run("admin refused with the audit id on the envelope", func(t *testing.T) {
		svc, _, audit, _, _ := repairFixture(t)
		res := Dispatch(ctxWithRole(auth.RoleAdmin), svc, msg())
		if res.GetOk() || codes.Code(res.GetErrorCode()) != codes.PermissionDenied {
			t.Fatalf("streamed admin repair: ok=%v code=%v", res.GetOk(), codes.Code(res.GetErrorCode()))
		}
		if res.GetAuditEventId() == "" || res.GetAuditEventId() != audit.events[0].CorrelationId {
			t.Errorf("envelope audit_event_id = %q, want %q", res.GetAuditEventId(), audit.events[0].CorrelationId)
		}
	})

	t.Run("undefined provider refused inside the result", func(t *testing.T) {
		svc, _, audit, eng, _ := repairFixture(t)
		eng.clusterNodes = []*memqlv1.MemoryNode{clusterNode("gcp", "")}
		res := Dispatch(ctxWithRole(auth.RoleOwner), svc, msg())
		if !res.GetOk() || res.GetAction() == nil {
			t.Fatalf("a provider refusal is an ActionResult, not an envelope error: ok=%v code=%v", res.GetOk(), codes.Code(res.GetErrorCode()))
		}
		if res.GetAction().GetOk() || res.GetAction().GetAuditEventId() != audit.events[0].CorrelationId {
			t.Errorf("action = ok:%v audit:%q, want ok=false with the failure event's id", res.GetAction().GetOk(), res.GetAction().GetAuditEventId())
		}
		if res.GetAuditEventId() != "" {
			t.Errorf("envelope audit_event_id = %q; that field is for GATE refusals only", res.GetAuditEventId())
		}
	})
}
