package work

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestExpiredScopeIsRenewedOnceAndAnActiveGrantIsReused(t *testing.T) {
	i, eng := newTestIntegration(t)
	// Production locks by key, every key of one call in one acquisition; the
	// renewal's locks must not be replaced by a single process mutex here.
	locks := map[string]*sync.Mutex{}
	var mutex sync.Mutex
	i.decisionGate = func(_ context.Context, keys ...string) (func(), error) {
		var held []*sync.Mutex
		for _, key := range keys {
			mutex.Lock()
			m := locks[key]
			if m == nil {
				m = &sync.Mutex{}
				locks[key] = m
			}
			mutex.Unlock()
			m.Lock()
			held = append(held, m)
		}
		return func() {
			for n := len(held) - 1; n >= 0; n-- {
				held[n].Unlock()
			}
		}, nil
	}
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r1", GoalId: "g1", OwnerUserId: "u-alice", StepKey: "reason"})
	eng.reply("workRunForOwner", map[string]any{"id": "r1", "ownerUserId": "u-alice", "status": "running"})
	subject := map[string]any{"scope": "observe", "summary": "Read the requested file", "agentId": "agent"}
	id, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)
	if err != nil {
		t.Fatal(err)
	}
	eng.replyWhen("workApprovalForOwner", id, map[string]any{"id": id, "decision": "approved", "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)})
	renewed, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)
	if err != nil || renewed == id {
		t.Fatalf("expired grant reused: %s %v", renewed, err)
	}
	eng.replyWhen("workApprovalForOwner", renewed, map[string]any{"id": renewed, "decision": "approved", "expiresAt": testNow.Add(time.Hour).Format(time.RFC3339Nano)})
	again, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)
	if err != nil || again != renewed || len(eng.callsTo("createWorkApproval")) != 2 {
		t.Fatalf("grant renewal duplicated: %s %v", again, err)
	}
}
func TestFeedbackValidatesRecordedAnswersAgainstTheQuestion(t *testing.T) {
	options := []map[string]any{{"label": "PDF", "value": "pdf"}, {"label": "Text", "value": "txt"}}
	for _, tc := range []struct {
		kind, decision string
		answer         map[string]any
		valid          bool
	}{
		{"choice", "answered", map[string]any{"text": "A spreadsheet instead"}, true},
		{"multi", "answered", map[string]any{"text": "A spreadsheet instead"}, true},
		{"choice", "answered", map[string]any{"text": "Other", "value": "invented"}, false},
		{"text", "answered", map[string]any{"text": "PDF please"}, true}, {"text", "answered", map[string]any{"text": " "}, false},
		{"choice", "answered", map[string]any{"value": "pdf"}, true}, {"choice", "approved", nil, false}, {"choice", "answered", map[string]any{"value": "invented"}, false},
		{"multi", "answered", map[string]any{"values": []string{"pdf", "txt"}}, true}, {"multi", "answered", map[string]any{"values": []string{"pdf", "pdf"}}, false},
	} {
		err := validateFeedbackAnswer(map[string]any{"kind": tc.kind, "options": options}, tc.decision, tc.answer)
		if (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	i, eng := newTestIntegration(t)
	subject := map[string]any{"scope": "full"}
	eng.reply("workApprovalsForOwner", map[string]any{"id": "q", "runId": "r", "ownerUserId": "u-alice", "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)})
	if _, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{"approvalId": "q", "decision": "approved"}, 0); err == nil || len(eng.callsTo("decideWorkApproval")) != 0 {
		t.Fatal("expired computer access was approved")
	}
}

// replyFirst answers `name` with these rows when the rendered query contains
// `contains`, ahead of every reply added before: a row that changed.
func (e *recordingEngine) replyFirst(name, contains string, rows ...map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.conditional = append([]conditionalReply{{name: name, contains: contains, rows: rows}}, e.conditional...)
}

// TestARenewalLocksItsChainInOneCallAndWalksItAgainUnderTheLocks: the chain's
// receipts are locked with ONE call of the decision gate, in chain order, and the
// chain is walked again under them. A grant that expired between the two walks
// takes the chain past the keys held, and the renewal walks and locks it again
// rather than park on a receipt it does not hold.
func TestARenewalLocksItsChainInOneCallAndWalksItAgainUnderTheLocks(t *testing.T) {
	i, eng := newTestIntegration(t)
	subject := map[string]any{"scope": "observe", "summary": "Read the requested file", "agentId": "agent"}
	chain := renewalChainIDs("r1", "reason", subject, 2)
	eng.reply("workRunForOwner", map[string]any{"id": "r1", "ownerUserId": "u-alice", "status": "running"})
	eng.replyWhen("workApprovalForOwner", chain[0], map[string]any{"id": chain[0], "decision": "approved", "expiresAt": testNow.Add(time.Second).Format(time.RFC3339Nano)})
	var calls [][]string
	i.decisionGate = func(_ context.Context, keys ...string) (func(), error) {
		calls = append(calls, append([]string(nil), keys...))
		if len(calls) == 1 {
			// The grant expires while the first walk's locks are being taken.
			eng.replyFirst("workApprovalForOwner", chain[0], map[string]any{"id": chain[0], "decision": "approved", "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)})
		}
		return func() {}, nil
	}
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r1", GoalId: "g1", OwnerUserId: "u-alice", StepKey: "reason"})

	got, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)

	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !slices.Equal(calls[0], chain[:1]) || !slices.Equal(calls[1], chain) {
		t.Fatalf("the decision gate was called with %v, want [%v] and then %v in one call", calls, chain[:1], chain)
	}
	if got != chain[1] {
		t.Errorf("the renewal answered %s, want the successor %s", got, chain[1])
	}
	if creates := eng.callsTo("createWorkApproval"); len(creates) != 1 || !strings.Contains(creates[0].Query, chain[1]) {
		t.Errorf("the renewal created %v, want exactly the successor", creates)
	}
}

// TestARenewalWhoseChainKeepsGrowingGivesUp: a chain that is longer under its
// locks every time it is walked is not chased for ever; the renewal stops and asks
// for another try, having written nothing.
func TestARenewalWhoseChainKeepsGrowingGivesUp(t *testing.T) {
	i, eng := newTestIntegration(t)
	subject := map[string]any{"scope": "observe", "summary": "Read the requested file", "agentId": "agent"}
	chain := renewalChainIDs("r1", "reason", subject, renewalLockAttempts+2)
	eng.reply("workRunForOwner", map[string]any{"id": "r1", "ownerUserId": "u-alice", "status": "running"})
	expired := func(id string) map[string]any {
		return map[string]any{"id": id, "decision": "approved", "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)}
	}
	eng.replyWhen("workApprovalForOwner", chain[0], map[string]any{"id": chain[0], "decision": "approved", "expiresAt": testNow.Add(time.Second).Format(time.RFC3339Nano)})
	calls := 0
	i.decisionGate = func(_ context.Context, keys ...string) (func(), error) {
		calls++
		// The last receipt the walk reached expires, and the one after it appears
		// already expired: every walk under the locks is one longer.
		last := keys[len(keys)-1]
		eng.replyFirst("workApprovalForOwner", last, expired(last))
		return func() {}, nil
	}
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r1", GoalId: "g1", OwnerUserId: "u-alice", StepKey: "reason"})

	_, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)

	if err == nil || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("a chain that kept growing answered %v, want to be asked again", err)
	}
	if calls != renewalLockAttempts {
		t.Errorf("the chain was locked %d times, want %d", calls, renewalLockAttempts)
	}
	if len(eng.callsTo("createWorkApproval")) != 0 || len(eng.callsTo("updateWorkRun")) != 0 {
		t.Errorf("a renewal that gave up wrote: %s", eng.summary())
	}
}
