package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIsConnSlotExhaustion(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sqlstate 53300", errors.New(`ERROR: remaining connection slots are reserved for roles with privileges of the "pg_use_reserved_connections" role (SQLSTATE 53300)`), true},
		{"too many clients", errors.New("FATAL: sorry, too many clients already"), true},
		{"bare code", errors.New("pg error code=53300"), true},
		{"unrelated", errors.New("syntax error at or near \"slect\""), false},
		{"other fatal", errors.New("FATAL: password authentication failed"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isConnSlotExhaustion(c.err); got != c.want {
				t.Errorf("isConnSlotExhaustion(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// fakeConnector returns 53300 for the first failUntil calls, then succeeds.
type fakeConnector struct {
	calls     int
	failUntil int
	err       error
}

func (f *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	f.calls++
	if f.calls <= f.failUntil {
		return nil, f.err
	}
	return nil, nil // success (nil conn is fine for this test; we only assert retry control flow)
}
func (f *fakeConnector) Driver() driver.Driver { return nil }

func newTestRetryConnector(base driver.Connector) *retryingConnector {
	return &retryingConnector{base: base, attempts: 5, baseWait: time.Millisecond, maxWait: 5 * time.Millisecond, budget: time.Second}
}

// TestRetryingConnector_RetriesAndRecovers: a 53300 that clears within the
// attempt budget is retried until it succeeds.
func TestRetryingConnector_RetriesAndRecovers(t *testing.T) {
	fc := &fakeConnector{failUntil: 3, err: errors.New("SQLSTATE 53300")}
	rc := newTestRetryConnector(fc)
	if _, err := rc.Connect(context.Background()); err != nil {
		t.Fatalf("expected recovery after retries, got %v", err)
	}
	if fc.calls != 4 {
		t.Errorf("expected 4 Connect calls (3 fail + 1 success), got %d", fc.calls)
	}
}

// TestRetryingConnector_GivesUp: persistent 53300 exhausts the budget and the
// last error is returned (does NOT retry forever).
func TestRetryingConnector_GivesUp(t *testing.T) {
	fc := &fakeConnector{failUntil: 100, err: errors.New("too many clients already")}
	rc := newTestRetryConnector(fc)
	if _, err := rc.Connect(context.Background()); err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if fc.calls != rc.attempts+1 {
		t.Errorf("expected %d Connect calls, got %d", rc.attempts+1, fc.calls)
	}
}

// TestRetryingConnector_NonRetryablePassesThrough: a non-53300 error is
// returned immediately without retry.
func TestRetryingConnector_NonRetryablePassesThrough(t *testing.T) {
	fc := &fakeConnector{failUntil: 100, err: errors.New("FATAL: password authentication failed")}
	rc := newTestRetryConnector(fc)
	if _, err := rc.Connect(context.Background()); err == nil {
		t.Fatal("expected the auth error to surface")
	}
	if fc.calls != 1 {
		t.Errorf("non-retryable error must not retry; got %d calls", fc.calls)
	}
}

// TestRetryingConnector_RespectsContext: a cancelled context stops retrying.
func TestRetryingConnector_RespectsContext(t *testing.T) {
	fc := &fakeConnector{failUntil: 100, err: errors.New("SQLSTATE 53300")}
	rc := &retryingConnector{base: fc, attempts: 100, baseWait: 50 * time.Millisecond, maxWait: time.Second, budget: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := rc.Connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline error, got %v", err)
	}
}

// timeoutErr is what net returns for a dial that ran out of time: a net.Error
// whose Timeout() is true. pgdriver wraps it, so the string form is matched
// too.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp 10.0.185.57:5432: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestIsRetryableConnectError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"53300", errors.New("FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute (SQLSTATE=53300)"), true},
		{"net timeout", timeoutErr{}, true},
		{"wrapped net timeout", fmt.Errorf("connect: %w", timeoutErr{}), true},
		{"timeout by text", errors.New("read tcp 10.244.0.38:38472->10.0.185.57:5432: i/o timeout"), true},
		{"auth", errors.New("FATAL: password authentication failed"), false},
		{"refused", errors.New("dial tcp 10.0.185.57:5432: connect: connection refused"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetryableConnectError(c.err); got != c.want {
				t.Errorf("isRetryableConnectError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// A DIAL TIMEOUT IS RETRIED (D7 of the 2026-09-14 readiness-convergence
// record). On 2026-09-13 the boot readiness write of a pod that never got a
// slot failed on "read tcp ...: i/o timeout" against the -rw Service while old
// and new pods overlapped; the connector retried only 53300, so the timeout
// surfaced as a failed query on the first try.
func TestRetryingConnector_RetriesDialTimeout(t *testing.T) {
	fc := &fakeConnector{failUntil: 2, err: timeoutErr{}}
	rc := newTestRetryConnector(fc)
	if _, err := rc.Connect(context.Background()); err != nil {
		t.Fatalf("expected recovery after a dial timeout cleared, got %v", err)
	}
	if fc.calls != 3 {
		t.Errorf("expected 3 Connect calls (2 timeouts + 1 success), got %d", fc.calls)
	}
}

// THE BUDGET IS BOUNDED. A server that stays saturated must not be retried
// forever; the wall-clock budget ends the ladder even when the attempt cap has
// not been reached.
func TestRetryingConnector_BudgetIsBounded(t *testing.T) {
	fc := &fakeConnector{failUntil: 1000, err: errors.New("SQLSTATE 53300")}
	rc := &retryingConnector{base: fc, attempts: 1000, baseWait: 2 * time.Millisecond, maxWait: 5 * time.Millisecond, budget: 40 * time.Millisecond}
	started := time.Now()
	_, err := rc.Connect(context.Background())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected the last error after the budget elapsed")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the budget did not bound the retry: %s elapsed", elapsed)
	}
	if fc.calls >= 1000 || fc.calls < 2 {
		t.Fatalf("calls=%d; the budget, not the attempt cap, should have ended the ladder after a few tries", fc.calls)
	}
}

// The production budget is what the record says: ~15 s, wide enough to cover
// a rollout's overlap window, narrow enough that a request path with its own
// deadline is bounded by that deadline first.
func TestTheProductionConnectBudgetIsWhatTheRecordSays(t *testing.T) {
	rc, ok := newRetryingConnector(&fakeConnector{}, nil).(*retryingConnector)
	if !ok {
		t.Fatal("newRetryingConnector did not return a *retryingConnector")
	}
	if rc.budget != 15*time.Second {
		t.Errorf("budget %s, want 15s", rc.budget)
	}
	if rc.maxWait != 2*time.Second || rc.baseWait != 100*time.Millisecond {
		t.Errorf("waits %s..%s, want 100ms..2s", rc.baseWait, rc.maxWait)
	}
	// Enough attempts that the budget, not the cap, is the binding limit:
	// 100ms, 200, 400, 800, 1.6s, then 2s x 7 = ~17s of ceilings > 15s.
	if rc.attempts < 12 {
		t.Errorf("attempts %d; fewer than 12 lets the cap end the ladder before the budget", rc.attempts)
	}
}
