package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/repowalk"
)

// provider_db_test.go -- the provider, its rollup builtin and the one way the
// package writes, against a real engine and database (harness_db_test.go).

// lockWaiters counts the sessions waiting for a lock on store_sales_store.
func lockWaiters(t *testing.T, db *bun.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE relation = 'store_sales_store'::regclass AND NOT granted`).Scan(&n); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return n
}

// ensureRow gives the store its store_sales_store row, as a run does.
func ensureRow(t *testing.T, db *bun.DB, storeID string) {
	t.Helper()
	if err := writeTx(context.Background(), db, storeID, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		return ensureStore(ctx, tx, storeID)
	}); err != nil {
		t.Fatalf("give %s its row: %v", storeID, err)
	}
}

// A WRITER WAITS FOR THE LOCK, A READER DOES NOT. While another transaction
// holds the lock writeTx takes, a writeTx started meanwhile runs its body only
// once that one has ended, and a readTx of the same table answers at once.
func TestAWriterWaitsForTheLock(t *testing.T) {
	_, db := statsEngine(t)
	storeID := newStore(t)
	ensureRow(t, db, storeID)

	// The holder is a handle of its own, as another node's writer would be.
	ctx := context.Background()
	other := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = other.Close() })
	holder, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, "LOCK TABLE store_sales_store IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}

	var ran atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- writeTx(ctx, db, storeID, anyGeneration, func(context.Context, bun.Tx) error {
			ran.Store(true)
			return nil
		})
	}()
	// The writer is waiting when the database says a session waits for the
	// table, not when a timer has run out.
	deadline := time.Now().Add(15 * time.Second)
	for lockWaiters(t, db) == 0 {
		if ran.Load() || time.Now().After(deadline) {
			t.Fatalf("the writer did not wait for the lock: its body ran %v", ran.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ran.Load() {
		t.Fatal("the writer's body ran while another transaction held the lock")
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var name string
	if err := readTx(readCtx, db, func(ctx context.Context, tx bun.Tx) error {
		return tx.NewSelect().Table("store_sales_store").Column("store_name").Where("store_id = ?", storeID).Scan(ctx, &name)
	}); err != nil {
		t.Fatalf("a reader waited for the lock or failed: %v", err)
	}
	if ran.Load() {
		t.Fatal("the writer's body ran while another transaction held the lock")
	}

	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the writer failed once the lock was free: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the writer did not run once the lock was free")
	}
	if !ran.Load() {
		t.Fatal("the writer finished without running its body")
	}
}

// A WRITE UNDER AN OLDER GENERATION WRITES NOTHING. A page read before a
// rebuild and written after it would put old facts over a store the rebuild
// set back: writeTx refuses it with errRebuilt, before its body runs. A write
// under the current generation, or under anyGeneration, runs.
func TestAWriteUnderAnOlderGenerationWritesNothing(t *testing.T) {
	_, db := statsEngine(t)
	storeID := newStore(t)
	ensureRow(t, db, storeID)
	ctx := context.Background()

	read, err := generationOf(ctx, db, storeID)
	if err != nil {
		t.Fatal(err)
	}
	// A rebuild between the read and the write.
	if _, err := db.ExecContext(ctx, `UPDATE store_sales_store SET generation = generation + 1 WHERE store_id = ?`, storeID); err != nil {
		t.Fatal(err)
	}
	rename := func(name string, ran *bool) func(context.Context, bun.Tx) error {
		return func(ctx context.Context, tx bun.Tx) error {
			*ran = true
			_, err := tx.ExecContext(ctx, `UPDATE store_sales_store SET store_name = ? WHERE store_id = ?`, name, storeID)
			return err
		}
	}

	var ran bool
	if err := writeTx(ctx, db, storeID, read, rename("stale", &ran)); !errors.Is(err, errRebuilt) {
		t.Fatalf("a write under generation %d after a rebuild answered %v, want errRebuilt", read, err)
	}
	if ran {
		t.Error("the body of a write under an older generation ran")
	}
	if got := storeRow(t, storeID)["store_name"]; got == "stale" {
		t.Error("a write under an older generation wrote")
	}

	current, err := generationOf(ctx, db, storeID)
	if err != nil {
		t.Fatal(err)
	}
	if current != read+1 {
		t.Fatalf("the generation is %d after the rebuild, want %d", current, read+1)
	}
	for _, c := range []struct {
		what       string
		generation int64
		name       string
	}{
		{"the current generation", current, "current"},
		{"anyGeneration", anyGeneration, "any"},
	} {
		ran = false
		if err := writeTx(ctx, db, storeID, c.generation, rename(c.name, &ran)); err != nil || !ran {
			t.Errorf("a write under %s answered %v and ran %v, want it run", c.what, err, ran)
		}
		if got := storeRow(t, storeID)["store_name"]; got != c.name {
			t.Errorf("after a write under %s the store's name is %v, want %s", c.what, got, c.name)
		}
	}

	// A store with no row at all is a store removed: a write under a
	// generation writes nothing to it either.
	gone := fmt.Sprintf("dbtest-storestats-gone-%d", time.Now().UnixNano())
	ran = false
	if err := writeTx(ctx, db, gone, 0, rename("gone", &ran)); !errors.Is(err, errRebuilt) || ran {
		t.Errorf("a write under a generation to a store with no row answered %v and ran %v, want errRebuilt and no body", err, ran)
	}
}

// walkRecorder is a walkStore seam that keeps, in order, each store it was
// handed and the generation it was handed with.
type walkRecorder struct {
	mu          sync.Mutex
	stores      []string
	generations map[string]int64
	fail        func(storeID string) error
}

func (w *walkRecorder) walk(_ context.Context, store map[string]any, generation int64, _ time.Time, _ wholesaleState, _ *RollupReport) error {
	id, _ := store["id"].(string)
	w.mu.Lock()
	w.stores = append(w.stores, id)
	if w.generations == nil {
		w.generations = map[string]int64{}
	}
	w.generations[id] = generation
	w.mu.Unlock()
	if w.fail != nil {
		return w.fail(id)
	}
	return nil
}

func (w *walkRecorder) count(storeID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, s := range w.stores {
		if s == storeID {
			n++
		}
	}
	return n
}

// contextEngine answers from the real engine and notes how every read was
// asked: its origin, whether it was fresh, whether it was roots-only.
type contextEngine struct {
	inner Engine
	mu    sync.Mutex
	calls []askedRead
}

type askedRead struct {
	query                      string
	internal, fresh, rootsOnly bool
}

func (e *contextEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.mu.Lock()
	e.calls = append(e.calls, askedRead{
		query:     query,
		internal:  auth.OriginFromContext(ctx).IsInternal(),
		fresh:     memql.FreshReadFromContext(ctx),
		rootsOnly: memql.RootsOnlyReadFromContext(ctx),
	})
	e.mu.Unlock()
	if e.inner == nil {
		return nil, errors.New("this engine answers nothing")
	}
	return e.inner.Execute(ctx, query)
}

func (e *contextEngine) asked() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

// lowerStoresPage pages the stores read in pages of n for the rest of the test.
func lowerStoresPage(t *testing.T, n int) {
	t.Helper()
	was := storesPageSize
	storesPageSize = n
	t.Cleanup(func() { storesPageSize = was })
}

// EVERY STORE ONCE, EACH WITH ITS ROW. Three new stores, read in pages of two:
// each is handed to the walk once a run, with the generation its row holds,
// and has its store_sales_store row; a second run adds none. Every store the
// run lists is listed once, in bare-id order, and every read is internal,
// fresh and roots-only.
func TestTheRunKeepsARowForEveryStoreAndListsEachOnce(t *testing.T) {
	i := testIntegration(t)
	_, db := statsEngine(t)
	lowerStoresPage(t, 2)
	stores := []string{newStore(t), newStore(t), newStore(t)}
	engine := &contextEngine{inner: i.engine}
	i.engine = engine
	queries := storeSalesQueries(t)

	for run := 1; run <= 2; run++ {
		rec := &walkRecorder{}
		i.walkStore = rec.walk
		rep, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now())
		if err != nil {
			t.Fatalf("run %d failed: %v", run, err)
		}
		for _, id := range stores {
			if n := rec.count(id); n != 1 {
				t.Errorf("run %d handed %s to the walk %d times, want once", run, id, n)
			}
			row := storeRow(t, id)
			if row == nil {
				t.Errorf("run %d left %s without a store_sales_store row", run, id)
				continue
			}
			if g, ok := row["generation"].(int64); !ok || g != rec.generations[id] {
				t.Errorf("run %d handed %s to the walk with generation %d, and its row holds %v", run, id, rec.generations[id], row["generation"])
			}
		}
		if !sort.StringsAreSorted(rec.stores) {
			t.Errorf("run %d walked the stores out of bare-id order: %v", run, rec.stores)
		}
		if len(slices.Compact(slices.Clone(rec.stores))) != len(rec.stores) {
			t.Errorf("run %d walked a store twice: %v", run, rec.stores)
		}
		if rep.Stores != len(rec.stores) {
			t.Errorf("run %d reports %d stores and walked %d", run, rep.Stores, len(rec.stores))
		}
		if len(engine.calls) < 2 {
			t.Errorf("run %d asked %d pages of stores; with pages of two and three new stores it takes at least two", run, len(engine.calls))
		}
		for _, c := range engine.calls {
			if !c.internal || !c.fresh || !c.rootsOnly {
				t.Errorf("%s was read with internal origin %v, fresh %v, roots-only %v; want all three", c.query, c.internal, c.fresh, c.rootsOnly)
			}
		}
		engine.calls = nil

		var inserts int
		for _, q := range queries() {
			if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(q)), "INSERT") && strings.Contains(q, "store_sales_store") {
				inserts++
			}
		}
		if inserts == 0 {
			t.Errorf("run %d wrote no store_sales_store row", run)
		}
	}
	var n int
	if err := db.NewSelect().Table("store_sales_store").Where("store_id IN (?)", bun.In(stores)).ColumnExpr("count(*)").Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	if n != len(stores) {
		t.Errorf("after two runs the three stores have %d store_sales_store rows, want %d", n, len(stores))
	}
}

// ONE STORE'S FAILURE COSTS ONLY ITSELF. The walk fails for the first of two
// new stores: the second is still handed to it, and the run's error names the
// first and not the second. A store rebuilt under the walk is no failure: it
// is left for the next run.
func TestOneStoresFailureLeavesTheNextStoreWalked(t *testing.T) {
	i := testIntegration(t)
	stores := []string{newStore(t), newStore(t), newStore(t)}
	sort.Strings(stores)
	failing, walked, rebuilt := stores[0], stores[1], stores[2]
	rec := &walkRecorder{fail: func(id string) error {
		switch id {
		case failing:
			return errors.New("the walk of this store failed")
		case rebuilt:
			return fmt.Errorf("fold a page: %w", errRebuilt)
		}
		return nil
	}}
	i.walkStore = rec.walk

	_, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now())
	if err == nil {
		t.Fatal("the run answered no error though a store's walk failed")
	}
	if !strings.Contains(err.Error(), failing) {
		t.Errorf("the run's error %q does not name the store that failed, %s", err, failing)
	}
	if strings.Contains(err.Error(), walked) || strings.Contains(err.Error(), rebuilt) {
		t.Errorf("the run's error %q names a store that did not fail", err)
	}
	if rec.count(walked) != 1 || rec.count(rebuilt) != 1 {
		t.Errorf("after %s failed, %s was walked %d times and %s %d, want once each", failing, walked, rec.count(walked), rebuilt, rec.count(rebuilt))
	}
}

// THE ROLLUP IS THE ENGINE'S. Each of these is refused, before any read and
// before the database is reached: a cluster owner by hand, an automation's
// ordinary reader actor, another automation's maintenance principal, the
// principal's own id as a reader, the principal reached at client origin (the
// cluster-owner lock alone), and nobody at all. The principal at internal
// origin runs it, through the engine, as the automation calls it.
func TestOnlyTheRollupsPrincipalRunsIt(t *testing.T) {
	principal := auth.MaintenanceActor("storeStatisticsRollupSweep")
	if principal == nil {
		t.Fatal("storeStatisticsRollupSweep is not on the maintenance list, so its automation runs as a reader and every run is refused")
	}
	other := auth.MaintenanceActor("emailStatisticsRollupSweep")
	internal := func(ac *auth.AccessContext) context.Context {
		return auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), ac))
	}
	engine := &contextEngine{}
	var touched atomic.Bool
	refusing := NewIntegration(engine, func() *bun.DB { touched.Store(true); return nil }, quietLogger())
	walked := false
	refusing.walkStore = func(context.Context, map[string]any, int64, time.Time, wholesaleState, *RollupReport) error {
		walked = true
		return nil
	}
	for name, ctx := range map[string]context.Context{
		"a cluster owner by hand, at internal origin":    internal(&auth.AccessContext{UserId: "user-owner", Role: auth.RoleOwner}),
		"the automation's reader actor":                  internal(&auth.AccessContext{UserId: "system:automation:storeStatisticsRollupSweep", Role: auth.RoleReader}),
		"another automation's principal":                 internal(other),
		"the principal's id as a reader":                 internal(&auth.AccessContext{UserId: principal.UserId, Role: auth.RoleReader}),
		"the principal at client origin":                 auth.ContextWithAccess(context.Background(), principal),
		"the principal stamped client origin explicitly": auth.ContextWithClientOrigin(auth.ContextWithAccess(context.Background(), principal)),
		"nobody at all, at internal origin":              auth.ContextWithInternalOrigin(context.Background()),
	} {
		nodes, err := refusing.handleRollup(ctx, nil, 0)
		if err == nil || nodes != nil {
			t.Errorf("%s: got %d nodes and error %v, want a refusal", name, len(nodes), err)
			continue
		}
		if !strings.Contains(err.Error(), "requires the engine maintenance principal") {
			t.Errorf("%s: refused with %q, want the sentence that names the principal", name, err)
		}
	}
	if engine.asked() != 0 || touched.Load() || walked {
		t.Errorf("a refused call asked %d queries, reached for the database %v, walked %v; want none of them", engine.asked(), touched.Load(), walked)
	}

	eng, _ := statsEngine(t)
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("the engine's boot-time executor audit refuses this wiring: %v", err)
	}
	if _, err := eng.Execute(principalCtx(t, "storeStatisticsRollupSweep"), "builtin storeStatisticsRollup()"); err == nil {
		t.Error("the engine ran the rollup for the principal at client origin")
	}
	res, err := eng.Execute(auth.ContextWithInternalOrigin(principalCtx(t, "storeStatisticsRollupSweep")), "builtin storeStatisticsRollup()")
	if err != nil {
		t.Fatalf("the maintenance principal was refused: %v", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("the builtin answered %d rows, want one report", len(rows))
	}
	raw, _ := json.Marshal(rows[0])
	if rows[0]["concept"] != rollupConcept || !strings.Contains(string(raw), `"stores"`) || !strings.Contains(string(raw), `"facts"`) {
		t.Errorf("the report is %s, want one %s row with stores and facts", raw, rollupConcept)
	}
}

// ONE RUN AT A TIME ON A NODE. A run is held inside its walk while a second
// starts: the second stands down -- no node, no error, no store walked -- and
// the first finishes with its report.
func TestTwoRunsAtOnceInOneProcessAreOne(t *testing.T) {
	i := testIntegration(t)
	storeID := newStore(t)
	inside := make(chan struct{})
	release := make(chan struct{})
	var walks, held atomic.Int32
	i.walkStore = func(_ context.Context, store map[string]any, _ int64, _ time.Time, _ wholesaleState, _ *RollupReport) error {
		walks.Add(1)
		// Only the first walk of the test's store is held, so a second run
		// that did not stand down is seen walking rather than hanging.
		if store["id"] == storeID && held.Add(1) == 1 {
			close(inside)
			<-release
		}
		return nil
	}
	ctx := auth.ContextWithInternalOrigin(principalCtx(t, "storeStatisticsRollupSweep"))
	logs := captureLogs(i)
	firstBegan := time.Now()

	type answer struct {
		n   int
		err error
	}
	first := make(chan answer, 1)
	go func() {
		nodes, err := i.handleRollup(ctx, nil, 0)
		first <- answer{len(nodes), err}
	}()
	select {
	case <-inside:
	case <-time.After(60 * time.Second):
		close(release)
		t.Fatal("the first run never reached the test's store")
	}
	before := walks.Load()
	second := make(chan answer, 1)
	go func() {
		nodes, err := i.handleRollup(ctx, nil, 0)
		second <- answer{len(nodes), err}
	}()
	var got2 answer
	select {
	case got2 = <-second:
	case <-time.After(60 * time.Second):
		close(release)
		t.Fatal("the second run neither stood down nor finished")
	}
	nodes, err := got2.n, got2.err
	if err != nil || nodes != 0 {
		t.Errorf("the second run answered %d nodes and error %v while the first ran, want it to stand down with neither", nodes, err)
	}
	if !stoodDownAtInfo(t, logs(), firstBegan) {
		t.Errorf("the second run did not say at Info that it stood down, with when the running one started: %v", logs())
	}
	if walks.Load() != before {
		t.Errorf("the second run walked %d stores while the first ran, want none", walks.Load()-before)
	}
	close(release)
	got := <-first
	if got.err != nil || got.n != 1 {
		t.Errorf("the first run answered %d nodes and error %v, want its one report", got.n, got.err)
	}

	// Once the first has finished, a run is a run again.
	if nodes, err := i.handleRollup(ctx, nil, 0); err != nil || len(nodes) != 1 {
		t.Errorf("a run after the first finished answered %d nodes and error %v, want its report", len(nodes), err)
	}
}

// THE REPORT HOLDS NOTHING OF A ROW, AND NEITHER DOES THE LOG. The engine's
// work journal keeps what the automation's step answers, and a node's log is
// read by more people than a store's figures are. So with a store, its shop
// row, and an order of it whose email, address and buyer are recognisable, a
// real run -- the provider's own store state, and a walk that reads the
// buyers read through walkSource and judges every row's email as the fold
// does -- answers a report and writes log lines that hold none of them. The
// walk is the test's because the fold is a later task's; the reads, the store
// state, the report and the logger are the provider's own.
func TestTheReportHoldsNothingDrawnFromARow(t *testing.T) {
	i := testIntegration(t)
	logs := captureLogs(i)
	storeID := newStore(t)
	shopRow(t, storeID, "EUR", "Europe/Madrid", false)
	n := time.Now().UnixNano()
	email := fmt.Sprintf("statistics-buyer-%d@example.com", n)
	address := fmt.Sprintf("%d Recognisable Lane", n)
	buyerName := fmt.Sprintf("Recognisable Buyer %d", n)
	buyer := fmt.Sprintf("gid://shopify/Customer/%d", n)
	gid := fmt.Sprintf("gid://shopify/Order/%d", n)
	mirror(t, "v1:shopify:order", storeID, gid, map[string]any{
		"email": email, "name": "#1001", "currencyCode": "EUR", "customerGid": buyer,
		"processedAt":     time.Now().UTC().Format(time.RFC3339),
		"shippingAddress": map[string]any{"address1": address, "name": buyerName, "city": "Example City"},
	})
	i.wholesaleFor = func(context.Context, []string) (map[string]wholesaleState, error) {
		return map[string]wholesaleState{storeID: {Enabled: true, Granted: map[string]bool{email: true}}}, nil
	}
	var read, wholesale int
	i.walkStore = func(ctx context.Context, store map[string]any, _ int64, now time.Time, state wholesaleState, rep *RollupReport) error {
		id, _ := store["id"].(string)
		_, _, err := walkSource(ctx, i.engine, "storeStatisticsOrderBuyers", id, now.Add(-24*time.Hour), walkWindow{}, time.Now, maxPagesPerRun,
			func(page walkPage) error {
				for _, row := range page.Rows {
					read++
					rep.Rows[sourceOrder]++
					if e, _ := row["email"].(string); state.isWholesale(e) {
						wholesale++
					}
				}
				return nil
			})
		return err
	}

	nodes, err := i.handleRollup(auth.ContextWithInternalOrigin(principalCtx(t, "storeStatisticsRollupSweep")), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("the run answered %d nodes, want its report", len(nodes))
	}
	if read != 1 || wholesale != 1 {
		t.Fatalf("the walk read %d rows and judged %d wholesale; want the order read and its buyer judged, or nothing below measures a leak", read, wholesale)
	}
	raw, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nodes[0].Payload), `"stores"`) {
		t.Fatalf("the run answered %s, not a report", nodes[0].Payload)
	}
	drawn := []string{email, address, buyerName, buyer, gid, storeID + ".myshopify.com", "Store " + storeID, "Shop " + storeID}
	for _, d := range drawn {
		if strings.Contains(string(raw), d) || strings.Contains(string(nodes[0].Payload), d) {
			t.Errorf("the report holds %q, drawn from a row: %s", d, nodes[0].Payload)
		}
	}
	lines := logs()
	if len(lines) == 0 {
		t.Fatal("the run wrote no log line, so nothing below measures one")
	}
	for _, line := range lines {
		for _, d := range drawn {
			if strings.Contains(line, d) {
				t.Errorf("a log line holds %q, drawn from a row: %s", d, line)
			}
		}
	}
}

// THE HARNESS RUNS AGAINST ITS OWN STORES. A store another package's test
// left in the shared database, whose walk would fail, changes nothing: the
// run lists this test's store alone, walks it, and gives the other no row.
func TestAStoreAnotherTestLeftChangesNothing(t *testing.T) {
	i := testIntegration(t)
	own := newStore(t)
	foreign := foreignStore(t)
	rec := &walkRecorder{fail: func(id string) error {
		if id != own {
			return errors.New("a store this test did not seed was walked")
		}
		return nil
	}}
	i.walkStore = rec.walk
	rep, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now())
	if err != nil {
		t.Fatalf("a store this test did not seed failed the run: %v", err)
	}
	if rec.count(own) != 1 || rec.count(foreign) != 0 || rep.Stores != 1 {
		t.Errorf("the run listed %d stores, walked its own %d times and the other %d; want 1, once and never", rep.Stores, rec.count(own), rec.count(foreign))
	}
	if storeRow(t, foreign) != nil {
		t.Error("the run gave a store this test did not seed its row")
	}
}

// THE WHOLESALE STATE IS ASKED ONCE A RUN. wholesaleFor is called once, with
// every store the run is about to roll, and each store is handed its own
// judgement: an enabled store's granted buyer is wholesale, whatever the case
// and spaces of the address; anybody else, a store that is not enabled and a
// store wholesaleFor says nothing of answer false for every address. A nil
// wholesaleFor is nobody wholesale.
func TestTheWholesaleStateIsAskedOncePerRun(t *testing.T) {
	i := testIntegration(t)
	stores := []string{newStore(t), newStore(t), newStore(t)}
	sort.Strings(stores)
	calls := 0
	var asked []string
	i.wholesaleFor = func(_ context.Context, ids []string) (map[string]wholesaleState, error) {
		calls++
		asked = slices.Clone(ids)
		granted := map[string]bool{"buyer@example.com": true}
		return map[string]wholesaleState{
			stores[0]: {Enabled: true, Granted: granted},
			stores[1]: {Enabled: false, Granted: granted},
		}, nil
	}
	judged := map[string][3]bool{}
	i.walkStore = func(_ context.Context, store map[string]any, _ int64, _ time.Time, wholesale wholesaleState, _ *RollupReport) error {
		id, _ := store["id"].(string)
		judged[id] = [3]bool{wholesale.isWholesale(" Buyer@Example.COM "), wholesale.isWholesale("other@example.com"), wholesale.isWholesale("")}
		return nil
	}
	if _, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("wholesaleFor was asked %d times in one run, want once", calls)
	}
	if !slices.Equal(asked, stores) {
		t.Errorf("wholesaleFor was asked for %v, want the stores the run rolls, %v", asked, stores)
	}
	for id, want := range map[string][3]bool{stores[0]: {true, false, false}, stores[1]: {}, stores[2]: {}} {
		if judged[id] != want {
			t.Errorf("store %s judged the granted buyer, another address and no address %v, want %v", id, judged[id], want)
		}
	}

	i.wholesaleFor = nil
	clear(judged)
	if _, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, id := range stores {
		if judged[id] != ([3]bool{}) {
			t.Errorf("with no wholesaleFor, store %s judged %v, want nobody wholesale", id, judged[id])
		}
	}
}

// A WHOLESALE STATE THAT CANNOT BE READ ROLLS NO STORE. The brief is silent
// on it, so no store's facts are folded under a judgement that would be
// wrong: the run answers the error and walks no store.
func TestAWholesaleStateThatCannotBeReadRollsNoStore(t *testing.T) {
	i := testIntegration(t)
	newStore(t)
	i.wholesaleFor = func(context.Context, []string) (map[string]wholesaleState, error) {
		return nil, errors.New("the wholesale settings could not be read")
	}
	walked := 0
	i.walkStore = func(context.Context, map[string]any, int64, time.Time, wholesaleState, *RollupReport) error {
		walked++
		return nil
	}
	_, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now())
	if err == nil || !strings.Contains(err.Error(), "the wholesale settings could not be read") {
		t.Errorf("the run answered %v, want the wholesale read's error", err)
	}
	if walked != 0 {
		t.Errorf("the run walked %d stores with no wholesale judgement, want none", walked)
	}
}

// lowerRunDeadline gives a run d before it is cancelled, and unwind after
// that, for the rest of the test.
func lowerRunDeadline(t *testing.T, d, unwind time.Duration) {
	t.Helper()
	wasD, wasU := runDeadline, runUnwind
	runDeadline, runUnwind = d, unwind
	t.Cleanup(func() { runDeadline, runUnwind = wasD, wasU })
}

// A RUN HAS A DEADLINE. A walk stuck past it, deaf to its context, does not
// silence every later minute: the run answers an error that names the
// deadline, its context is cancelled, and the next run is a run, not a stand
// down.
func TestARunThatOutlastsItsDeadlineIsCancelledAndTheNextRuns(t *testing.T) {
	i := testIntegration(t)
	newStore(t)
	lowerRunDeadline(t, 300*time.Millisecond, 200*time.Millisecond)
	release := make(chan struct{})
	stuck := make(chan error, 1)
	var calls atomic.Int32
	i.walkStore = func(ctx context.Context, _ map[string]any, _ int64, _ time.Time, _ wholesaleState, _ *RollupReport) error {
		if calls.Add(1) == 1 {
			<-release
			stuck <- ctx.Err()
		}
		return nil
	}
	ctx := auth.ContextWithInternalOrigin(principalCtx(t, "storeStatisticsRollupSweep"))
	began := time.Now()
	type answer struct {
		nodes []memorynodes.MemoryNode
		err   error
	}
	first := make(chan answer, 1)
	go func() {
		nodes, err := i.handleRollup(ctx, nil, 0)
		first <- answer{nodes, err}
	}()
	var got answer
	select {
	case got = <-first:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("a run stuck in its walk did not answer within ten seconds: it has no deadline")
	}
	nodes, err := got.nodes, got.err
	if err == nil || nodes != nil || !strings.Contains(err.Error(), "did not end within") {
		close(release)
		t.Fatalf("a run stuck past its deadline answered %d nodes and error %v, want the deadline's error", len(nodes), err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("the stuck run answered after %s, want about its deadline", took)
	}
	nodes, err = i.handleRollup(ctx, nil, 0)
	if err != nil || len(nodes) != 1 {
		t.Errorf("the run after a stuck one answered %d nodes and error %v, want its report", len(nodes), err)
	}
	close(release)
	select {
	case cerr := <-stuck:
		if !errors.Is(cerr, context.DeadlineExceeded) {
			t.Errorf("the stuck walk's context ended with %v, want its deadline", cerr)
		}
	case <-time.After(10 * time.Second):
		t.Error("the stuck walk never ended")
	}
}

// THE PROVIDER OFFERS WHAT ITS BUILTINS NAME. It registers as storeStats, and
// every integration.storeStats.* executor in the DSL tree names a capability
// it has. Not a database test.
func TestTheProviderOffersWhatItsBuiltinsName(t *testing.T) {
	var provider memql.IntegrationProvider
	for _, p := range memql.RegisteredPlugins() {
		if p.Name != "storeStats" {
			continue
		}
		var err error
		if provider, err = p.Factory(memql.PluginContext{Logger: quietLogger()}); err != nil {
			t.Fatalf("the factory failed: %v", err)
		}
	}
	if provider == nil {
		t.Fatal("no provider registered as storeStats, so integration.storeStats.* names nothing")
	}
	if provider.IntegrationName() != integrationName || integrationName != "storeStats" {
		t.Errorf("the provider calls itself %q, want storeStats", provider.IntegrationName())
	}
	offered := map[string]bool{}
	for _, c := range provider.Capabilities() {
		offered[c.Name] = true
	}

	executor := regexp.MustCompile(`@executor\("integration\.storeStats\.([A-Za-z0-9_]+)"\)`)
	var named []string
	root := filepath.Join("..", "..", "dsl")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range executor.FindAllStringSubmatch(string(src), -1) {
			named = append(named, m[1])
			if !offered[m[1]] {
				t.Errorf("%s names integration.storeStats.%s, which the provider does not offer", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(named, "rollup") {
		t.Errorf("no builtin in the tree names integration.storeStats.rollup; found %v", named)
	}
	if _, ok := any(provider).(*Integration); !ok {
		t.Errorf("the registered provider is %T, want this package's Integration", provider)
	}
}

// stoodDownAtInfo reports whether a line says, at Info, that a run stood down,
// with a startedAt no earlier than the running one began.
func stoodDownAtInfo(t *testing.T, lines []string, began time.Time) bool {
	t.Helper()
	for _, line := range lines {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		if rec["level"] != "INFO" || !strings.Contains(msg, "stood down") {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(rec["startedAt"]))
		if err == nil && !at.Before(began.Add(-time.Second)) && !at.After(time.Now()) {
			return true
		}
	}
	return false
}
