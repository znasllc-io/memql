package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
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
// Content and not only version, because an erasure rewrites a row in place:
// one pass after it, an erased order is retail and a scrubbed custom item's
// fact holds the marker for its title. Every order page re-derives its
// orders, so a derived column is recomputed once a pass whatever differed.
// Every fact a page names is marked checked_at = pass_started_at, and so is
// the record of every row it met that could not be read.
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

// maintenancePrefix is what every maintenance principal's user id begins
// with (component/auth, maintenanceUserIdPrefix): a caller whose id begins so
// is the engine acting, not a person.
const maintenancePrefix = "system:maintenance:"

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
// own work (a row an overlap reads again is compared again); Disagreeing is
// the sum over the passes that ended in this run.
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
// pass's start, each a transaction.
func checkStartUnit() time.Duration { return 3 * txTimeout }

// checkRunDeadlineBound is the most a check run can take, its budgets spent:
//
//	the stores read:      maxStorePages (20) reads x readBound (10 s)    200 s
//	the wholesale read:   wholesaleReadsBound (10) x readBound (10 s)     100 s
//	the run's budget:     checkRunBudget                                 1800 s
//	the turn's budget:    turnBudget, of the last store taken up           30 s
//	the unit under way:   the largest of a page (one read and its
//	                      transaction, 30 s), a batch or the end of a
//	                      pass (20 s) and a pass's start (60 s)            60 s
//	                                                             total   2190 s
//
// Thirty-six minutes and a half, and the deadline adds a minute over it.
func checkRunDeadlineBound() time.Duration {
	unit := max(readBound+txTimeout, txTimeout, checkStartUnit())
	return maxStorePages*readBound + wholesaleReadsBound*readBound + checkRunBudget + turnBudget + unit
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
		p := checkedPage{source: s.source, started: pass.started, from: expect, to: to, width: seconds(page.Width), facts: facts}
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
		for _, o := range p.facts.orders {
			s, held := stored[o.fact.OrderGID]
			var version *time.Time
			if held {
				version = &s.OrderVersion
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
	if len(named) > 0 {
		if _, err := tx.NewUpdate().TableExpr(table).Set("checked_at = ?", p.started).
			Where("store_id = ? AND ? IN (?)", storeID, bun.Ident(gidColumn), bun.In(named)).Exec(ctx); err != nil {
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

// ---- the stored facts, as the rules would make them ----

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

// ---- the end of a pass ----

// orphanBatch deletes, inside tx, up to deleteBatch facts of the store that no
// row answered for in the pass begun at started: not deleted, not marked by
// it, at or before its cut. A refund's lines go with it, and the orders whose
// lines or refunds went are derived again. Records of rows that could not be
// read that the pass did not meet go in the same batch, within what is left
// of it. deleted counts every row it deleted, facts counts the facts.
func orphanBatch(ctx context.Context, tx bun.Tx, storeID string, started time.Time) (deleted, facts int, err error) {
	if err := passStands(ctx, tx, storeID, started, "", ""); err != nil {
		return 0, 0, err
	}
	cut := started.Add(-checkCut)
	left := deleteBatch
	touched := map[string]bool{}
	unanswered := "store_id = ? AND NOT deleted AND (checked_at IS NULL OR checked_at < ?) AND ? <= ?"

	var orders []string
	if err := tx.NewDelete().TableExpr("store_sales_order_fact").
		Where("ctid IN (SELECT ctid FROM store_sales_order_fact WHERE "+unanswered+" LIMIT ?)", storeID, started, bun.Ident("order_version"), cut, left).
		Returning("order_gid").Scan(ctx, &orders); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("delete the order facts no row answered for: %w", err)
	}
	facts += len(orders)
	left -= len(orders)

	if left > 0 {
		var of []string
		if err := tx.NewDelete().TableExpr("store_sales_line_fact").
			Where("ctid IN (SELECT ctid FROM store_sales_line_fact WHERE "+unanswered+" LIMIT ?)", storeID, started, bun.Ident("line_version"), cut, left).
			Returning("order_gid").Scan(ctx, &of); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, fmt.Errorf("delete the line facts no row answered for: %w", err)
		}
		for _, o := range of {
			touched[o] = true
		}
		facts += len(of)
		left -= len(of)
	}

	if left > 0 {
		var refunds, of []string
		if err := tx.NewDelete().TableExpr("store_sales_refund_fact").
			Where("ctid IN (SELECT ctid FROM store_sales_refund_fact WHERE "+unanswered+" LIMIT ?)", storeID, started, bun.Ident("refund_version"), cut, left).
			Returning("refund_gid, order_gid").Scan(ctx, &refunds, &of); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, fmt.Errorf("delete the refund facts no row answered for: %w", err)
		}
		if len(refunds) > 0 {
			if _, err := tx.NewDelete().TableExpr("store_sales_refund_line_fact").
				Where("store_id = ? AND refund_gid IN (?)", storeID, bun.In(refunds)).Exec(ctx); err != nil {
				return 0, 0, fmt.Errorf("delete the lines of the refunds no row answered for: %w", err)
			}
		}
		for _, o := range of {
			touched[o] = true
		}
		facts += len(refunds)
		left -= len(refunds)
	}

	deleted = facts
	if left > 0 {
		res, err := tx.NewDelete().TableExpr("store_sales_unreadable").
			Where("ctid IN (SELECT ctid FROM store_sales_unreadable WHERE store_id = ? AND (checked_at IS NULL OR checked_at < ?) AND version <= ? LIMIT ?)",
				storeID, started, cut, left).
			Exec(ctx)
		if err != nil {
			return 0, 0, fmt.Errorf("delete the records of rows the pass did not meet: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		deleted += int(n)
	}

	var derive []string
	for o := range touched {
		if o != "" {
			derive = append(derive, o)
		}
	}
	if err := deriveOrders(ctx, tx, storeID, derive); err != nil {
		return 0, 0, err
	}
	if facts > 0 {
		if _, err := tx.NewUpdate().TableExpr("store_sales_check").
			Set("mismatched = mismatched + ?", facts).Set("repaired = repaired + ?", facts).
			Where("store_id = ? AND pass_started_at = ?", storeID, started).Exec(ctx); err != nil {
			return 0, 0, fmt.Errorf("count the facts no row answered for: %w", err)
		}
	}
	return deleted, facts, nil
}

// finishCheckPass ends the pass begun at started, inside tx: see "The end of
// a pass". finished is false when another call ended it first.
func finishCheckPass(ctx context.Context, tx bun.Tx, storeID string, started, now time.Time) (finished bool, disagreeing int, err error) {
	if err := passStands(ctx, tx, storeID, started, "", ""); err != nil {
		return false, 0, err
	}
	if disagreeing, err = disagreeingOrders(ctx, tx, storeID); err != nil {
		return false, 0, err
	}
	res, err := tx.NewUpdate().TableExpr("store_sales_check").
		Set("last_started_at = pass_started_at").
		Set("last_compared = compared").Set("last_mismatched = mismatched").Set("last_repaired = repaired").
		Set("last_disagreeing = ?", disagreeing).
		Set("last_finished_at = ?", now.UTC()).
		Set("clean_at = CASE WHEN mismatched = 0 THEN ?::timestamptz ELSE clean_at END", now.UTC()).
		Set("pass_started_at = NULL").
		Set("order_cursor = ''").Set("line_cursor = ''").Set("refund_cursor = ''").
		Set("order_window_seconds = ?", seconds(defaultWindow)).
		Set("line_window_seconds = ?", seconds(defaultWindow)).
		Set("refund_window_seconds = ?", seconds(defaultWindow)).
		Set("compared = 0").Set("mismatched = 0").Set("repaired = 0").
		Where("store_id = ? AND pass_started_at = ?", storeID, started).Exec(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("end the check's pass: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	return n == 1, disagreeing, nil
}

// disagreeingOrders counts the store's counted orders with complete lines
// whose Shopify totals the facts do not add up to (D14), over facts that are
// not deleted, amounts compared by value: the refunds' money against
// shopify_total_refunded; every line's allocated discounts against
// shopify_total_discounts; and, when no line is partly removed, the lines
// still whole, each unit_price * quantity - allocated_discounts, against
// shopify_current_subtotal. A total the row did not hold disagrees with
// nothing.
func disagreeingOrders(ctx context.Context, tx bun.Tx, storeID string) (int, error) {
	var n int
	err := tx.NewSelect().TableExpr("store_sales_order_fact AS o").ColumnExpr("count(*)").
		Where("o.store_id = ? AND NOT o.deleted AND NOT o.cancelled AND o.lines_complete", storeID).
		Where("(NOT o.test OR EXISTS (SELECT 1 FROM store_sales_store s WHERE s.store_id = o.store_id AND s.development))").
		Where(`(o.shopify_total_refunded <> (SELECT COALESCE(sum(r.money_refunded), 0) FROM store_sales_refund_fact r
					WHERE r.store_id = o.store_id AND r.order_gid = o.order_gid AND NOT r.deleted)
			OR o.shopify_total_discounts <> (SELECT COALESCE(sum(l.allocated_discounts), 0) FROM store_sales_line_fact l
					WHERE l.store_id = o.store_id AND l.order_gid = o.order_gid AND NOT l.deleted)
			OR (NOT EXISTS (SELECT 1 FROM store_sales_line_fact l
					WHERE l.store_id = o.store_id AND l.order_gid = o.order_gid AND NOT l.deleted
					AND l.current_quantity > 0 AND l.current_quantity < l.quantity)
				AND o.shopify_current_subtotal <> (SELECT COALESCE(sum(l.unit_price * l.quantity - l.allocated_discounts), 0) FROM store_sales_line_fact l
					WHERE l.store_id = o.store_id AND l.order_gid = o.order_gid AND NOT l.deleted AND l.current_quantity = l.quantity)))`).
		Scan(ctx, &n)
	if err != nil {
		return 0, fmt.Errorf("count the orders whose totals disagree: %w", err)
	}
	return n, nil
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

// ---- the three builtins, and who may call each ----

// Each refusal is the one sentence for every caller its handler does not
// admit; errNoSuchStore is a person's to read.
var (
	errNotTheCheckPrincipal = errors.New("store statistics check requires the engine maintenance principal")
	errRebuildNotAnOwner    = errors.New("store statistics rebuild is for a cluster owner")
	errCheckStateNotAnOwner = errors.New("store statistics check state is for a cluster owner")
)

//nolint:staticcheck // ST1005: person-facing copy, not a log fragment.
var errNoSuchStore = errors.New("Store statistics has no store with that id.")

// admitsTheCheck is the one place handleCheck admits its caller: the check's
// own maintenance principal, with the cluster-owner role it carries, at the
// internal origin the automation runner gives the engine's own automations,
// as handleRollup admits the rollup's. The check reads every store's mirror
// and spends the database's time on a schedule; nobody runs it by hand.
func admitsTheCheck(ctx context.Context) bool {
	actor, ok := auth.AccessFromContext(ctx)
	return ok && actor != nil && actor.UserId == checkPrincipal && actor.IsClusterOwner() && auth.OriginFromContext(ctx).IsInternal()
}

// admitsAPersonWhoIsAClusterOwner is the one place handleRebuild and
// handleCheckState admit their caller: a cluster owner who is a person, not
// the engine acting under a maintenance principal (each of which is a
// cluster owner too). Neither builtin is in the generated clients, and each
// can be called by name by any signed-in user, so this is the gate.
func admitsAPersonWhoIsAClusterOwner(ctx context.Context) bool {
	actor, ok := auth.AccessFromContext(ctx)
	return ok && actor != nil && actor.IsClusterOwner() && !strings.HasPrefix(actor.UserId, maintenancePrefix)
}

// storeArg is the call's storeId, bare: "<id>" and "v1:shopify:store:<id>"
// name one store.
func storeArg(args map[string]any) string {
	s, _ := args["storeId"].(string)
	return memql.BareShortId(strings.TrimSpace(s))
}

func (i *Integration) clock() time.Time {
	if i.now != nil {
		return i.now()
	}
	return time.Now()
}

// answerNode is a builtin's one node.
func answerNode(id, concept string, body any, at time.Time) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("store statistics: encode the answer: %w", err)
	}
	return []memorynodes.MemoryNode{{ID: id, Concept: concept, Type: memorynodes.NodeTypeObject, CreatedAt: at.UTC(), Payload: payload}}, nil
}

// handleCheck runs the check for its scheduled automation, and for nobody
// else, under checkRunDeadline, and answers one node with its report: counts
// and store ids, since the work journal keeps what a step answers.
func (i *Integration) handleCheck(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !admitsTheCheck(ctx) {
		return nil, errNotTheCheckPrincipal
	}
	now := i.clock()
	ctx, cancel := context.WithTimeout(ctx, checkRunDeadline)
	defer cancel()
	rep, err := runCheck(ctx, i, now)
	level := slog.LevelDebug
	if rep.Mismatched > 0 || rep.Disagreeing > 0 {
		level = slog.LevelInfo
	}
	i.logger.Log(ctx, level, "Store statistics checked",
		"component", "storestats", "stores", rep.Stores, "compared", rep.Compared, "mismatched", rep.Mismatched,
		"repaired", rep.Repaired, "disagreeing", rep.Disagreeing, "finished", rep.Finished)
	if err != nil {
		return nil, err
	}
	return answerNode("storeStatisticsCheck", checkConcept, rep, now)
}

// handleRebuild marks a store for a rebuild, for a person who is a cluster
// owner: in one transaction, with anyGeneration, rebuildStore raises the
// store's generation, sets rebuilding_since and deletes its watermarks and
// check row. The walk then deletes its facts in batches and refills them, and
// when the rebuild ends a check begins (rollup.go, rollStore). A store the
// statistics hold no row for is refused, and none is made.
func (i *Integration) handleRebuild(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !admitsAPersonWhoIsAClusterOwner(ctx) {
		return nil, errRebuildNotAnOwner
	}
	id := storeArg(args)
	if id == "" {
		return nil, errNoSuchStore
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	now := micro(i.clock())
	if err := writeTx(ctx, db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		var held bool
		if err := tx.NewSelect().Table("store_sales_store").ColumnExpr("true").Where("store_id = ?", id).Scan(ctx, &held); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNoSuchStore
			}
			return err
		}
		return rebuildStore(ctx, tx, id, now)
	}); err != nil {
		if errors.Is(err, errNoSuchStore) {
			return nil, err
		}
		return nil, fmt.Errorf("store %s: mark the store for a rebuild: %w", id, sanitized(err))
	}
	i.logger.Info("Store statistics: a cluster owner asked for a store's rebuild; its facts are deleted and walked again",
		"component", "storestats", "storeId", id)
	return answerNode("storeStatisticsRebuild", rebuildConcept, map[string]any{
		"storeId": id, "rebuildingSince": now.Format(time.RFC3339Nano),
	}, now)
}

// handleCheckState answers, for a person who is a cluster owner, one store's
// check as store_sales_check holds it: when the pass under way began, when
// the last one began and ended, when one last ended clean, and the last
// ended pass's four counts. A time that never was is null. It reads in one
// readTx and nothing else.
func (i *Integration) handleCheckState(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !admitsAPersonWhoIsAClusterOwner(ctx) {
		return nil, errCheckStateNotAnOwner
	}
	id := storeArg(args)
	if id == "" {
		return nil, errNoSuchStore
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	var row checkRow
	if err := readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		var held bool
		if err := tx.NewSelect().Table("store_sales_store").ColumnExpr("true").Where("store_id = ?", id).Scan(ctx, &held); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNoSuchStore
			}
			return err
		}
		var err error
		row, _, err = readCheckRow(ctx, tx, id)
		return err
	}); err != nil {
		if errors.Is(err, errNoSuchStore) {
			return nil, err
		}
		return nil, fmt.Errorf("store %s: read the store's check: %w", id, sanitized(err))
	}
	at := func(t sql.NullTime) any {
		if !t.Valid {
			return nil
		}
		return t.Time.UTC().Format(time.RFC3339Nano)
	}
	return answerNode("checkState", checkStateConcept, map[string]any{
		"storeId":           id,
		"passUnderWaySince": at(row.PassStartedAt),
		"lastStartedAt":     at(row.LastStartedAt),
		"lastFinishedAt":    at(row.LastFinishedAt),
		"cleanAt":           at(row.CleanAt),
		"compared":          row.LastCompared,
		"mismatched":        row.LastMismatched,
		"repaired":          row.LastRepaired,
		"disagreeing":       row.LastDisagreeing,
	}, i.clock())
}
