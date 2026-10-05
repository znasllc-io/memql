package work

import (
	"context"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"sync"
	"testing"
	"time"
)

func TestExpiredScopeIsRenewedOnceAndAnActiveGrantIsReused(t *testing.T) {
	i, eng := newTestIntegration(t)
	// Production locks by key; nested renewal locks must not be replaced by
	// a single process mutex in this fixture.
	locks := map[string]*sync.Mutex{}
	var mutex sync.Mutex
	i.decisionGate = func(_ context.Context, key string) (func(), error) {
		mutex.Lock()
		m := locks[key]
		if m == nil {
			m = &sync.Mutex{}
			locks[key] = m
		}
		mutex.Unlock()
		m.Lock()
		return m.Unlock, nil
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
