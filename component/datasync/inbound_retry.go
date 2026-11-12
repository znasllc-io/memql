package datasync

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// inbound_retry.go -- a failed delivery is tried again (store mirror, U4).

// ---- A failed delivery is tried again (store mirror, U4) ----
//
// A delivery whose try failed for a reason worth another (worthAnotherTry) is
// stamped with when that try is due, and the retry pass -- the
// retryFailedInboundDeliveries automation, every minute -- tries it then,
// through DispatchStaged, so a retry passes the same judgement as the first
// try. Four tries at most; the hourly sweep stays the net under all of them.

// maxInboundAttempts is how many tries a delivery gets, the first included.
const maxInboundAttempts = 4

// inboundRetryAfter is the wait before each retry, counted from the try
// before it: the second try comes a minute after the first, the third five
// minutes after the second, the fourth thirty minutes after the third.
var inboundRetryAfter = [3]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// inboundRetryClaim is how far a claim moves a row's next try: long enough for
// one dispatch, an Admin fetch that may wait out a throttle included, so a
// second pass does not take the row while this one works it. A pass that dies
// mid-dispatch leaves the row due again when the window passes.
const inboundRetryClaim = 10 * time.Minute

// maxRequeuePages bounds one Requeue or ReturnHeld call: fifty rows a page.
const maxRequeuePages = 100

// RetryResult is what one retry pass did.
type RetryResult struct {
	// Tried counts the deliveries this pass claimed and dispatched.
	Tried int
	// Recovered counts the ones of those a connector worked.
	Recovered int
	// Held counts the ones of those a connector held: its tenant's recording
	// is paused, and the delivery waits for the resume.
	Held int
}

// RetryDue tries again ONE page of the failed deliveries whose next try has
// come. Each is claimed first (Store.ClaimInboundRetry) and skipped when the
// claim loses, so a pass that overlaps the last, or a second cron leader for a
// moment during a failover, never dispatches one row from two places at once.
// The scheduler does not prevent the overlap: a cron firing runs in its own
// goroutine whether or not the previous one has returned, and the automation's
// @mode(single) holds only within one process. A row of the page that cannot
// be claimed or dispatched does not stop the rest; its row says what happened.
//
// A row whose source no connector on this node serves -- its store was
// removed, or the node binds no connector -- is resolved BEFORE any claim and
// leaves the due set with one write (Store.ParkUnservedInbound): no next time,
// its try count as it was. A claim would only make it due again when the
// window passed, writing a version, an index entry and an event every ten
// minutes for ever; and left unwritten it would keep its place among the least
// recently written rows and hold the page against every real retry.
func (d *Dispatcher) RetryDue(ctx context.Context) (RetryResult, error) {
	opCtx := OperatorContext(ctx)
	due, err := d.store.InboundRequestsDueForRetry(opCtx, d.now().UTC())
	if err != nil {
		return RetryResult{}, err
	}
	var out RetryResult
	for _, staged := range due {
		if _, served := d.connectorFor(ctx, staged.Source); !served {
			if !d.definitelyUnclaimed(ctx, staged.Source) {
				// Nobody could say whether the source is a connector's -- a
				// tenant list that could not be read answers "not served"
				// above -- so the row is left as it is and judged again on
				// the next pass.
				continue
			}
			if err := d.store.ParkUnservedInbound(opCtx, staged); err != nil && d.logger != nil {
				d.logger.Warn("datasync: could not take a delivery no connector serves out of the retries; the next pass tries again",
					"request", staged.RequestId, "source", staged.Source, "error", err)
			}
			continue
		}
		// The window counts from the claim, not from the pass's start: a long
		// pass would otherwise claim its later rows with a window already gone.
		claimed, err := d.store.ClaimInboundRetry(opCtx, staged, d.now().UTC().Add(inboundRetryClaim))
		if err != nil {
			if d.logger != nil {
				d.logger.Warn("datasync: could not claim a failed delivery for its retry; the next pass tries again",
					"request", staged.RequestId, "source", staged.Source, "error", err)
			}
			continue
		}
		if !claimed {
			continue
		}
		out.Tried++
		res, err := d.DispatchStaged(ctx, staged)
		switch {
		case err == nil && res.Held:
			out.Held++
		case err == nil && res.Handled:
			out.Recovered++
		}
	}
	return out, nil
}

// definitelyUnclaimed reports whether no bound connector claims source, said
// definitely. The routing answer (connectorFor) is two-valued: a connector
// that could not read its tenants answers "not mine" there, and retiring a
// row on that answer would retire every tenant's row on the page for good
// whenever the read failed. memqlsync.SourceClaimed is three-valued -- a
// connector implementing InboundSourceClaimer returns an error when it cannot
// tell -- and only its definite "nobody" retires a row. A connector that
// claims a source it did not route (its secret could not be resolved just
// now) is "could not tell" as well. One with no claimer is asked through
// InboundSource, as before.
func (d *Dispatcher) definitelyUnclaimed(ctx context.Context, source string) bool {
	if d.claimed == nil {
		return true
	}
	_, claimed, err := d.claimed(ctx, strings.TrimSpace(source))
	if err != nil {
		if d.logger != nil {
			d.logger.Warn("datasync: could not tell whether a connector serves a failed delivery's source; it waits for the next pass",
				"source", source, "error", err)
		}
		return false
	}
	return !claimed
}

// Requeue puts every given-up delivery of one source back in line -- failed,
// with its four tries spent -- and returns how many. Its tries start again from
// none and it is due now. A delivery still waiting for its next try is not
// given up and is left as it is. A call that stops at its bound returns what it
// did; an operator presses again for the rest.
func (d *Dispatcher) Requeue(ctx context.Context, source string) (int, error) {
	n, _, err := d.backInLine(ctx, source, "requeueInbound", "given-up", d.store.InboundRequestsGivenUp)
	return n, err
}

// ReturnHeld puts every HELD delivery of one source back in line -- the ones a
// connector would not apply while its tenant's recording was paused (hold) --
// and returns how many. A connector calls it when the tenant resumes. Like a
// requeued delivery, each has its tries set to none and is due now, and the
// retry pass works it; one whose tenant is still paused is held again.
//
// A return that stopped at its bound with deliveries still held answers an
// error beside the count. Nobody presses this one again: its caller must know
// the job is not done, or the rest would stay held until the next resume.
func (d *Dispatcher) ReturnHeld(ctx context.Context, source string) (int, error) {
	n, more, err := d.backInLine(ctx, source, "returnHeldInbound", "held", d.store.InboundRequestsHeld)
	if err == nil && more {
		err = fmt.Errorf("datasync: %d held deliveries of %q were put back in line and more remain; it is to be run again", n, strings.TrimSpace(source))
	}
	return n, err
}

// backInLine puts the deliveries `page` answers for one source back in line:
// no tries counted, due now. It returns how many, and whether it stopped at
// its bound of maxRequeuePages reads with the last one still answering rows.
//
// The page is read again until it holds nothing this call has not already
// moved, so a write that did not take cannot keep the call reading. Each write
// is fenced on the version the read returned (Store.RequeueInboundRequest): a
// row another writer has moved since -- a second call's requeue, the retry
// pass's claim of a row the first call put back -- is left to that writer and
// not counted. Without the fence a second call could make due again a row a
// pass is working, and the delivery would be dispatched from two places.
func (d *Dispatcher) backInLine(ctx context.Context, source, builtin, what string, page func(context.Context, string) ([]StagedRequest, error)) (int, bool, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return 0, false, fmt.Errorf("datasync: %s needs the source whose %s deliveries go back in line", builtin, what)
	}
	opCtx := OperatorContext(ctx)
	at := d.now().UTC()
	seen := map[string]bool{}
	moved := 0
	for read := 0; read < maxRequeuePages; read++ {
		if err := ctx.Err(); err != nil {
			return moved, true, err
		}
		rows, err := page(opCtx, source)
		if err != nil {
			return moved, false, err
		}
		progressed := false
		for _, r := range rows {
			if seen[r.RequestId] || strings.TrimSpace(r.Source) != source {
				continue
			}
			seen[r.RequestId] = true
			progressed = true
			won, err := d.store.RequeueInboundRequest(opCtx, r, at)
			if err != nil {
				return moved, false, err
			}
			if won {
				moved++
			}
		}
		if !progressed {
			return moved, false, nil
		}
	}
	return moved, true, nil
}
