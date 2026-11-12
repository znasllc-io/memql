package shopify

import (
	"context"
	"fmt"
)

// resume.go -- what a resume owes a store.
//
// While a store is paused the runtime HOLDS its deliveries (apply.go answers
// memqlsync.ErrRecordingPaused) and the continuing pass leaves its backfills
// where they were. Writing the status back to live undoes neither: a held
// delivery has no next try, and a domain the store never started to backfill
// is queued only at a boot. resumeStore does both, and it is asked from the
// two places that can know a store resumed:
//
//   - the Store page's Resume (handleStoreSetRecording), right after the
//     status was written;
//   - the watch (watchStoreVerdict), on the pass that records that the store
//     left its pause. That covers a resume made by writing the status
//     directly, and a Resume whose own call failed half-way.
//
// Both may run it for one resume, on different nodes at once, so it does
// nothing twice: a returned delivery is no longer held, the return's writes
// are fenced, and a queued backfill is no longer unstarted.

// resumeStore puts the store's held deliveries back in line and queues the
// backfills it never started. It answers an error when deliveries may still be
// held -- the return failed, or stopped at its bound -- so its caller knows
// the resume is not finished. A backfill that could not be queued is logged,
// as at a boot, which queues it again.
//
// EVERY HELD DELIVERY OF A STORE IS UNDER ITS OWN SOURCE, shopify-<store>,
// whether it was connected through the shared app or its own. The app-level
// source carries the three privacy topics and app/uninstalled, which Apply
// works before it asks whether the store is paused, and binds a store to no
// other topic; so nothing is ever held there and nothing is returned from it.
func (c *Connector) resumeStore(ctx context.Context, store Store) error {
	source := storeInboundSource(store.ID)
	_, err := c.engine.Execute(operatorContext(ctx), "builtin "+renderCall("datasyncReturnHeldInbound", map[string]any{"source": source}))
	c.queueUnstartedBackfillsOf(ctx, store)
	if err != nil {
		return fmt.Errorf("shopify: the deliveries held while store %q was paused could not all be put back in line: %w", store.ID, err)
	}
	return nil
}

// resumeSeenByWatch is resumeStore for the watch: it runs when the verdict the
// pass is about to record says the store left paused or not connected since
// the last one recorded -- its resumedAt moved. The watch records the verdict
// only when this answers no error, so a return that failed is tried again by
// the next pass, which still sees the store leaving its pause.
func (c *Connector) resumeSeenByWatch(ctx context.Context, store Store, previous map[string]any, v Verdict) error {
	resumed := timeOrEmpty(v.ResumedAt)
	if resumed == "" || resumed == mapString(previous, "resumedAt") {
		return nil
	}
	return c.resumeStore(ctx, store)
}
