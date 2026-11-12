package datasync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// runners_rounds_test.go -- a continuing pass works in rounds until its budget
// is spent (ContinueBackfills).
//
// A store's backfill is a row of exports the origin prepares one at a time:
// each is started in one step and can be read only once the origin has
// finished it. A pass that took one step for a store and came back five
// minutes later left an empty store "starting" for hours. These hold the pass
// to its rounds: every store with work left takes a step in turn, a round in
// which every store only waited is followed by a pause, and the pass stops
// starting rounds when its budget is spent. The clock is the test's
// (clockedRunner): nothing here sleeps.

// roundsConcepts are the inbound domains of the origin below, in Domains()
// order.
var roundsConcepts = []string{testMirrorConcept, secondMirrorConcept, "v1:shopify:order", "v1:shopify:collection"}

// roundsOrigin is a partitioned connector whose every domain is an export the
// origin prepares: a step with no cursor starts one and hands back its cursor,
// a step with that cursor answers it again while the export is being prepared,
// and the step after that reads it to its end.
type roundsOrigin struct {
	*fakeConnector
	parts []string
	// preparing is how many polls of an export answer "still being prepared",
	// by partition/domain ("a/product"). Absent is none.
	preparing map[string]int
	// answer, when it answers true, is a step's answer in place of the export.
	answer func(partition, domain, cursor string) (memqlsync.BackfillPage, error, bool)

	stepMu  sync.Mutex
	polls   map[string]int
	asked   []string // partition/domain, in order
	cursors []string // the cursor each step was handed, in order
}

func newRoundsOrigin(partitions ...string) *roundsOrigin {
	c := mirrorConnector()
	c.domains = nil
	for _, concept := range roundsConcepts {
		c.domains = append(c.domains, memqlsync.DomainSpec{Concept: concept, Direction: memqlsync.DirectionInbound})
	}
	return &roundsOrigin{fakeConnector: c, parts: partitions, preparing: map[string]int{}, polls: map[string]int{}}
}

func (o *roundsOrigin) Partitions(context.Context) ([]string, error) { return o.parts, nil }

func (o *roundsOrigin) BackfillPartition(_ context.Context, partition, concept, cursor string) (memqlsync.BackfillPage, error) {
	o.stepMu.Lock()
	defer o.stepMu.Unlock()
	domain := strings.TrimPrefix(concept, "v1:shopify:")
	key := partition + "/" + domain
	o.asked = append(o.asked, key)
	o.cursors = append(o.cursors, cursor)
	if o.answer != nil {
		if page, err, ok := o.answer(partition, domain, cursor); ok {
			return page, err
		}
	}
	if cursor == "" {
		return memqlsync.BackfillPage{NextCursor: "export of " + key}, nil
	}
	if o.polls[key] < o.preparing[key] {
		o.polls[key]++
		return memqlsync.BackfillPage{NextCursor: cursor}, nil
	}
	return memqlsync.BackfillPage{Done: true}, nil
}

func (o *roundsOrigin) ReconcilePartition(context.Context, string, string, time.Time) (memqlsync.ReconcileReport, error) {
	return memqlsync.ReconcileReport{}, nil
}

// askedOf is how many steps were asked of one partition/domain, or of every
// domain of a partition ("a/").
func (o *roundsOrigin) askedOf(prefix string) int {
	o.stepMu.Lock()
	defer o.stepMu.Unlock()
	n := 0
	for _, key := range o.asked {
		if strings.HasPrefix(key, prefix) {
			n++
		}
	}
	return n
}

// firstAndLast are the positions of a partition/domain's first and last step.
func (o *roundsOrigin) firstAndLast(key string) (int, int) {
	o.stepMu.Lock()
	defer o.stepMu.Unlock()
	first, last := -1, -1
	for i, k := range o.asked {
		if k == key {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return first, last
}

// queueDomains queues the given domains of one partition.
func queueDomains(t *testing.T, r *Runner, partition string, concepts ...string) {
	t.Helper()
	for _, concept := range concepts {
		if err := r.QueueBackfill(context.Background(), "shopify", concept, partition); err != nil {
			t.Fatalf("QueueBackfill(%s, %s): %v", concept, partition, err)
		}
	}
}

func TestAStoreWithFourQueuedEmptyDomainsIsCompleteWithinOnePass(t *testing.T) {
	engine := newStateEngine()
	o := newRoundsOrigin("a")
	for _, concept := range roundsConcepts {
		// Each export answers "still being prepared" twice: to the step that
		// started it, and once more.
		o.preparing["a/"+strings.TrimPrefix(concept, "v1:shopify:")] = 2
	}
	r, clock := clockedRunner(engine, o)
	queueDomains(t, r, "a", roundsConcepts...)

	res := continueOnce(t, r)
	if res != (ContinueResult{Started: 4, Completed: 4}) {
		t.Errorf("one pass = %+v, want all four domains started and completed in it", res)
	}
	for _, concept := range roundsConcepts {
		if st := keptState(t, engine, concept, "a"); st.BackfillStatus != "complete" || st.LastError != "" {
			t.Errorf("%s = %s, lastError %q; want complete after one pass", concept, st.BackfillStatus, st.LastError)
		}
	}
	// One domain at a time, in Domains() order: each is carried to its end
	// before the next is started.
	want := strings.Repeat("a/product,", 4) + strings.Repeat("a/customer,", 4) + strings.Repeat("a/order,", 4) + strings.Repeat("a/collection,", 4)
	if got := strings.Join(o.asked, ",") + ","; got != want {
		t.Errorf("steps = %s\nwant   %s", got, want)
	}
	// One pause for each export: after the round in which it only waited.
	if len(clock.pauses) != 4 {
		t.Fatalf("the pass paused %d times, want once for each export it waited on: %v", len(clock.pauses), clock.pauses)
	}
	for _, p := range clock.pauses {
		if p != continueIdlePause {
			t.Errorf("a pause of %s, want %s", p, continueIdlePause)
		}
	}
}

func TestAnExportStillBeingPreparedWhenTheBudgetEndsKeepsItsCursorAndIsContinuedByTheNextPass(t *testing.T) {
	engine := newStateEngine()
	o := newRoundsOrigin("a")
	o.preparing["a/product"] = 1 << 20
	r, clock := clockedRunner(engine, o)
	queueDomains(t, r, "a", testMirrorConcept)

	began := clock.at
	res := continueOnce(t, r)
	if res != (ContinueResult{Started: 1}) {
		t.Errorf("the pass = %+v, want the domain started and nothing more: its export never became ready", res)
	}
	st := keptState(t, engine, testMirrorConcept, "a")
	if st.BackfillStatus != "running" || st.BackfillCursor != "export of a/product" || st.LastError != "" {
		t.Fatalf("after the budget: %s at %q, lastError %q; want still running at its export's cursor", st.BackfillStatus, st.BackfillCursor, st.LastError)
	}
	spent := clock.at.Sub(began)
	if spent > continueBudget || spent < continueBudget-continueIdlePause {
		t.Fatalf("the pass went on for %s, want it to stop starting rounds once its budget of %s is spent", spent, continueBudget)
	}
	// The first round starts the export and polls it once; every round after
	// it polls once, with a pause between two of them.
	if asked, want := o.askedOf("a/product"), len(clock.pauses)+3; asked != want {
		t.Errorf("the export was asked for %d times over %d pauses, want %d: its start, then one poll a round", asked, len(clock.pauses), want)
	}

	// The next pass finds the export ready and reads it, from the same cursor.
	o.preparing["a/product"] = 0
	if res := continueOnce(t, r); res != (ContinueResult{Completed: 1}) {
		t.Errorf("the next pass = %+v, want the domain completed", res)
	}
	if last := o.cursors[len(o.cursors)-1]; last != "export of a/product" {
		t.Errorf("the next pass asked from %q, want the cursor the last pass left", last)
	}
	if st := keptState(t, engine, testMirrorConcept, "a"); st.BackfillStatus != "complete" {
		t.Errorf("status = %q, want complete", st.BackfillStatus)
	}
}

func TestTwoStoresAreBothServedInOnePassNeitherWaitingForTheOtherToFinish(t *testing.T) {
	engine := newStateEngine()
	o := newRoundsOrigin("a", "b")
	for _, key := range []string{"a/product", "a/customer", "b/product", "b/customer"} {
		o.preparing[key] = 1
	}
	r, _ := clockedRunner(engine, o)
	for _, part := range []string{"a", "b"} {
		queueDomains(t, r, part, testMirrorConcept, secondMirrorConcept)
	}

	res := continueOnce(t, r)
	if res != (ContinueResult{Started: 4, Completed: 4}) {
		t.Errorf("one pass = %+v, want both stores' two domains started and completed", res)
	}
	for _, part := range []string{"a", "b"} {
		for _, concept := range []string{testMirrorConcept, secondMirrorConcept} {
			if st := keptState(t, engine, concept, part); st.BackfillStatus != "complete" {
				t.Errorf("store %s %s = %s, want complete", part, concept, st.BackfillStatus)
			}
		}
	}
	// Store b's first step comes before store a's first domain is done, and
	// store a goes on to its second domain before store b has finished.
	bFirst, bLast := o.firstAndLast("b/product")
	_, aFirstDomainDone := o.firstAndLast("a/product")
	aSecond, _ := o.firstAndLast("a/customer")
	_, bDone := o.firstAndLast("b/customer")
	if bFirst > aFirstDomainDone {
		t.Errorf("store b was first asked at step %d, after store a's first domain was done at %d: it waited for a\nsteps %v", bFirst, aFirstDomainDone, o.asked)
	}
	if aSecond > bDone || bLast < 0 {
		t.Errorf("store a began its second domain at step %d, after store b had finished at %d\nsteps %v", aSecond, bDone, o.asked)
	}
}

func TestAStoreWhoseStepFailsDoesNotStopTheOtherAndIsNotAskedAgainInThisPass(t *testing.T) {
	engine := newStateEngine()
	o := newRoundsOrigin("a", "b")
	o.preparing["b/product"] = 3
	o.answer = func(partition, _, _ string) (memqlsync.BackfillPage, error, bool) {
		if partition == "a" {
			return memqlsync.BackfillPage{}, errors.New("shopify admin: HTTP 502"), true
		}
		return memqlsync.BackfillPage{}, nil, false
	}
	r, _ := clockedRunner(engine, o)
	queueDomains(t, r, "a", testMirrorConcept, secondMirrorConcept)
	queueDomains(t, r, "b", testMirrorConcept)

	res, err := r.ContinueBackfills(context.Background())
	if err == nil || !strings.Contains(err.Error(), "partition a") || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("err = %v, want store a's failure reported", err)
	}
	if n := o.askedOf("a/"); n != 1 {
		t.Errorf("store a was asked %d times in the pass, want once: a store whose step failed is left alone for the rest of it\nsteps %v", n, o.asked)
	}
	if st := keptState(t, engine, testMirrorConcept, "a"); st.BackfillStatus != "failed" {
		t.Errorf("store a's failed domain = %s, want failed", st.BackfillStatus)
	}
	if st := keptState(t, engine, secondMirrorConcept, "a"); st.BackfillStatus != "queued" {
		t.Errorf("store a's next domain = %s, want it left queued for the next pass", st.BackfillStatus)
	}
	if st := keptState(t, engine, testMirrorConcept, "b"); st.BackfillStatus != "complete" {
		t.Errorf("store b = %s, want complete: store a's failure does not stop it", st.BackfillStatus)
	}
	if res != (ContinueResult{Started: 2, Completed: 1}) {
		t.Errorf("the pass = %+v, want both stores' domains started and store b's completed", res)
	}
}

func TestACancelledContextEndsThePassAtOnce(t *testing.T) {
	engine := newStateEngine()
	o := newRoundsOrigin("a", "b")
	o.preparing["a/product"], o.preparing["b/product"] = 1<<20, 1<<20
	r, clock := clockedRunner(engine, o)
	queueDomains(t, r, "a", testMirrorConcept)
	queueDomains(t, r, "b", testMirrorConcept)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancelled while the pass waits between two rounds.
	r.pause = func(ctx context.Context, d time.Duration) error {
		cancel()
		return clock.pause(ctx, d)
	}
	_, err := r.ContinueBackfills(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context's", err)
	}
	if len(clock.pauses) != 1 {
		t.Errorf("the pass paused %d times, want once: the cancelled pause ends it", len(clock.pauses))
	}
	// Two rounds were taken before the pause: the one that started both
	// exports, and one in which both only waited. Nothing after it.
	if got := strings.Join(o.asked, ","); got != "a/product,a/product,b/product,b/product,a/product,b/product" {
		t.Errorf("steps = %s, want none after the cancelled pause", got)
	}
	for _, part := range []string{"a", "b"} {
		if st := keptState(t, engine, testMirrorConcept, part); st.BackfillStatus != "running" || st.BackfillCursor == "" {
			t.Errorf("store %s = %s at %q, want left running at its cursor", part, st.BackfillStatus, st.BackfillCursor)
		}
	}

	// Cancelled between two stores of one round: the second is not asked.
	engine = newStateEngine()
	o = newRoundsOrigin("a", "b")
	r, _ = clockedRunner(engine, o)
	queueDomains(t, r, "a", testMirrorConcept)
	queueDomains(t, r, "b", testMirrorConcept)
	ctx, cancel = context.WithCancel(context.Background())
	o.answer = func(string, string, string) (memqlsync.BackfillPage, error, bool) {
		cancel()
		return memqlsync.BackfillPage{}, nil, false
	}
	if _, err := r.ContinueBackfills(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context's", err)
	}
	if n := o.askedOf("b/"); n != 0 {
		t.Errorf("store b was asked %d times after the context was done", n)
	}
}

func TestADomainIsCountedOnceHoweverManyRoundsItTook(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(
		memqlsync.BackfillPage{NextCursor: "c-1"},
		memqlsync.BackfillPage{NextCursor: "c-2"},
		memqlsync.BackfillPage{NextCursor: "c-3"},
		memqlsync.BackfillPage{NextCursor: "c-4"},
		memqlsync.BackfillPage{Done: true},
	)
	r, clock := clockedRunner(engine, c)
	r.maxPages = 1
	// Each page takes ninety seconds, so three rounds are started inside the
	// budget of four minutes: at 0s, 90s and 180s.
	c.duringBackfill = func(string) { clock.at = clock.at.Add(90 * time.Second) }
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-0"})

	if res := continueOnce(t, r); res != (ContinueResult{Advanced: 1}) {
		t.Errorf("a running domain read over three rounds = %+v, want advanced once", res)
	}
	if got := strings.Join(c.backfillSeen, ","); got != "c-0,c-1,c-2" {
		t.Errorf("cursors asked for = %q, want three pages, one a round, each from where the last stopped", got)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "running" || st.BackfillCursor != "c-3" {
		t.Errorf("row = %s at %q, want running at c-3", st.BackfillStatus, st.BackfillCursor)
	}
	// The pass that finishes it counts it completed, and not advanced as well.
	if res := continueOnce(t, r); res != (ContinueResult{Completed: 1}) {
		t.Errorf("the pass that finished it = %+v, want completed once", res)
	}

	// Queued, started, and read over two more rounds without finishing: started
	// once, and never advanced as well.
	engine = newStateEngine()
	c = mirrorConnector(
		memqlsync.BackfillPage{NextCursor: "c-1"},
		memqlsync.BackfillPage{NextCursor: "c-2"},
		memqlsync.BackfillPage{NextCursor: "c-3"},
		memqlsync.BackfillPage{NextCursor: "c-4"},
	)
	r, clock = clockedRunner(engine, c)
	r.maxPages = 1
	c.duringBackfill = func(string) { clock.at = clock.at.Add(90 * time.Second) }
	queueDomains(t, r, "", testMirrorConcept)
	if res := continueOnce(t, r); res != (ContinueResult{Started: 1}) {
		t.Errorf("a queued domain read over three rounds = %+v, want started once", res)
	}
}

func TestAPassPausesOnlyAfterARoundInWhichEveryStoreOnlyWaited(t *testing.T) {
	engine := newStateEngine()
	o := newRoundsOrigin("a", "b")
	// Store a waits on an export for five polls. Store b reads three pages
	// meanwhile, one a round.
	o.preparing["a/product"] = 5
	o.answer = func(partition, _, cursor string) (memqlsync.BackfillPage, error, bool) {
		if partition != "b" {
			return memqlsync.BackfillPage{}, nil, false
		}
		switch cursor {
		case "":
			return memqlsync.BackfillPage{NextCursor: "b-1"}, nil, true
		case "b-1":
			return memqlsync.BackfillPage{NextCursor: "b-2"}, nil, true
		}
		return memqlsync.BackfillPage{Done: true}, nil, true
	}
	r, clock := clockedRunner(engine, o)
	r.maxPages = 1
	queueDomains(t, r, "a", testMirrorConcept)
	queueDomains(t, r, "b", testMirrorConcept)

	if res := continueOnce(t, r); res != (ContinueResult{Started: 2, Completed: 2}) {
		t.Errorf("the pass = %+v, want both started and completed", res)
	}
	// Rounds 1 to 3 move store b, so nothing pauses although store a waits in
	// rounds 2 and 3. Rounds 4, 5 and 6 are store a's last three polls, alone:
	// a pause after each. Round 7 reads store a's export.
	if len(clock.pauses) != 3 {
		t.Errorf("the pass paused %d times, want 3: only after a round in which every store with work left waited\nsteps %v", len(clock.pauses), o.asked)
	}
	if n := o.askedOf("b/"); n != 3 {
		t.Errorf("store b was asked %d times, want its three pages", n)
	}
}

func TestWhileADomainRunsOnlyItsOwnRowIsReadAgain(t *testing.T) {
	engine := newStateEngine()
	o := newRoundsOrigin("a")
	o.preparing["a/product"] = 4
	r, clock := clockedRunner(engine, o)
	for _, concept := range roundsConcepts[1:] {
		writeKept(t, engine, SyncState{ConceptID: concept, Partition: "a", BackfillStatus: "complete"})
	}
	queueDomains(t, r, "a", testMirrorConcept)
	reads := func(concept string) int {
		return engine.countContaining(`query syncStateById(stateId: "` + SyncStateID(concept, "shopify", "inbound", "a") + `")`)
	}
	before := map[string]int{}
	for _, concept := range roundsConcepts {
		before[concept] = reads(concept)
	}

	if res := continueOnce(t, r); res != (ContinueResult{Started: 1, Completed: 1}) {
		t.Fatalf("the pass = %+v, want the one queued domain started and completed", res)
	}
	if len(clock.pauses) != 3 {
		t.Fatalf("the pass paused %d times, want 3: it must have taken several rounds for this to prove anything", len(clock.pauses))
	}
	// The other domains' rows are read when the pass picks a domain -- at its
	// first round, and once the running one is complete -- and in no round
	// between.
	for _, concept := range roundsConcepts[1:] {
		if n := reads(concept) - before[concept]; n != 2 {
			t.Errorf("%s's row was read %d times in a pass of several rounds, want 2: once for each pick", concept, n)
		}
	}
}

// A store that stopped ingesting while the pass ran -- paused, or uninstalled --
// answers that it cannot be asked now (memqlsync.Busy). Its row is left as it
// is, it is asked again in every round, and the pass after it resumes carries
// it on from the cursor it had.
func TestAStoreThatStopsIngestingMidPassKeepsItsRowAndIsContinuedOnceItResumes(t *testing.T) {
	for _, held := range []SyncState{
		{Partition: "a", BackfillStatus: "running", BackfillCursor: "export of a/product"},
		{Partition: "a", BackfillStatus: "queued"},
	} {
		t.Run(held.BackfillStatus, func(t *testing.T) {
			engine := newStateEngine()
			o := newRoundsOrigin("a")
			paused := true
			o.answer = func(string, string, string) (memqlsync.BackfillPage, error, bool) {
				if paused {
					return memqlsync.BackfillPage{}, memqlsync.Busy("shopify", `store "a" does not ingest now, so its mirror waits for it`), true
				}
				return memqlsync.BackfillPage{}, nil, false
			}
			r, clock := clockedRunner(engine, o)
			writeKept(t, engine, held)

			res, err := r.ContinueBackfills(context.Background())
			if err != nil {
				t.Fatalf("a store that stopped ingesting failed the pass: %v", err)
			}
			if res != (ContinueResult{}) {
				t.Errorf("the pass = %+v, want nothing counted", res)
			}
			st := keptState(t, engine, testMirrorConcept, "a")
			if st.BackfillStatus != held.BackfillStatus || st.BackfillCursor != held.BackfillCursor || st.LastError != "" {
				t.Fatalf("row = %s at %q, lastError %q; want it left %s at %q with no error", st.BackfillStatus, st.BackfillCursor, st.LastError, held.BackfillStatus, held.BackfillCursor)
			}
			if n := o.askedOf("a/"); n < 2 || len(clock.pauses) != n-1 {
				t.Errorf("the store was asked %d times with %d pauses, want it asked again in every round, a pause between each", n, len(clock.pauses))
			}

			paused = false
			asked := len(o.cursors)
			want := ContinueResult{Completed: 1}
			if held.BackfillStatus == "queued" {
				want.Started = 1
			}
			if res := continueOnce(t, r); res != want {
				t.Errorf("the pass after the resume = %+v, want %+v", res, want)
			}
			if got := o.cursors[asked]; got != held.BackfillCursor {
				t.Errorf("after the resume it was asked from %q, want the cursor it had, %q", got, held.BackfillCursor)
			}
			if st := keptState(t, engine, testMirrorConcept, "a"); st.BackfillStatus != "complete" {
				t.Errorf("status = %q, want complete", st.BackfillStatus)
			}
		})
	}
}

// A sweep of a store that stopped ingesting is skipped with nothing recorded,
// as a paused domain is: its row keeps what it had.
func TestASweepToldTheStoreCannotBeAskedNowIsSkippedAndRecordsNothing(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector()
	c.reconcileErr = memqlsync.Busy("shopify", `store "a" does not ingest now, so its mirror waits for it`)
	r := stateRunner(engine, c)
	swept := testNow.Add(-time.Hour)
	writeKept(t, engine, SyncState{BackfillStatus: "complete", LastReconcileAt: swept, LastAttemptAt: swept})
	before := writesCount(engine)

	res, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil {
		t.Fatalf("a sweep of a store that cannot be asked now failed: %v", err)
	}
	if !res.Skipped {
		t.Errorf("result = %+v, want skipped", res)
	}
	if got := rowWritesSince(engine, before); len(got) != 0 {
		t.Errorf("the skipped sweep wrote: %v", got)
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.LastError != "" || !st.LastReconcileAt.Equal(swept) {
		t.Errorf("row = lastError %q, swept %s; want it left as it was", st.LastError, st.LastReconcileAt)
	}
}

// ---- a write the store could not make is an error, not an abandonment ----

func TestABackfillWriteThatLosesTheFenceFiveTimesIsAnErrorNotAnAbandonment(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)
	r.store.retryWait = func(int) {}
	writeKept(t, engine, SyncState{BackfillStatus: "queued"})
	engine.loseFences = maxPatchAttempts

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err == nil {
		t.Fatalf("a write given up after %d lost fences reported no error: %+v", maxPatchAttempts, res)
	}
	if res.Abandoned {
		t.Errorf("result = %+v, want no abandonment: nothing says another writer holds the row", res)
	}
	if !strings.Contains(err.Error(), "kept changing under") {
		t.Errorf("err = %v, want the store's own reason", err)
	}
	if len(c.backfillSeen) != 0 {
		t.Errorf("the origin was asked %d times by a backfill that could not take its row", len(c.backfillSeen))
	}
	if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" {
		t.Errorf("row = %s, want it left queued: nothing was written", st.BackfillStatus)
	}

	// An operator's Backfill now answers the error, not "abandoned".
	engine.loseFences = maxPatchAttempts
	i := &Integration{runner: r, logger: discardLogger()}
	nodes, err := i.handleStartBackfill(clusterOwnerCtx(), map[string]any{"connector": "shopify", "conceptId": testMirrorConcept}, 0)
	if err == nil {
		t.Fatalf("the operator's start answered %v, want the error", nodes)
	}

	// The continuing pass reports it, counts nothing, and leaves the row for
	// the next pass.
	engine.loseFences = maxPatchAttempts
	passed, err := r.ContinueBackfills(context.Background())
	if err == nil || passed != (ContinueResult{}) {
		t.Errorf("the pass = %+v, %v; want the error and nothing counted", passed, err)
	}
	if res := continueOnce(t, r); res != (ContinueResult{Started: 1, Completed: 1}) {
		t.Errorf("the next pass = %+v, want the domain started and completed", res)
	}
}

// A row another writer took is still an abandonment.
func TestARowTakenByAnotherInvocationIsStillAbandoned(t *testing.T) {
	engine := newStateEngine()
	c := mirrorConnector(memqlsync.BackfillPage{NextCursor: "c-6"}, memqlsync.BackfillPage{Done: true})
	r := stateRunner(engine, c)
	writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-5"})
	c.duringBackfill = func(string) { writeKept(t, engine, SyncState{BackfillStatus: "running", BackfillCursor: "c-9"}) }

	res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
	if err != nil || !res.Abandoned {
		t.Errorf("result = %+v, %v; want abandoned with no error", res, err)
	}
}

// The pause a runner is built with is the real one: it ends when its time is
// up, and at once, with the context's error, when the context is done first.
func TestThePauseBetweenRoundsEndsWhenItsContextDoes(t *testing.T) {
	r := NewRunner(NewStore(newFakeEngine()), nil, discardLogger())
	if r.pause == nil {
		t.Fatal("a runner is built with no pause: its first idle round would panic")
	}
	if err := r.pause(context.Background(), time.Millisecond); err != nil {
		t.Errorf("a pause that ran its time answered %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	began := time.Now()
	if err := r.pause(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("a pause under a cancelled context answered %v, want the context's error", err)
	}
	if waited := time.Since(began); waited > time.Second {
		t.Errorf("a pause under a cancelled context waited %s", waited)
	}
}
