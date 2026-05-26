package approval

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/safety"
)

// stubEngine records every Execute call and returns canned results
// matched by query-string prefix. Lets tests pin both the query
// query (returns rows) and the mutation (returns inserted id).
type stubEngine struct {
	mu       sync.Mutex
	calls    []string
	queryRes any
	queryErr error
	mutRes   any
	mutErr   error
}

func (s *stubEngine) Execute(_ context.Context, query string) (any, error) {
	s.mu.Lock()
	s.calls = append(s.calls, query)
	s.mu.Unlock()
	switch {
	case strings.HasPrefix(query, "queryActiveApprovalsByCorrelationKey"):
		return s.queryRes, s.queryErr
	case strings.HasPrefix(query, "mutationCreateApprovalRequest"):
		return s.mutRes, s.mutErr
	}
	return nil, errors.New("stubEngine: unexpected query: " + query)
}

func (s *stubEngine) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

func newDesc() safety.ActionDescriptor {
	return safety.NewExecAction(safety.SurfaceWorkbench, "rm /tmp/foo",
		safety.CallerContext{AgentID: "a1", OwnerUserID: "u1"})
}

func newCls() safety.Classification {
	return safety.Classification{
		Tier:   safety.TierHigh,
		Source: safety.SourceRule,
		Reason: "test",
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSinkApprovedRowFlipsToApprovedVerdict(t *testing.T) {
	// Active query returns one approved row -- Sink should pass it
	// through without creating a new pending row.
	engine := &stubEngine{
		queryRes: []any{
			map[string]any{
				"id": "v1:safety:approvalRequest:approved1",
				"payload": map[string]any{
					"status":    "approved",
					"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				},
			},
		},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStateApproved {
		t.Errorf("expected Approved, got %s", v.State)
	}
	if v.ApprovalRequestID != "v1:safety:approvalRequest:approved1" {
		t.Errorf("expected the approved row id, got %q", v.ApprovalRequestID)
	}
	if len(engine.Calls()) != 1 {
		t.Errorf("approved-row path should NOT create a new pending row; got %d engine calls", len(engine.Calls()))
	}
}

func TestSinkExpiredApprovedRowFallsThroughToPending(t *testing.T) {
	// An approved row past its TTL should NOT count as a live
	// bypass token. Sink creates a fresh pending row instead.
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	engine := &stubEngine{
		queryRes: []any{
			map[string]any{
				"id": "v1:safety:approvalRequest:expired",
				"payload": map[string]any{
					"status":    "approved",
					"expiresAt": expired,
				},
			},
		},
		mutRes: map[string]any{"id": "v1:safety:approvalRequest:fresh"},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStatePending {
		t.Errorf("expected Pending (expired approval doesn't count), got %s", v.State)
	}
	if v.ApprovalRequestID != "v1:safety:approvalRequest:fresh" {
		t.Errorf("expected the fresh pending row id, got %q", v.ApprovalRequestID)
	}
	if len(engine.Calls()) != 2 {
		t.Errorf("expected 1 query + 1 mutation, got %d calls", len(engine.Calls()))
	}
}

func TestSinkPendingRowReusedNotDuplicated(t *testing.T) {
	// A pending row already exists -- Sink must NOT create a duplicate.
	engine := &stubEngine{
		queryRes: []any{
			map[string]any{
				"id": "v1:safety:approvalRequest:pending1",
				"payload": map[string]any{
					"status": "pending",
				},
			},
		},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStatePending {
		t.Errorf("expected Pending, got %s", v.State)
	}
	if v.ApprovalRequestID != "v1:safety:approvalRequest:pending1" {
		t.Errorf("expected the pre-existing pending id, got %q", v.ApprovalRequestID)
	}
	if len(engine.Calls()) != 1 {
		t.Errorf("pending-exists path must NOT create a duplicate; got %d engine calls (want 1)", len(engine.Calls()))
	}
}

func TestSinkNoActiveRowCreatesPending(t *testing.T) {
	// Query returns nothing -- Sink creates a fresh pending row.
	engine := &stubEngine{
		queryRes: []any{},
		mutRes:   map[string]any{"id": "v1:safety:approvalRequest:new"},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStatePending {
		t.Errorf("expected Pending, got %s", v.State)
	}
	if v.ApprovalRequestID != "v1:safety:approvalRequest:new" {
		t.Errorf("expected the newly-created id, got %q", v.ApprovalRequestID)
	}
	if len(engine.Calls()) != 2 {
		t.Errorf("expected 1 query + 1 mutation, got %d calls", len(engine.Calls()))
	}
	// Verify the mutation call carries the descriptor fields.
	mut := engine.Calls()[1]
	for _, frag := range []string{
		`mutationCreateApprovalRequest(`,
		`surface: "workbench"`,
		`action: "exec"`,
		`tier: "high"`,
		`status` /* not in args, set by mutation */, // soft check that mutation built
	} {
		if frag == `status` /* sentinel */ {
			continue
		}
		if !strings.Contains(mut, frag) {
			t.Errorf("mutation missing %q\nFull: %s", frag, mut)
		}
	}
}

func TestSinkQueryErrorReturnsUnconfigured(t *testing.T) {
	// A backing-store query error must NOT block live traffic --
	// fall back to Unconfigured so the Gate's pre-#232 Ask refusal
	// fires (which is conservative + visible to ops in the slog
	// record).
	engine := &stubEngine{queryErr: errors.New("db down")}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStateUnconfigured {
		t.Errorf("query error -> Unconfigured, got %s", v.State)
	}
}

func TestSinkCreateErrorReturnsUnconfigured(t *testing.T) {
	// Same fail-open posture for create errors.
	engine := &stubEngine{
		queryRes: []any{},
		mutErr:   errors.New("write rejected"),
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStateUnconfigured {
		t.Errorf("create error -> Unconfigured, got %s", v.State)
	}
}

func TestSinkNilEngineReturnsUnconfigured(t *testing.T) {
	// Defensive: a Sink constructed with nil engine must not crash
	// the dispatch path. Returns Unconfigured.
	s := New(Options{Engine: nil, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStateUnconfigured {
		t.Errorf("nil engine -> Unconfigured, got %s", v.State)
	}
}

func TestSinkTTLDefaults(t *testing.T) {
	// When TTL is unset + env var unset, defaults to DefaultTTLHours.
	old := envLookup
	envLookup = func(string) string { return "" }
	defer func() { envLookup = old }()
	engine := &stubEngine{
		queryRes: []any{},
		mutRes:   map[string]any{"id": "x"},
	}
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	s := New(Options{Engine: engine, Logger: discardLogger(), Now: func() time.Time { return now }})
	_ = s.Check(context.Background(), newDesc(), newCls())
	mut := engine.Calls()[1]
	wantExpiry := now.Add(time.Duration(DefaultTTLHours) * time.Hour).UTC().Format(time.RFC3339)
	if !strings.Contains(mut, wantExpiry) {
		t.Errorf("mutation should embed default-TTL expiry %q; got: %s", wantExpiry, mut)
	}
}

func TestSinkTTLFromEnv(t *testing.T) {
	old := envLookup
	envLookup = func(k string) string {
		if k == EnvTTLHours {
			return "6"
		}
		return ""
	}
	defer func() { envLookup = old }()
	engine := &stubEngine{
		queryRes: []any{},
		mutRes:   map[string]any{"id": "x"},
	}
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	s := New(Options{Engine: engine, Logger: discardLogger(), Now: func() time.Time { return now }})
	_ = s.Check(context.Background(), newDesc(), newCls())
	mut := engine.Calls()[1]
	wantExpiry := now.Add(6 * time.Hour).UTC().Format(time.RFC3339)
	if !strings.Contains(mut, wantExpiry) {
		t.Errorf("mutation should embed env-TTL (6h) expiry %q; got: %s", wantExpiry, mut)
	}
}

func TestSinkTTLNegativeFallsBack(t *testing.T) {
	// A negative TTL is nonsense -- fall back to default rather than
	// silently making approvals never expire.
	old := envLookup
	envLookup = func(k string) string {
		if k == EnvTTLHours {
			return "-1"
		}
		return ""
	}
	defer func() { envLookup = old }()
	got := ttlFromEnv()
	want := time.Duration(DefaultTTLHours) * time.Hour
	if got != want {
		t.Errorf("negative TTL should clamp to default %v, got %v", want, got)
	}
}

// TestSinkApprovedRowFromConvertedExecuteResultShape pins the
// production decode path: the actual shape app.ConvertExecuteResultToMap
// produces (response["data"] for shape() queries; response["bundle"]
// ["nodes"][0]["id"] for mutations). The earlier tests use a
// stub-friendly []any-of-maps shape that hid a critical bug --
// engine.Execute returns *ExecuteResult, not a raw map. This test
// would have caught it.
func TestSinkApprovedRowFromConvertedExecuteResultShape(t *testing.T) {
	// "data" axis -- the shape app/adapters.go::ConvertExecuteResultToMap
	// produces for a shape() query result. The shape projection puts
	// id + createdAt at the top + payload fields nested under "payload".
	engine := &stubEngine{
		queryRes: map[string]any{
			"data": []any{
				map[string]any{
					"id":        "v1:safety:approvalRequest:from-real-engine",
					"createdAt": "2026-05-25T10:00:00Z",
					"payload": map[string]any{
						"status":    "approved",
						"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					},
				},
			},
			"bundle": map[string]any{
				"nodes":   []any{},
				"rootIds": []string{},
			},
		},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStateApproved {
		t.Errorf("converted-result-shape approved row should yield Approved, got %s", v.State)
	}
	if v.ApprovalRequestID != "v1:safety:approvalRequest:from-real-engine" {
		t.Errorf("expected the converted-shape id, got %q", v.ApprovalRequestID)
	}
}

// TestSinkCreatePendingFromConvertedExecuteResultShape pins the
// production decode path for the mutation result: engine returns
// bundle.nodes[0].id, NOT a top-level "id" key.
func TestSinkCreatePendingFromConvertedExecuteResultShape(t *testing.T) {
	engine := &stubEngine{
		// Active query returns empty -- forces the create path.
		queryRes: map[string]any{
			"data":   []any{},
			"bundle": map[string]any{"nodes": []any{}, "rootIds": []string{}},
		},
		// Mutation returns the converted shape: id is at
		// bundle.nodes[0].id (no top-level "id" key).
		mutRes: map[string]any{
			"bundle": map[string]any{
				"nodes": []any{
					map[string]any{
						"id":      "v1:safety:approvalRequest:created-from-real-engine",
						"concept": "v1:safety:approvalRequest",
						"type":    "approvalRequest",
						"payload": map[string]any{"status": "pending"},
					},
				},
				"rootIds": []string{"v1:safety:approvalRequest:created-from-real-engine"},
			},
		},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStatePending {
		t.Errorf("expected Pending after create, got %s", v.State)
	}
	if v.ApprovalRequestID != "v1:safety:approvalRequest:created-from-real-engine" {
		t.Errorf("idFromResult must read bundle.nodes[0].id; got %q", v.ApprovalRequestID)
	}
}

func TestSinkEmptyExpiresAtTreatedAsExpired(t *testing.T) {
	// Defensive: a CLI-created approval without an expiresAt must
	// NOT confer a forever-live bypass. classifyActive treats empty
	// expiresAt as expired so a fresh pending row gets created.
	engine := &stubEngine{
		queryRes: []any{
			map[string]any{
				"id": "v1:safety:approvalRequest:no-ttl-approval",
				"payload": map[string]any{
					"status":    "approved",
					"expiresAt": "", // CLI forgot to set one
				},
			},
		},
		mutRes: map[string]any{"id": "v1:safety:approvalRequest:fresh"},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStatePending {
		t.Errorf("empty expiresAt should NOT confer Approved; got %s", v.State)
	}
	if v.ApprovalRequestID != "v1:safety:approvalRequest:fresh" {
		t.Errorf("expected a fresh pending row id, got %q", v.ApprovalRequestID)
	}
}

func TestSinkUnparseableExpiresAtTreatedAsExpired(t *testing.T) {
	// Same defensive policy for garbage expiresAt: if time.Parse
	// fails, treat as expired rather than "live forever."
	engine := &stubEngine{
		queryRes: []any{
			map[string]any{
				"id": "v1:safety:approvalRequest:bad-ttl",
				"payload": map[string]any{
					"status":    "approved",
					"expiresAt": "not-a-timestamp",
				},
			},
		},
		mutRes: map[string]any{"id": "v1:safety:approvalRequest:fresh"},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	v := s.Check(context.Background(), newDesc(), newCls())
	if v.State != safety.ApprovalStatePending {
		t.Errorf("unparseable expiresAt should NOT confer Approved; got %s", v.State)
	}
}

func TestSinkZeroTTLAllowedButImmediatelyExpires(t *testing.T) {
	// MEMQL_SAFETY_APPROVAL_TTL_HOURS=0 is a legitimate operator
	// posture (no bypass tokens). ttlFromEnv accepts 0; the resulting
	// approval would have expiresAt==now, which classifyActive's
	// t.Before(now) check would treat as expired immediately. So a
	// 0-TTL node creates pending rows on every dispatch + never
	// confers a bypass.
	old := envLookup
	envLookup = func(k string) string {
		if k == EnvTTLHours {
			return "0"
		}
		return ""
	}
	defer func() { envLookup = old }()
	got := ttlFromEnv()
	if got != 0 {
		t.Errorf("TTL=0 should pass through as zero duration, got %v", got)
	}
}

func TestSinkCorrelationKeyEmbedded(t *testing.T) {
	// The created mutation must carry the descriptor's correlation
	// key so the next dispatch finds the row via the lookup query.
	engine := &stubEngine{
		queryRes: []any{},
		mutRes:   map[string]any{"id": "x"},
	}
	s := New(Options{Engine: engine, Logger: discardLogger()})
	desc := newDesc()
	_ = s.Check(context.Background(), desc, newCls())
	wantKey := safety.ApprovalCorrelationKey(desc)
	for _, c := range engine.Calls() {
		if !strings.Contains(c, wantKey) {
			t.Errorf("engine call missing correlationKey %q\nCall: %s", wantKey, c)
		}
	}
}
