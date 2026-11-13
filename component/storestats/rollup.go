package storestats

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// rollup.go -- the run builtin storeStatisticsRollup makes every minute on the
// cron leader, under its own maintenance principal (integration.go,
// handleRollup).
//
// # Every store, once
//
// A run reads every page of storeStatisticsStores() and then takes each store
// once, in bare-id order. For each it makes the store's store_sales_store row
// when there is none; asks storeState what the store is now (kept, rebuilt or
// removed), and leaves a removed one; reads the store's generation, after
// storeState has answered, since storeState may itself start a rebuild, and
// before any page of the store is asked; and hands the store to walkStore
// with that generation. Each later part of the walk is one of those seams.
//
// ONE STORE'S FAILURE COSTS ONLY ITSELF. The stores are independent, so a
// store whose turn fails is logged with its id and the next store still runs;
// the run's error joins every failure, so the automation's run says it
// failed. errRebuilt is no failure: the store was rebuilt under its walk, and
// the next run walks it under the new generation.
//
// # A turn has a budget
//
// A store's turn has a time budget, turnBudget, carried in the context the
// run hands it (withTurn). Once it is spent the turn makes no floor probe and
// walks no new source (fold.go), asks no new page of any source (walk.go),
// starts no new purge batch (store.go) and begins no wholesale pass or check
// (wholesale.go, check.go), and ends normally: where each source stands is kept as for any partial read, the
// store is not a failure, and its next turn carries on. turnSpent is the one
// place that answers whether it is spent. Work a later task adds to a turn
// asks it between its units.
//
// # Who is wholesale
//
// A run asks wholesaleFor once, after it has listed the stores and before it
// rolls the first, for every store it is about to roll, and hands each store's
// walk that store's own state, a value (wholesaleState.isWholesale), the zero
// one when wholesaleFor is nil or names none for the store: the walk folds every
// order it reads with it, and the store's wholesale pass, after the walk,
// judges again the orders written before its granted set changed
// (wholesale.go). When wholesaleFor cannot answer, the run answers
// that error once and walks no store's facts: a store walked with "nobody is
// wholesale" would write its granted buyers' new orders retail, and nothing
// would read them again until the next change of the granted set. Every
// store's housekeeping -- its row, its verdict, its purge and its removal --
// runs as usual: an outage of an optional pack holds back no erasure.
//
// # One long store does not starve the others
//
// A run has a budget, runBudget: once it is spent, the run takes up no new
// store and ends normally, and says how many stores it did not reach
// (RollupReport.NotReached). A store under way finishes within its own
// limits. The next run begins with the store after the last one this node's
// run took up, in bare-id order, and wraps round, so every store gets its turn
// however long one of them takes. Where it stands is kept in the
// integration's memory (lastTaken): a new cron leader begins at the first
// store, which costs only that store an early turn. The last store taken up is
// the last one finished, unless the run was cut off at its hard deadline
// (runDeadline), and then the store that hung is not taken up first again.
//
// # The reads are called from here, never as steps
//
// The engine's work journal keeps what an automation step answers, rows
// included, and two of the store statistics reads carry a buyer's email. So
// the automation's one step is the builtin, every read is made from Go under
// walkContext, and the report the builtin answers is counts and store ids.
//
// # What the reads guarantee
//
// A walk's page is ordered by write time, ties broken by id; a row rewritten
// while a walk is under way comes back on a later page, so a consumer dedupes
// by id; `since` is inclusive; a version that commits late can land behind a
// watermark already passed, so a consumer reads with an overlap. The pack's
// reads refuse an empty list of store ids, and a blank member of one also
// matches rows with no store id, so neither is ever passed.

// RollupReport says what one run did: counts, and the ids of the stores its
// lists name. Nothing in it is drawn from a row but those ids (see "The reads
// are called from here, never as steps").
type RollupReport struct {
	unreadableLogged int
	rebuildEnded     map[string]bool // this turn completed every source and ended its rebuild
	Stores           int             `json:"stores"`
	Rows             map[string]int  `json:"rows"`
	Facts            int             `json:"facts"`
	// Unreadable is the rows the run's pages met that could not be read,
	// counted after each page's unreadable rows are reduced to one a row id,
	// at its newest version (fold.go, readPageFrom): a row a page holds twice
	// counts once, a row an overlap reads again on a later page counts again,
	// and a row with no id of its own or no write time counts each time.
	Unreadable int      `json:"unreadable"`
	Behind     []string `json:"behind,omitempty"`     // "<store>/<source>"
	Rebuilt    []string `json:"rebuilt,omitempty"`    // stores storeState rebuilt this run
	Removed    []string `json:"removed,omitempty"`    // stores storeState removed this run
	NotReached int      `json:"notReached,omitempty"` // stores the run did not take up: its budget was spent
}

// storesPageSize is how many stores one page of the stores read holds: the
// query's own `paginate 100`. A variable so a test can list a few stores
// across many pages.
var storesPageSize = 100

// storesRead is the stores read's statement: a format taking the page size.
// The construct's name is written out in the literal, so test/dslconformance
// sees this file call the @serverOnly query.
const storesRead = "paginate(query storeStatisticsStores(), %d)"

// shopRead is the shop read's statement: a format taking the quoted store id.
// Here beside storesRead, in the file that stamps the reads' origin
// (walkContext), for the same reason: test/dslconformance sees the call.
const shopRead = "query storeStatisticsShop(storeId: %s)"

// onlyStoresForTests is nil outside tests. When a test sets it, a run lists
// only the stores it admits: this package's harness sets it to the stores its
// tests seeded, so a store another package's test left in the shared database
// is not walked by every run here (harness_db_test.go, seededStores).
// TestNoProductionCodeSetsOnlyStoresForTests holds that only a test file sets
// it.
var onlyStoresForTests func(id string) bool

// storeAtRead is the floor search's statement (walk.go, floorOf): a format
// taking the quoted store id and the quoted instant. Here for the same reason.
const storeAtRead = "query storeStatisticsStoreAt(storeId: %s, at: %s)"

// changedReads are the walk's statements (walk.go, walkSource), by the name
// of the read: each a format taking the quoted store id, the quoted since and
// the page size. Here for the same reason.
var changedReads = map[string]string{
	"storeStatisticsOrdersChanged":  "paginate(query storeStatisticsOrdersChanged(storeId: %s, since: %s), %d)",
	"storeStatisticsLinesChanged":   "paginate(query storeStatisticsLinesChanged(storeId: %s, since: %s), %d)",
	"storeStatisticsRefundsChanged": "paginate(query storeStatisticsRefundsChanged(storeId: %s, since: %s), %d)",
	"storeStatisticsOrderBuyers":    "paginate(query storeStatisticsOrderBuyers(storeId: %s, since: %s), %d)",
}

// The wholesale pack's two reads (wholesale.go, wholesaleOf): each a format
// taking the quoted list of store ids and the page size. Here for the same
// reason.
const (
	wholesaleSettingsRead = "paginate(query wholesaleStatisticsSettings(storeIds: %s), %d)"
	wholesaleGrantedRead  = "paginate(query wholesaleStatisticsGranted(storeIds: %s), %d)"
)

// maxStorePages bounds the stores read: a cursor that never ends is an error,
// never a loop. Twenty pages of storesPageSize is two thousand stores, and
// the reads are counted in the run's hard deadline (integration.go,
// runDeadlineBound): a cluster past that fails the listing, every run, with
// an error that says so and that no store's figures advance until the cluster
// holds fewer stores or the bound is raised, with the deadline's sum.
const maxStorePages = 20

// What storeState answers of a store; any other answer keeps it.
const (
	outcomeRebuilt = "rebuilt"
	outcomeRemoved = "removed" // the walk leaves the store
	outcomePurging = "purging" // its facts are still being deleted; the walk leaves the store for this run
	outcomeLeft    = "left"    // nothing is done for the store this run, and nothing is reported of it
	// The store's rebuild ended this turn: it is walked, and then its check
	// begins in the same turn (check.go).
	outcomeRebuildEnded = "rebuildEnded"
)

// walkContext is the context every read of the run is made under, and the
// package's one internal-origin stamp. Internal origin, because the store
// statistics reads are @serverOnly: the principal reads every store's rows,
// and nothing on the wire may. A fresh read, because the result cache must not
// answer a walk with a page from before the rows it is looking for were
// written. A roots-only read, because every shape the walk reads projects the
// row's own fields, and left to expand each row would fetch its relations
// besides.
func walkContext(ctx context.Context) context.Context {
	return auth.ContextWithInternalOrigin(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(ctx)))
}

// connectionState is called only after servedRequest admits the sales part,
// site and bound store. Its server-only projection contains two installation
// markers; neither the row nor any other store fields enter the answer.
func (i *Integration) connectionState(ctx context.Context, storeID string) (string, error) {
	ctx = auth.ContextWithAccess(ctx, auth.MaintenanceActor("storeStatisticsRollupSweep"))
	res, err := i.engine.Execute(walkContext(ctx), "query storeStatisticsConnection(storeId: "+langparser.QuoteString(storeID)+")")
	if err != nil {
		return "", fmt.Errorf("read the store statistics connection: %w", sanitized(err))
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return "not connected", nil
	}
	if len(rows) != 1 {
		return "", errors.New("read the store statistics connection: expected one store")
	}
	fields := rowFields(rows[0])
	if storeDataRemoved(fields) || fields["uninstalledAt"] != nil && rowText(fields, "uninstalledAt") != "" {
		return "not connected", nil
	}
	return "starting", nil
}

// storeFailure is one store's turn failing. The other stores still run.
type storeFailure struct {
	StoreID string
	Err     error
}

func (f *storeFailure) Error() string { return fmt.Sprintf("store %s: %v", f.StoreID, f.Err) }
func (f *storeFailure) Unwrap() error { return f.Err }

// runRollup lists every store and takes each in turn. See "Every store, once".
func runRollup(ctx context.Context, i *Integration, now time.Time) (RollupReport, error) {
	rep := RollupReport{Rows: map[string]int{}}
	if i.engine == nil {
		return rep, errors.New("this node has no engine handle, so the store statistics cannot be read from the mirror")
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	if db == nil {
		return rep, errors.New("this node has no database handle, so the store statistics cannot be written")
	}
	ctx = walkContext(ctx)

	stores, err := listStores(ctx, i.engine)
	if err != nil {
		return rep, fmt.Errorf("list the stores: %w", err)
	}
	rep.Stores = len(stores)

	// The wholesale state, once a run, for the stores the run rolls. When it
	// cannot be read, every store's housekeeping still runs and only the walk
	// of facts is skipped. See "Who is wholesale".
	var failures []error
	wholesale, err := i.wholesaleStates(ctx, stores)
	walk := err == nil
	if err != nil {
		failures = append(failures, err)
	}

	// Each store in turn from the one after the store the last run took up
	// last, wrapping round, and no new one once the budget is spent. See
	// "One long store does not starve the others".
	began := budgetNow()
	first := i.firstToTake(stores)
	for k := range stores {
		store := stores[(first+k)%len(stores)]
		id := store["id"].(string)
		if k > 0 && !budgetNow().Before(began.Add(runBudget)) {
			rep.NotReached = len(stores) - k
			break
		}
		i.tookUp(id)
		beforeUnreadable := rep.unreadableLogged
		err := i.rollStore(ctx, db, store, now, wholesale[id], walk, &rep)
		if n := rep.unreadableLogged - beforeUnreadable; n > 0 {
			i.logger.Warn("Store statistics: mirrored rows could not be read; their last readable facts are kept", "component", "storestats", "storeId", id, "count", n)
		}
		permanent := errors.Is(err, errOverlapTooDense)
		if err != nil && !errors.Is(err, errRebuilt) {
			if !permanent || i.noted.change("walk:"+id, "overlap") {
				message := "Store statistics for a store were not kept this run; the next run carries on"
				if permanent {
					message = "Store statistics: a store's overlap exceeds the walk's bound; its figures cannot advance"
				}
				i.logger.Warn(message, "component", "storestats", "storeId", id, "error", err)
			}
			failures = append(failures, &storeFailure{StoreID: id, Err: err})
		} else if i.noted.change("walk:"+id, "") {
			i.logger.Info("Store statistics: the store's walk is progressing again", "component", "storestats", "storeId", id)
		}
	}
	return rep, errors.Join(failures...)
}

// rollStore is one store's turn.
// walk is false when the run could not read the wholesale state: the store's
// housekeeping runs, and its facts are not walked.
func (i *Integration) rollStore(ctx context.Context, db *bun.DB, store map[string]any, now time.Time, wholesale wholesaleState, walk bool, rep *RollupReport) error {
	id := store["id"].(string)
	ctx = withTurn(ctx)
	// keepStore makes the store's row itself, and gives a store whose data
	// was removed none (store.go).
	if i.storeState == nil {
		if err := writeTx(ctx, db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
			return ensureStore(ctx, tx, id)
		}); err != nil {
			return err
		}
	}
	var outcome string
	if i.storeState != nil {
		var err error
		outcome, err = i.storeState(ctx, store, now)
		if err != nil {
			return err
		}
		switch outcome {
		case outcomeRebuilt:
			rep.Rebuilt = append(rep.Rebuilt, id)
		case outcomeRemoved:
			rep.Removed = append(rep.Removed, id)
			return nil
		case outcomePurging, outcomeLeft:
			return nil
		}
	}
	// After storeState, which may itself have started a rebuild, and before
	// any page of the store is asked.
	generation, err := generationOf(ctx, db, id)
	if err != nil {
		return err
	}
	if !walk || i.walkStore == nil {
		return nil
	}
	err = i.walkStore(ctx, store, generation, now, wholesale, rep)
	var noFloor floorFailed
	if errors.Is(err, errRebuilt) || errors.As(err, &noFloor) {
		return err
	}
	// After the walk, which judged the orders it wrote: the pass judges again
	// those written before the granted set changed (wholesale.go). Not after
	// a walk that failed on the store's floor, which the pass needs too: it
	// would probe again and report the same failure.
	perr := wholesalePass(ctx, i, id, generation, wholesale, i.walkClock(), maxPagesPerRun)
	if errors.Is(perr, errRebuilt) {
		return perr
	}
	if err = errors.Join(err, perr); err != nil || (outcome != outcomeRebuildEnded && !rep.rebuildEnded[id]) {
		return err
	}
	// A rebuild is proven by the check its end begins, here, once its walk has
	// caught up (check.go, checkStore, which asks the turn's budget first).
	_, err = checkStore(ctx, i, id, now, checkPagesPerRun, wholesale, nil)
	return err
}

// budgetNow is the clock the run's and the turn's budgets read: the time, or
// clockForTests in a test that drives it.
func budgetNow() time.Time {
	if clockForTests != nil {
		return clockForTests()
	}
	return time.Now()
}

// turnKey keys the end of a store's turn in the context the run hands the
// turn.
type turnKey struct{}

// withTurn starts a store's turn: the context carries the instant its budget
// ends. See "A turn has a budget".
func withTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnKey{}, budgetNow().Add(turnBudget))
}

// turnSpent reports whether the turn ctx belongs to has spent its budget: the
// one place a page loop and a purge loop ask. A context with no turn -- a test
// that calls the walk or the purge directly -- has no budget to spend.
func turnSpent(ctx context.Context) bool {
	ends, ok := ctx.Value(turnKey{}).(time.Time)
	return ok && !budgetNow().Before(ends)
}

// firstToTake is the index in stores, which are in bare-id order, of the
// first store after the one the last run on this node took up last, or 0.
func (i *Integration) firstToTake(stores []map[string]any) int {
	i.runningMu.Lock()
	last := i.lastTaken
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

// tookUp records the store a run is taking up.
func (i *Integration) tookUp(id string) {
	i.runningMu.Lock()
	i.lastTaken = id
	i.runningMu.Unlock()
}

// wholesaleStates asks wholesaleFor once for every store the run rolls. A nil
// wholesaleFor, and a run with no store, ask nothing: no store is wholesale.
// An error is the run's: see "Who is wholesale".
func (i *Integration) wholesaleStates(ctx context.Context, stores []map[string]any) (map[string]wholesaleState, error) {
	if i.wholesaleFor == nil || len(stores) == 0 {
		return nil, nil
	}
	ids := make([]string, len(stores))
	for n, store := range stores {
		ids[n] = store["id"].(string)
	}
	states, err := i.wholesaleFor(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read the stores' wholesale state, so no store's facts are walked this run: %w", err)
	}
	return states, nil
}

// foldEmail is an address as a granted set holds it: trimmed and in lower
// case.
func foldEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// listStores reads every page of the stores read and answers each store once,
// in bare-id order, with its id bare. A row whose id is blank is no store and
// is left out; a store a later page answers again is kept once.
func listStores(ctx context.Context, engine Engine) ([]map[string]any, error) {
	query := fmt.Sprintf(storesRead, storesPageSize)
	byID := map[string]map[string]any{}
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxStorePages {
			// A cursor that never ends reads the same: past the bound, there
			// are more stores than the statistics list, so none is rolled.
			return nil, fmt.Errorf("this cluster holds more stores than the store statistics can list (%d); "+
				"no store's figures advance until the cluster holds fewer, or the bound is raised", maxStorePages*storesPageSize)
		}
		res, err := engine.Execute(memql.ContextWithCursor(ctx, cursor), query)
		if err != nil {
			return nil, fmt.Errorf("read a page: %w", err)
		}
		// A page that says there are more stores and offers no way to reach
		// them is refused, never read as the end of the list.
		next, err := memql.ResultPageCursor(res)
		if err != nil {
			return nil, err
		}
		for _, row := range memql.MaterializeRows(res) {
			raw, _ := row["id"].(string)
			id := memql.BareShortId(raw)
			if id == "" {
				continue
			}
			if onlyStoresForTests != nil && !onlyStoresForTests(id) {
				continue
			}
			row["id"] = id
			byID[id] = row
		}
		if next == "" {
			break
		}
		if next == cursor {
			return nil, errors.New("the stores read did not advance past a page: the engine answered the same cursor it was asked with")
		}
		cursor = next
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, len(ids))
	for n, id := range ids {
		out[n] = byID[id]
	}
	return out, nil
}

// ensureStore makes the store's store_sales_store row when it has none, with
// every column at its default. It changes nothing of a row that is there.
func ensureStore(ctx context.Context, tx bun.Tx, storeID string) error {
	row := map[string]any{"store_id": storeID}
	if _, err := tx.NewInsert().Model(&row).TableExpr("store_sales_store").
		On("CONFLICT (store_id) DO NOTHING").
		Exec(ctx); err != nil {
		return fmt.Errorf("make the store's row: %w", err)
	}
	return nil
}
