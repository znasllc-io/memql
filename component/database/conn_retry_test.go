package database

import (
	"context"
	"database/sql/driver"
	"errors"
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
	return &retryingConnector{base: base, attempts: 5, baseWait: time.Millisecond, maxWait: 5 * time.Millisecond}
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
	rc := &retryingConnector{base: fc, attempts: 100, baseWait: 50 * time.Millisecond, maxWait: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := rc.Connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline error, got %v", err)
	}
}
