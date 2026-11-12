package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/money"
)

// check.go -- the periodic check: a pass over each store's mirrored orders,
// lines and refunds that proves the facts against the rows by version and by
// content, and repairs what differs.
//
// # Why a check
//
// The walk trusts versions: it folds what was written since its watermark,
// and a fact is written only over an older version of itself. Two things it
// cannot see. A row rewritten in place keeps its write time, so nothing asks
// for it again: a customer's erasure rewrites every version of a row that way.
// And a fact that was lost, or changed by hand, is never read back. So once a
// pass, the check walks each store's three sources from the floor again, in
// the walk's own windows (walkSource), and compares instead of trusting.
//
// # A pass
//
// A pass's state is the store's store_sales_check row: when it began
// (pass_started_at), and for each source a cursor -- empty, not begun; an
// instant, go on from there; or done -- with the window width the walk learned
// for that source, so a run that follows one that halved a window does not
// start again at a day. A call carries a store's pass on by at most budget
// pages of each source (checkPagesPerRun for a run); a pass that ended starts
// again at the next call.
//
// A store is checked only when its walk has caught up (walked_through within
// checkCaughtUp of the clock), it is neither rebuilding nor purging, and no
// wholesale pass is under way (wholesale_cursor empty); otherwise the call
// leaves it.
//
// # One transaction a page
//
// A page is read into facts before its transaction (readPage), as the walk
// reads one; checkPage then reads the stored facts the page names, compares,
// writes what differs through foldPage, re-derives, marks and moves the
// pass's cursor, all in one writeTx under the generation read when the call
// began the store. A rebuild that lands meanwhile answers errRebuilt: the
// call leaves the store, and the rebuild has deleted the check row.
//
// TWO CHECKS AT ONCE carry on one pass. Every transaction of a pass names the
// pass's start and the cursor it read; when the stored ones differ another
// call has moved the pass, and this one leaves the store (errPassMoved), so
// one call's counts are the pass's.
//
// # What is compared
//
// The cut is pass_started_at less checkCut. A row written after it is the
// walk's to fold, not a mismatch: its fact, if any, is marked and not
// compared. For a row at or before the cut, its fact is recomputed from the
// row in hand and compared with the stored one:
//
//   - no fact, or one older than the row: a mismatch, and the fact is written
//     from the row, live or deleted as the row is;
//   - a fact newer than the row: the row is an older version, read again
//     because the window is in the past. Nothing is compared or written;
//   - a fact at the row's own version: its content is compared with what the
//     row gives now. For a live row that is same, deleted false and, for an
//     order, the wholesale judgement; for a tombstone it is deleted true and
//     nothing else, since a deleted fact's other columns are never read. A
//     difference is a mismatch, written again from the row: the fold's guard
//     admits an equal version.
//
// A row rewritten in place between a page's read and its transaction -- an
// erasure landing then -- is compared by the content the page read. That is
// safe: it writes only when the stored fact already differs from what was
// read, so it never undoes the walk's work, and the next pass, which reads
// the rewritten row, repairs the fact from it.
//
// Content and not only version, because an erasure rewrites a row in place:
// one pass after it, an erased order is retail and a scrubbed custom item's
// fact holds the marker for its title. Every order page re-derives its
// orders, so a derived column is recomputed once a pass whatever differed.
// Every fact a page names is marked checked_at = pass_started_at, and so is
// the stored fact and the record of every row it met that could not be read:
// that row exists, and the walk keeps its fact from its last readable
// version.
//
// # The end of a pass
//
// When all three cursors are done, the facts no mirrored row answered for are
// deleted in batches (orphanBatch): facts that are not deleted, not marked by
// this pass, and at or before the cut. A deleted fact is left alone: it is the
// guard against the versions before it. A record of a row that could not be
// read and that the pass did not meet goes the same way. When a batch deletes
// nothing the pass ends (finishCheckPass): its counts and start become the
// last pass's, disagreeing is counted, clean_at moves when nothing was
// mismatched, and the pass's own columns are cleared.
//
// # Nothing of a row is reported
//
// What a check answers and logs is counts and store ids. A row that could not
// be read is logged as the walk logs it: its gid and a field name.

// checkPrincipal is the only caller the check admits: the maintenance
// principal its scheduled automation runs under
// (auth.MaintenanceActor("storeStatisticsCheckSweep")).
const checkPrincipal = "system:maintenance:storeStatisticsCheckSweep"

// The concepts of the nodes the three builtins answer with. Not DSL concepts:
// nothing stores them, as with the rollup's report.
const (
	checkConcept      = "integration:storeStats:check"
	rebuildConcept    = "integration:storeStats:rebuild"
	checkStateConcept = "integration:storeStats:checkState"
)

const (
	// checkPagesPerRun is how many counted pages of each source a run
	// carries a store's pass on by: the walk's own page budget.
	checkPagesPerRun = maxPagesPerRun
	// checkCaughtUp is how near the clock a store's walked_through must be
	// for the store to be checked.
	checkCaughtUp = 10 * time.Minute
	// checkCut is how long before the pass began a row may have been written
	// and be compared: the walk's overlap, the room a late commit has.
	checkCut = watermarkOverlap
	// cursorDone is a source's cursor once the pass has read it to the clock.
	cursorDone = "done"
)

// errPassMoved answers a transaction of a pass that another call has moved
// on or ended: nothing is written, and the call leaves the store.
var errPassMoved = errors.New("storestats: another check moved this pass on")

// CheckReport says what one run of the check did: counts and the ids of the
// stores whose pass ended. Compared, Mismatched and Repaired are this run's
// own work. Compared counts comparisons, not rows: a row an overlap reads
// again is compared, and counted, again. Disagreeing is the sum over the
// passes that ended in this run.
type CheckReport struct {
	Stores      int      `json:"stores"`
	Compared    int      `json:"compared"`
	Mismatched  int      `json:"mismatched"`
	Repaired    int      `json:"repaired"`
	Disagreeing int      `json:"disagreeing"`
	Finished    []string `json:"finished,omitempty"`
}

// A CHECK RUN HAS A BUDGET AND A DEADLINE OF ITS OWN. The check's sweep runs
// on its own schedule, outside a rollup's turn, so it does not share the
// rollup's deadline. Each store of a run has a turn of its own (withTurn),
// whose budget its pages and batches ask, as a rollup's turn does; once
// checkRunBudget is spent the run takes up no new store. checkRunDeadline
// cancels a run that hangs, and sits above checkRunDeadlineBound and below
// the schedule's hour (TestTheCheckRunDeadlineIsBelowTheHour).
var (
	checkRunBudget   = 30 * time.Minute
	checkRunDeadline = checkRunDeadlineBound() + time.Minute
)

// checkStartUnit is the most a pass's start takes, the one unit of a check
// asked no budget within: the store's state read, its floor read and the
// pass's start, each a transaction. The floor is only read: a store is
// checked once its walk has caught up, and its walk kept the floor first.
func checkStartUnit() time.Duration { return 3 * txTimeout }

// checkRunDeadlineBound is the most a check run can take, its budgets spent:
//
//	the stores read:      maxStorePages (20) reads x readBound (10 s)    200 s
//	the wholesale reads:  wholesaleReadBudget (10 s) + the read under way
//	                      when it ran out, readBound (10 s)                20 s
//	the run's budget:     checkRunBudget                                 1800 s
//	the turn's budget:    turnBudget, of the last store taken up           30 s
//	the turn's tail:      what still runs once that budget is spent,
//	                      the longest of: a page (one read and its
//	                      transaction, or a read that outlasted its
//	                      bound and the width write after it, 30 s);
//	                      the last batch and the end of the pass that
//	                      follows it (2 x 20 s, 40 s); and a pass's
//	                      start (60 s)                                     60 s
//	                                                             total   2110 s
//
// The five terms follow one another and add; the tail's three are
// alternatives, since a turn's budget runs out at one point. Thirty-five minutes and
// ten seconds, and the deadline adds a minute over it.
func checkRunDeadlineBound() time.Duration {
	unit := max(readBound+txTimeout, 2*txTimeout, checkStartUnit())
	return maxStorePages*readBound + wholesaleReadBudget + readBound + checkRunBudget + turnBudget + unit
}

// runCheck lists every store and carries each one's pass on in turn, from the
// store after the one the last check run on this node took up last. When the
// wholesale state cannot be read, no store is checked: a check judged with
// nobody wholesale would write every granted buyer's order retail.
func runCheck(ctx context.Context, i *Integration, now time.Time) (CheckReport, error) {
	rep := CheckReport{}
	if i.engine == nil {
		return rep, errors.New("this node has no engine handle, so the store statistics cannot be checked against the mirror")
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	if db == nil {
		return rep, errors.New("this node has no database handle, so the store statistics cannot be checked")
	}
	ctx = walkContext(ctx)
	stores, err := listStores(ctx, i.engine)
	if err != nil {
		return rep, fmt.Errorf("list the stores: %w", err)
	}
	rep.Stores = len(stores)
	wholesale, err := i.wholesaleStates(ctx, stores)
	if err != nil {
		return rep, fmt.Errorf("check no store: %w", err)
	}
	var failures []error
	began := budgetNow()
	first := i.firstToCheck(stores)
	for k := range stores {
		id := stores[(first+k)%len(stores)]["id"].(string)
		if k > 0 && !budgetNow().Before(began.Add(checkRunBudget)) {
			break
		}
		i.checkTookUp(id)
		if _, err := checkStore(withTurn(ctx), i, id, now, checkPagesPerRun, wholesale[id], &rep); err != nil {
			i.logger.Warn("Store statistics for a store were not checked this run; the next run carries the pass on",
				"component", "storestats", "storeId", id, "error", err)
			failures = append(failures, &storeFailure{StoreID: id, Err: err})
		}
	}
	return rep, errors.Join(failures...)
}

// firstToCheck is the index in stores, in bare-id order, of the first store
// after the one the last check run on this node took up last, or 0.
func (i *Integration) firstToCheck(stores []map[string]any) int {
	i.runningMu.Lock()
	last := i.checkLastTaken
	i.runningMu.Unlock()
	if last == "" {
		return 0
	}
	for k, store := range stores {
		if store["id"].(string) > last {
			return k
		}
	}
	return 0
}

func (i *Integration) checkTookUp(id string) {
	i.runningMu.Lock()
	i.checkLastTaken = id
	i.runningMu.Unlock()
}

// checkPass is a pass as the store's check row holds it.
type checkPass struct {
	started time.Time         // zero: no pass under way
	cursor  map[string]string // by source
	width   map[string]int64  // by source, in seconds
}

// window is where the pass stands in a source, as the walk reads it.
func (p checkPass) window(source string) (walkWindow, error) {
	w := walkWindow{Width: time.Duration(p.width[source]) * time.Second}
	c := p.cursor[source]
	if c == "" {
		return w, nil
	}
	t, err := time.Parse(time.RFC3339Nano, c)
	if err != nil {
		return walkWindow{}, fmt.Errorf("the check's %s cursor is not an instant", source)
	}
	w.Through = t.UTC()
	return w, nil
}

// cursorAt is a source's cursor standing at through.
func cursorAt(through time.Time) string {
	if through.IsZero() {
		return ""
	}
	return through.UTC().Format(time.RFC3339Nano)
}

func seconds(d time.Duration) int64 { return int64(windowWidth(d) / time.Second) }

// checkStore carries one store's pass on by budget pages of each source;
// finished is true when the pass ended in this call. wholesale is the store's
// state as the run read it; rep, when not nil, adds this call's counts. A
// store that is not to be checked now, a rebuild that lands under the pass,
// and a pass another call moved on each leave the store with no error.
func checkStore(ctx context.Context, i *Integration, storeID string, now time.Time, budget int, wholesale wholesaleState,
	rep *CheckReport) (finished bool, err error) {
	if strings.TrimSpace(storeID) == "" {
		return false, fmt.Errorf("check a store: %w", errNoStore)
	}
	if rep == nil {
		rep = &CheckReport{}
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	if db == nil {
		return false, errors.New("this node has no database handle, so the store statistics cannot be checked")
	}
	if turnSpent(ctx) {
		return false, nil
	}
	generation, pass, ok, err := checkable(ctx, db, storeID, now)
	if err != nil || !ok {
		return false, err
	}
	floor, err := ensureFloor(ctx, i, storeID, now)
	if err != nil {
		return false, err
	}
	if pass.started.IsZero() {
		if err := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			var err error
			pass, err = startCheckPass(ctx, tx, storeID, now)
			return err
		}); err != nil {
			return false, leaves(err)
		}
	}

	clock := func() time.Time { return now }
	var failures []error
	for _, s := range walkedSources {
		if pass.cursor[s.source] == cursorDone {
			continue
		}
		done, err := i.checkSource(ctx, db, storeID, s, floor, generation, pass, clock, budget, wholesale, rep)
		if errors.Is(err, errRebuilt) || errors.Is(err, errPassMoved) {
			return false, nil
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if done {
			pass.cursor[s.source] = cursorDone
		}
	}
	if len(failures) > 0 {
		return false, errors.Join(failures...)
	}
	for _, s := range walkedSources {
		if pass.cursor[s.source] != cursorDone {
			return false, nil
		}
	}

	// The facts no row answered for, in batches, each asked the turn's budget.
	for {
		if turnSpent(ctx) {
			return false, nil
		}
		var deleted, facts int
		if err := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			var err error
			deleted, facts, err = orphanBatch(ctx, tx, storeID, pass.started)
			return err
		}); err != nil {
			return false, leaves(err)
		}
		rep.Mismatched += facts
		rep.Repaired += facts
		if deleted == 0 {
			break
		}
	}
	var disagreeing int
	if err := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
		var err error
		finished, disagreeing, err = finishCheckPass(ctx, tx, storeID, pass.started, now)
		return err
	}); err != nil {
		return false, leaves(err)
	}
	if finished {
		rep.Finished = append(rep.Finished, storeID)
		rep.Disagreeing += disagreeing
	}
	return finished, nil
}

// leaves is a pass's error as a call answers it: a rebuild under the pass and
// a pass another call moved on leave the store with no error.
func leaves(err error) error {
	if errors.Is(err, errRebuilt) || errors.Is(err, errPassMoved) {
		return nil
	}
	return sanitized(err)
}

// checkable reads, before any write, whether the store is to be checked now,
// its generation and its pass. See "A pass".
func checkable(ctx context.Context, db *bun.DB, storeID string, now time.Time) (generation int64, pass checkPass, ok bool, err error) {
	err = readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		var purging bool
		var rebuilding, walked sql.NullTime
		var wholesaleCursor string
		if err := tx.NewSelect().Table("store_sales_store").
			Column("generation", "purging", "rebuilding_since", "walked_through", "wholesale_cursor").
			Where("store_id = ?", storeID).Scan(ctx, &generation, &purging, &rebuilding, &walked, &wholesaleCursor); err != nil {
			return err
		}
		ok = !purging && !rebuilding.Valid && wholesaleCursor == "" &&
			walked.Valid && !walked.Time.Before(now.Add(-checkCaughtUp))
		if !ok {
			return nil
		}
		var err error
		pass, err = readPass(ctx, tx, storeID)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, checkPass{}, false, nil
	}
	if err != nil {
		return 0, checkPass{}, false, fmt.Errorf("store %s: read whether the store is to be checked: %w", storeID, sanitized(err))
	}
	return generation, pass, ok, nil
}

// checkRow is a store's store_sales_check row.
type checkRow struct {
	bun.BaseModel `bun:"table:store_sales_check,alias:c"`

	StoreID             string       `bun:"store_id"`
	PassStartedAt       sql.NullTime `bun:"pass_started_at"`
	OrderCursor         string       `bun:"order_cursor"`
	OrderWindowSeconds  int64        `bun:"order_window_seconds"`
	LineCursor          string       `bun:"line_cursor"`
	LineWindowSeconds   int64        `bun:"line_window_seconds"`
	RefundCursor        string       `bun:"refund_cursor"`
	RefundWindowSeconds int64        `bun:"refund_window_seconds"`
	Compared            int          `bun:"compared"`
	Mismatched          int          `bun:"mismatched"`
	Repaired            int          `bun:"repaired"`
	LastStartedAt       sql.NullTime `bun:"last_started_at"`
	LastFinishedAt      sql.NullTime `bun:"last_finished_at"`
	LastCompared        int          `bun:"last_compared"`
	LastMismatched      int          `bun:"last_mismatched"`
	LastRepaired        int          `bun:"last_repaired"`
	LastDisagreeing     int          `bun:"last_disagreeing"`
	CleanAt             sql.NullTime `bun:"clean_at"`
}

// readCheckRow reads the store's check row inside tx; found is false when it
// has none.
//
// staged-data: INDIFFERENT -- this reads a store_sales_ side table of this package, never MemoryNodes; no staged-data row can exist in it, so the gate has nothing to withhold.
func readCheckRow(ctx context.Context, tx bun.Tx, storeID string) (row checkRow, found bool, err error) {
	err = tx.NewSelect().Model(&row).Where("store_id = ?", storeID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return checkRow{}, false, nil
	}
	if err != nil {
		return checkRow{}, false, fmt.Errorf("read the store's check: %w", err)
	}
	return row, true, nil
}

// readPass reads the store's pass inside tx: one with no start when none is
// under way.
func readPass(ctx context.Context, tx bun.Tx, storeID string) (checkPass, error) {
	row, _, err := readCheckRow(ctx, tx, storeID)
	if err != nil {
		return checkPass{}, err
	}
	p := checkPass{
		cursor: map[string]string{sourceOrder: row.OrderCursor, sourceLine: row.LineCursor, sourceRefund: row.RefundCursor},
		width:  map[string]int64{sourceOrder: row.OrderWindowSeconds, sourceLine: row.LineWindowSeconds, sourceRefund: row.RefundWindowSeconds},
	}
	if row.PassStartedAt.Valid {
		p.started = row.PassStartedAt.Time.UTC()
	}
	return p, nil
}

// passStands answers errPassMoved unless the store's pass is the one begun
// at started and, when source is named, that source's cursor is from; with
// no source, unless every cursor is done.
func passStands(ctx context.Context, tx bun.Tx, storeID string, started time.Time, source, from string) error {
	p, err := readPass(ctx, tx, storeID)
	if err != nil {
		return err
	}
	if !p.started.Equal(started) {
		return errPassMoved
	}
	if source != "" {
		if p.cursor[source] != from {
			return errPassMoved
		}
		return nil
	}
	for _, s := range walkedSources {
		if p.cursor[s.source] != cursorDone {
			return errPassMoved
		}
	}
	return nil
}

// checkSource carries the pass on in one source. done is true when the source
// was read to the clock.
func (i *Integration) checkSource(ctx context.Context, db *bun.DB, storeID string, s walkedSource, floor time.Time, generation int64,
	pass checkPass, clock func() time.Time, budget int, wholesale wholesaleState, rep *CheckReport) (done bool, err error) {
	w, err := pass.window(s.source)
	if err != nil {
		return false, fmt.Errorf("store %s: %w", storeID, err)
	}
	expect, kept := pass.cursor[s.source], windowWidth(w.Width)
	next, _, err := walkSource(ctx, i.engine, s.query, storeID, floor, w, clock, budget, func(page walkPage) error {
		facts, err := readPage(storeID, s.source, page.Rows, wholesale)
		if err != nil {
			return fmt.Errorf("store %s, %s: a page could not be read: %w", storeID, s.source, err)
		}
		to := cursorAt(page.Through)
		if !page.CaughtUp.IsZero() {
			to = cursorDone
		}
		p := checkedPage{source: s.source, started: pass.started, from: expect, to: to, width: seconds(page.Width), facts: facts,
			wholesaleEnabled: wholesale.Enabled, wholesaleDigest: heldDigest(wholesale)}
		var res checkResult
		if err := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			var err error
			res, err = checkPage(ctx, tx, storeID, p)
			return err
		}); err != nil {
			if errors.Is(err, errRebuilt) || errors.Is(err, errPassMoved) {
				return err
			}
			return fmt.Errorf("store %s, %s: a page could not be checked: %w", storeID, s.source, sanitized(err))
		}
		expect, kept = to, windowWidth(page.Width)
		rep.Compared += res.compared
		rep.Mismatched += res.mismatched
		rep.Repaired += res.repaired
		for _, u := range res.logged {
			i.logger.Warn("Store statistics: a mirrored row could not be read; it was recorded and no fact was written",
				"component", "storestats", "storeId", storeID, "source", s.source, "gid", u.gid, "field", u.field, "error", u.err.Error())
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if expect == cursorDone {
		return true, nil
	}
	// A width the walk changed with no page handed on -- a page that
	// outlasted a read halves it -- is kept for the pass's next call.
	if windowWidth(next.Width) != kept {
		p := checkedPage{source: s.source, started: pass.started, from: expect, to: expect, width: seconds(next.Width), facts: pageFacts{source: s.source}}
		if err := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			_, err := checkPage(ctx, tx, storeID, p)
			return err
		}); err != nil {
			if errors.Is(err, errRebuilt) || errors.Is(err, errPassMoved) {
				return false, err
			}
			return false, fmt.Errorf("store %s, %s: keep the check's window: %w", storeID, s.source, sanitized(err))
		}
	}
	return false, nil
}

// checkedPage is one page of a pass as its transaction is handed it.
type checkedPage struct {
	source   string
	started  time.Time // the pass's
	from, to string    // the source's cursor before the page and after it
	width    int64     // the source's window, in seconds, after it
	facts    pageFacts
	// The granted set the page's orders were judged with: whether the store
	// is enabled in it and its digest (see "Who is wholesale, for the check").
	wholesaleEnabled bool
	wholesaleDigest  string
}

// checkResult is what one page of a pass did.
type checkResult struct {
	compared, mismatched, repaired int
	logged                         []unreadableRow
}

// The four answers a row in hand gives about its stored fact. See "What is
// compared".
type rowAgainstFact int

const (
	rowAfterTheCut   rowAgainstFact = iota // the walk's to fold: marked, not compared
	rowOlderThanFact                       // read again from a past window: marked, not compared
	rowNewerThanFact                       // no fact, or an older one: a mismatch
	rowAtTheFact                           // compared by content
)

// againstFact says where a row of this version stands to a stored fact of
// that one (nil: no fact).
func againstFact(row time.Time, stored *time.Time, cut time.Time) rowAgainstFact {
	switch {
	case row.After(cut):
		return rowAfterTheCut
	case stored == nil || stored.Before(row):
		return rowNewerThanFact
	case stored.After(row):
		return rowOlderThanFact
	}
	return rowAtTheFact
}

// micro is an instant as a TIMESTAMPTZ column holds it: bun writes six
// digits of the second and drops the rest.
func micro(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// checkPage is one page of a pass inside tx. See "One transaction a page" and
// "What is compared".
func checkPage(ctx context.Context, tx bun.Tx, storeID string, p checkedPage) (checkResult, error) {
	if err := passStands(ctx, tx, storeID, p.started, p.source, p.from); err != nil {
		return checkResult{}, err
	}
	cut := p.started.Add(-checkCut)
	repair := pageFacts{source: p.source, unreadable: p.facts.unreadable}
	var res checkResult
	var named []string
	var table, gidColumn string
	switch p.source {
	case sourceOrder:
		table, gidColumn = "store_sales_order_fact", "order_gid"
		for _, o := range p.facts.orders {
			named = append(named, o.fact.OrderGID)
		}
		stored, err := storedOrders(ctx, tx, storeID, named)
		if err != nil {
			return checkResult{}, err
		}
		judges, err := checkJudgesWholesale(ctx, tx, storeID, p)
		if err != nil {
			return checkResult{}, err
		}
		for _, o := range p.facts.orders {
			s, held := stored[o.fact.OrderGID]
			var version *time.Time
			if held {
				version = &s.OrderVersion
				if !judges {
					// The side the walk and the pass wrote stands.
					o.wholesale = s.Wholesale
				}
			}
			switch againstFact(micro(o.fact.Version), version, cut) {
			case rowNewerThanFact:
				res.compared++
				res.mismatched++
				repair.orders = append(repair.orders, o)
			case rowAtTheFact:
				res.compared++
				same, err := orderAgrees(o, s)
				if err != nil {
					return checkResult{}, err
				}
				if !same {
					res.mismatched++
					repair.orders = append(repair.orders, o)
				}
			}
		}
	case sourceLine:
		table, gidColumn = "store_sales_line_fact", "line_gid"
		for _, l := range p.facts.lines {
			named = append(named, l.fact.LineGID)
		}
		stored, err := storedLines(ctx, tx, storeID, named)
		if err != nil {
			return checkResult{}, err
		}
		for _, l := range p.facts.lines {
			s, held := stored[l.fact.LineGID]
			var version *time.Time
			if held {
				version = &s.LineVersion
			}
			switch againstFact(micro(l.fact.Version), version, cut) {
			case rowNewerThanFact:
				res.compared++
				res.mismatched++
				repair.lines = append(repair.lines, l)
			case rowAtTheFact:
				res.compared++
				same, err := lineAgrees(l, s)
				if err != nil {
					return checkResult{}, err
				}
				if !same {
					res.mismatched++
					repair.lines = append(repair.lines, l)
				}
			}
		}
	case sourceRefund:
		table, gidColumn = "store_sales_refund_fact", "refund_gid"
		for _, r := range p.facts.refunds {
			named = append(named, r.fact.RefundGID)
		}
		stored, lines, err := storedRefunds(ctx, tx, storeID, named)
		if err != nil {
			return checkResult{}, err
		}
		for _, r := range p.facts.refunds {
			s, held := stored[r.fact.RefundGID]
			var version *time.Time
			if held {
				version = &s.RefundVersion
			}
			switch againstFact(micro(r.fact.Version), version, cut) {
			case rowNewerThanFact:
				res.compared++
				res.mismatched++
				repair.refunds = append(repair.refunds, r)
			case rowAtTheFact:
				res.compared++
				same, err := refundAgrees(r, s, lines[r.fact.RefundGID])
				if err != nil {
					return checkResult{}, err
				}
				if !same {
					res.mismatched++
					repair.refunds = append(repair.refunds, r)
				}
			}
		}
	default:
		return checkResult{}, errors.New("storestats: a check page of a source the walk does not have")
	}

	folded, err := foldPage(ctx, tx, storeID, p.source, repair)
	if err != nil {
		return checkResult{}, err
	}
	res.repaired, res.logged = folded.Facts, folded.logged
	if p.source == sourceOrder {
		if err := deriveOrders(ctx, tx, storeID, named); err != nil {
			return checkResult{}, err
		}
	}
	// Every row the page met answers for its fact, a row whose newest version
	// cannot be read included: the walk keeps that row's fact from its last
	// readable version, and it is marked here, neither compared nor written,
	// so the end of the pass does not take it for a fact no row answers for.
	// A row with no gid has no fact to keep.
	seen := append([]string(nil), named...)
	for _, u := range p.facts.unreadable {
		if u.gid != "" {
			seen = append(seen, u.gid)
		}
	}
	if len(seen) > 0 {
		if _, err := tx.NewUpdate().TableExpr(table).Set("checked_at = ?", p.started).
			Where("store_id = ? AND ? IN (?)", storeID, bun.Ident(gidColumn), bun.In(seen)).Exec(ctx); err != nil {
			return checkResult{}, fmt.Errorf("mark the facts the page names: %w", err)
		}
	}
	var met []string
	for _, u := range p.facts.unreadable {
		if u.recordable() {
			met = append(met, u.rowID)
		}
	}
	if len(met) > 0 {
		if _, err := tx.NewUpdate().TableExpr("store_sales_unreadable").Set("checked_at = ?", p.started).
			Where("store_id = ? AND source = ? AND row_id IN (?)", storeID, p.source, bun.In(met)).Exec(ctx); err != nil {
			return checkResult{}, fmt.Errorf("mark the records of the rows the page met: %w", err)
		}
	}
	if _, err := tx.NewUpdate().TableExpr("store_sales_check").
		Set("? = ?", bun.Ident(p.source+"_cursor"), p.to).
		Set("? = ?", bun.Ident(p.source+"_window_seconds"), p.width).
		Set("compared = compared + ?", res.compared).
		Set("mismatched = mismatched + ?", res.mismatched).
		Set("repaired = repaired + ?", res.repaired).
		Where("store_id = ? AND pass_started_at = ?", storeID, p.started).Exec(ctx); err != nil {
		return checkResult{}, fmt.Errorf("move the check on: %w", err)
	}
	return res, nil
}

// WHO IS WHOLESALE, FOR THE CHECK (ruling 45). The check reads the granted
// sets once a run, and a run lasts up to half an hour: a buyer granted
// meanwhile has had their orders written wholesale by the walk and the
// wholesale pass with the fresh set, and the store's row already keeps that
// set's digest, so no pass would start again after a check that judged with
// its older set. So the check judges an order's wholesale side only when the
// set it holds is the one the store's row keeps -- the store enabled in both,
// the same digest, and no wholesale pass under way -- read in the page's own
// transaction. Otherwise it compares and writes everything else of an order
// and leaves the side as it stands: not a mismatch, and written back as the
// stored fact holds it. An order with no fact at all takes the check's own
// judgement, there being no side to leave; the next run's check, holding the
// set the store keeps, judges it again.

// heldDigest is the digest of the granted set a check holds, as the wholesale
// pass computes the one it keeps (wholesale.go, wholesalePass).
func heldDigest(st wholesaleState) string {
	if st.Digest != "" {
		return st.Digest
	}
	return grantedDigest(st.Granted)
}

// checkJudgesWholesale says, inside tx, whether the page's orders may have
// their wholesale side compared and written. See "Who is wholesale, for the
// check".
func checkJudgesWholesale(ctx context.Context, tx bun.Tx, storeID string, p checkedPage) (bool, error) {
	var enabled bool
	var digest, cursor string
	if err := tx.NewSelect().Table("store_sales_store").Column("wholesale_enabled", "wholesale_digest", "wholesale_cursor").
		Where("store_id = ?", storeID).Scan(ctx, &enabled, &digest, &cursor); err != nil {
		return false, fmt.Errorf("read the store's wholesale state: %w", err)
	}
	return p.wholesaleEnabled && enabled && cursor == "" && p.wholesaleDigest == digest, nil
}

// ---- the stored facts, as the rules would make them ----

// storedOrders reads, inside tx, the stored order facts a check page names, by gid.
//
// staged-data: INDIFFERENT -- this reads a store_sales_ side table of this package, never MemoryNodes; no staged-data row can exist in it, so the gate has nothing to withhold.
func storedOrders(ctx context.Context, tx bun.Tx, storeID string, gids []string) (map[string]orderFactRow, error) {
	out := map[string]orderFactRow{}
	if len(gids) == 0 {
		return out, nil
	}
	var rows []orderFactRow
	if err := tx.NewSelect().Model(&rows).Where("store_id = ? AND order_gid IN (?)", storeID, bun.In(gids)).
		Scan(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read the order facts the page names: %w", err)
	}
	for _, r := range rows {
		out[r.OrderGID] = r
	}
	return out, nil
}

// storedLines reads, inside tx, the stored line facts a check page names, by gid.
//
// staged-data: INDIFFERENT -- this reads a store_sales_ side table of this package, never MemoryNodes; no staged-data row can exist in it, so the gate has nothing to withhold.
func storedLines(ctx context.Context, tx bun.Tx, storeID string, gids []string) (map[string]lineFactRow, error) {
	out := map[string]lineFactRow{}
	if len(gids) == 0 {
		return out, nil
	}
	var rows []lineFactRow
	if err := tx.NewSelect().Model(&rows).Where("store_id = ? AND line_gid IN (?)", storeID, bun.In(gids)).
		Scan(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read the line facts the page names: %w", err)
	}
	for _, r := range rows {
		out[r.LineGID] = r
	}
	return out, nil
}

// storedRefunds reads, inside tx, the stored refund facts and their lines a check page names, by gid.
//
// staged-data: INDIFFERENT -- this reads a store_sales_ side table of this package, never MemoryNodes; no staged-data row can exist in it, so the gate has nothing to withhold.
func storedRefunds(ctx context.Context, tx bun.Tx, storeID string, gids []string) (map[string]refundFactRow, map[string][]refundLineFactRow, error) {
	out, lines := map[string]refundFactRow{}, map[string][]refundLineFactRow{}
	if len(gids) == 0 {
		return out, lines, nil
	}
	var rows []refundFactRow
	if err := tx.NewSelect().Model(&rows).Where("store_id = ? AND refund_gid IN (?)", storeID, bun.In(gids)).
		Scan(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("read the refund facts the page names: %w", err)
	}
	for _, r := range rows {
		out[r.RefundGID] = r
	}
	var held []refundLineFactRow
	if err := tx.NewSelect().Model(&held).Where("store_id = ? AND refund_gid IN (?)", storeID, bun.In(gids)).
		OrderExpr("refund_gid, position").Scan(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("read the refunds' lines the page names: %w", err)
	}
	for _, l := range held {
		lines[l.RefundGID] = append(lines[l.RefundGID], l)
	}
	return out, lines, nil
}

func storedOptional(s *string, table, column string) (*money.Amount, error) {
	if s == nil {
		return nil, nil
	}
	a, err := parseStored(*s, table, column)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// orderAgrees says whether a stored order fact at the row's own version holds
// what the row gives now. A tombstone agrees with a fact that is deleted,
// whatever else it holds.
func orderAgrees(o foldedOrder, s orderFactRow) (bool, error) {
	if o.gone || s.Deleted {
		return o.gone && s.Deleted, nil
	}
	if s.Wholesale != o.wholesale {
		return false, nil
	}
	const t = "store_sales_order_fact"
	held := OrderFact{StoreID: o.fact.StoreID, OrderGID: s.OrderGID, Version: s.OrderVersion, RecordedAt: s.RecordedAt,
		OrderedAt: s.OrderedAt, Currency: s.Currency, BuyerKey: s.BuyerKey, Cancelled: s.Cancelled, Test: s.Test,
		TaxIncluded: s.TaxesIncluded, ExpectedQuantity: s.ExpectedQuantity}
	var err error
	if held.Shipping, err = parseStored(s.Shipping, t, "shipping"); err != nil {
		return false, err
	}
	if held.Tax, err = parseStored(s.Tax, t, "tax"); err != nil {
		return false, err
	}
	if held.Duties, err = parseStored(s.Duties, t, "duties"); err != nil {
		return false, err
	}
	if held.ShopifyCurrentSubtotal, err = storedOptional(s.ShopifyCurrentSubtotal, t, "shopify_current_subtotal"); err != nil {
		return false, err
	}
	if held.ShopifyTotalDiscounts, err = storedOptional(s.ShopifyTotalDiscounts, t, "shopify_total_discounts"); err != nil {
		return false, err
	}
	if held.ShopifyTotalRefunded, err = storedOptional(s.ShopifyTotalRefunded, t, "shopify_total_refunded"); err != nil {
		return false, err
	}
	want := o.fact
	want.Version, want.RecordedAt, want.OrderedAt = micro(want.Version), micro(recordedOr(want.RecordedAt, want.Version)), micro(want.OrderedAt)
	return want.same(held), nil
}

// lineAgrees is orderAgrees for a line.
func lineAgrees(l foldedLine, s lineFactRow) (bool, error) {
	if l.gone || s.Deleted {
		return l.gone && s.Deleted, nil
	}
	const t = "store_sales_line_fact"
	held := LineFact{StoreID: l.fact.StoreID, OrderGID: s.OrderGID, LineGID: s.LineGID, Version: s.LineVersion,
		Currency: s.Currency, GiftCard: s.GiftCard, ProductKey: s.ProductKey, Title: s.Title,
		Quantity: s.Quantity, CurrentQuantity: s.CurrentQuantity}
	var err error
	if held.UnitPrice, err = parseStored(s.UnitPrice, t, "unit_price"); err != nil {
		return false, err
	}
	if held.AllocatedDiscounts, err = parseStored(s.AllocatedDiscounts, t, "allocated_discounts"); err != nil {
		return false, err
	}
	want := l.fact
	want.Version = micro(want.Version)
	return want.same(held), nil
}

// refundAgrees is orderAgrees for a refund and its lines.
func refundAgrees(r foldedRefund, s refundFactRow, lines []refundLineFactRow) (bool, error) {
	if r.gone || s.Deleted {
		return r.gone && s.Deleted, nil
	}
	const t = "store_sales_refund_fact"
	held := RefundFact{StoreID: r.fact.StoreID, RefundGID: s.RefundGID, OrderGID: s.OrderGID, Version: s.RefundVersion,
		RecordedAt: s.RecordedAt, RefundedAt: s.RefundedAt, DetailFetched: s.DetailFetched, LinesComplete: s.LinesComplete}
	var err error
	if held.MoneyRefunded, err = parseStored(s.MoneyRefunded, t, "money_refunded"); err != nil {
		return false, err
	}
	if held.ShippingRefunded, err = storedOptional(s.ShippingRefunded, t, "shipping_refunded"); err != nil {
		return false, err
	}
	if held.TaxRefunded, err = storedOptional(s.TaxRefunded, t, "tax_refunded"); err != nil {
		return false, err
	}
	if held.DutiesRefunded, err = storedOptional(s.DutiesRefunded, t, "duties_refunded"); err != nil {
		return false, err
	}
	for _, l := range lines {
		h := RefundLineFact{Position: l.Position, LineGID: l.LineGID, Quantity: l.Quantity}
		if h.Subtotal, err = parseStored(l.Subtotal, "store_sales_refund_line_fact", "subtotal"); err != nil {
			return false, err
		}
		if h.Tax, err = parseStored(l.Tax, "store_sales_refund_line_fact", "tax"); err != nil {
			return false, err
		}
		held.Lines = append(held.Lines, h)
	}
	want := r.fact
	want.Version, want.RecordedAt, want.RefundedAt = micro(want.Version), micro(recordedOr(want.RecordedAt, want.Version)), micro(want.RefundedAt)
	return want.same(held), nil
}

// startCheckPass begins a pass of the store inside tx, unless one is under
// way -- another call's, begun meanwhile -- and answers the pass that stands.
func startCheckPass(ctx context.Context, tx bun.Tx, storeID string, now time.Time) (checkPass, error) {
	day := seconds(defaultWindow)
	if _, err := tx.NewRaw(`INSERT INTO store_sales_check AS c (store_id, pass_started_at) VALUES (?, ?)
		ON CONFLICT (store_id) DO UPDATE SET pass_started_at = EXCLUDED.pass_started_at,
			order_cursor = '', order_window_seconds = ?, line_cursor = '', line_window_seconds = ?,
			refund_cursor = '', refund_window_seconds = ?, compared = 0, mismatched = 0, repaired = 0
		WHERE c.pass_started_at IS NULL`, storeID, now.UTC(), day, day, day).Exec(ctx); err != nil {
		return checkPass{}, fmt.Errorf("begin the check's pass: %w", err)
	}
	return readPass(ctx, tx, storeID)
}
