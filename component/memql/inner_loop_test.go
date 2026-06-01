package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Structured tool errors + classification
// ---------------------------------------------------------------------------

func TestClassifyToolError_Types(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantType    ToolErrorType
		wantRetry   bool
		wantUserFix bool
	}{
		{"nil", nil, "", false, false}, // handled below
		{"validation", errors.New("invalid argument: missing required field foo"), ToolErrorValidation, false, true},
		{"additional property", errors.New("additional property bar not allowed"), ToolErrorValidation, false, true},
		{"not found", errors.New(`tool "doThing" not found`), ToolErrorNotFound, false, true},
		{"unknown tool", errors.New("unknown tool: zap"), ToolErrorNotFound, false, true},
		{"permission", errors.New("tool \"x\" is not allowed for caller role \"reader\""), ToolErrorPermission, false, false},
		{"agent-only", errors.New("tools are agent-only -- no acting agent"), ToolErrorPermission, false, false},
		{"timeout", errors.New("context deadline exceeded"), ToolErrorTimeout, true, false},
		{"unavailable", errors.New("provider overloaded (529)"), ToolErrorUnavailable, true, false},
		{"rate limit", errors.New("rate_limit exceeded"), ToolErrorUnavailable, true, false},
		{"internal", errors.New("something exploded unexpectedly"), ToolErrorInternal, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyToolError(tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("expected nil for nil error, got %+v", got)
				}
				return
			}
			if got.Type != tc.wantType {
				t.Errorf("type: got %q want %q (msg=%q)", got.Type, tc.wantType, tc.err.Error())
			}
			if got.Retryable != tc.wantRetry {
				t.Errorf("retryable: got %v want %v", got.Retryable, tc.wantRetry)
			}
			if got.UserFixable != tc.wantUserFix {
				t.Errorf("userFixable: got %v want %v", got.UserFixable, tc.wantUserFix)
			}
		})
	}
}

func TestClassifyToolError_FromStructuredError(t *testing.T) {
	se := NewStructuredError(ErrorCodeMissingRequiredFields, "need name").
		WithDetails(map[string]any{"missing": []string{"name"}})
	got := ClassifyToolError(se)
	if got.Type != ToolErrorValidation {
		t.Fatalf("expected validation, got %q", got.Type)
	}
	if !got.UserFixable {
		t.Errorf("expected user-fixable")
	}
	if got.Details["missing"] == nil {
		t.Errorf("expected details to carry through, got %+v", got.Details)
	}
}

func TestClassifyToolError_UnwrapsWrapped(t *testing.T) {
	se := NewStructuredError(ErrorCodeNotFound, "missing")
	wrapped := fmt.Errorf("dispatch failed: %w", se)
	got := ClassifyToolError(wrapped)
	if got.Type != ToolErrorNotFound {
		t.Fatalf("expected not_found through wrap, got %q", got.Type)
	}
}

func TestStructuredToolError_JSON(t *testing.T) {
	se := newStructuredToolError(ToolErrorValidation, "bad arg",
		map[string]any{"allowed": []string{"a", "b"}})
	se.Attempts = 2
	var decoded map[string]any
	if err := json.Unmarshal([]byte(se.JSON()), &decoded); err != nil {
		t.Fatalf("JSON did not round-trip: %v", err)
	}
	if decoded["type"] != "validation" {
		t.Errorf("type wrong: %v", decoded["type"])
	}
	if decoded["userFixable"] != true {
		t.Errorf("userFixable wrong: %v", decoded["userFixable"])
	}
	if decoded["attempts"].(float64) != 2 {
		t.Errorf("attempts wrong: %v", decoded["attempts"])
	}
}

// TestModelRecoversFromStructuredError proves a model CAN recover within
// the same loop: a user-fixable error is flagged so the model knows to
// retry with corrected args, while a permission error is not. This is the
// classification contract the loop relies on for recovery.
func TestModelRecoversFromStructuredError(t *testing.T) {
	// Round 1: model sends bad args, gets a validation error.
	validationErr := ClassifyToolError(errors.New("invalid argument: must be one of [red, green]"))
	if !validationErr.UserFixable {
		t.Fatalf("validation error must be user-fixable so the model retries")
	}
	if validationErr.Retryable {
		t.Fatalf("validation error must NOT be mechanically retryable (same call would fail)")
	}
	// A permission error, by contrast, gives the model no recovery path.
	permErr := ClassifyToolError(errors.New("not authorized: scope_elevation_required"))
	if permErr.UserFixable || permErr.Retryable {
		t.Fatalf("permission error must be neither user-fixable nor retryable")
	}
}

// ---------------------------------------------------------------------------
// Loop detection
// ---------------------------------------------------------------------------

func TestToolCallSignature_KeyOrderIndependent(t *testing.T) {
	a := ToolCallSignature("doThing", `{"x":1,"y":2}`)
	b := ToolCallSignature("doThing", `{"y":2,"x":1}`)
	if a != b {
		t.Errorf("expected key-order-independent signatures to match:\n%s\n%s", a, b)
	}
	c := ToolCallSignature("doThing", `{"x":1,"y":3}`)
	if a == c {
		t.Errorf("expected different args to produce different signatures")
	}
}

func TestLoopDetector_ConsecutiveTrip(t *testing.T) {
	d := NewLoopDetector(3)
	sig := ToolCallSignature("search", `{"q":"foo"}`)
	if d.Observe(sig) {
		t.Fatalf("should not trip on 1st call")
	}
	if d.Observe(sig) {
		t.Fatalf("should not trip on 2nd call")
	}
	if !d.Observe(sig) {
		t.Fatalf("should trip on 3rd identical call")
	}
}

func TestLoopDetector_ResetsOnDifferentCall(t *testing.T) {
	d := NewLoopDetector(3)
	a := ToolCallSignature("a", `{}`)
	b := ToolCallSignature("b", `{}`)
	d.Observe(a)
	d.Observe(a)
	d.Observe(b) // resets the consecutive counter
	if d.Observe(a) {
		t.Fatalf("counter should have reset after the differing call")
	}
}

func TestLoopDetector_TotalThrashingTrip(t *testing.T) {
	d := NewLoopDetector(10) // high consecutive threshold so only total trips
	a := ToolCallSignature("a", `{}`)
	b := ToolCallSignature("b", `{}`)
	// Alternate A/B; A appears defaultLoopTotalThreshold times.
	tripped := false
	for i := 0; i < defaultLoopTotalThreshold*2; i++ {
		if i%2 == 0 {
			if d.Observe(a) {
				tripped = true
				break
			}
		} else {
			d.Observe(b)
		}
	}
	if !tripped {
		t.Fatalf("expected total-occurrence threshold to trip on A/B thrashing")
	}
}

// ---------------------------------------------------------------------------
// Context budget
// ---------------------------------------------------------------------------

func TestContextBudget_Exceeded(t *testing.T) {
	b := NewContextBudget(10) // 10 tokens, 4 chars/token => ~40 chars
	if b.Exceeded("short") {
		t.Errorf("short content should not exceed")
	}
	big := strings.Repeat("x", 100) // ~25 tokens
	if !b.Exceeded(big) {
		t.Errorf("100-char content should exceed a 10-token budget")
	}
	// Disabled budget never trips.
	if (ContextBudget{}).Exceeded(big) {
		t.Errorf("zero budget must be disabled")
	}
}

func TestPlanContextTrim(t *testing.T) {
	// sizes: [system=5][a=10][b=10][c=10][tail1=3][tail2=3] = 41 tokens
	sizes := []int{5, 10, 10, 10, 3, 3}
	// budget 30, pin head 1 (system), keep tail 2.
	plan := PlanContextTrim(sizes, 30, 1, 2)
	if plan.DropCount == 0 {
		t.Fatalf("expected a trim, got none")
	}
	// total 41 -> need to drop >= 11 tokens from candidates [a,b,c].
	// dropping a(10)+b(10)=20 brings us to 21 <= 30; one drop (10) -> 31 > 30
	// so it should drop 2.
	if plan.DropCount != 2 {
		t.Errorf("expected DropCount 2, got %d", plan.DropCount)
	}
	if plan.Summary == "" {
		t.Errorf("expected a summary note")
	}
}

func TestPlanContextTrim_NothingToTrim(t *testing.T) {
	sizes := []int{5, 5, 5}
	if plan := PlanContextTrim(sizes, 100, 1, 1); plan.DropCount != 0 {
		t.Errorf("under-budget window should not trim, got %d", plan.DropCount)
	}
	// All pinned/tail, no trimmable middle.
	if plan := PlanContextTrim(sizes, 1, 2, 1); plan.DropCount != 0 {
		t.Errorf("no trimmable candidates should yield no plan, got %d", plan.DropCount)
	}
}

// ---------------------------------------------------------------------------
// Retry + timeout (flaky-tool fixture)
// ---------------------------------------------------------------------------

func TestToolRetryPolicy_Backoff(t *testing.T) {
	p := ToolRetryPolicy{BaseBackoff: 100 * time.Millisecond, MaxBackoff: 400 * time.Millisecond}
	if p.Backoff(1) != 0 {
		t.Errorf("attempt 1 has no preceding backoff")
	}
	if p.Backoff(2) != 100*time.Millisecond {
		t.Errorf("attempt 2 backoff: got %v", p.Backoff(2))
	}
	if p.Backoff(3) != 200*time.Millisecond {
		t.Errorf("attempt 3 backoff: got %v", p.Backoff(3))
	}
	if p.Backoff(10) != 400*time.Millisecond {
		t.Errorf("backoff must cap at MaxBackoff, got %v", p.Backoff(10))
	}
}

// TestExecuteWithRetry_FlakyToolSucceeds is the flaky-tool fixture: a tool
// that fails with a transient (retryable) error twice, then succeeds. The
// retry policy must keep trying and surface success with the right attempt
// count.
func TestExecuteWithRetry_FlakyToolSucceeds(t *testing.T) {
	p := ToolRetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
	calls := 0
	result, se, attempts := p.ExecuteWithRetry(context.Background(), func(ctx context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("service unavailable (503)")
		}
		return `{"ok":true}`, nil
	})
	if se != nil {
		t.Fatalf("expected success, got error %+v", se)
	}
	if result != `{"ok":true}` {
		t.Errorf("unexpected result: %s", result)
	}
	if attempts != 3 || calls != 3 {
		t.Errorf("expected 3 attempts, got attempts=%d calls=%d", attempts, calls)
	}
}

// TestExecuteWithRetry_UserFixableNotRetried proves a user-fixable error is
// returned immediately (retrying the identical call is pointless) with the
// attempt count surfaced as state.
func TestExecuteWithRetry_UserFixableNotRetried(t *testing.T) {
	p := ToolRetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond}
	calls := 0
	_, se, attempts := p.ExecuteWithRetry(context.Background(), func(ctx context.Context) (string, error) {
		calls++
		return "", errors.New("invalid argument: missing field name")
	})
	if se == nil {
		t.Fatalf("expected a structured error")
	}
	if se.Type != ToolErrorValidation {
		t.Errorf("expected validation, got %q", se.Type)
	}
	if calls != 1 {
		t.Errorf("user-fixable error must not be retried, got %d calls", calls)
	}
	if attempts != 1 {
		t.Errorf("expected attempts=1, got %d", attempts)
	}
}

func TestExecuteWithRetry_ExhaustsAndReportsAttempts(t *testing.T) {
	p := ToolRetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
	calls := 0
	_, se, attempts := p.ExecuteWithRetry(context.Background(), func(ctx context.Context) (string, error) {
		calls++
		return "", errors.New("overloaded")
	})
	if se == nil || se.Type != ToolErrorUnavailable {
		t.Fatalf("expected unavailable error, got %+v", se)
	}
	if calls != 2 || attempts != 2 {
		t.Errorf("expected 2 attempts, got calls=%d attempts=%d", calls, attempts)
	}
	if se.Attempts != 2 {
		t.Errorf("attempt count must be surfaced as state, got %d", se.Attempts)
	}
}

func TestExecuteWithRetry_PerCallTimeout(t *testing.T) {
	p := ToolRetryPolicy{MaxAttempts: 1, PerCallTimeout: 20 * time.Millisecond}
	_, se, _ := p.ExecuteWithRetry(context.Background(), func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
			return "late", nil
		}
	})
	if se == nil {
		t.Fatalf("expected a timeout error")
	}
	if se.Type != ToolErrorTimeout {
		t.Errorf("expected timeout type, got %q", se.Type)
	}
}

// ---------------------------------------------------------------------------
// Tool scoping
// ---------------------------------------------------------------------------

func TestScopeToolNames(t *testing.T) {
	all := []string{"searchUsers", "canvasPublish", "workerHost"}
	scoped := ScopeToolNames(all, []string{"canvasPublish", "notInList"})
	if len(scoped) != 1 || scoped[0] != "canvasPublish" {
		t.Errorf("expected only canvasPublish, got %v", scoped)
	}
	// Empty scope = no scoping requested -> unchanged.
	if got := ScopeToolNames(all, nil); len(got) != 3 {
		t.Errorf("nil scope should pass through, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

func TestIdempotencyKeyFor_StableAndDistinct(t *testing.T) {
	k1 := IdempotencyKeyFor("step-1", "doThing", `{"a":1}`)
	k2 := IdempotencyKeyFor("step-1", "doThing", `{"a":1}`)
	if k1 != k2 {
		t.Errorf("same inputs must produce same key")
	}
	// Key-order independence rides on the canonical signature.
	k3 := IdempotencyKeyFor("step-1", "doThing", `{"a":1,"b":2}`)
	k4 := IdempotencyKeyFor("step-1", "doThing", `{"b":2,"a":1}`)
	if k3 != k4 {
		t.Errorf("arg key order must not change the idempotency key")
	}
	// Different step keys diverge.
	if IdempotencyKeyFor("step-2", "doThing", `{"a":1}`) == k1 {
		t.Errorf("different step keys must produce different idempotency keys")
	}
}

func TestLoopIdempotencyStore(t *testing.T) {
	s := newLoopIdempotencyStore()
	if _, ok := s.Get("k"); ok {
		t.Errorf("empty store should miss")
	}
	s.Put("k", "result")
	if v, ok := s.Get("k"); !ok || v != "result" {
		t.Errorf("store should return what was put, got %q ok=%v", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Options wiring
// ---------------------------------------------------------------------------

type recordingSink struct {
	notes []string
}

func (r *recordingSink) RecordObservation(_ context.Context, kind, text string, _ map[string]any) {
	r.notes = append(r.notes, kind+":"+text)
}

func TestToolLoopOptions_Defaults(t *testing.T) {
	var opts *ToolLoopOptions
	if opts.resolvedRetryPolicy().MaxAttempts != DefaultToolRetryPolicy().MaxAttempts {
		t.Errorf("nil opts should yield default retry policy")
	}
	// recordObservation on nil opts must not panic.
	opts.recordObservation(context.Background(), "note", "x", nil)

	sink := &recordingSink{}
	o := &ToolLoopOptions{Observations: sink}
	o.recordObservation(context.Background(), "note", "hi", nil)
	if len(sink.notes) != 1 || sink.notes[0] != "note:hi" {
		t.Errorf("expected observation recorded, got %v", sink.notes)
	}
}
