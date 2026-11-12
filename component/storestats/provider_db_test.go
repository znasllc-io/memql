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
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/repowalk"
	"github.com/znasllc-io/memql/integrations/shopify"
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

func (w *walkRecorder) walk(_ context.Context, store map[string]any, generation int64, _ time.Time, _ *RollupReport) error {
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
	refusing.walkStore = func(context.Context, map[string]any, int64, time.Time, *RollupReport) error {
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
	i.walkStore = func(_ context.Context, store map[string]any, _ int64, _ time.Time, _ *RollupReport) error {
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

// THE REPORT HOLDS NOTHING OF A ROW. The engine's work journal keeps what the
// automation's step answers, so the builtin answers counts: with a store whose
// domain and name are recognisable, and an order of it whose email is, the
// rendered report holds none of the three.
func TestTheReportHoldsNothingDrawnFromARow(t *testing.T) {
	eng, db := statsEngine(t)
	storeID := newStore(t)
	email := fmt.Sprintf("statistics-buyer-%d@example.com", time.Now().UnixNano())
	gid := fmt.Sprintf("gid://shopify/Order/%d", time.Now().UnixNano())
	rowID := shopify.MirrorRowID(storeID, gid)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:order' AND id = ?`, "v1:shopify:order:"+rowID)
	})
	payload, _ := json.Marshal(map[string]any{
		"storeId": storeID, "gid": gid, "deleted": false, "email": email, "name": "#1001",
		"currencyCode": "EUR", "processedAt": time.Now().UTC().Format(time.RFC3339),
		"updatedAt": time.Now().UTC().Format(time.RFC3339), "syncedAt": time.Now().UTC().Format(time.RFC3339),
	})
	connector := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "shopify"))
	if _, err := eng.Execute(connector, fmt.Sprintf(`insert("v1:shopify:order", id=%s, payload=%s)`, langparser.QuoteString(rowID), payload)); err != nil {
		t.Fatalf("write the order: %v", err)
	}
	// The order is there to be read, so a report that carried rows would
	// carry it.
	res, err := eng.Execute(auth.ContextWithInternalOrigin(principalCtx(t, "storeStatisticsRollupSweep")),
		fmt.Sprintf(`query storeStatisticsOrderBuyers(storeId: %s, since: %s)`, langparser.QuoteString(storeID),
			langparser.QuoteString(time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))))
	if err != nil {
		t.Fatal(err)
	}
	if rows := memql.MaterializeRows(res); len(rows) != 1 || rows[0]["email"] != email {
		t.Fatalf("the order's buyer read answered %v, want the order with its email", rows)
	}

	res, err = eng.Execute(auth.ContextWithInternalOrigin(principalCtx(t, "storeStatisticsRollupSweep")), "builtin storeStatisticsRollup()")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(memql.MaterializeRows(res))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stores"`) {
		t.Fatalf("the builtin answered %s, not a report", raw)
	}
	for _, drawn := range []string{email, storeID + ".myshopify.com", "Store " + storeID, gid} {
		if strings.Contains(string(raw), drawn) {
			t.Errorf("the report %s holds %q, drawn from a row", raw, drawn)
		}
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
