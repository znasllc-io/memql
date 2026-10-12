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
// real database. These pin the SWEEP around it: when a round starts, how many
// statements a tick may carry, which concept gets each one, and what one
// concept's failure does to the others.

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

// tick calls the sweep once, the way the leader's tick does, and answers how
// many statements that issued.
func (f *fakeStatusHistoryPurger) tick(r *TopologyReconciler) int {
	before := len(f.calls)
	r.sweepStatusHistory(context.Background())
	return len(f.calls) - before
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

// The sweep is only a feature if the leader's tick calls it: every test below
// drives sweepStatusHistory directly, so a tick that stopped calling it would
// leave them all green and the table growing again. This one holds the lease
// the way the loop does and lets a real tick run -- twice, because what a tick
// carries is ONE statement and the second tick is the next concept's.
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
	if want := statusHistoryConcepts[:1]; !slices.Equal(history.concepts(), want) {
		t.Fatalf("a leader's tick must carry exactly one status history statement: calls = %v, want %v",
			history.concepts(), want)
	}
	r.tick(ctx)
	if want := statusHistoryConcepts[:2]; !slices.Equal(history.concepts(), want) {
		t.Fatalf("the next tick must carry the next concept's statement: calls = %v, want %v",
			history.concepts(), want)
	}
	if readiness.sweeps != 1 {
		t.Fatalf("the ticks must still run the readiness sweep they ran before, once per its interval, got %d", readiness.sweeps)
	}
}

func TestStatusHistorySweepRunsAtMostOncePerInterval(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	started := clock
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	purger := &fakeStatusHistoryPurger{}
	r.statusPurger = purger

	// The fake answers 0, a short batch, so a round is exactly one statement per
	// concept, one per tick.
	for i := range statusHistoryConcepts {
		if got := purger.tick(r); got != 1 {
			t.Fatalf("tick %d of the first round must carry one statement, got %d", i+1, got)
		}
		clock = clock.Add(defaultReconcileInterval)
	}
	if !slices.Equal(purger.concepts(), statusHistoryConcepts) {
		t.Fatalf("the first round must ask each concept once, in order: calls = %v, want %v",
			purger.concepts(), statusHistoryConcepts)
	}

	// Every concept came back short, so the sweep rests. The reconciler ticks
	// every 3 s: a statement on each of those ticks would be 200 passes over
	// three concepts per interval for the few hundred versions the heartbeats
	// add in ten minutes.
	for ; clock.Sub(started) < 5*time.Minute; clock = clock.Add(defaultReconcileInterval) {
		if got := purger.tick(r); got != 0 {
			t.Fatalf("%v into the interval the sweep must rest, got %d statements", clock.Sub(started), got)
		}
	}
	clock = started.Add(statusHistoryPurgeInterval - time.Second)
	if got := purger.tick(r); got != 0 {
		t.Fatalf("a second short of the interval the sweep must still rest, got %d statements", got)
	}

	// The interval is counted from when the round BEGAN, not from when it ended
	// six seconds later, and once it has passed the next round starts with the
	// first concept again.
	clock = started.Add(statusHistoryPurgeInterval)
	if got := purger.tick(r); got != 1 || purger.calls[len(statusHistoryConcepts)].concept != statusHistoryConcepts[0] {
		t.Fatalf("an interval after the first round began, a second must begin with %s, got %d statements: %v",
			statusHistoryConcepts[0], got, purger.concepts())
	}
	for i := 1; i < len(statusHistoryConcepts); i++ {
		purger.tick(r)
	}
	if len(purger.calls) != 2*len(statusHistoryConcepts) {
		t.Fatalf("the second round must be one statement per concept too, got %v", purger.concepts())
	}
	if got := purger.tick(r); got != 0 {
		t.Fatalf("after the second round the sweep rests again, got %d statements", got)
	}
}

func TestStatusHistorySweepLoopsUntilABatchComesBackShort(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	started := clock
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	logs := &statusHistoryLog{}
	r.logger = slog.New(logs)
	first := statusHistoryConcepts[0]
	purger := &fakeStatusHistoryPurger{answer: func(concept string, nth int) (int64, error) {
		if concept != first || nth > 3 {
			return 0, nil
		}
		return []int64{500, 500, 12}[nth-1], nil
	}}
	r.statusPurger = purger
	before := metrics.StatusHistoryVersionsPurgedValue(first)

	for range 5 {
		if got := purger.tick(r); got != 1 {
			t.Fatalf("every tick of a round carries exactly one statement, got %d", got)
		}
	}

	// The first concept's full batch means there may be more, so it is asked
	// again on its next turn -- after the others have had theirs. The 12 is
	// short, so it is finished and the round with it.
	want := []string{first, statusHistoryConcepts[1], statusHistoryConcepts[2], first, first}
	if got := purger.concepts(); !slices.Equal(got, want) {
		t.Fatalf("calls in order = %v, want %v", got, want)
	}
	if got := purger.tick(r); got != 0 {
		t.Fatalf("with every concept short the round is over and the sweep rests, got %d statements", got)
	}

	// Every statement is bounded by the batch size, by one cutoff for the whole
	// round (the grace before the round began), and by its own deadline.
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

	// One Info line for the whole round, when it ends, with what it took.
	infos := logs.atLevel(slog.LevelInfo)
	if len(infos) != 1 {
		t.Fatalf("want one Info line for the round, got %d: %+v", len(infos), infos)
	}
	if got := infos[0].attrs[first]; got != int64(1012) {
		t.Errorf("Info total for %s = %v, want 1012", first, got)
	}
	if got := infos[0].attrs["versions"]; got != int64(1012) {
		t.Errorf("Info versions = %v, want 1012", got)
	}
	if got := infos[0].attrs["statements"]; got != int64(5) {
		t.Errorf("Info statements = %v, want 5", got)
	}
}

// The property that keeps the sweep from starving the reconciler: a tick never
// carries more than one statement, however large the backlog and however slow
// the statement. A sweep that looped on the tick for as long as batches came
// back full would hold it for the length of the backlog.
func TestStatusHistoryTickIssuesAtMostOneStatement(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	purger := &fakeStatusHistoryPurger{
		// Every concept has a backlog that never ends, and every statement takes
		// 10 s on the clock.
		answer: func(string, int) (int64, error) { return statusHistoryBatch, nil },
		after:  func() { clock = clock.Add(10 * time.Second) },
	}
	r.statusPurger = purger

	for i := range 30 {
		if got := purger.tick(r); got != 1 {
			t.Fatalf("tick %d carried %d statements, want exactly 1: %v", i+1, got, purger.concepts())
		}
	}
	if len(purger.calls) != 30 {
		t.Fatalf("30 ticks must be 30 statements, got %d", len(purger.calls))
	}
}

// Two concepts with backlogs are served in turn, neither waiting behind the
// other, and a concept that has nothing left drops out of the rotation.
func TestStatusHistoryConceptsTakeTurnsWhileTwoHaveBacklogs(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	logs := &statusHistoryLog{}
	r.logger = slog.New(logs)
	c0, c1, c2 := statusHistoryConcepts[0], statusHistoryConcepts[1], statusHistoryConcepts[2]
	purger := &fakeStatusHistoryPurger{answer: func(concept string, nth int) (int64, error) {
		switch concept {
		case c0: // three full batches, then 7
			if nth <= 3 {
				return statusHistoryBatch, nil
			}
			return 7, nil
		case c1: // two full batches, then nothing
			if nth <= 2 {
				return statusHistoryBatch, nil
			}
			return 0, nil
		}
		return 0, nil // c2 has nothing at all
	}}
	r.statusPurger = purger

	for range 8 {
		if got := purger.tick(r); got != 1 {
			t.Fatalf("every tick of the round carries one statement, got %d", got)
		}
	}

	// Round one visits each concept; c2 is short and drops out; c0 and c1 then
	// alternate until c1 runs dry, and c0 finishes alone.
	want := []string{c0, c1, c2, c0, c1, c0, c1, c0}
	if got := purger.concepts(); !slices.Equal(got, want) {
		t.Fatalf("calls in order = %v, want %v", got, want)
	}
	if got := purger.tick(r); got != 0 {
		t.Fatalf("the round is over once every concept's latest batch was short, got %d statements", got)
	}

	infos := logs.atLevel(slog.LevelInfo)
	if len(infos) != 1 {
		t.Fatalf("want one Info line for the round, got %d: %+v", len(infos), infos)
	}
	if got := infos[0].attrs[c0]; got != int64(1507) {
		t.Errorf("Info total for %s = %v, want 1507", c0, got)
	}
	if got := infos[0].attrs[c1]; got != int64(1000) {
		t.Errorf("Info total for %s = %v, want 1000", c1, got)
	}
	if _, listed := infos[0].attrs[c2]; listed {
		t.Errorf("the Info line must not list %s, which purged nothing: %+v", c2, infos[0].attrs)
	}
}

// The cutoff belongs to the round, not to the tick: a round that spans minutes
// of backlog must not chase the clock, and the next round starts afresh.
func TestStatusHistoryRoundKeepsOneCutoff(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	started := clock
	r := newTestReconciler(&fakeReconcileEngine{}, fakeMesh{}, &clock)
	purger := &fakeStatusHistoryPurger{}
	r.statusPurger = purger

	for range statusHistoryConcepts {
		purger.tick(r)
		clock = clock.Add(defaultReconcileInterval)
	}
	second := started.Add(11 * time.Minute)
	clock = second
	for range statusHistoryConcepts {
		purger.tick(r)
		clock = clock.Add(defaultReconcileInterval)
	}

	if len(purger.calls) != 2*len(statusHistoryConcepts) {
		t.Fatalf("two rounds of one statement per concept, got %v", purger.concepts())
	}
	for i, c := range purger.calls {
		want := started.Add(-statusHistoryGrace)
		if i >= len(statusHistoryConcepts) {
			want = second.Add(-statusHistoryGrace)
		}
		if !c.olderThan.Equal(want) {
			t.Errorf("call %d olderThan = %v, want its round's cutoff %v", i, c.olderThan, want)
		}
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

	for range statusHistoryConcepts {
		purger.tick(r)
	}

	// One concept's failure is that concept's problem. The sweep is the only
	// thing keeping the table from growing again, so it does not give up on
	// the others, and it does not retry the failing one inside the same round.
	if want := statusHistoryConcepts; !slices.Equal(purger.concepts(), want) {
		t.Fatalf("calls = %v, want one per concept %v", purger.concepts(), want)
	}
	if got := purger.tick(r); got != 0 {
		t.Fatalf("a failed concept is done for the round, not asked again: got %d statements", got)
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
		t.Fatalf("want one Info line for the round, got %d: %+v", len(infos), infos)
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
	// rounds that were working hardest.
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

	// first, second, third, then first's second turn, which fails.
	for range 4 {
		purger.tick(r)
	}

	if got := purger.callsFor(first); got != 2 {
		t.Fatalf("a failed batch ends that concept's round: want 2 calls for %s, got %d", first, got)
	}
	if got := purger.tick(r); got != 0 {
		t.Fatalf("with every concept done the sweep rests, got %d statements", got)
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

	// A shutdown must not turn into a "context canceled" Warn per concept.
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
// the relations between them are what keep a statement safe on the leader's
// tick: inside the serving timeouts, and inside the liveness grace.
func TestStatusHistoryBoundsAreInsideTheServingTimeouts(t *testing.T) {
	if statusHistoryGrace != time.Hour || statusHistoryPurgeInterval != 10*time.Minute ||
		statusHistoryBatch != 500 || statusHistoryStatementTimeout != 7*time.Second {
		t.Fatalf("the decided values changed: grace %v, interval %v, batch %d, statement %v",
			statusHistoryGrace, statusHistoryPurgeInterval, statusHistoryBatch, statusHistoryStatementTimeout)
	}
	// 8 s is the request pool's statement_timeout (defaultStatementTimeoutMs in
	// component/database); the direct handle the reconciler uses carries the
	// same session parameters, and pgdriver's read deadline is 10 s.
	if statusHistoryStatementTimeout >= 8*time.Second {
		t.Errorf("a call's deadline %v must end before the server's 8 s statement_timeout", statusHistoryStatementTimeout)
	}
	// The relation that matters on the tick. The sweep shares the leader's tick
	// with reconcile, which retires a node it has found absent on two passes a
	// liveness grace apart. A tick held for the grace or longer leaves a gap
	// between passes in which a node can go absent, come back and go absent
	// again unseen, and be retired while it is serving. One statement is the
	// most a tick carries, so its deadline is the most a tick can be held.
	if statusHistoryStatementTimeout >= defaultReconcileGrace {
		t.Errorf("a statement (%v) holds the leader's tick, so it must be shorter than the reconciler's %v liveness grace",
			statusHistoryStatementTimeout, defaultReconcileGrace)
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
	if !r.lastStatusHistoryPurge.IsZero() || r.statusRound.active {
		t.Fatal("a sweep that had nothing to run must not start a round or the interval clock")
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
