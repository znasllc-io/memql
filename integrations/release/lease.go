package release

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errLeaseLost means the row no longer names this holder, or its time ran out
// before a renewal landed. Another holder may be running, so the work must stop.
var errLeaseLost = errors.New("release lease is no longer held")

// leaseTiming is one durable lease: how long it outlives its last renewal, which
// is how long a dead holder blocks the next one, and how often a live holder
// renews it.
type leaseTiming struct{ ttl, renew time.Duration }

var (
	buildLeaseTiming = leaseTiming{ttl: 6 * time.Minute, renew: time.Minute}
	// Shorter than the build lease on purpose. A retried publication starts only
	// after a dead action's build lease lapsed, and by then that action's upload
	// lease has lapsed too, so the retry is never refused by its own predecessor.
	uploadLeaseTiming = leaseTiming{ttl: 3 * time.Minute, renew: 30 * time.Second}
)

// keepLease renews a durable lease for as long as the work it protects runs, so
// a holder's time is bounded by being alive rather than by a guess at how long
// its work takes. The returned context ends when the lease is lost, and also when
// renewals have failed for so long that it may be about to lapse: a holder that
// cannot show it still holds the lease must stop before another may claim it.
// That is checked only as each attempt ends, up to one renew period apart, so the
// limit leaves two periods clear of the lease's end. Renewal ignores cancellation of
// ctx itself, which already ends the work.
//
// The stop function must be called before the lease is closed. It is safe to call
// twice, and it waits, so no renewal can land after it returns.
func keepLease(ctx context.Context, timing leaseTiming, renew func(context.Context) error) (context.Context, func()) {
	work, cancel := context.WithCancelCause(ctx)
	finished, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(timing.renew)
		defer ticker.Stop()
		held := time.Now()
		for {
			select {
			case <-finished:
				return
			case <-work.Done():
				return
			case <-ticker.C:
			}
			attempt, done := context.WithTimeout(context.WithoutCancel(ctx), timing.renew)
			err := renew(attempt)
			done()
			switch {
			case err == nil:
				held = time.Now()
			case errors.Is(err, errLeaseLost) || time.Since(held) >= timing.ttl-2*timing.renew:
				cancel(errLeaseLost)
				return
			}
		}
	}()
	var once sync.Once
	return work, func() {
		once.Do(func() { close(finished) })
		<-stopped
		cancel(nil)
	}
}
