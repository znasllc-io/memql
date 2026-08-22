package memql

// deploy_control_repair_db_test.go is memql#4209's end-to-end evidence, on the
// path an operator actually takes: a repair forwarded from a bff to the
// identity node with the caller as a ForwardedAuthority, served by a REAL
// deploycontrol.Service over a REAL engine on a REAL database, with only the
// kubectl boundary scripted. What it proves that the unit suites cannot:
//
//   - the query strings the verb issues (existingCluster, deploymentsForCluster,
//     createDeployment with the repair note, deploymentById,
//     updateDeploymentStatus) parse and execute against the live engine;
//   - the repair record lands on the v1:cluster:deployment timeline at
//     in_progress BEFORE the kick-off, attributed to the ORIGINATING human
//     (triggeredBy AND the row's createdBy), marked as a repair;
//   - the watcher's terminal write -- which runs after the RPC has answered,
//     on a context detached from the hop -- is accepted by the engine and
//     resolves the record to succeeded, so polling the record the way the
//     portal does reaches a terminal state rather than invented progress.
//
// Postgres-gated: openWireTestDB skips when no database is reachable, and the
// db-tests lane runs with MEMQL_REQUIRE_DB=1 so a skip there is a failure.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/deploycontrol"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/provenance"
)

// scriptedRepairExecutor stands in for kubectl. Before the kick-off the
// Application reads report its PREVIOUS operation -- finished, synced, healthy,
// carrying no repair marker -- which is both the state the pre-check must
// admit and the stale verdict the watcher must not mistake for its own. The
// kick-off records the marker the service stamped; from then on the reads
// report that operation as Running until the test releases it, then as
// Succeeded + Synced + Healthy -- so the in_progress state is OBSERVABLE for
// as long as the test needs, with no race against a fast watcher.
type scriptedRepairExecutor struct {
	mu       sync.Mutex
	marker   string
	released bool
	reads    int
}

func (e *scriptedRepairExecutor) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.released = true
}

func (e *scriptedRepairExecutor) RunRepair(_ context.Context, marker string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.marker = marker
	return "application.argoproj.io/memql patched", nil
}

func (e *scriptedRepairExecutor) KubectlJSON(_ context.Context, args ...string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reads++
	phase, sync, health := "Succeeded", "Synced", "Healthy"
	var info []map[string]string
	if e.marker != "" {
		info = []map[string]string{{"name": "memql.io/repair", "value": e.marker}}
		if !e.released {
			phase, sync, health = "Running", "OutOfSync", "Progressing"
		}
	}
	raw, err := json.Marshal(map[string]any{"status": map[string]any{
		"sync":   map[string]any{"status": sync},
		"health": map[string]any{"status": health},
		"operationState": map[string]any{
			"phase":     phase,
			"operation": map[string]any{"info": info},
		},
	}})
	return raw, err
}

func (e *scriptedRepairExecutor) RunRollback(context.Context, string) (string, error) {
	return "", fmt.Errorf("not under test")
}
func (e *scriptedRepairExecutor) RunRolloutAction(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("not under test")
}
func (e *scriptedRepairExecutor) Git(context.Context, ...string) (string, error) {
	return "", fmt.Errorf("not under test")
}

// readDeployment runs the read the portal runs, and returns the record's
// status, triggeredBy, notes and the row's createdBy.
func readDeployment(t *testing.T, ctx context.Context, eng *memqlengine.MemQLEngine, id string) (status, triggeredBy, notes, createdBy string, found bool) {
	t.Helper()
	res, err := eng.Execute(ctx, fmt.Sprintf("deploymentById(deploymentId: %q)", id))
	require.NoError(t, err, "deploymentById must execute")
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return "", "", "", "", false
	}
	node := res.Bundle.Nodes[0]
	fields := node.GetPayload().GetFields()
	return fields["status"].GetStringValue(), fields["triggeredBy"].GetStringValue(),
		fields["notes"].GetStringValue(), node.GetCreatedBy(), true
}

func TestForwardedRepairRecordsAndResolvesOnTheLiveEngine(t *testing.T) {
	db := openWireTestDB(t)
	bg := provenance.ContextWithProvenance(context.Background(), provenance.Direct("test:#4209"))

	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(concept.DefaultRegistry()))

	ownerId := fmt.Sprintf("v1:identity:user:repair-4209-owner-%d", time.Now().UnixNano())
	// The engine's createdBy is the actor string auth.ActorFromToken derives
	// -- the principal's email when the token carries one -- so that, and not
	// the subject, is what "attributed to the originating human" reads as on
	// the row itself.
	const ownerEmail = "repair-owner@example.test"
	seedUserRow(bg, t, db, ownerId)
	// The read side of the test reads as the same human the hop asserts.
	readCtx := auth.ContextWithUserActor(bg, ownerId)

	// The reads the verb relies on must execute on the live engine; a wrong
	// call form would otherwise degrade silently into "nothing said".
	_, err = eng.Execute(readCtx, "existingCluster()")
	require.NoError(t, err, "existingCluster() is the provider read the verb issues")
	_, err = eng.Execute(readCtx, `deploymentsForCluster(clusterId: "")`)
	require.NoError(t, err, "deploymentsForCluster is the fallback provider read the verb issues")

	exec := &scriptedRepairExecutor{}
	audit := &recordingAudit{}
	svc, err := deploycontrol.NewService(deploycontrol.Options{
		Logger:             testLogger(),
		Audit:              audit,
		RepoRoot:           t.TempDir(),
		Executor:           exec,
		Engine:             eng,
		RepairPollInterval: 25 * time.Millisecond,
		RepairCeiling:      20 * time.Second,
	})
	require.NoError(t, err)
	h := NewDeployControlForwardHandler(svc, testLogger())
	require.NotNil(t, h)

	// The ORIGINATING caller, asserted the way a bff asserts a session.
	authority, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{UserId: ownerId, PrimaryEmail: ownerEmail, Role: auth.RoleOwner},
		auth.ForwardedClassUser, "", time.Time{}, time.Now())
	require.NoError(t, err)

	// The hop: a bare receiving context, as in production.
	res, err := runHop(t, h, authority.Principal(), &memqlv1.DeployControlMsg{
		RequestId: "req-repair-db",
		Request:   &memqlv1.DeployControlMsg_Repair{Repair: &memqlv1.RepairRequest{}},
	})
	require.NoError(t, err, "the hop itself must succeed")
	require.True(t, res.GetOk(), "forwarded owner repair: code=%d %s", res.GetErrorCode(), res.GetErrorMessage())
	action := res.GetAction()
	require.NotNil(t, action)
	require.True(t, action.GetOk(), "repair not accepted: %s", action.GetMessage())
	recordID := action.GetDetails()["deploymentId"]
	require.NotEmpty(t, recordID, "the ack must name the record to poll")
	t.Cleanup(func() {
		for _, id := range []string{"v1:cluster:deployment:" + recordID, recordID, ownerId} {
			_, _ = db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("id = ?", id).Exec(context.Background())
		}
	})
	require.Equal(t, recordID, exec.marker, "the sync must be stamped with the record id the watcher looks for")

	// (1) The record is on the timeline NOW, at in_progress, as a repair, by
	// the originating human -- before anything has resolved.
	status, triggeredBy, notes, createdBy, found := readDeployment(t, readCtx, eng, recordID)
	require.True(t, found, "repair record %s not readable through deploymentById", recordID)
	require.Equal(t, "in_progress", status, "the record must be observable in_progress while the sync runs")
	require.True(t, strings.HasPrefix(notes, "repair:"), "record is not marked as a repair: %q", notes)
	require.Equal(t, ownerId, triggeredBy, "triggeredBy must be the ORIGINATING human, not the relaying node")
	require.Equal(t, ownerEmail, createdBy, "the row's createdBy must be the ORIGINATING human, not the relaying node")

	// The kick-off is audited once, as the originating human, and the ack
	// names the event.
	events := audit.all()
	require.Len(t, events, 1)
	require.Equal(t, "deployment_console_repair", events[0].Action)
	require.Equal(t, ownerId, events[0].ActorUserId)
	require.Equal(t, events[0].CorrelationId, action.GetAuditEventId())

	// (2) Let ArgoCD "finish", then poll the record exactly as the portal
	// does until it reaches a terminal state.
	exec.release()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, _, _, _, _ = readDeployment(t, readCtx, eng, recordID)
		if status == "succeeded" || status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("repair record %s never left in_progress (status %q after 20s): the watcher's terminal write did not land", recordID, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, "succeeded", status, "the observed Synced+Healthy operation must resolve the record to succeeded")

	// (3) The terminal transition preserved the record's identity: same
	// repair note, same operator -- updateDeploymentStatus is a read-merge
	// update, and the watcher wrote it as the caller.
	_, triggeredBy, notes, createdBy, _ = readDeployment(t, readCtx, eng, recordID)
	require.True(t, strings.HasPrefix(notes, "repair:"), "terminal row lost the repair note: %q", notes)
	require.Equal(t, ownerId, triggeredBy)
	require.Equal(t, ownerEmail, createdBy, "the terminal write must be attributed to the initiating owner")
}
