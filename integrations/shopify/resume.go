package shopify

import (
	"context"
	"fmt"
)

// resume.go -- what is owed a store that records again.
//
// While a store is paused the runtime HOLDS its deliveries (apply.go answers
// memqlsync.ErrRecordingPaused) and the continuing pass leaves its backfills
// where they were. Writing the status back to live undoes neither: a held
// delivery has no next try, and a domain the store never started to backfill
// is queued only at a boot. Two things put that right, and they are told
// apart by how they know there is something to do.
//
// HELD DELIVERIES COME BACK BY STATE. Every pass of the store watch asks the
// runtime to return the held deliveries of every store that ingests
// (WatchStores, returnHeld). It does not matter what resumed the store, or
// whether anything noticed: a store paused and resumed by two direct status
// writes between two passes was never seen paused, and a return that stopped
// at its bound has more to give. A held delivery of a store that ingests never
// stays held past the next pass. With nothing held the question is one read.
// A store that does not ingest is never asked, so its deliveries stay held.
//
// The Store page's Resume (handleStoreSetRecording, resumeStore) returns ONE
// PAGE before it answers, so the person is not kept waiting and what they are
// looking at moves at once; the rest is the watch's.
//
// THE UNSTARTED BACKFILLS ARE QUEUED ON THE EDGE: by the act, and by the pass
// that records that the store left its pause (resumeSeenByWatch). Both may run
// for one resume, on different nodes at once, and nothing is done twice: a
// returned delivery is no longer held, the return's writes are fenced, and a
// queued backfill is no longer unstarted.

// resumeReturnsAtMost is how many held deliveries the act that resumes a store
// puts back in line before it answers: one page of the runtime's read.
const resumeReturnsAtMost = 50

// returnHeld asks the runtime to put a store's held deliveries back in line:
// at most `limit` of them, or as many as the runtime moves in one call when
// limit is zero. As the operator, which is whom the builtin admits.
//
// EVERY HELD DELIVERY OF A STORE IS UNDER ITS OWN SOURCE, shopify-<store>,
// whether it was connected through the shared app or its own. The app-level
// source carries the three privacy topics and app/uninstalled, which Apply
// works before it asks whether the store is paused, and binds a store to no
// other topic; so nothing is ever held there and nothing is returned from it.
func (c *Connector) returnHeld(ctx context.Context, store Store, limit int) error {
	args := map[string]any{"source": storeInboundSource(store.ID)}
	if limit > 0 {
		args["limit"] = limit
	}
	if _, err := c.engine.Execute(operatorContext(ctx), "builtin "+renderCall("datasyncReturnHeldInbound", args)); err != nil {
		return fmt.Errorf("shopify: the deliveries held while store %q was paused could not be put back in line: %w", store.ID, err)
	}
	return nil
}

// resumeStore is what the act that resumes a store does for it before it
// answers: one page of its held deliveries goes back in line, and the
// backfills it never started are queued. It answers nothing, because the
// resume stands whatever happens here -- the status is written and audited --
// and the watch does what this could not: it returns what is still held on
// its next pass, and queues the backfills on the pass that records the
// resume.
func (c *Connector) resumeStore(ctx context.Context, store Store) {
	if err := c.returnHeld(ctx, store, resumeReturnsAtMost); err != nil {
		c.logger.Warn("shopify: a resumed store's held deliveries were not put back in line; the store watch returns them",
			"store", store.ID, "error", err.Error())
	}
	if err := c.queueUnstartedBackfillsOf(ctx, store); err != nil {
		c.logger.Warn("shopify: a resumed store's backfills were not all queued; the store watch queues them",
			"store", store.ID, "error", err.Error())
	}
}

// resumeSeenByWatch is the edge, for the watch: when the verdict the pass is
// about to record says the store left paused or not connected since the last
// one recorded -- its resumedAt moved -- the backfills the store never started
// are queued. The watch records the verdict only when this answers no error,
// so a queue that failed is tried again by the next pass, which still sees the
// store leaving its pause. Held deliveries are not its business: every pass
// returns those, by state.
func (c *Connector) resumeSeenByWatch(ctx context.Context, store Store, previous map[string]any, v Verdict) error {
	resumed := timeOrEmpty(v.ResumedAt)
	if resumed == "" || resumed == mapString(previous, "resumedAt") {
		return nil
	}
	return c.queueUnstartedBackfillsOf(ctx, store)
}
