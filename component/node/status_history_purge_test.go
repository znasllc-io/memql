package node

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/metrics"
)

// What the DELETE does is status_history_purge_db_test.go's job, against a
// real database. These pin the SWEEP around it: when it runs, how long it keeps
// going, and what one concept's failure does to the others.

// statusHistoryCall is one purgeSuperseded request, as the fake saw it.
type statusHistoryCall struct {
	concept   string
	olderThan time.Time
	limit     int
	// timeLeft is how long the call's context had left when it was made, and
	// bounded says whether it had a deadline at all.
	timeLeft time.Duration
	bounded  bool
}

// fakeStatusHistoryPurger answers each call from a function and records it.
// after runs once the call has been made, which is how a test makes a
// statement take ten seconds on the fake clock.
type fakeStatusHistoryPurger struct {
	answer func(concept string, nth int) (int64, error) // nth counts this concept's calls from 1
	after  func()
	calls  []statusHistoryCall
}

func (f *fakeStatusHistoryPurger) purgeSuperseded(ctx context.Context, concept string, olderThan time.Time, limit int) (int64, error) {
	call := statusHistoryCall{concept: concept, olderThan: olderThan, limit: limit}
	if deadline, ok := ctx.Deadline(); ok {
		call.bounded, call.timeLeft = true, time.Until(deadline)
	}
	f.calls = append(f.calls, call)
	// A sweep that loops without end must fail the test with a message rather
	// than hang it until the package timeout.
	if len(f.calls) > 100 {
		panic("status history sweep made more than 100 calls: it is not stopping")
	}
	if f.after != nil {
		f.after()
	}
	if f.answer == nil {
		return 0, nil
	}
	return f.answer(concept, f.callsFor(concept))
}

func (f *fakeStatusHistoryPurger) callsFor(concept string) int {
	n := 0
	for _, c := range f.calls {
		if c.concept == concept {
			n++
		}
	}
	return n
}

// concepts is the order the sweep asked about them in, one entry per call.
func (f *fakeStatusHistoryPurger) concepts() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.concept)
	}
	return out
}

// statusHistoryLog keeps every record with its level and attributes, so a case
// can say how loudly something was said and what it carried.
type statusHistoryLog struct {
	mu      sync.Mutex
	records []statusHistoryLogRecord
}

type statusHistoryLogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func (l *statusHistoryLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *statusHistoryLog) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]any{}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, statusHistoryLogRecord{level: rec.Level, msg: rec.Message, attrs: attrs})
	return nil
}
func (l *statusHistoryLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *statusHistoryLog) WithGroup(string) slog.Handler      { return l }

func (l *statusHistoryLog) atLevel(level slog.Level) []statusHistoryLogRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []statusHistoryLogRecord
	for _, rec := range l.records {
		if rec.level == level {
			out = append(out, rec)
		}
	}
	return out
}

// leaderLeaseConnector is the smallest database/sql driver that lets a
// reconciler hold a *sql.Conn for its lease without Postgres: connecting and
// pinging succeed, and nothing else is ever asked of it.
type leaderLeaseConnector struct{}

func (leaderLeaseConnector) Connect(context.Context) (driver.Conn, error) {
	return leaderLeaseConn{}, nil
}
func (leaderLeaseConnector) Driver() driver.Driver { return leaderLeaseDriver{} }

type leaderLeaseDriver struct{}

func (leaderLeaseDriver) Open(string) (driver.Conn, error) { return leaderLeaseConn{}, nil }

type leaderLeaseConn struct{}

func (leaderLeaseConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (leaderLeaseConn) Close() error                        { return nil }
func (leaderLeaseConn) Begin() (driver.Tx, error)           { return nil, errors.New("not used") }

// The sweep is only a feature if the leader's tick calls it: every test above
// drives sweepStatusHistory directly, so a tick that stopped calling it would
// leave them all green and the table growing again. This one holds the lease
// the way the loop does and lets a real tick run.
func TestTheLeaderTickRunsTheStatusHistorySweep(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	readiness := &fakePurger{}
	history := &fakeStatusHistoryPurger{}
	r.readinessPurger = readiness
	r.statusPurger = history

	ctx := context.Background()
	sqlDB := sql.OpenDB(leaderLeaseConnector{})
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("hold a lease connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(); _ = sqlDB.Close() })
	r.conn, r.leader = conn, true

	r.tick(ctx)

	if readiness.sweeps != 1 {
		t.Fatalf("the tick must still run the readiness sweep it ran before, got %d", readiness.sweeps)
	}
	if want := statusHistoryConcepts; !slices.Equal(history.concepts(), want) {
		t.Fatalf("a leader's tick must run the status history sweep: calls = %v, want one per concept %v",
			history.concepts(), want)
	}
}

func TestStatusHistorySweepRunsAtMostOncePerInterval(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	purger := &fakeStatusHistoryPurger{}
	r.statusPurger = purger
	ctx := context.Background()

	// The fake answers 0, a short batch, so one round is exactly one call per
	// concept. The reconciler ticks every 3 s: a sweep on every tick would be
	// 200 passes over three concepts per interval for the few hundred versions
	// the heartbeats add in ten minutes.
	round := len(statusHistoryConcepts)

	r.sweepStatusHistory(ctx)
	if len(purger.calls) != round {
		t.Fatalf("the first sweep must make one round of purges (%d calls), got %d", round, len(purger.calls))
	}

	clock = clock.Add(5 * time.Minute)
	r.sweepStatusHistory(ctx)
	if len(purger.calls) != round {
		t.Fatalf("a sweep 5 minutes after the last must be skipped, got %d calls", len(purger.calls))
	}

	clock = clock.Add(6 * time.Minute) // 11 minutes after the first
	r.sweepStatusHistory(ctx)
	if len(purger.calls) != 2*round {
		t.Fatalf("a sweep 11 minutes after the last must run a second round (%d calls), got %d", 2*round, len(purger.calls))
	}
}

func TestStatusHistorySweepLoopsUntilABatchComesBackShort(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	started := clock
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	first := statusHistoryConcepts[0]
	purger := &fakeStatusHistoryPurger{answer: func(concept string, nth int) (int64, error) {
		if concept != first || nth > 3 {
			return 0, nil
		}
		return []int64{500, 500, 12}[nth-1], nil
	}}
	r.statusPurger = purger
	before := metrics.StatusHistoryVersionsPurgedValue(first)

	r.sweepStatusHistory(context.Background())

	// A full batch means there may be more, so the same concept is asked again;
	// the 12 is short, so it is finished and the next concept is asked once.
	want := []string{first, first, first, statusHistoryConcepts[1], statusHistoryConcepts[2]}
	if got := purger.concepts(); !slices.Equal(got, want) {
		t.Fatalf("calls in order = %v, want %v", got, want)
	}
	// Every statement is bounded by the batch size, by one cutoff for the whole
	// sweep (the grace before the sweep began), and by its own deadline.
	for i, c := range purger.calls {
		if c.limit != statusHistoryBatch {
			t.Errorf("call %d limit = %d, want %d", i, c.limit, statusHistoryBatch)
		}
		if wantCutoff := started.Add(-statusHistoryGrace); !c.olderThan.Equal(wantCutoff) {
			t.Errorf("call %d olderThan = %v, want %v", i, c.olderThan, wantCutoff)
		}
		if !c.bounded || c.timeLeft <= 0 || c.timeLeft > statusHistoryStatementTimeout {
			t.Errorf("call %d must carry a deadline of at most %v, got bounded=%v with %v left",
				i, statusHistoryStatementTimeout, c.bounded, c.timeLeft)
		}
	}
	if got := metrics.StatusHistoryVersionsPurgedValue(first) - before; got != 1012 {
		t.Errorf("metric for %s: want 1012 versions counted, got %v", first, got)
	}
}

func TestStatusHistorySweepStopsAtTheRunBudget(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	started := clock
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	first := statusHistoryConcepts[0]
	purger := &fakeStatusHistoryPurger{
		// Always a full batch, and every statement takes 10 s: a backlog that
		// would keep the leader's tick busy for as long as it lasted.
		answer: func(string, int) (int64, error) { return statusHistoryBatch, nil },
		after:  func() { clock = clock.Add(10 * time.Second) },
	}
	r.statusPurger = purger
	ctx := context.Background()

	r.sweepStatusHistory(ctx)

	// 10 s, 20 s, 30 s: the third call is the one that crosses 25 s, and the
	// sweep ends there rather than starting a fourth.
	if want := []string{first, first, first}; !slices.Equal(purger.concepts(), want) {
		t.Fatalf("calls = %v, want %v: the sweep stops after the call that crosses the %v budget",
			purger.concepts(), want, statusHistoryRunBudget)
	}

	// The clock moved 30 s during the sweep and the cutoff did not: it is one
	// figure for the whole sweep, taken when the sweep began.
	for i, c := range purger.calls {
		if want := started.Add(-statusHistoryGrace); !c.olderThan.Equal(want) {
			t.Errorf("call %d olderThan = %v, want the sweep's own cutoff %v", i, c.olderThan, want)
		}
	}

	// What is left is left for the next interval, not dropped: a sweep an
	// interval later runs again, from the start.
	clock = clock.Add(statusHistoryPurgeInterval)
	r.sweepStatusHistory(ctx)
	if len(purger.calls) != 6 || purger.calls[3].concept != first {
		t.Fatalf("the next interval must start another budgeted sweep, got calls %v", purger.concepts())
	}
}

func TestStatusHistorySweepLogsAndContinuesOnError(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	logs := &statusHistoryLog{}
	r.logger = slog.New(logs)
	failing := statusHistoryConcepts[1]
	boom := errors.New("statement timeout")
	purger := &fakeStatusHistoryPurger{answer: func(concept string, _ int) (int64, error) {
		if concept == failing {
			return 0, boom
		}
		return 7, nil
	}}
	r.statusPurger = purger
	beforeFailing := metrics.StatusHistoryVersionsPurgedValue(failing)
	beforeOther := metrics.StatusHistoryVersionsPurgedValue(statusHistoryConcepts[2])

	r.sweepStatusHistory(context.Background())

	// One concept's failure is that concept's problem. The sweep is the only
	// thing keeping the table from growing again, so it does not give up on
	// the others, and it does not retry the failing one inside the same round.
	if want := statusHistoryConcepts; !slices.Equal(purger.concepts(), want) {
		t.Fatalf("calls = %v, want one per concept %v", purger.concepts(), want)
	}

	warns := logs.atLevel(slog.LevelWarn)
	if len(warns) != 1 {
		t.Fatalf("want exactly one Warn (the failing concept), got %d: %+v", len(warns), warns)
	}
	if got := warns[0].attrs["concept"]; got != failing {
		t.Errorf("the Warn must name the failing concept %q, got %v", failing, got)
	}
	if got, ok := warns[0].attrs["error"].(error); !ok || !errors.Is(got, boom) {
		t.Errorf("the Warn must carry the error, got %v", warns[0].attrs["error"])
	}

	// The concepts that worked are reported and counted; the failing one is not.
	infos := logs.atLevel(slog.LevelInfo)
	if len(infos) != 1 {
		t.Fatalf("want one Info line for the sweep, got %d: %+v", len(infos), infos)
	}
	if got := infos[0].attrs[statusHistoryConcepts[0]]; got != int64(7) {
		t.Errorf("Info total for %s = %v, want 7", statusHistoryConcepts[0], got)
	}
	if _, listed := infos[0].attrs[failing]; listed {
		t.Errorf("the Info line must not list %s, which purged nothing: %+v", failing, infos[0].attrs)
	}
	if got := metrics.StatusHistoryVersionsPurgedValue(failing) - beforeFailing; got != 0 {
		t.Errorf("a concept that failed must count nothing, got %v", got)
	}
	if got := metrics.StatusHistoryVersionsPurgedValue(statusHistoryConcepts[2]) - beforeOther; got != 7 {
		t.Errorf("a concept after the failure must still be counted, want 7 got %v", got)
	}
}

func TestStatusHistorySweepKeepsEarlierBatchesWhenALaterOneFails(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	first := statusHistoryConcepts[0]
	// The first batch deletes 500 and the second times out. The 500 are gone
	// from the table whatever happens next, so the counter has to say so: a
	// counter that dropped them on the failure would under-report exactly the
	// sweeps that were working hardest.
	purger := &fakeStatusHistoryPurger{answer: func(concept string, nth int) (int64, error) {
		if concept != first {
			return 0, nil
		}
		if nth == 1 {
			return 500, nil
		}
		return 0, errors.New("statement timeout")
	}}
	r.statusPurger = purger
	before := metrics.StatusHistoryVersionsPurgedValue(first)

	r.sweepStatusHistory(context.Background())

	if got := purger.callsFor(first); got != 2 {
		t.Fatalf("a failed batch ends that concept's loop: want 2 calls for %s, got %d", first, got)
	}
	if got := metrics.StatusHistoryVersionsPurgedValue(first) - before; got != 500 {
		t.Errorf("the batch that landed before the failure must be counted: want 500, got %v", got)
	}
}

func TestStatusHistorySweepStopsWhenTheContextIsCancelled(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	logs := &statusHistoryLog{}
	r.logger = slog.New(logs)
	purger := &fakeStatusHistoryPurger{}
	r.statusPurger = purger
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r.sweepStatusHistory(ctx)

	// A shutdown must not turn into one "context canceled" Warn per concept.
	if len(purger.calls) != 0 || len(logs.atLevel(slog.LevelWarn)) != 0 {
		t.Fatalf("a cancelled context must end the sweep quietly, got %d calls and %d warnings",
			len(purger.calls), len(logs.atLevel(slog.LevelWarn)))
	}
}

// The list is closed on purpose: the statement deletes every superseded version
// of whatever concept it is given, and these three are the heartbeat concepts
// the design record rules nobody needs the history of. Adding a fourth has to be
// a decision someone reads, so the literal strings are pinned here rather than
// derived from the constants the variable is built from.
func TestStatusHistoryConceptsAreTheClosedList(t *testing.T) {
	want := []string{"v1:worker:registration", "v1:cluster:node", "v1:platform:moduleReadiness"}
	if !slices.Equal(statusHistoryConcepts, want) {
		t.Fatalf("statusHistoryConcepts = %v, want %v", statusHistoryConcepts, want)
	}
	// The counter exports one series per concept at boot, from its own copy of
	// the list. They are the same list, or a concept is purged that no series
	// reports, or a series reports a concept nothing purges.
	if got := metrics.StatusHistoryConcepts(); !slices.Equal(got, want) {
		t.Fatalf("metrics.StatusHistoryConcepts() = %v, want %v", got, want)
	}
}

// The numbers are the release's decisions (the design record's decision 1), and
// the relations between them are what keeps a sweep safe to run on the leader's
// tick: a statement inside the serving timeouts, a sweep that has ended before
// the next is due.
func TestStatusHistoryBoundsAreInsideTheServingTimeouts(t *testing.T) {
	if statusHistoryGrace != time.Hour || statusHistoryPurgeInterval != 10*time.Minute ||
		statusHistoryBatch != 500 || statusHistoryRunBudget != 25*time.Second ||
		statusHistoryStatementTimeout != 7*time.Second {
		t.Fatalf("the decided values changed: grace %v, interval %v, batch %d, budget %v, statement %v",
			statusHistoryGrace, statusHistoryPurgeInterval, statusHistoryBatch,
			statusHistoryRunBudget, statusHistoryStatementTimeout)
	}
	// 8 s is the request pool's statement_timeout (defaultStatementTimeoutMs in
	// component/database); the direct handle the reconciler uses carries the
	// same session parameters, and pgdriver's read deadline is 10 s.
	if statusHistoryStatementTimeout >= 8*time.Second {
		t.Errorf("a call's deadline %v must end before the server's 8 s statement_timeout", statusHistoryStatementTimeout)
	}
	// The budget is checked after each call, so a sweep can overshoot it by the
	// call in flight. Even then it must be over before the next one is due, or
	// sweeps would queue up behind each other on the leader's tick.
	if statusHistoryRunBudget+statusHistoryStatementTimeout >= statusHistoryPurgeInterval {
		t.Errorf("a sweep (%v budget + %v call) must end inside the %v interval",
			statusHistoryRunBudget, statusHistoryStatementTimeout, statusHistoryPurgeInterval)
	}
}

func TestStatusHistorySweepWithNoDatabaseDoesNothingAndDoesNotPanic(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	// newTestReconciler leaves dbGetter nil, which is what a DB-less binary and
	// every pre-existing reconciler test look like.
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	if p := r.historyPurger(); p != nil {
		t.Fatalf("a reconciler with no db must have no status-history purger, got %#v", p)
	}
	r.sweepStatusHistory(context.Background())
	if !r.lastStatusHistoryPurge.IsZero() {
		t.Fatal("a sweep that had nothing to run must not start the interval clock")
	}
}

func TestStatusHistoryPurgerIsBuiltFromTheReconcilersDbHandle(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)

	r.dbGetter = func() *bun.DB { return nil }
	if p := r.historyPurger(); p != nil {
		t.Fatalf("a dbGetter that has no handle yet means no purger, got %#v", p)
	}

	// The same handle purger() builds the readiness purger from: the direct,
	// non-pooled one the reconciler holds its lease on.
	db := &bun.DB{}
	r.dbGetter = func() *bun.DB { return db }
	p, ok := r.historyPurger().(sqlStatusHistoryPurger)
	if !ok || p.db != db {
		t.Fatalf("want a sqlStatusHistoryPurger over the reconciler's handle, got %#v", r.historyPurger())
	}
}

func TestStatusHistoryPurgerRefusesWhatItMustNotRun(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC)
	// No database: the refusals below come BEFORE the nil-db shortcut, so they
	// are refusals on every build rather than only on one with a database.
	p := sqlStatusHistoryPurger{}

	// The statement deletes every superseded version of the concept it is
	// given. Outside the closed list that is somebody's history: an identity's
	// past credentials, a work run's past states.
	for _, concept := range []string{"", "v1:identity:identity", "v1:work:run", "v1:cluster:nodeType"} {
		if _, err := p.purgeSuperseded(ctx, concept, cutoff, statusHistoryBatch); err == nil {
			t.Errorf("concept %q is not on the closed list and must be refused, not run", concept)
		}
	}
	// A LIMIT that is not positive is not a batch.
	for _, limit := range []int{0, -1} {
		if _, err := p.purgeSuperseded(ctx, statusHistoryConcepts[0], cutoff, limit); err == nil {
			t.Errorf("limit %d must be refused, not run", limit)
		}
	}
	// A real request with no database is simply nothing to do.
	n, err := p.purgeSuperseded(ctx, statusHistoryConcepts[0], cutoff, statusHistoryBatch)
	if err != nil || n != 0 {
		t.Fatalf("no database means no work and no error, got %d %v", n, err)
	}
}
