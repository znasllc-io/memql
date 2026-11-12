package storestats

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
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
	Stores     int            `json:"stores"`
	Rows       map[string]int `json:"rows"`
	Facts      int            `json:"facts"`
	Unreadable int            `json:"unreadable"`
	Behind     []string       `json:"behind,omitempty"`  // "<store>/<source>"
	Rebuilt    []string       `json:"rebuilt,omitempty"` // stores storeState rebuilt this run
	Removed    []string       `json:"removed,omitempty"` // stores storeState removed this run
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

// maxStorePages bounds the stores read: a cursor that never ends is an error,
// never a loop.
const maxStorePages = 10000

// What storeState answers of a store; any other answer keeps it.
const (
	outcomeRebuilt = "rebuilt"
	outcomeRemoved = "removed" // the walk leaves the store
	outcomePurging = "purging" // its facts are still being deleted; the walk leaves the store for this run
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

	var failures []error
	for _, store := range stores {
		id := store["id"].(string)
		if err := i.rollStore(ctx, db, store, now, &rep); err != nil && !errors.Is(err, errRebuilt) {
			i.logger.Warn("Store statistics for a store were not kept this run; the next run carries on",
				"component", "storestats", "storeId", id, "error", err)
			failures = append(failures, &storeFailure{StoreID: id, Err: err})
		}
	}
	return rep, errors.Join(failures...)
}

// rollStore is one store's turn.
func (i *Integration) rollStore(ctx context.Context, db *bun.DB, store map[string]any, now time.Time, rep *RollupReport) error {
	id := store["id"].(string)
	// A store whose data was removed is given no new row (store.go).
	if !storeDataRemoved(store) {
		if err := writeTx(ctx, db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
			return ensureStore(ctx, tx, id)
		}); err != nil {
			return err
		}
	}
	if i.storeState != nil {
		outcome, err := i.storeState(ctx, store, now)
		if err != nil {
			return err
		}
		switch outcome {
		case outcomeRebuilt:
			rep.Rebuilt = append(rep.Rebuilt, id)
		case outcomeRemoved:
			rep.Removed = append(rep.Removed, id)
			return nil
		case outcomePurging:
			return nil
		}
	}
	// After storeState, which may itself have started a rebuild, and before
	// any page of the store is asked.
	generation, err := storeGeneration(ctx, db, id)
	if err != nil {
		return err
	}
	if i.walkStore != nil {
		if err := i.walkStore(ctx, store, generation, now, rep); err != nil {
			return err
		}
	}
	// A rebuild is over once the walk is within rebuildCaughtUp of the clock.
	return writeTx(ctx, db, id, generation, func(ctx context.Context, tx bun.Tx) error {
		return clearRebuilding(ctx, tx, id, now)
	})
}

// listStores reads every page of the stores read and answers each store once,
// in bare-id order, with its id bare. A row whose id is blank is no store and
// is left out; a store a later page answers again is kept once.
func listStores(ctx context.Context, engine Engine) ([]map[string]any, error) {
	query := fmt.Sprintf(storesRead, storesPageSize)
	byID := map[string]map[string]any{}
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxStorePages {
			return nil, fmt.Errorf("the stores read did not end within %d pages", maxStorePages)
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
