package memql

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIsTransientDBSurge(t *testing.T) {
	transient := []string{
		"createSkill: ... FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute (SQLSTATE=53300)",
		"sorry, too many clients already",
		"dial tcp: connection refused",
		"read: connection reset by peer",
		"the database system is starting up",
	}
	for _, s := range transient {
		if !isTransientDBSurge(fmt.Errorf("%s", s)) {
			t.Errorf("want transient for %q", s)
		}
	}
	nonTransient := []string{
		"validation: skill tier refused",
		"render args: bad field",
		"no existing row for concept",
	}
	for _, s := range nonTransient {
		if isTransientDBSurge(fmt.Errorf("%s", s)) {
			t.Errorf("must NOT retry %q", s)
		}
	}
	if isTransientDBSurge(nil) {
		t.Error("nil is not transient")
	}
}

func TestRetryOnDBSurge_SuccessFirstTry(t *testing.T) {
	calls := 0
	err := retryOnDBSurge(context.Background(), nil, func() error { calls++; return nil })
	if err != nil || calls != 1 {
		t.Fatalf("want 1 call no error, got calls=%d err=%v", calls, err)
	}
}

func TestRetryOnDBSurge_NonTransientNoRetry(t *testing.T) {
	calls := 0
	err := retryOnDBSurge(context.Background(), nil, func() error {
		calls++
		return fmt.Errorf("validation: bad value")
	})
	if err == nil || calls != 1 {
		t.Fatalf("non-transient must not retry; got calls=%d err=%v", calls, err)
	}
}

func TestRetryOnDBSurge_RetriesTransientToSuccess(t *testing.T) {
	calls := 0
	err := retryOnDBSurge(context.Background(), nil, func() error {
		calls++
		if calls < 2 {
			return fmt.Errorf("FATAL: ... (SQLSTATE=53300)")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("transient should retry to success; got calls=%d err=%v", calls, err)
	}
}

func TestRetryOnDBSurge_ContextCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled -> the backoff select returns ctx.Err immediately
	calls := 0
	err := retryOnDBSurge(ctx, nil, func() error {
		calls++
		return fmt.Errorf("SQLSTATE=53300")
	})
	if err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("should stop after first transient when ctx is done, got calls=%d", calls)
	}
}

var _ = time.Second
