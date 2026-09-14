package memql

import (
	"context"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// COALESCED CATALOG RELOADS (memql#5252).
//
// ===========================================================================
// WHAT THIS REPLACES
// ===========================================================================
// StartCapabilityCatalog subscribes four topics -- role and capability,
// created and updated -- and every event used to run ReloadCapabilityCatalog
// itself: two full latest-per-id reads, of v1:rbac:role and of
// v1:rbac:capability. The event bus delivers each event on its OWN goroutine
// (events.Bus.Publish), so a burst of N writes was N reloads running AT ONCE
// on every engine, each holding a pooled connection. The rollout that ended in
// the SkipScan incident wrote 2,628 v1:rbac:capability versions in an hour,
// each raising the reload on every node
// (docs/internal/ops/2026-09-13-skipscan-connection-exhaustion.md).
//
// Overlap was a correctness hazard as well as a cost: two reloads in flight
// race to install, and the one that READ first can finish last, installing a
// catalog older than one already installed -- which nothing replaced until
// the next change arrived.
//
// ===========================================================================
// WHAT IT DOES INSTEAD
// ===========================================================================
// An event marks the catalog dirty. If no reload is in flight, one starts on
// a goroutine of its own -- never on the bus's delivery goroutine, which
// returns at once. When a reload finishes and the catalog was marked dirty
// while it ran, exactly one more runs. So:
//
//   - no two reloads ever run at once;
//   - a burst that lands while a reload runs costs exactly one more;
//   - the reload after the LAST event is never skipped: the dirty mark is read
//     and cleared under the same lock that decides whether the loop exits, so
//     an event is either seen by the running loop or starts a new one;
//   - every event is followed by a reload that STARTS after it: the mark is
//     cleared immediately before a reload begins, so a mark set later is a
//     reload still to come.
//
// Authorization invalidation is merged, never suppressed: the reload that
// runs reads the rows as they stand when it starts, which covers every write
// whose event it absorbed. Cancelling the context stops the loop; a reload
// already reading finishes its read under the cancelled context.
//
// ===========================================================================
// THE SETTLE DELAY, AND WHY IT IS NOT A DEBOUNCE
// ===========================================================================
// Each reload cycle waits rbacCatalogReloadSettle before it reads. Catalog
// writes arrive in bursts -- the seed materializer writing every role and
// capability row at boot, a role grid saved at once -- and a short wait lets
// the burst land before the read, so one reload covers it instead of the first
// write's reload reading a half-written catalog (a role row with no capability
// rows yet, which installCapabilityCatalog refuses and the log reports as a
// failed reload for writes that were fine).
//
// The wait is counted from the start of each cycle and is NOT reset by later
// events. A debounce that restarts on every event starves under a sustained
// burst -- exactly the rollout this exists for -- and an authorization change
// must not wait for the writers to go quiet. The longest any one change waits
// is one reload already in flight, one settle, and its own reload; the event
// that carries it here was delivered asynchronously anyway.
const rbacCatalogReloadSettle = 200 * time.Millisecond

// coalescedReloader runs reload for a stream of change notifications: never
// two at once, and never skipping the one after the last notification.
// Constructed with the context that bounds its life -- for the catalog, the
// engine's.
type coalescedReloader struct {
	ctx    context.Context
	settle time.Duration
	reload func(context.Context)

	mu      sync.Mutex
	running bool // a loop goroutine is live
	dirty   bool // a change has arrived that no started reload has read
}

func newCoalescedReloader(ctx context.Context, settle time.Duration, reload func(context.Context)) *coalescedReloader {
	return &coalescedReloader{ctx: ctx, settle: settle, reload: reload}
}

// request records a change and makes sure a reload will START after this
// call. It never blocks on a reload: with none running it starts the loop on
// a goroutine of its own and returns.
func (r *coalescedReloader) request() {
	r.mu.Lock()
	r.dirty = true
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()
	go r.loop()
}

func (r *coalescedReloader) loop() {
	for {
		if r.settle > 0 {
			timer := time.NewTimer(r.settle)
			select {
			case <-r.ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		r.mu.Lock()
		if !r.dirty || r.ctx.Err() != nil {
			r.running = false
			r.mu.Unlock()
			return
		}
		r.dirty = false
		r.mu.Unlock()
		r.reload(r.ctx)
	}
}

// subscribeCoalescedReloads routes every event on topics to r, and tears the
// subscriptions down when r's context ends. The handler only records the
// change: the bus's delivery goroutine is back before any reload reads a row.
func subscribeCoalescedReloads(bus *events.Bus, topics []string, subscriber string, r *coalescedReloader) {
	if bus == nil {
		return
	}
	unsubscribes := make([]func(), 0, len(topics))
	for _, topic := range topics {
		unsubscribe := bus.Subscribe(topic, func(events.Event) { r.request() }, events.WithSubscriberName(subscriber))
		if unsubscribe != nil {
			unsubscribes = append(unsubscribes, unsubscribe)
		}
	}
	if len(unsubscribes) == 0 {
		return
	}
	go func() {
		<-r.ctx.Done()
		for _, unsubscribe := range unsubscribes {
			unsubscribe()
		}
	}()
}
