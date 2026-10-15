package storagequota

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestInventorySurvivesCallerCancellationWithoutOpeningWrites(t *testing.T) {
	s, replica := fixture(t)
	started, finish := make(chan struct{}), make(chan struct{})
	var scans atomic.Int32
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- s.EnsureInitialized(request, "bulk", func(ctx context.Context, add func(string, string, int64) error) error {
			scans.Add(1)
			// Cross batch boundaries using metadata only, never large payloads.
			for n := range 1025 {
				if err := add(fmt.Sprint("bulk-", n), "", 1); err != nil {
					return err
				}
			}
			close(started)
			select {
			case <-finish:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer close(finish)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("inventory did not start")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", err)
	}
	other := replica()
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if ready, err := other.Initialized(ctx, "bulk"); err != nil || ready {
		t.Fatalf("partial inventory advertised readiness: %v %v", ready, err)
	}
	var visible int
	if err := other.DB().QueryRowContext(ctx, `SELECT count(*) FROM storage_capacity_objects WHERE account='bulk'`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("partial import escaped: %d %v", visible, err)
	}
	err := other.EnsureInitialized(ctx, "bulk", func(context.Context, func(string, string, int64) error) error { scans.Add(100); return nil })
	if !errors.Is(err, ErrInventoryPending) || scans.Load() != 1 {
		t.Fatalf("replica started a competing scan: %v %d", err, scans.Load())
	}
	finish <- struct{}{}
	err = s.EnsureInitialized(ctx, "bulk", func(context.Context, func(string, string, int64) error) error { scans.Add(100); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := other.Initialized(ctx, "bulk"); err != nil || !ready {
		t.Fatalf("committed inventory unavailable on replica: %v %v", ready, err)
	}
	u, err := other.Usage(ctx, "")
	if err != nil || u.ClusterUsed != 1025*(1+ObjectOverhead) || u.ClusterReserved != 0 || scans.Load() != 1 {
		t.Fatalf("inventory totals: %+v %v scans=%d", u, err, scans.Load())
	}
}

func TestInventoryFailureRollsBackBatchesAndCanRetry(t *testing.T) {
	s, replica := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lost := errors.New("inventory connection lost")
	err := s.EnsureInitialized(ctx, "retry", func(_ context.Context, add func(string, string, int64) error) error {
		for n := range 600 {
			if err := add(fmt.Sprint("retry-", n), "member", 2); err != nil {
				return err
			}
		}
		return lost
	})
	if !errors.Is(err, lost) {
		t.Fatal(err)
	}
	other := replica()
	if ready, err := other.Initialized(ctx, "retry"); err != nil || ready {
		t.Fatalf("failed inventory ready: %v %v", ready, err)
	}
	err = other.EnsureInitialized(ctx, "retry", func(_ context.Context, add func(string, string, int64) error) error {
		for n := range 600 {
			if err := add(fmt.Sprint("retry-", n), "member", 2); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := other.Usage(ctx, "member")
	if err != nil || u.Used != 600*(2+ObjectOverhead) || u.ClusterUsed != u.Used {
		t.Fatalf("retry double counted: %+v %v", u, err)
	}
}
