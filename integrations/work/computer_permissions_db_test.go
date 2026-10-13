package work

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func TestComputerPermissionDBTwoReplicasConsumeOnceAndRevokeAlways(t *testing.T) {
	a := openActsDB(t)
	runID := a.openRun(t, nil)
	peerDB := workHeadsPeer(t, a.db)
	b := New(dispatchDBEngine(t, peerDB), testLogger(), func() *bun.DB { return peerDB })
	b.SetGateDB(testGates(t))
	person := actorCtx(a.owner)
	row, req := computerPermissionFixture()
	req.OwnerID = a.owner
	req.RunID = runID
	subject := rowMap(row, "subject")
	create := func(mode string) string {
		id := newRowId(approvalConcept)
		a.write(t, "createWorkApproval", map[string]any{"approvalId": id, "runId": runID, "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": rfc(time.Now()), "expiresAt": rfc(time.Now().Add(time.Hour))})
		// Warm the other replica's pending-row cache before the actual decision.
		_, err := b.store().query(person, "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
		if err != nil {
			t.Fatal(err)
		}
		a.write(t, "decideWorkApproval", map[string]any{"approvalId": id, "decision": "approved", "decidedBy": a.owner, "decidedAt": rfc(time.Now()), "answer": map[string]any{"permissionMode": mode}})
		return id
	}
	once := create("once")
	if found, err := b.Find(person, req); err != nil || found == nil {
		t.Fatalf("stale negative read: %v %v", found, err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for n, replica := range []*Integration{a.i, b} {
		wg.Add(1)
		go func(n int, i *Integration) {
			defer wg.Done()
			_, errs[n] = i.Claim(person, once, req, "worker", "machine")
		}(n, replica)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("once must admit exactly one replica: %v", errs)
	}
	rows, err := b.store().query(memql.ContextWithFreshRead(person), "query "+call("workApprovalForOwner", map[string]any{"approvalId": once}))
	if err != nil || len(rows) != 1 || rowString(rows[0], "permissionUsedAt") == "" {
		t.Fatalf("missing durable consumption: %v %v", rows, err)
	}
	always := create("always")
	// An Always decision is not a one-hour grant: only its pending question expires.
	future := time.Now().Add(2 * time.Hour)
	a.i.SetNow(func() time.Time { return future })
	b.SetNow(func() time.Time { return future })
	for _, replica := range []*Integration{a.i, b} {
		if _, err := replica.Claim(person, always, req, "worker", "machine"); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := b.store().query(memql.ContextWithFreshRead(person), "query workComputerPermissions()")
	if err != nil || len(listed) != 1 {
		t.Fatalf("active saved list: %v %v", listed, err)
	}
	for _, tc := range []struct{ registration, machine string }{{"other", "machine"}, {"worker", "other"}} {
		if _, err := b.Claim(person, always, req, tc.registration, tc.machine); err == nil {
			t.Fatal("machine mismatch admitted")
		}
	}
	if _, err := b.handleRevokeComputerPermission(actorCtx("someone-else"), map[string]any{"permissionId": always}, 0); err == nil {
		t.Fatal("another owner revoked permission")
	}
	if _, err := a.i.handleRevokeComputerPermission(person, map[string]any{"permissionId": always}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(person, always, req, "worker", "machine"); err == nil {
		t.Fatal("stale other replica ignored revocation")
	}
	if _, err := b.handleRevokeComputerPermission(person, map[string]any{"permissionId": always}, 0); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	listed, err = b.store().query(memql.ContextWithFreshRead(person), "query workComputerPermissions()")
	if err != nil || len(listed) != 0 {
		t.Fatalf("revoked remains listed: %v %v", listed, err)
	}
	cancelled, cancel := context.WithCancel(person)
	cancel()
	if _, err := b.Claim(cancelled, always, req, "worker", "machine"); err == nil {
		t.Fatal("cancelled request claimed")
	}
}

// The engine may finish a bounded durable write after its caller disconnects.
// Its receipt gate must survive that disconnect until the actual commit lands.
type permissionCommitEngine struct {
	Engine
	mutation string
	entered  chan struct{}
	proceed  chan struct{}
	cancel   context.CancelFunc
}

func (e *permissionCommitEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "mutation "+e.mutation+"(") {
		e.cancel()
		close(e.entered)
		<-e.proceed
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("durable commit inherited cancellation: %w", err)
		}
	}
	return e.Engine.Execute(ctx, q)
}
func TestComputerPermissionDBCancelledCallerRetainsClaimAndDecisionGate(t *testing.T) {
	for _, mutation := range []string{"useWorkComputerPermission", "decideWorkApproval"} {
		t.Run(mutation, func(t *testing.T) {
			a := openActsDB(t)
			run := a.openRun(t, nil)
			row, req := computerPermissionFixture()
			req.OwnerID = a.owner
			req.RunID = run
			id := newRowId(approvalConcept)
			subject := rowMap(row, "subject")
			a.write(t, "createWorkApproval", map[string]any{"approvalId": id, "runId": run, "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": rfc(time.Now()), "expiresAt": rfc(time.Now().Add(time.Hour))})
			if mutation == "useWorkComputerPermission" {
				a.write(t, "decideWorkApproval", map[string]any{"approvalId": id, "decision": "approved", "decidedBy": a.owner, "decidedAt": rfc(time.Now()), "answer": map[string]any{"permissionMode": "once"}})
			}
			peerDB := workHeadsPeer(t, a.db)
			b := New(dispatchDBEngine(t, peerDB), testLogger())
			b.SetGateDB(testGates(t))
			person := actorCtx(a.owner)
			ctx, cancel := context.WithCancel(person)
			defer cancel()
			wrapper := &permissionCommitEngine{Engine: a.eng, mutation: mutation, entered: make(chan struct{}), proceed: make(chan struct{}), cancel: cancel}
			a.i.engine = wrapper
			done := make(chan error, 1)
			go func() {
				if mutation == "useWorkComputerPermission" {
					_, err := a.i.Claim(ctx, id, req, "worker", "machine")
					done <- err
				} else {
					_, err := a.i.handleDecideApproval(ctx, map[string]any{"approvalId": id, "decision": "approved", "answer": map[string]any{"permissionMode": "once"}}, 0)
					done <- err
				}
			}()
			select {
			case <-wrapper.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("commit not reached")
			}
			other := make(chan error, 1)
			go func() { _, err := b.Claim(person, id, req, "worker", "machine"); other <- err }()
			select {
			case err := <-other:
				t.Fatalf("other replica entered before detached commit: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(wrapper.proceed)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			err := <-other
			if mutation == "useWorkComputerPermission" && err == nil {
				t.Fatal("second replica reused consumed once")
			}
			if mutation == "decideWorkApproval" && err != nil {
				t.Fatalf("decision did not become visible after commit: %v", err)
			}
		})
	}
}

// A typed permission approves one tool request. It is not the generic failed
// step Retry action, whose rerun explicitly permits repeating side effects.
func TestComputerPermissionDBApprovalContinuesWithoutBlanketRerun(t *testing.T) {
	a := openActsDB(t)
	runID := a.openRun(t, map[string]any{"head": map[string]any{"done": map[string]any{"runId": "", "version": 1}}})
	a.writeVersion(t, runID, runID, "done", 0, 1, nil, "saved", nil)
	a.writeVersion(t, runID, runID, "section", 1, 1, nil, "", map[string]any{"status": "waiting"})
	row, _ := computerPermissionFixture()
	subject := rowMap(row, "subject")
	approval := newRowId(approvalConcept)
	a.write(t, "createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "stepKey": "section", "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": rfc(time.Now()), "expiresAt": rfc(time.Now().Add(time.Hour))})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"done", "section"}, "spent": map[string]any{"modelCalls": 3}, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approval, "since": rfc(time.Now().Add(-time.Second))}})
	peerDB := workHeadsPeer(t, a.db)
	b := New(dispatchDBEngine(t, peerDB), testLogger(), func() *bun.DB { return peerDB })
	b.SetGateDB(testGates(t))
	b.admitRow = memql.AdmitSourceRow
	if _, err := b.handleDecideApproval(actorCtx(a.owner), map[string]any{"approvalId": approval, "decision": "approved", "answer": map[string]any{"permissionMode": "once"}}, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := b.store().query(memql.ContextWithFreshRead(actorCtx(a.owner)), "query "+call("workRunForOwner", map[string]any{"runId": runID}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("run read: %v %v", rows, err)
	}
	got := rows[0]
	if rowString(got, "status") != "running" || !samePermissionID(rowString(got, "humanResumeId"), approval) || len(rowMap(got, "rerun")) != 0 || len(rowStringSlice(got, "staleSteps")) != 0 {
		t.Fatalf("typed approval became a blanket rerun: %v", got)
	}
	if rowInt(rowMap(got, "spent"), "modelCalls") != 3 {
		t.Fatalf("approval reset spend: %v", got["spent"])
	}
}

func TestComputerPermissionDBDefaultTimeoutKeepsOneDurableClaim(t *testing.T) {
	for _, explicitApproval := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-approval-%v", explicitApproval), func(t *testing.T) {
			a := openActsDB(t)
			runID := a.openRun(t, nil)
			peerDB := workHeadsPeer(t, a.db)
			b := New(dispatchDBEngine(t, peerDB), testLogger(), func() *bun.DB { return peerDB })
			b.SetGateDB(testGates(t))
			person := actorCtx(a.owner)
			approved := map[string]any{"cmd": "pwd", "cwd": "/fixture/repository"}
			requested := map[string]any{"cmd": "pwd", "cwd": "/fixture/repository"}
			if explicitApproval {
				approved["timeoutSec"] = 60
			} else {
				requested["timeoutSec"] = 60
			}
			hash, err := permission.Hash("workerHost", "exec", approved)
			if err != nil {
				t.Fatal(err)
			}
			scope := permission.Scope{Version: 1, Kind: "exact_command", RegistrationID: "worker", MachineID: "machine", MachineLabel: "Test machine", Tool: "workerHost", Action: "exec", Command: "pwd", CWD: "/fixture/repository", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}
			subject := map[string]any{"agentId": "agent", "computerPermission": scope.Map()}
			id := newRowId(approvalConcept)
			a.write(t, "createWorkApproval", map[string]any{"approvalId": id, "runId": runID, "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": rfc(time.Now()), "expiresAt": rfc(time.Now().Add(time.Hour))})
			req := permission.Request{OwnerID: a.owner, AgentID: "agent", RunID: runID, Tool: "workerHost", Action: "exec", Args: requested}
			if found, err := b.Find(person, req); err != nil || found != nil {
				t.Fatalf("pending receipt admitted: %v %v", found, err)
			}
			if _, err := a.i.handleDecideApproval(person, map[string]any{"approvalId": id, "decision": "approved", "answer": map[string]any{"permissionMode": "once"}}, 0); err != nil {
				t.Fatal(err)
			}
			if found, err := b.Find(person, req); err != nil || found == nil {
				t.Fatalf("peer refused the equivalent default: %v %v", found, err)
			}
			for _, changed := range []string{"owner", "agent", "run", "machine"} {
				other := req
				machine := "machine"
				switch changed {
				case "owner":
					other.OwnerID = "other"
				case "agent":
					other.AgentID = "other"
				case "run":
					other.RunID = a.openRun(t, nil)
				case "machine":
					machine = "other"
				}
				if _, err := b.Claim(person, id, other, "worker", machine); err == nil {
					t.Fatalf("changed %s claimed permission", changed)
				}
			}
			// The two encodings compete for the SAME durable once receipt.
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for n, replica := range []*Integration{a.i, b} {
				wg.Add(1)
				go func(n int, replica *Integration) {
					defer wg.Done()
					request := req
					if n == 0 {
						request.Args = approved
					}
					_, errs[n] = replica.Claim(person, id, request, "worker", "machine")
				}(n, replica)
			}
			wg.Wait()
			if (errs[0] == nil) == (errs[1] == nil) {
				t.Fatalf("exactly one peer must consume: %v", errs)
			}
			if found, err := b.Find(person, req); err != nil || found != nil {
				t.Fatalf("consumed receipt returned: %v %v", found, err)
			}
			rows, err := b.store().query(memql.ContextWithFreshRead(person), "query "+call("workApprovalForOwner", map[string]any{"approvalId": id}))
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rowString(rows[0], "permissionUsedAt") == "" || rowString(rowMap(rowMap(rows[0], "subject"), "computerPermission"), "argsHash") != hash {
				t.Fatal("claim changed or lost original receipt")
			}
		})
	}
}
