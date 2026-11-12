package datasync

import (
	"context"
	"fmt"
	"sort"
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

// maxBackInLine is that bound in deliveries: the most one call moves.
const maxBackInLine = maxRequeuePages * 50

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
//
// A row whose source NOBODY COULD TELL is a connector's -- a tenant list that
// could not be read answers "not served" to the routing question -- is left as
// it is and judged again on the next pass. The pass asks once for each such
// source and says so once, with how many deliveries wait on it: one line a
// row, every minute, is what an outage of a store list used to write.
func (d *Dispatcher) RetryDue(ctx context.Context) (RetryResult, error) {
	opCtx := OperatorContext(ctx)
	due, err := d.store.InboundRequestsDueForRetry(opCtx, d.now().UTC())
	if err != nil {
		return RetryResult{}, err
	}
	var out RetryResult
	undecided := map[string]*undecidedSource{}
	defer d.warnUndecided(undecided)
	for _, staged := range due {
		source := strings.TrimSpace(staged.Source)
		if waiting := undecided[source]; waiting != nil {
			waiting.deliveries++
			continue
		}
		if _, served := d.connectorFor(ctx, staged.Source); !served {
			unclaimed, err := d.unclaimed(ctx, source)
			if err != nil {
				undecided[source] = &undecidedSource{deliveries: 1, err: err}
				continue
			}
			if !unclaimed {
				// A connector claims the source and did not route it (its
				// secret could not be resolved just now): not this pass's to
				// retire, and judged again on the next.
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

// undecidedSource is a source nobody could tell is a connector's, in one
// pass: how many of the pass's deliveries wait on it, and why.
type undecidedSource struct {
	deliveries int
	err        error
}

// warnUndecided says, once for each source of a pass nobody could tell is a
// connector's, how many deliveries wait on it.
func (d *Dispatcher) warnUndecided(undecided map[string]*undecidedSource) {
	if d.logger == nil || len(undecided) == 0 {
		return
	}
	sources := make([]string, 0, len(undecided))
	for source := range undecided {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		d.logger.Warn("datasync: could not tell whether a connector serves the source of failed deliveries; they wait for the next pass",
			"source", source, "deliveries", undecided[source].deliveries, "error", undecided[source].err)
	}
}

// unclaimed reports whether no bound connector claims source, said
// definitely, or an error when one could not tell. The routing answer
// (connectorFor) is two-valued: a connector that could not read its tenants
// answers "not mine" there, and retiring a row on that answer would retire
// every tenant's row on the page for good whenever the read failed.
// memqlsync.SourceClaimed is three-valued -- a connector implementing
// InboundSourceClaimer returns an error when it cannot tell -- and only its
// definite "nobody" retires a row. A connector that claims a source it did
// not route (its secret could not be resolved just now) is not "nobody"
// either. One with no claimer is asked through InboundSource, as before.
func (d *Dispatcher) unclaimed(ctx context.Context, source string) (bool, error) {
	if d.claimed == nil {
		return true, nil
	}
	_, claimed, err := d.claimed(ctx, source)
	if err != nil {
		return false, err
	}
	return !claimed, nil
}

// Requeue puts every given-up delivery of one source back in line -- failed,
// with its four tries spent -- and returns how many. Its tries start again from
// none and it is due now. A delivery still waiting for its next try is not
// given up and is left as it is. A call that stops at its bound returns what it
// did; an operator presses again for the rest.
func (d *Dispatcher) Requeue(ctx context.Context, source string) (int, error) {
	n, _, err := d.backInLine(ctx, source, "requeueInbound", "given-up", maxBackInLine, d.store.InboundRequestsGivenUp)
	return n, err
}

// ReturnHeld puts the HELD deliveries of one source back in line -- the ones a
// connector would not apply while its tenant's recording was paused (hold) --
// at most `limit` of them, and returns how many it moved and whether it
// stopped at that bound. Like a requeued delivery, each has its tries set to
// none and is due now, and the retry pass works it; one whose tenant is still
// paused is held again.
//
// THE CALLER NAMES THE BOUND. A limit of zero or less is the runtime's own,
// maxBackInLine. The act that resumes a tenant names one page, so a person is
// answered at once; the connector's watch names nothing and asks again on
// every pass for every tenant that ingests, which is what makes stopping at
// the bound no failure: whatever is still held is returned by state, not
// because somebody remembered there was more.
//
// With nothing held, a call is one read and no write.
func (d *Dispatcher) ReturnHeld(ctx context.Context, source string, limit int) (int, bool, error) {
	if limit <= 0 || limit > maxBackInLine {
		limit = maxBackInLine
	}
	return d.backInLine(ctx, source, "returnHeldInbound", "held", limit, d.store.InboundRequestsHeld)
}

// backInLine puts the deliveries `page` answers for one source back in line:
// no tries counted, due now. It moves at most `limit` of them and reads at
// most maxRequeuePages pages, and returns how many it won and whether it
// stopped at a bound, with deliveries possibly left.
//
// The page is read again until it holds nothing this call has not already
// moved, so a write that did not take cannot keep the call reading. Each write
// is fenced on the version the read returned (Store.RequeueInboundRequest): a
// row another writer has moved since -- a second call's requeue, the retry
// pass's claim of a row the first call put back -- is left to that writer and
// not counted. Without the fence a second call could make due again a row a
// pass is working, and the delivery would be dispatched from two places.
func (d *Dispatcher) backInLine(ctx context.Context, source, builtin, what string, limit int, page func(context.Context, string) ([]StagedRequest, error)) (int, bool, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return 0, false, fmt.Errorf("datasync: %s needs the source whose %s deliveries go back in line", builtin, what)
	}
	opCtx := OperatorContext(ctx)
	at := d.now().UTC()
	seen := map[string]bool{}
	moved := 0
	for read := 0; read < maxRequeuePages && len(seen) < limit; read++ {
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
			if len(seen) >= limit {
				// The bound falls inside this page: what is left of it is
				// left held.
				return moved, true, nil
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
