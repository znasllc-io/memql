package datasync

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// inbound_hold_db_test.go -- a held delivery through a real engine: what the
// held read answers and what it must not, and what a return leaves on the row.
// db-gated, like inbound_retry_db_test.go.

// paused is what a connector answers for a delivery of a paused tenant.
var paused = memqlsync.RecordingPaused("shopify", "store-1")

// drain runs the retry pass until it tries nothing more: the database is
// shared, and a page may be taken by other rows that are due.
func (f *retryFixture) drain(at time.Time, applyErr error) {
	f.t.Helper()
	f.clock, f.c.applyErr = at, applyErr
	for pass := 0; pass < 20; pass++ {
		res, err := f.d.RetryDue(f.ctx)
		if err != nil {
			f.t.Fatalf("RetryDue: %v", err)
		}
		if res.Tried == 0 {
			return
		}
	}
	f.t.Fatal("the retry pass kept finding deliveries to try")
}

func heldIDs(t *testing.T, f *retryFixture) []string {
	t.Helper()
	rows, err := f.store.InboundRequestsHeld(f.ctx, f.source)
	if err != nil {
		t.Fatalf("InboundRequestsHeld: %v", err)
	}
	return shortIDs(rows)
}

// THE HELD READ ANSWERS THE HELD DELIVERIES OF ONE SOURCE AND NOTHING ELSE. A
// held delivery is failed with no next try, like a refusal no retry changes;
// what tells the two apart is the stored reason, compared EXACTLY. A delivery
// refused for another reason, one given up, one waiting for its next try, one
// processed, one whose reason merely begins with the same words, and another
// source's held delivery are none of them answered. Nor is a held delivery
// answered by the due read or the given-up read: it waits for its resume.
func TestTheHeldReadAnswersOnlyTheHeldDeliveriesOfOneSource(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	nanos := time.Now().UnixNano()
	mine, other := fmt.Sprintf("dbtest-held-%d", nanos), fmt.Sprintf("dbtest-held-other-%d", nanos)
	cleanInbound(t, db, mine, other)
	at := time.Now().UTC().Truncate(time.Second)
	transient := errors.New("origin timed out")

	f := newRetryFixture(t, engineSeam{eng}, mine)
	f.clock = at.Add(-2 * time.Hour)

	first := f.stage("held-on-its-first-try")
	f.try(first, at.Add(-50*time.Minute), paused)
	if r := f.row(first); stringField(r, "status") != "failed" || stringField(r, "lastError") != memqlsync.ErrRecordingPaused.Error() ||
		stringField(r, "nextAttemptAt") != "" || intField(r, "attempts") != 0 {
		t.Fatalf("a delivery held on its first try is %v, want failed with the pause as its reason, no next time and no try counted", r)
	}

	later := f.stage("held-after-two-tries")
	f.try(later, at.Add(-45*time.Minute), transient)
	f.try(later, at.Add(-44*time.Minute), transient)
	f.try(later, at.Add(-39*time.Minute), paused)
	if r := f.row(later); intField(r, "attempts") != 2 || stringField(r, "nextAttemptAt") != "" ||
		stringField(r, "lastError") != memqlsync.ErrRecordingPaused.Error() {
		t.Fatalf("a delivery held on its third try is %v, want its two tries as they were, no next time and the pause as its reason", r)
	}

	refused := f.stage("refused")
	f.try(refused, at.Add(-30*time.Minute), memqlsync.Permanentf("pinned to another version"))
	givenUp := f.stage("given-up")
	for i := 4; i > 0; i-- {
		f.try(givenUp, at.Add(-time.Duration(i)*time.Hour), transient)
	}
	waiting := f.stage("waiting")
	f.try(waiting, at, transient)
	processed := f.stage("processed")
	f.try(processed, at.Add(-20*time.Minute), nil)
	// Failed with no next try, and a reason that only BEGINS as the hold's
	// does: an exact comparison leaves it alone.
	almost := f.stage("almost")
	if _, err := f.store.exec(f.ctx, call("mutation", "updateInboundRequestStatus",
		arg{"requestId", almost}, arg{"status", "failed"}, arg{"attempts", 1}, arg{"nextAttemptAt", ""},
		arg{"lastError", memqlsync.ErrRecordingPaused.Error() + " (shopify store-1)"})); err != nil {
		t.Fatal(err)
	}

	g := newRetryFixture(t, engineSeam{eng}, other)
	g.clock = at.Add(-2 * time.Hour)
	othersHeld := g.stage("held")
	g.try(othersHeld, at.Add(-10*time.Minute), paused)

	if got, want := heldIDs(t, f), []string{mine + "-held-on-its-first-try", mine + "-held-after-two-tries"}; !slices.Equal(got, want) {
		t.Fatalf("the held read answers %v, want %v, least recently written first", got, want)
	}
	gave, err := f.store.InboundRequestsGivenUp(f.ctx, mine)
	if err != nil {
		t.Fatal(err)
	}
	if got := shortIDs(gave); !slices.Equal(got, []string{mine + "-given-up"}) {
		t.Fatalf("the given-up read answers %v, want only the delivery that used its four tries -- a held one is not a change that could not be recorded", got)
	}
	due, err := f.store.InboundRequestsDueForRetry(f.ctx, at.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.RequestId == inboundRequestConcept+":"+first || d.RequestId == inboundRequestConcept+":"+later {
			t.Fatalf("the due read answers held delivery %s: the retry pass would try it on a schedule", d.RequestId)
		}
	}

	// The return: both back in line, due now and with no tries counted; and
	// nothing else moved.
	f.clock = at
	n, err := f.d.ReturnHeld(f.ctx, mine)
	if err != nil || n != 2 {
		t.Fatalf("ReturnHeld = %d, %v; want the two held deliveries", n, err)
	}
	for _, id := range []string{first, later} {
		if r := f.row(id); stringField(r, "status") != "failed" || intField(r, "attempts") != 0 || !timeField(r, "nextAttemptAt").Equal(at) {
			t.Fatalf("returned delivery %s is %v, want failed with no tries counted and due now", id, r)
		}
	}
	if r := f.row(refused); intField(r, "attempts") != 1 || stringField(r, "nextAttemptAt") != "" {
		t.Fatalf("a delivery refused for another reason was moved: %v", r)
	}
	if r := f.row(almost); intField(r, "attempts") != 1 || stringField(r, "nextAttemptAt") != "" {
		t.Fatalf("a delivery whose reason only begins as the hold's does was moved: %v", r)
	}
	if r := f.row(givenUp); intField(r, "attempts") != 4 {
		t.Fatalf("a given-up delivery was moved by a return of held ones: %v", r)
	}
	if r := g.row(othersHeld); stringField(r, "nextAttemptAt") != "" {
		t.Fatalf("another source's held delivery was moved: %v", r)
	}
	if got := heldIDs(t, f); len(got) != 0 {
		t.Fatalf("after the return the held read still answers %v", got)
	}
	// A second return finds nothing, and moves nothing.
	if n, err := f.d.ReturnHeld(f.ctx, mine); err != nil || n != 0 {
		t.Fatalf("a second ReturnHeld = %d, %v; want 0", n, err)
	}

	// The store is STILL paused when the retry pass reaches them: each is held
	// again, exactly as before, its tries still none.
	f.drain(at, paused)
	if got, want := heldIDs(t, f), []string{mine + "-held-on-its-first-try", mine + "-held-after-two-tries"}; !slices.Equal(sortedCopy(got), sortedCopy(want)) {
		t.Fatalf("returned to a store still paused, the held read answers %v, want both held again", got)
	}
	if r := f.row(later); intField(r, "attempts") != 0 || stringField(r, "nextAttemptAt") != "" {
		t.Fatalf("held again, the delivery is %v, want no tries counted and no next time", r)
	}

	// The store has resumed: returned once more, they are worked.
	if n, err := f.d.ReturnHeld(f.ctx, mine); err != nil || n != 2 {
		t.Fatalf("ReturnHeld after the second hold = %d, %v; want 2", n, err)
	}
	f.drain(at, nil)
	for _, id := range []string{first, later} {
		if r := f.row(id); stringField(r, "status") != "processed" || intField(r, "attempts") != 1 || stringField(r, "lastError") != "" {
			t.Fatalf("a returned delivery of a resumed store is %v, want processed at its first counted try", r)
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return out
}

// "TRY AGAIN" DURING A PAUSE ENDS HELD, NOT PROCESSED. A given-up delivery an
// operator puts back in line while its store is paused is dispatched, answered
// as paused, and held: it is no longer counted as given up, and it waits for
// the resume like any other.
func TestAGivenUpDeliveryRequeuedDuringAPauseEndsHeld(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	source := fmt.Sprintf("dbtest-held-requeue-%d", time.Now().UnixNano())
	cleanInbound(t, db, source)
	at := time.Now().UTC().Truncate(time.Second)
	f := newRetryFixture(t, engineSeam{eng}, source)
	f.clock = at.Add(-6 * time.Hour)
	id := f.stage("given-up")
	for i := 4; i > 0; i-- {
		f.try(id, at.Add(-time.Duration(i)*time.Hour), errors.New("origin timed out"))
	}
	f.clock = at
	if n, err := f.d.Requeue(f.ctx, source); err != nil || n != 1 {
		t.Fatalf("Requeue = %d, %v", n, err)
	}
	f.drain(at, paused)
	r := f.row(id)
	if stringField(r, "status") != "failed" || stringField(r, "lastError") != memqlsync.ErrRecordingPaused.Error() ||
		stringField(r, "nextAttemptAt") != "" || intField(r, "attempts") != 0 {
		t.Fatalf("requeued during a pause, the delivery is %v, want held: failed, the pause as its reason, no next time, no tries counted", r)
	}
	if got := heldIDs(t, f); !slices.Equal(got, []string{source + "-given-up"}) {
		t.Fatalf("the held read answers %v, want the requeued delivery", got)
	}
	gave, err := f.store.InboundRequestsGivenUp(f.ctx, source)
	if err != nil || len(gave) != 0 {
		t.Fatalf("the given-up read still answers %v (%v)", shortIDs(gave), err)
	}
}

// TWO RETURNS, ONE HELD DELIVERY. A resume is answered from two places -- the
// act that resumed the store and the watch that notices it -- and both may
// read the same held delivery. The fence lets exactly one write through; and a
// return that read the row before the retry pass claimed it cannot make it due
// again under the pass that is working it.
func TestOfTwoReturnsOfOneHeldDeliveryExactlyOneWins(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	source := fmt.Sprintf("dbtest-held-race-%d", time.Now().UnixNano())
	cleanInbound(t, db, source)
	at := time.Now().UTC().Truncate(time.Second)
	f := newRetryFixture(t, engineSeam{eng}, source)
	f.clock = at.Add(-time.Hour)
	id := f.stage("contended")
	f.try(id, at.Add(-30*time.Minute), paused)

	held, err := f.store.InboundRequestsHeld(f.ctx, source)
	if err != nil || len(held) != 1 {
		t.Fatalf("InboundRequestsHeld = %v, %v; want the one held delivery", shortIDs(held), err)
	}
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			won, err := f.store.RequeueInboundRequest(f.ctx, held[0], at)
			results <- won
			errs <- err
		}()
	}
	wins := 0
	for i := 0; i < 2; i++ {
		if <-results {
			wins++
		}
		if err := <-errs; err != nil {
			t.Fatalf("a return errored rather than losing: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d of two returns of one held delivery won, want exactly one", wins)
	}

	// The retry pass claims the returned row; a return still holding the
	// version it read before all of this loses, and the claim's window stands.
	due, found, err := f.d.Staged(f.ctx, id)
	if err != nil || !found {
		t.Fatalf("Staged: %v %t", err, found)
	}
	if claimed, err := f.store.ClaimInboundRetry(f.ctx, due, at.Add(inboundRetryClaim)); err != nil || !claimed {
		t.Fatalf("the claim of the returned row: %t, %v", claimed, err)
	}
	if won, err := f.store.RequeueInboundRequest(f.ctx, held[0], at); err != nil || won {
		t.Fatalf("a return that read the row before it was claimed: won=%t err=%v; want it to lose", won, err)
	}
	if r := f.row(id); !timeField(r, "nextAttemptAt").Equal(at.Add(inboundRetryClaim)) {
		t.Fatalf("after the late return the row is %v, want the claim's window still on it", r)
	}
}
