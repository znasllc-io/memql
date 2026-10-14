package work

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func fullComputerSubject() map[string]any {
	row, _ := computerPermissionFixture()
	s := rowMap(row, "subject")
	s["requireLabels"] = map[string]string{"machineId": "machine"}
	s["intent"] = "Inspect directory"
	s["summary"] = "Read /docs on Laptop"
	s["target"] = "Laptop"
	return s
}
func TestComputerPermissionDBFullSubjectAcrossReplicas(t *testing.T) {
	for _, mode := range []string{"once", "always"} {
		t.Run(mode, func(t *testing.T) {
			a := openActsDB(t)
			run := a.openRun(t, nil)
			peer := workHeadsPeer(t, a.db)
			b := New(dispatchDBEngine(t, peer), testLogger(), func() *bun.DB { return peer })
			b.SetGateDB(testGates(t))
			person := actorCtx(a.owner)
			ctx := common.ContextWithRun(person, common.RunContext{RunId: run, OwnerUserId: a.owner, StepKey: "read"})
			id, err := a.i.AskComputerScope(ctx, a.owner, run, fullComputerSubject())
			require.NoError(t, err)
			rows, err := b.store().query(memql.ContextWithFreshRead(person), "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, workstate.ArtifactHash(rowMap(rows[0], "subject")), rowString(rows[0], "artifactHash"))
			answer := map[string]any{"permissionMode": mode, "permissionHash": rowString(rows[0], "artifactHash")}
			args := map[string]any{"approvalId": id, "decision": "approved", "answer": answer}
			_, err = b.handleDecideApproval(person, args, 0)
			require.NoError(t, err)
			// A lost response is recovered through another replica, without rewriting
			// the decision or starting a second execution.
			_, err = a.i.handleDecideApproval(person, args, 0)
			require.NoError(t, err)
			_, req := computerPermissionFixture()
			req.OwnerID = a.owner
			req.RunID = run
			req.MachineID = "machine"
			if mode == "always" {
				b.SetNow(func() time.Time { return time.Now().Add(24 * 365 * time.Hour) })
				req.RunID = a.openRun(t, nil) // a later conversation still has the same saved scope
			}
			grant, err := b.Find(person, req)
			require.NoError(t, err)
			require.NotNil(t, grant)
			_, err = b.Claim(person, id, req, "worker", "machine")
			require.NoError(t, err)
			if mode == "once" {
				_, err = a.i.Claim(person, id, req, "worker", "machine")
				require.Error(t, err)
			} else {
				_, err = a.i.Claim(person, id, req, "worker", "machine")
				require.NoError(t, err)
				_, err = a.i.Claim(person, id, req, "new-enrollment", "machine")
				require.Error(t, err)
				_, err = a.i.handleRevokeComputerPermission(person, map[string]any{"permissionId": id}, 0)
				require.NoError(t, err)
				grant, err = b.Find(person, req)
				require.NoError(t, err)
				require.Nil(t, grant)
			}
		})
	}
}
func TestComputerPermissionRepairProof(t *testing.T) {
	s := fullComputerSubject()
	old := workstate.ArtifactHash(s)
	stored, err := workstate.NormalizeApprovalSubject(s)
	require.NoError(t, err)
	row := map[string]any{"kind": "scopeElevation", "decision": "", "subject": stored, "artifactHash": old}
	require.True(t, repairableComputerFingerprint(row))
	for _, field := range []string{"agentId", "intent", "summary", "target"} {
		t.Run(field, func(t *testing.T) {
			copy, _ := workstate.NormalizeApprovalSubject(stored)
			copy[field] = "changed"
			row["subject"] = copy
			require.False(t, repairableComputerFingerprint(row))
		})
	}
	for _, field := range []string{"directory", "machineId", "registrationId", "action", "argsHash"} {
		t.Run(field, func(t *testing.T) {
			copy, _ := workstate.NormalizeApprovalSubject(stored)
			copy["computerPermission"].(map[string]any)[field] = "changed"
			row["subject"] = copy
			require.False(t, repairableComputerFingerprint(row))
		})
	}
	row["subject"] = stored
	row["decision"] = "approved"
	require.False(t, repairableComputerFingerprint(row))
}

// Fail only the release write: the approval commit really lands in Postgres.
type interruptedComputerResume struct {
	Engine
	fail bool
}

func (e *interruptedComputerResume) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if e.fail && strings.HasPrefix(q, "mutation updateWorkRun") {
		return nil, fmt.Errorf("synthetic release interruption")
	}
	return e.Engine.Execute(ctx, q)
}

func TestComputerPermissionDBRecoverInterruptedResume(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	run := a.openRun(t, nil)
	a.writeVersion(t, run, run, "read", 0, 1, nil, "", map[string]any{"status": "running"})
	person := actorCtx(a.owner)
	ctx := common.ContextWithRun(person, common.RunContext{RunId: run, OwnerUserId: a.owner, StepKey: "read"})
	id, err := a.i.AskComputerScope(ctx, a.owner, run, fullComputerSubject())
	require.NoError(t, err)
	a.i.engine = &interruptedComputerResume{Engine: a.eng, fail: true}
	args := map[string]any{"approvalId": id, "decision": "approved", "answer": map[string]any{"permissionMode": "once"}}
	result, err := a.i.handleDecideApproval(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, false, decodeReply(t, result)["runResumed"])
	require.Equal(t, "waiting", rowString(a.runRow(t, run), "status"))
	result, err = b.i.handleDecideApproval(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, result)["runResumed"])
	require.Equal(t, "running", rowString(a.runRow(t, run), "status"))
	args["answer"] = map[string]any{"permissionMode": "always"}
	_, err = b.i.handleDecideApproval(person, args, 0)
	require.Error(t, err)
}
func TestComputerPermissionDBRepairStuckRequestAndExpiry(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	run := a.openRun(t, nil)
	a.writeVersion(t, run, run, "read", 0, 1, nil, "", map[string]any{"status": "running"})
	s := fullComputerSubject()
	id := newRowId(approvalConcept)
	a.write(t, "createWorkApproval", map[string]any{"approvalId": id, "runId": run, "stepKey": "read", "kind": "scopeElevation", "subject": s, "artifactHash": workstate.ArtifactHash(s), "requestedAt": rfc(time.Now().Add(-2 * time.Hour)), "expiresAt": rfc(time.Now().Add(-time.Hour))})
	a.write(t, "updateWorkRun", map[string]any{"runId": run, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": id}})
	a.write(t, "createWorkerRegistration", map[string]any{"registrationId": "worker", "identityId": "identity", "name": "Laptop", "capabilities": []string{"HEADLESS"}, "labels": map[string]any{"machineId": "machine"}, "concurrency": map[string]any{}, "registeredAt": rfc(time.Now())})
	person := actorCtx(a.owner)
	result, err := b.i.handleDecideApproval(person, map[string]any{"approvalId": id, "decision": "approved", "answer": map[string]any{"permissionMode": "once"}}, 0)
	require.NoError(t, err)
	require.Equal(t, "review", decodeReply(t, result)["permissionState"])
	_, err = b.i.handleRefreshComputerPermission(actorCtx("different-owner"), map[string]any{"approvalId": id}, 0)
	require.Error(t, err)
	result, err = b.i.handleRefreshComputerPermission(person, map[string]any{"approvalId": id}, 0)
	require.NoError(t, err)
	require.Equal(t, "review_ready", decodeReply(t, result)["permissionState"])
	rows := a.query(t, a.owner, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
	require.Len(t, rows, 1)
	require.Empty(t, rowString(rows[0], "decision"))
	require.Equal(t, "waiting", rowString(a.runRow(t, run), "status"))
	require.Equal(t, workstate.ArtifactHash(rowMap(rows[0], "subject")), rowString(rows[0], "artifactHash"))
	normalized, _ := workstate.NormalizeApprovalSubject(s)
	actual := rowMap(rows[0], "subject")
	require.Equal(t, true, actual["requiresReviewHash"])
	delete(actual, "requiresReviewHash")
	delete(actual, "reviewedAt")
	require.Equal(t, normalized, actual)
	_, err = b.i.handleDecideApproval(person, map[string]any{"approvalId": id, "decision": "approved", "answer": map[string]any{"permissionMode": "once", "permissionHash": rowString(rows[0], "artifactHash")}}, 0)
	require.NoError(t, err)
	require.Equal(t, "running", rowString(a.runRow(t, run), "status"))
}

func TestComputerPermissionDecisionStates(t *testing.T) {
	row, _ := computerPermissionFixture()
	row["decision"] = ""
	row["expiresAt"] = rfc(time.Now().Add(time.Hour))
	hash := rowString(row, "artifactHash")
	answer := map[string]any{"permissionMode": "always", "permissionHash": hash}
	state, _ := computerDecisionState(row, answer, time.Now())
	require.Empty(t, state)
	answer["permissionHash"] = "not-the-request-seen"
	state, _ = computerDecisionState(row, answer, time.Now())
	require.Equal(t, "changed", state)
	answer["permissionHash"] = hash
	row["expiresAt"] = rfc(time.Now().Add(-time.Hour))
	state, _ = computerDecisionState(row, answer, time.Now())
	require.Equal(t, "expired", state)
	row["decision"] = "approved"
	row["answer"] = answer
	state, _ = computerDecisionState(row, answer, time.Now())
	require.Empty(t, state, "Always never inherits the review deadline")
	row["permissionRevokedAt"] = rfc(time.Now())
	state, _ = computerDecisionState(row, answer, time.Now())
	require.Equal(t, "revoked", state)
}

// A renewal is a new review, not a transfer of the old decision. This includes
// an already-recorded decision whose continuation failed before the deadline.
func TestComputerPermissionDBRenewalRequiresFreshReview(t *testing.T) {
	for _, scenario := range []string{"expired once", "revoked always", "new enrollment"} {
		t.Run(scenario, func(t *testing.T) {
			a, b := openActsDB(t), openActsDB(t)
			b.owner = a.owner
			person := actorCtx(a.owner)
			run := a.openRun(t, nil)
			a.writeVersion(t, run, run, "read", 0, 1, nil, "", map[string]any{"status": "running"})
			ctx := common.ContextWithRun(person, common.RunContext{RunId: run, OwnerUserId: a.owner, StepKey: "read"})
			id, err := a.i.AskComputerScope(ctx, a.owner, run, fullComputerSubject())
			require.NoError(t, err)
			read := func() map[string]any {
				rows := a.query(t, a.owner, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
				require.Len(t, rows, 1)
				return rows[0]
			}
			oldHash := rowString(read(), "artifactHash")
			mode := "once"
			if scenario == "revoked always" {
				mode = "always"
			}
			args := map[string]any{"approvalId": id, "decision": "approved", "answer": map[string]any{"permissionMode": mode, "permissionHash": oldHash}}
			registration := "worker"
			if scenario == "new enrollment" {
				registration = "replacement-worker"
			} else {
				a.i.engine = &interruptedComputerResume{Engine: a.eng, fail: true}
				result, err := a.i.handleDecideApproval(person, args, 0)
				require.NoError(t, err)
				require.Equal(t, false, decodeReply(t, result)["runResumed"])
				if scenario == "revoked always" {
					_, err = b.i.handleRevokeComputerPermission(person, map[string]any{"permissionId": id}, 0)
					require.NoError(t, err)
				} else {
					b.i.SetNow(func() time.Time { return time.Now().Add(2 * time.Hour) })
				}
			}
			a.write(t, "createWorkerRegistration", map[string]any{"registrationId": registration, "identityId": "identity", "name": "Laptop", "capabilities": []string{"HEADLESS"}, "labels": map[string]any{"machineId": "machine"}, "concurrency": map[string]any{}, "registeredAt": rfc(time.Now())})
			result, err := b.i.handleRefreshComputerPermission(person, map[string]any{"approvalId": id}, 0)
			require.NoError(t, err)
			require.Equal(t, "review_ready", decodeReply(t, result)["permissionState"])
			fresh := read()
			require.Empty(t, fresh["decision"])
			require.Empty(t, fresh["answer"])
			require.NotEqual(t, oldHash, fresh["artifactHash"])
			require.True(t, samePermissionID(registration, rowString(rowMap(rowMap(fresh, "subject"), "computerPermission"), "registrationId")))
			for _, answer := range []map[string]any{{"permissionMode": mode}, {"permissionMode": mode, "permissionHash": oldHash}} {
				args["answer"] = answer
				result, err = b.i.handleDecideApproval(person, args, 0)
				require.NoError(t, err)
				require.Equal(t, "changed", decodeReply(t, result)["permissionState"])
				require.Empty(t, read()["decision"])
			}
			args["answer"] = map[string]any{"permissionMode": mode, "permissionHash": fresh["artifactHash"]}
			result, err = b.i.handleDecideApproval(person, args, 0)
			require.NoError(t, err)
			require.Equal(t, true, decodeReply(t, result)["runResumed"])
			require.Equal(t, "approved", read()["decision"])
		})
	}
}
