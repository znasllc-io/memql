package campaigns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// roster.go -- walking an audience of any size (memql#3460).
//
// # What was actually wrong
//
// The send read the roster WHOLE on every batch and diffed it against the
// whole delivery ledger. Both reads were bounded by a `paginate 5000`, and a
// bounded read of an unbounded set is a truncation -- so past the bound the
// worker would have diffed a PREFIX of the audience and declared the campaign
// complete having never seen the tail. campaignStartSend refused such a send
// instead, which was the right call for the bug and left "5000 is the largest
// mailing list this can send to" as a product ceiling.
//
// # The walk
//
// The roster is now read one page at a time through the engine's own keyset
// cursor, and the LEDGER IS READ FOR THAT PAGE rather than for the campaign.
// The second half matters as much as the first: reading the whole ledger is
// exactly as unbounded as reading the whole roster, so cursoring one and not
// the other would have moved the ceiling rather than removed it. The slice of
// the ledger a batch needs is precisely the slice of the roster it is looking
// at.
//
// # Why ascending createdAt is safe under concurrent roster edits
//
// This is the question a cursor over versioned rows has to answer, and the
// answer is an asymmetry rather than a lock.
//
// Reads collapse to the latest version per id (DISTINCT ON), so a row's
// position in the enumeration is its LATEST createdAt. Editing a recipient
// mid-send -- which this very worker does, converging a suppressed address's
// subscriptionStatus -- moves it to a NEWER timestamp, i.e. FORWARD in an
// ascending walk, never backward past the cursor.
//
//	seen already, then edited   reappears later in the pass  (duplicate)
//	not yet seen, then edited   still ahead of the cursor    (still seen)
//
// So a row can be seen twice and can never be missed. A duplicate costs a
// wasted comparison; a skip costs somebody their mail. The one place a
// duplicate would be harmful is twice in the SAME batch -- the delivery row
// is written after the send, so both copies would be mailed before either was
// recorded -- and dedupeing within the batch closes that, which is why the
// walk carries a `seen` set.
//
// # Completion without holding the roster
//
// "Is this campaign finished" used to be answerable by looking at the whole
// roster at once. It is now answered across ticks by one persisted bit: a
// PASS starts at the beginning of the audience with rosterOutstanding=false,
// and any recipient still to deal with -- unmailed, or pending a retry not
// yet due -- sets it. A pass that reaches the end with the bit still false has
// proved there is nothing left.
//
// # None of this is the idempotency mechanism
//
// The cursor is an optimization and is treated as one everywhere: losing it,
// finding it stale, or having the engine refuse it after a codec change costs
// one re-scan from the beginning. The delivery ledger decides who gets mailed,
// as it did before, so no correctness claim in this file rests on a stored
// position.

const (
	// maxRosterPagesPerDrain bounds how much of a large audience one tick
	// walks when it finds no work. The cursor is persisted, so a bounded
	// scan still makes progress every tick; without the bound, a single
	// pass over a million-address audience would hold the claim for the
	// whole walk.
	maxRosterPagesPerDrain = 40

	// rosterPageSize mirrors the `paginate` on audienceRosterForSend. Kept
	// as a named constant so the two cannot drift silently: the walk reads
	// exhaustion off the engine's cursor rather than off this number, but a
	// reader comparing the Go and the DSL should find one value.
	rosterPageSize = 500
)

// rosterWalk is one tick's worth of progress through an audience.
type rosterWalk struct {
	// batch is what to mail now, de-duplicated by recipient.
	batch []batchItem
	// cursor is where the NEXT tick resumes. Empty means "start a fresh
	// pass", which is also what exhaustion leaves behind.
	cursor string
	// exhausted reports that this tick reached the end of the audience.
	exhausted bool
	// outstanding is the per-PASS bit: has anything still to be dealt with
	// been seen since the pass began.
	outstanding bool
	// scanned counts recipients examined this tick, for the log line.
	scanned int
}

// walkRoster advances one job through its audience.
func (w *Worker) walkRoster(ownerCtx context.Context, job SendJob, now time.Time) (rosterWalk, error) {
	walk := rosterWalk{cursor: job.RosterCursor, outstanding: job.RosterOutstanding}
	if walk.cursor == "" {
		// A fresh pass proves its own claim from scratch; inheriting the
		// previous pass's bit would make the job never complete.
		walk.outstanding = false
	}

	seen := make(map[string]struct{}, w.cfg.BatchSize)
	restarted := false

	for pages := 0; pages < maxRosterPagesPerDrain; pages++ {
		page, next, err := w.store.RosterPage(ownerCtx, job.AudienceID, walk.cursor)
		if err != nil {
			if walk.cursor == "" || restarted {
				return walk, err
			}
			// A stored cursor the engine will not accept -- a pagination
			// codec change, an altered sort. It is a position, not data, so
			// the recovery is to forget it. Once: a second failure with no
			// cursor is a real read error and has to surface.
			w.logger.Warn("campaigns worker: the stored roster cursor was refused; restarting the pass from the beginning",
				"job", job.ID, "error", err)
			restarted = true
			walk.cursor = ""
			walk.outstanding = false
			continue
		}
		if len(page) == 0 {
			walk.exhausted = true
			walk.cursor = ""
			return walk, nil
		}

		ledger, err := w.store.LedgerFor(ownerCtx, job.CampaignID, canonicalIDs(page))
		if err != nil {
			return walk, err
		}

		filled := false
		for _, r := range page {
			walk.scanned++
			if _, duplicate := seen[r.ID]; duplicate {
				continue
			}
			entry, known := ledger[r.ID]
			switch {
			case !known:
				// Never attempted.
			case entry.Status != "pending":
				// Terminal: sent, skipped or failed. NEVER re-mailed.
				continue
			case !entry.NextAttemptAt.IsZero() && now.Before(entry.NextAttemptAt):
				// Outstanding, but waiting out a backoff.
				walk.outstanding = true
				continue
			}
			walk.outstanding = true
			seen[r.ID] = struct{}{}
			walk.batch = append(walk.batch, batchItem{recipient: r, attempts: entry.Attempts})
			if len(walk.batch) >= w.cfg.BatchSize {
				filled = true
				break
			}
		}

		if filled {
			// The cursor deliberately does NOT advance past a page the batch
			// limit cut short: the rest of this page still has to be dealt
			// with, and the next tick re-reads it. The recipients already
			// taken carry delivery rows by then, so re-reading costs a
			// comparison rather than a message.
			return walk, nil
		}

		walk.cursor = next
		if next == "" {
			walk.exhausted = true
			return walk, nil
		}
	}
	return walk, nil
}

// canonicalIDs projects a page's stored ids for the ledger read. See
// Recipient.CanonicalID for why the bare form would silently match nothing.
func canonicalIDs(page []Recipient) []string {
	out := make([]string, 0, len(page))
	for _, r := range page {
		if r.CanonicalID != "" {
			out = append(out, r.CanonicalID)
		}
	}
	return out
}

// cursorFingerprint is a short, stable digest of a walk position, for use in
// the cross-replica claim key.
//
// The claim key has to include the position, or a tick that only SCANNED --
// advancing the cursor without producing a terminal outcome -- would re-claim
// the same (job, progress) key next tick and be blocked until the lease
// expired. On a large audience that is the difference between a scan that
// makes progress every 15 seconds and one that makes progress every 5
// minutes. Hashed rather than embedded so the key stays a fixed short length
// whatever the cursor codec does.
func cursorFingerprint(cursor string) string {
	if cursor == "" {
		return "start"
	}
	sum := sha256.Sum256([]byte(cursor))
	return hex.EncodeToString(sum[:6])
}
