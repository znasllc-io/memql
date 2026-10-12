package campaigns

import (
	"context"
	"errors"
	"time"
)

// digest_sweep.go -- the campaigns worker's sweep of recipients that carry no
// address digest (memql#6022, F9).
//
// WHAT IT IS FOR. Every recipient addRecipient writes carries emailDigest, the
// key a newsletter signup and an email rule find a mailbox's rows by
// (mailbox.go). A recipient written without one -- before the field existed, by a
// replica still running the previous release, or by a raw write -- is found
// instead by a second, bounded read that compares addresses. The sweep puts the
// digest on those rows so that second read returns nothing; until it has, a
// signup is slower and, in an audience of more than 5,000 recipients, declined for
// now (mailbox.go), but never wrong. The pass itself is
// database.BackfillRecipientEmailDigests: idempotent, one batch per advisory-lock
// transaction, safe to run on every replica at once. Its header says what it does
// to the rows and why it is not a migration.
//
// WHEN IT RUNS. Ten minutes after the worker starts, and hourly after that.
//
//   - NOT in the pre-deploy migration Job. A recipient that carries emailDigest
//     cannot be written by a replica of the previous release (the concept is
//     closed), so stamping before the rollout would fail every unsubscribe,
//     bounce and by-hand change those replicas took until they were gone.
//   - Ten minutes after a worker starts, because by then a rollout is normally
//     over. It is a guess about the rollout and not a check of it: a rollout that
//     outlasts it is swept while an old replica still serves, and the recipients
//     that pass reached cannot be updated by that replica until it is gone.
//   - EVERY replica sweeps, ten minutes after its own boot, rather than one
//     replica per hour. The last replica to start sweeps after the rollout has
//     finished, which a one-per-hour claim cannot promise: the claim would go to
//     whichever replica's timer fired first, possibly before the rollout ended,
//     and leave the rows the old release wrote until the next hour. Replicas take
//     turns batch by batch under the pass's advisory lock, and a replica that
//     finds the work done reads and writes nothing.
//   - Hourly after that, for the rows a straggler or a raw write leaves behind.
//     A pass over recipients that all carry a digest writes nothing, and reads the
//     newest version of each in batches of 500 ids.
//
// The sweep never blocks the drain: it runs on its own goroutine, and the worker's
// Stop waits for it (an in-flight batch is bounded by a few seconds).

const (
	// digestSweepFirst is how long after a worker starts its first sweep runs.
	digestSweepFirst = 10 * time.Minute

	// digestSweepEvery is the gap between one sweep finishing and the next.
	digestSweepEvery = time.Hour
)

// RecipientDigestSweep stamps the address digest onto recipients written without
// one and reports how many it stamped. The worker is given one by the application
// (it owns the database handle); a worker without one never sweeps.
type RecipientDigestSweep func(ctx context.Context) (stamped int, err error)

// SetRecipientDigestSweep installs the sweep the worker runs after a rollout. Call
// it before Start; a sweep installed afterwards is not run.
func (w *Worker) SetRecipientDigestSweep(sweep RecipientDigestSweep) {
	w.digestSweep = sweep
}

// digestSweepLoop runs the sweep on its schedule until ctx ends, and closes done
// when it has returned.
func (w *Worker) digestSweepLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	first, every := w.digestSweepFirst, w.digestSweepEvery
	if first <= 0 {
		first = digestSweepFirst
	}
	if every <= 0 {
		every = digestSweepEvery
	}
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		w.SweepRecipientDigests(ctx)
		timer.Reset(every)
	}
}

// SweepRecipientDigests runs one pass. Exported so a test drives it directly
// rather than racing the schedule. A failed pass is a warning and nothing more: it
// leaves rows for the next pass, and no read depends on it having run.
func (w *Worker) SweepRecipientDigests(ctx context.Context) {
	if w.digestSweep == nil {
		return
	}
	stamped, err := w.digestSweep(ctx)
	switch {
	case err != nil && errors.Is(err, context.Canceled):
		// A node shutting down. The batches that committed stand and the next
		// worker's first pass carries on.
		w.logger.Info("campaigns worker: recipient digest sweep stopped by shutdown", "stamped", stamped)
	case err != nil:
		w.logger.Warn("campaigns worker: recipient digest sweep failed; the next pass walks again",
			"stamped", stamped, "error", err)
	case stamped > 0:
		w.logger.Info("campaigns worker: put the address digest on recipients written without one", "stamped", stamped)
	default:
		w.logger.Debug("campaigns worker: recipient digest sweep found every recipient stamped")
	}
}
