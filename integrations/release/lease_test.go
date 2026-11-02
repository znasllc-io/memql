package release

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeepLeaseEndsTheWorkWhenTheLeaseIsLost(t *testing.T) {
	work, stop := keepLease(t.Context(), leaseTiming{ttl: time.Minute, renew: 10 * time.Millisecond}, func(context.Context) error { return errLeaseLost })
	defer stop()
	select {
	case <-work.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("work kept running without its lease")
	}
	if !errors.Is(context.Cause(work), errLeaseLost) {
		t.Fatal("the work does not say why it ended", context.Cause(work))
	}
}

// A holder that cannot show it still holds the lease must stop while it still does,
// because at the lease's end another holder may legally claim it.
func TestKeepLeaseStopsBeforeTheLeaseCanLapse(t *testing.T) {
	timing := leaseTiming{ttl: 3 * time.Second, renew: 500 * time.Millisecond}
	transient := errors.New("the journal did not answer")
	for name, renew := range map[string]func(context.Context) error{
		"fails at once": func(context.Context) error { return transient },
		"hangs to its deadline": func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			work, stop := keepLease(t.Context(), timing, renew)
			defer stop()
			select {
			case <-work.Done():
			case <-time.After(2 * timing.ttl):
				t.Fatal("work kept running with no proof of its lease")
			}
			if held := time.Since(started); held >= timing.ttl {
				t.Fatal("the work outlasted the lease it could not renew", held)
			}
		})
	}
}

func TestKeepLeaseOutlivesARenewalThatFailsThenRecovers(t *testing.T) {
	var calls atomic.Int64
	timing := leaseTiming{ttl: 300 * time.Millisecond, renew: 20 * time.Millisecond}
	work, stop := keepLease(t.Context(), timing, func(context.Context) error {
		if calls.Add(1)%3 == 0 {
			return nil
		}
		return errors.New("the journal did not answer")
	})
	defer stop()
	// Several lease lifetimes of intermittent failure that always recovers in time.
	select {
	case <-work.Done():
		t.Fatal("a recovered renewal ended the work", context.Cause(work))
	case <-time.After(3 * timing.ttl):
	}
}

func TestKeepLeaseStopEndsRenewalAndCanRepeat(t *testing.T) {
	var calls atomic.Int64
	work, stop := keepLease(context.Background(), leaseTiming{ttl: time.Minute, renew: 5 * time.Millisecond}, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	time.Sleep(50 * time.Millisecond)
	stop()
	stop()
	seen := calls.Load()
	if seen == 0 || work.Err() == nil {
		t.Fatal("renewal never ran, or stop left the work's context open", seen, work.Err())
	}
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != seen {
		t.Fatal("a renewal landed after stop returned")
	}
}

func TestKeepLeaseEndsWithItsParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	work, stop := keepLease(parent, leaseTiming{ttl: time.Minute, renew: time.Millisecond}, func(context.Context) error {
		calls.Add(1)
		return nil
	})
	defer stop()
	cancel()
	<-work.Done()
	stop()
	seen := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != seen {
		t.Fatal("renewal outlived the work")
	}
}
