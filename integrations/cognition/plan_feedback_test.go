package cognition

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/events"
)

// ---- announce-half: event extraction + message rendering ----

func TestPlanFromUpdateEvent_FlattenedAwaitingFeedback(t *testing.T) {
	ev := events.Event{Payload: map[string]any{
		"nodeId":         "v1:planner:plan:abc",
		"spaceId":        "v1:cognition:space:s1",
		"status":         "awaitingFeedback",
		"feedbackReason": "scope_elevation_required",
		"requestedBy":    "user-1",
		"feedbackRequest": map[string]any{
			"question": "May I read your Downloads folder?",
			"options": []any{
				map[string]any{"label": "Allow", "value": "approve"},
				map[string]any{"label": "Deny", "value": "decline"},
			},
		},
	}}
	plan := planFromUpdateEvent(ev)
	if plan == nil {
		t.Fatal("expected a plan from a flattened awaitingFeedback event")
	}
	if plan.ID != "v1:planner:plan:abc" || plan.SpaceId != "v1:cognition:space:s1" {
		t.Fatalf("wrong id/space: %+v", plan)
	}
	if plan.Status != "awaitingFeedback" {
		t.Fatalf("wrong status: %q", plan.Status)
	}
	if plan.Question != "May I read your Downloads folder?" {
		t.Fatalf("wrong question: %q", plan.Question)
	}
	if len(plan.Options) != 2 {
		t.Fatalf("expected 2 options, got %d", len(plan.Options))
	}
	if plan.FeedbackReason != "scope_elevation_required" {
		t.Fatalf("wrong reason: %q", plan.FeedbackReason)
	}
}

func TestPlanFromUpdateEvent_NestedPayload(t *testing.T) {
	ev := events.Event{Payload: map[string]any{
		"id": "v1:planner:plan:nested",
		"payload": map[string]any{
			"spaceId":         "v1:cognition:space:s2",
			"status":          "awaitingFeedback",
			"feedbackRequest": map[string]any{"question": "Which format -- PDF or DOCX?"},
		},
	}}
	plan := planFromUpdateEvent(ev)
	if plan == nil {
		t.Fatal("expected a plan from a nested-payload event")
	}
	if plan.ID != "v1:planner:plan:nested" {
		t.Fatalf("wrong id: %q", plan.ID)
	}
	if plan.SpaceId != "v1:cognition:space:s2" || plan.Question == "" {
		t.Fatalf("nested payload not read: %+v", plan)
	}
}

func TestPlanFromUpdateEvent_NonFeedbackIsIgnoredByHandlerPredicate(t *testing.T) {
	// planFromUpdateEvent itself returns the projection; the handler's
	// status/question predicate is what filters. Verify the projection
	// faithfully reflects a running plan so the predicate can reject it.
	ev := events.Event{Payload: map[string]any{
		"nodeId":  "v1:planner:plan:run",
		"spaceId": "v1:cognition:space:s3",
		"status":  "running",
	}}
	plan := planFromUpdateEvent(ev)
	if plan == nil {
		t.Fatal("expected a projection even for a running plan")
	}
	if plan.Status == planStatusAwaitingFeedback {
		t.Fatalf("running plan must not project awaitingFeedback")
	}
	if strings.TrimSpace(plan.Question) != "" {
		t.Fatalf("running plan should carry no question")
	}
}

func TestPlanFromUpdateEvent_NilAndEmpty(t *testing.T) {
	if planFromUpdateEvent(events.Event{}) != nil {
		t.Fatal("nil payload must produce nil plan")
	}
	if planFromUpdateEvent(events.Event{Payload: map[string]any{"status": "awaitingFeedback"}}) != nil {
		t.Fatal("missing id must produce nil plan")
	}
}

func TestBuildFeedbackAnnouncement_QuestionAndOptions(t *testing.T) {
	plan := &awaitingFeedbackPlan{
		Question: "Approve the budget increase?",
		Options: []map[string]any{
			{"label": "Approve", "value": "approve"},
			{"label": "Decline", "value": "decline"},
		},
	}
	msg := buildFeedbackAnnouncement(plan)
	if !strings.Contains(msg, "Approve the budget increase?") {
		t.Fatalf("announcement must surface the question verbatim: %q", msg)
	}
	if !strings.Contains(msg, "Approve / Decline") {
		t.Fatalf("announcement must list the option labels: %q", msg)
	}
	if !strings.Contains(msg, "reply here") {
		t.Fatalf("announcement must invite a conversational reply: %q", msg)
	}
}

func TestBuildFeedbackAnnouncement_NoOptions(t *testing.T) {
	plan := &awaitingFeedbackPlan{Question: "What title should the report have?"}
	msg := buildFeedbackAnnouncement(plan)
	if !strings.Contains(msg, "What title should the report have?") {
		t.Fatalf("question missing: %q", msg)
	}
	if strings.Contains(msg, "(") {
		t.Fatalf("free-text question should not render an options list: %q", msg)
	}
}

// ---- result-parsing helpers ----

func TestNodesFromResult(t *testing.T) {
	result := map[string]any{
		"bundle": map[string]any{
			"nodes": []any{
				map[string]any{"id": "a", "payload": map[string]any{"status": "awaitingFeedback"}},
				map[string]any{"id": "b", "payload": map[string]any{"status": "running"}},
			},
		},
	}
	nodes := nodesFromResult(result)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodesFromResult(nil) != nil || nodesFromResult(map[string]any{}) != nil {
		t.Fatal("malformed results must yield nil")
	}
}

func TestAwaitingFeedbackPlanFromPayload_PrefersNodeId(t *testing.T) {
	node := map[string]any{"id": "v1:planner:plan:node"}
	payload := map[string]any{
		"spaceId":         "sp",
		"status":          "awaitingFeedback",
		"feedbackRequest": map[string]any{"question": "Q?"},
	}
	plan := awaitingFeedbackPlanFromPayload(node, payload)
	if plan == nil || plan.ID != "v1:planner:plan:node" {
		t.Fatalf("must take id from node intrinsic: %+v", plan)
	}
	if plan.Question != "Q?" {
		t.Fatalf("question not read: %+v", plan)
	}
}

func TestMapSliceFromAny(t *testing.T) {
	in := []any{
		map[string]any{"label": "x"},
		"not-an-object",
		map[string]any{"label": "y"},
	}
	out := mapSliceFromAny(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 object entries, got %d", len(out))
	}
	if mapSliceFromAny("nope") != nil {
		t.Fatal("non-slice must yield nil")
	}
}

// ---- announce gate: fail-safe + cross-replica exactly-once ----

func TestAnnounceGate_NilGateProceeds(t *testing.T) {
	var g *dispatchGate
	proceed, release, err := g.tryAnnounceFeedback(context.Background(), "sp", "v1:planner:plan:x")
	if err != nil || !proceed {
		t.Fatalf("nil gate must proceed (fail safe): proceed=%v err=%v", proceed, err)
	}
	release()
}

func TestAnnounceGate_NilGetterProceeds(t *testing.T) {
	g := newDispatchGate(nil, dispatchGateTestLogger())
	proceed, release, err := g.tryAnnounceFeedback(context.Background(), "sp", "v1:planner:plan:x")
	if err != nil || !proceed {
		t.Fatalf("nil getter must proceed (fail safe): proceed=%v err=%v", proceed, err)
	}
	release()
}

func TestAnnounceGate_ErroredConnProceeds(t *testing.T) {
	connector := pgdriver.NewConnector(pgdriver.WithDSN("postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable"))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	defer db.Close()

	g := newDispatchGate(func() *bun.DB { return db }, dispatchGateTestLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	proceed, release, err := g.tryAnnounceFeedback(ctx, "sp", "v1:planner:plan:bad-conn")
	if !proceed {
		t.Fatal("a DB/lock error must fail SAFE (proceed=true), got proceed=false")
	}
	if err == nil {
		t.Fatal("expected a non-nil error to log on the fail-safe path")
	}
	release()
}

func TestFeedbackAnnounceGateLockKey_StableAndDistinct(t *testing.T) {
	if feedbackAnnounceGateLockKey("sp", "p1") != feedbackAnnounceGateLockKey("sp", "p1") {
		t.Fatal("lock key must be stable for the same (space, plan)")
	}
	if feedbackAnnounceGateLockKey("sp", "p1") == feedbackAnnounceGateLockKey("sp", "p2") {
		t.Fatal("different plan ids should (almost always) hash to different keys")
	}
	// The announce lock CLASS is distinct from the greet + dispatch classes, so
	// even an FNV collision on the object key can't alias across surfaces.
	if feedbackAnnounceGateLockClass == greetGateLockClass || feedbackAnnounceGateLockClass == dispatchGateLockClass {
		t.Fatal("announce gate must use a distinct advisory-lock class")
	}
}

// TestAnnounceGate_ExactlyOneWinsUnderContention is the cross-node acceptance
// check for #1406: the graph.node.updated.v1:planner:plan event is broadcast to
// BOTH cognition replicas, so both run the announce handler for the SAME
// (space, plan). Exactly one must win the advisory lock and post; the other
// must bail (proceed=false), so the assistant's "needs your input" message is
// posted ONCE in the mesh, not twice. Each replica uses an independent *bun.DB
// session (== separate pods), so the advisory lock is the only serializer.
func TestAnnounceGate_ExactlyOneWinsUnderContention(t *testing.T) {
	dbA := openDispatchGateTestDB(t)
	defer dbA.Close()
	dbB := openDispatchGateTestDB(t)
	defer dbB.Close()

	gateA := newDispatchGate(func() *bun.DB { return dbA }, dispatchGateTestLogger())
	gateB := newDispatchGate(func() *bun.DB { return dbB }, dispatchGateTestLogger())

	spaceId := "v1:cognition:space:announce"
	planId := fmt.Sprintf("v1:planner:plan:announce-%d", time.Now().UnixNano())

	var (
		wg            sync.WaitGroup
		proceeded     int32
		bailed        int32
		errs          int32
		releaseOnce   sync.Once
		winnerRelease func()
	)

	attempt := func(g *dispatchGate) {
		defer wg.Done()
		proceed, release, err := g.tryAnnounceFeedback(context.Background(), spaceId, planId)
		if err != nil {
			atomic.AddInt32(&errs, 1)
			return
		}
		if proceed {
			atomic.AddInt32(&proceeded, 1)
			releaseOnce.Do(func() { winnerRelease = release })
		} else {
			atomic.AddInt32(&bailed, 1)
			release()
		}
	}

	wg.Add(2)
	go attempt(gateA)
	go attempt(gateB)
	wg.Wait()
	if winnerRelease != nil {
		winnerRelease()
	}

	if got := atomic.LoadInt32(&errs); got != 0 {
		t.Fatalf("no replica should error in the happy path, got %d errors", got)
	}
	if p, b := atomic.LoadInt32(&proceeded), atomic.LoadInt32(&bailed); p != 1 || b != 1 {
		t.Fatalf("exactly one replica must announce and one bail: proceeded=%d bailed=%d", p, b)
	}
}

// TestAnnounceGate_ReclaimableAfterRelease verifies no permanent wedge: once the
// winner posts + releases, the lock is free again (so a later legitimate
// announcement for a different question on the same plan can still proceed --
// the durable read-before-write dedup, not the session lock, is what prevents a
// re-post of the SAME announcement).
func TestAnnounceGate_ReclaimableAfterRelease(t *testing.T) {
	db := openDispatchGateTestDB(t)
	defer db.Close()
	g := newDispatchGate(func() *bun.DB { return db }, dispatchGateTestLogger())
	spaceId := "v1:cognition:space:reclaim"
	planId := fmt.Sprintf("v1:planner:plan:reclaim-%d", time.Now().UnixNano())

	proceed, release, err := g.tryAnnounceFeedback(context.Background(), spaceId, planId)
	if err != nil || !proceed {
		t.Fatalf("first acquire must win: proceed=%v err=%v", proceed, err)
	}
	release()

	proceed2, release2, err := g.tryAnnounceFeedback(context.Background(), spaceId, planId)
	if err != nil || !proceed2 {
		t.Fatalf("after release the lock must be re-acquirable: proceed=%v err=%v", proceed2, err)
	}
	release2()
}
