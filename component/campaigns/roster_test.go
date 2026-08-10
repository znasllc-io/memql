package campaigns

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// roster_test.go -- what memql#3460 has to be true for: an audience larger
// than one page is mailed completely, exactly once, and a resumed send does
// not re-mail anybody at that scale.
//
//	every member, exactly once   TestOversizedAudienceIsMailedCompletely
//	a resume does not re-mail    TestResumeAtScaleDoesNotReMail
//	the position is carried      TestWalkPersistsAndResumesItsPosition
//	a stale cursor is survivable TestRefusedCursorRestartsThePass
//	an edit cannot cause a skip  TestARecipientEditedMidPassIsNotSkipped
//	completion needs a full pass TestPartialPassDoesNotCompleteTheJob
//	the ledger read is bounded   TestLedgerIsReadPerPageNotPerCampaign

func bigRoster(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := range n {
		id := fmt.Sprintf("r%04d", i)
		out = append(out, recipientRow(id, id+"@example.test", "subscribed"))
	}
	return out
}

// oversizedEngine is a cluster with an audience several pages deep.
func oversizedEngine(n, pageSize int) *fakeEngine {
	return &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   bigRoster(n),
		pageSize: pageSize,
	}
}

// TestOversizedAudienceIsMailedCompletely is the issue's first acceptance
// criterion in one test: a send to an audience larger than one page
// completes, mailing every member exactly once.
//
// 1200 recipients over 100-row pages is twelve pages -- three times what the
// old whole-roster read could have held if the page size were the ceiling,
// which is the shape that used to be refused outright.
func TestOversizedAudienceIsMailedCompletely(t *testing.T) {
	const total = 1200
	engine := oversizedEngine(total, 100)
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.cfg.BatchSize = 250
	unthrottle(w)

	// Drain until the job completes, exactly as the poll loop would.
	for range 40 {
		w.DrainOnce(context.Background())
		if jobCompleted(engine) {
			break
		}
		applyDeliveries(engine)
	}
	applyDeliveries(engine)

	if got := sender.count(); got != total {
		t.Fatalf("mailed %d of %d recipients", got, total)
	}
	seen := map[string]int{}
	for _, addr := range sender.recipients() {
		seen[addr]++
	}
	for addr, n := range seen {
		if n != 1 {
			t.Errorf("%s was mailed %d times; the ledger is supposed to make that impossible", addr, n)
		}
	}
	if len(seen) != total {
		t.Errorf("%d distinct addresses mailed, want %d", len(seen), total)
	}
}

// TestResumeAtScaleDoesNotReMail is the second acceptance criterion: the
// ledger property, re-verified at a size where the walk's cursor is doing
// real work. The send is interrupted mid-way and started again from a fresh
// worker holding no memory of it at all.
func TestResumeAtScaleDoesNotReMail(t *testing.T) {
	const total = 800
	engine := oversizedEngine(total, 100)
	first := newTestWorker(t, engine, &recordingSender{})
	first.cfg.BatchSize = 120
	unthrottle(first)

	for range 3 {
		first.DrainOnce(context.Background())
		applyDeliveries(engine)
	}
	partway := len(engine.ledger)
	if partway == 0 || partway >= total {
		t.Fatalf("the setup did not stop mid-send: %d of %d delivered", partway, total)
	}
	alreadyMailed := map[string]bool{}
	for _, row := range engine.ledger {
		alreadyMailed[row["recipientId"].(string)] = true
	}

	// A DIFFERENT worker, with no cursor of its own and no in-memory state:
	// what a restarted pod, or the other replica, actually looks like.
	engine.jobs = []map[string]any{jobRow()}
	second := newTestWorker(t, engine, &recordingSender{})
	second.cfg.BatchSize = 120
	unthrottle(second)
	for range 40 {
		second.DrainOnce(context.Background())
		if jobCompleted(engine) {
			break
		}
		applyDeliveries(engine)
	}
	applyDeliveries(engine)

	sender := second.resolveSender().(*recordingSender)
	for _, addr := range sender.recipients() {
		id := "v1:campaigns:recipient:" + strings.TrimSuffix(addr, "@example.test")
		if alreadyMailed[id] {
			t.Fatalf("%s was mailed again after the resume", addr)
		}
	}
	if total-partway != sender.count() {
		t.Errorf("the resumed run mailed %d, want the %d that were left", sender.count(), total-partway)
	}
}

// TestWalkPersistsAndResumesItsPosition: the cursor is what stops every tick
// re-scanning a large audience from the beginning. Without it the walk is
// still correct and unusably slow, which is the kind of regression that hides.
func TestWalkPersistsAndResumesItsPosition(t *testing.T) {
	engine := oversizedEngine(500, 100)
	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.BatchSize = 10

	// Every recipient on the first two pages is already terminal, so the
	// walk has to move past them to find work.
	for i := range 200 {
		engine.ledger = append(engine.ledger, ledgerRow(fmt.Sprintf("r%04d", i), "sent", 1, time.Time{}))
	}

	walk, err := w.walkRoster(context.Background(), SendJob{AudienceID: testAudience, CampaignID: testCampaign}, time.Now())
	if err != nil {
		t.Fatalf("walkRoster: %v", err)
	}
	if walk.cursor == "" {
		t.Fatal("the walk left no position; the next tick would re-scan from the start of the audience")
	}
	if first := batchIDs(walk); len(first) == 0 || first[0] != "r0200" {
		t.Errorf("walk started at %v, want the first recipient past the terminal pages", first)
	}

	// Resuming from that position does not go back over the terminal pages.
	engine.calls = nil
	if _, err := w.walkRoster(context.Background(), SendJob{
		AudienceID: testAudience, CampaignID: testCampaign, RosterCursor: walk.cursor,
	}, time.Now()); err != nil {
		t.Fatalf("resumed walkRoster: %v", err)
	}
	for _, c := range engine.calls {
		if strings.HasPrefix(c.query, "query audienceRosterForSend") && !strings.Contains(c.query, "") {
			continue
		}
	}
	if n := countCalls(engine, "query audienceRosterForSend"); n > 2 {
		t.Errorf("the resumed walk read %d roster pages; it should have continued from the cursor", n)
	}
}

// TestRefusedCursorRestartsThePass: a stored position the engine will not
// accept -- a pagination codec change, an altered sort -- must cost a
// re-scan, not a wedged campaign. The cursor is an optimization and has to
// fail like one.
func TestRefusedCursorRestartsThePass(t *testing.T) {
	engine := &refusingCursorEngine{fakeEngine: oversizedEngine(50, 100)}
	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.BatchSize = 10

	walk, err := w.walkRoster(context.Background(), SendJob{
		AudienceID: testAudience, CampaignID: testCampaign, RosterCursor: "a-cursor-from-an-older-codec",
	}, time.Now())
	if err != nil {
		t.Fatalf("a refused cursor should have been survivable, got: %v", err)
	}
	if len(walk.batch) == 0 {
		t.Error("the walk gave up after the cursor was refused rather than restarting the pass")
	}
}

// TestARecipientEditedMidPassIsNotSkipped is the ordering argument, made
// executable. Reads collapse to the latest version per id, so editing a
// recipient moves it FORWARD in an ascending createdAt walk -- it can be seen
// twice and can never be missed. The batch de-duplication is what makes the
// duplicate harmless, and this is what proves it: without it the moved
// recipient is mailed twice in one batch, before either send is recorded.
func TestARecipientEditedMidPassIsNotSkipped(t *testing.T) {
	engine := oversizedEngine(6, 3)
	// r0000 was edited after the walk began, so it now sorts at the end --
	// present in page 1 (as the walk read it) and again in page 2.
	engine.roster = append(engine.roster, recipientRow("r0000", "r0000@example.test", "subscribed"))
	engine.pageSize = 3

	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.BatchSize = 50

	walk, err := w.walkRoster(context.Background(), SendJob{AudienceID: testAudience, CampaignID: testCampaign}, time.Now())
	if err != nil {
		t.Fatalf("walkRoster: %v", err)
	}
	counts := map[string]int{}
	for _, id := range batchIDs(walk) {
		counts[id]++
	}
	if counts["r0000"] != 1 {
		t.Errorf("r0000 appears %d times in one batch; a duplicate here is mailed twice before either send is recorded", counts["r0000"])
	}
	if len(counts) != 6 {
		t.Errorf("the walk reached %d distinct recipients, want 6 -- an edit must never cause a SKIP", len(counts))
	}
}

// TestPartialPassDoesNotCompleteTheJob: completion is proved by a full pass
// with nothing outstanding. A tick that stops early -- because it filled a
// batch, or hit its page budget -- has proved nothing, and a job completed on
// that evidence is a campaign that silently stopped mailing.
func TestPartialPassDoesNotCompleteTheJob(t *testing.T) {
	engine := oversizedEngine(400, 100)
	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.BatchSize = 10

	w.DrainOnce(context.Background())

	if jobCompleted(engine) {
		t.Fatal("the job completed after one partial pass over a 400-recipient audience")
	}
}

// TestLedgerIsReadPerPageNotPerCampaign pins the half of the fix that is easy
// to leave out. Cursoring the roster while still reading the whole ledger
// moves the ceiling rather than removing it -- the ledger for a large
// campaign is exactly as large as the roster.
func TestLedgerIsReadPerPageNotPerCampaign(t *testing.T) {
	engine := oversizedEngine(500, 100)
	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.BatchSize = 10

	w.DrainOnce(context.Background())

	if n := countCalls(engine, "query deliveryLedgerForCampaign"); n != 0 {
		t.Errorf("the send read the whole-campaign ledger %d times; that read is as unbounded as the roster was", n)
	}
	if n := countCalls(engine, "query deliveriesForRecipients"); n == 0 {
		t.Error("no per-page ledger read was issued")
	}
	for _, c := range engine.calls {
		if !strings.HasPrefix(c.query, "query deliveriesForRecipients") {
			continue
		}
		if got := len(idListOf(c.query, "recipientIds")); got > 100 {
			t.Errorf("a ledger read named %d recipients; it must be bounded by the roster page", got)
		}
		if c.actorID != testOwner {
			t.Errorf("the ledger was read under %q; it is owned-tier and must run as the campaign owner", c.actorID)
		}
	}
}

// --- harness ------------------------------------------------------------

// refusingCursorEngine refuses any read that carries a cursor, which is what
// a codec or sort change looks like from the store's side.
type refusingCursorEngine struct{ *fakeEngine }

func (e *refusingCursorEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query audienceRosterForSend") && cursorOf(ctx) != "" {
		return nil, fmt.Errorf("pagination cursor does not match the query sort order")
	}
	return e.fakeEngine.Execute(ctx, q)
}

// unthrottle lifts OUR own send-rate ceiling out of the way. These tests are
// about how much of an audience the walk reaches, and a token bucket sized
// for a real send would stop them at its own limit and look like a paging
// bug -- which is exactly how the first run of this file failed.
func unthrottle(w *Worker) {
	w.cfg.SendRatePerMinute = 1000000
	w.limiter = newRateLimiter(w.cfg.SendRatePerMinute, w.now)
}

func countCalls(engine *fakeEngine, prefix string) int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	n := 0
	for _, c := range engine.calls {
		if strings.HasPrefix(c.query, prefix) {
			n++
		}
	}
	return n
}

func jobCompleted(engine *fakeEngine) bool {
	for _, c := range engine.mutations("updateSendJob") {
		if argOf(c.query, "status") == "completed" {
			return true
		}
	}
	return false
}

// applyDeliveries folds the delivery rows the worker just wrote back into the
// fake's ledger, which is what a real database would have done between ticks.
// Without it every tick would re-select the same recipients and the test
// would be proving nothing about resumption.
func applyDeliveries(engine *fakeEngine) {
	engine.mu.Lock()
	calls := append([]recordedCall(nil), engine.calls...)
	engine.mu.Unlock()

	known := map[string]bool{}
	for _, row := range engine.ledger {
		known[row["recipientId"].(string)] = true
	}
	for _, c := range calls {
		if !strings.HasPrefix(c.query, "mutation recordCampaignDelivery(") {
			continue
		}
		// The worker writes BARE ids (the named-argument contract); the
		// ledger stores canonical ones, which is exactly the mismatch the
		// per-page ledger read has to get right. The fake re-canonicalizes
		// here the way the engine's relationship handling does.
		id := argOf(c.query, "recipientId")
		if id != "" && !strings.Contains(id, ":") {
			id = "v1:campaigns:recipient:" + id
		}
		if id == "" || known[id] {
			continue
		}
		known[id] = true
		engine.mu.Lock()
		engine.ledger = append(engine.ledger, map[string]any{
			"recipientId": id,
			"status":      argOf(c.query, "status"),
			"attempts":    1,
		})
		engine.mu.Unlock()
	}
}
