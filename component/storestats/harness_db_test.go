package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// harness_db_test.go -- the store statistics against a real engine and a real
// database, the harness every database test of this package uses.
//
// The stores are written by the store mutation the connector and Connect
// Shopify write with, and read back through the engine the way the scheduled
// run reads them: through the @serverOnly store statistics reads, under the
// automation's maintenance principal. A fake engine would assert its own rows;
// what is worth proving is that the engine's own paging, its id spellings and
// its locks give what the walk says.
//
// THE DATABASE IS SHARED, so every assertion is about the stores its own test
// made (each newStore is a store of its own, named by a fresh suffix). A run
// lists every store in the database, other tests' included, and gives each a
// row; nothing here asserts about those.
//
// WHAT THE READS GUARANTEE, which every consumer of them relies on: a walk's
// page is ordered by write time, ties broken by id; a row rewritten while a
// walk is under way comes back on a later page, so a consumer dedupes by id;
// `since` is inclusive; a version that commits late can land behind a
// watermark already passed, so a consumer reads with an overlap; and the
// pack's reads refuse an empty list of store ids while a blank member also
// matches rows with no store id, so neither is ever passed.
//
// Postgres-gated. CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1,
// so a skip there is a failure rather than a green.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// statsEngine builds one engine over the real database per process, with this
// package's provider registered on it, or skips.
//
// ONE BOOT PER PROCESS, the email statistics harness's rule: an engine boot
// loads the whole DSL tree. The Once records its own failure so every caller
// reports it rather than the first one swallowing it.
var statsEngineState struct {
	once    sync.Once
	eng     *memql.MemQLEngine
	db      *bun.DB
	pingErr error
	bootErr error
}

func statsEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	s := &statsEngineState
	s.once.Do(func() {
		if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
			s.pingErr = err
			return
		}
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		if err := db.PingContext(context.Background()); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		// The one hook storeSalesQueries records through: bun has no way to
		// take a hook off a handle, so it is added once and a test registers
		// a recorder with it for as long as it runs.
		db.AddQueryHook(statementRecorders)
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			_ = db.Close()
			return
		}
		eng, err := memql.New(db)
		if err != nil {
			s.bootErr = fmt.Errorf("New: %w", err)
			_ = db.Close()
			return
		}
		eng.Logger = quietLogger()
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("Init: %w", err)
			_ = db.Close()
			return
		}
		// The provider as a node registers it, so `builtin
		// storeStatisticsRollup()` dispatches here exactly as the scheduled
		// automation's run does.
		if err := eng.RegisterIntegration(NewIntegration(eng, func() *bun.DB { return db }, quietLogger())); err != nil {
			s.bootErr = fmt.Errorf("RegisterIntegration: %w", err)
			_ = db.Close()
			return
		}
		s.eng, s.db = eng, db
		// A run lists only the stores this package's tests seeded: see
		// seededStores.
		onlyStoresForTests = seededStores.has
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "the store statistics", dbtest.DSN(), s.pingErr)
		return nil, nil
	}
	if s.bootErr != nil {
		t.Fatalf("the statistics engine failed to boot: %v", s.bootErr)
	}
	return s.eng, s.db
}

// testIntegration is an Integration over the shared engine and database whose
// seams a test sets itself. The registered provider keeps its own.
func testIntegration(t *testing.T) *Integration {
	t.Helper()
	eng, db := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	return NewIntegration(eng, func() *bun.DB { return db }, quietLogger())
}

// principalCtx is the context a scheduled automation runs its builtin under:
// the cluster's maintenance principal for the automation named, with the
// claims and token component/automations stamps beside it. Internal origin is
// the caller's to add, as the automation runner adds it.
func principalCtx(t *testing.T, automation string) context.Context {
	t.Helper()
	actor := auth.MaintenanceActor(automation)
	if actor == nil {
		t.Fatalf("%s is not on the maintenance list, so its automation runs as a reader and every run is refused", automation)
	}
	claims := map[string]any{"sub": actor.UserId, "email": actor.UserId, "role": string(actor.Role)}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, actor)
}

// ownerCtx is a person who is a cluster owner, signed in.
func ownerCtx() context.Context {
	claims := map[string]any{"sub": "dbtest-storestats-owner", "role": "owner"}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "dbtest-storestats-owner", Role: auth.RoleOwner})
}

// newStore writes a store of the test's own through createStore, as its owner,
// and answers its bare id. The store's graph rows and its store_sales_store
// row are deleted when the test ends.
func newStore(t *testing.T) string {
	t.Helper()
	eng, db := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	storeID := fmt.Sprintf("dbtest-storestats-%d", time.Now().UnixNano())
	seededStores.add(storeID)
	t.Cleanup(func() {
		seededStores.remove(storeID)
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM store_sales_store WHERE store_id = ?`, storeID)
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = ?`, "v1:shopify:store:"+storeID)
	})
	call := fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s, name: %s, adminTokenRef: "test-admin", apiVersion: "2026-07")`,
		langparser.QuoteString(storeID), langparser.QuoteString(storeID+".myshopify.com"), langparser.QuoteString("Store "+storeID))
	if _, err := eng.Execute(ownerCtx(), call); err != nil {
		t.Fatalf("write the store %s: %v", storeID, err)
	}
	return storeID
}

// mirror writes one mirror row of the store, as the Shopify connector writes
// it: a raw insert of concept (a full id, "v1:shopify:order") under the
// connector's actor, at the id shopify.MirrorRowID(storeID, gid). storeId and
// gid are always the ones named; deleted is false, and updatedAt and syncedAt
// are the clock, unless payload says otherwise (the mirror's schema requires
// updatedAt). Writing the same gid again writes a newer version of the same
// row. Every version is deleted when the test ends.
func mirror(t *testing.T, concept, storeID, gid string, payload map[string]any) {
	t.Helper()
	eng, db := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	rowID := shopify.MirrorRowID(storeID, gid)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = ? AND id = ?`, concept, concept+":"+rowID)
	})
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	body := map[string]any{"deleted": false, "updatedAt": stamp, "syncedAt": stamp}
	for k, v := range payload {
		body[k] = v
	}
	body["storeId"], body["gid"] = storeID, gid
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	connector := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "shopify"))
	if _, err := eng.Execute(connector, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(concept), langparser.QuoteString(rowID), raw)); err != nil {
		t.Fatalf("write the mirror row %s of %s: %v", gid, concept, err)
	}
}

// storeRow is the store's store_sales_store row, or nil when it has none.
func storeRow(t *testing.T, storeID string) map[string]any {
	t.Helper()
	_, db := statsEngine(t)
	row := map[string]any{}
	err := db.NewSelect().Table("store_sales_store").Where("store_id = ?", storeID).Scan(context.Background(), &row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the store_sales_store row of %s: %v", storeID, err)
	}
	return row
}

// storeSalesQueries records, from now until the test ends, the statements the
// shared handle runs that name a store_sales_ table, and answers a function
// that hands back the ones recorded since it was last called.
func storeSalesQueries(t *testing.T) func() []string {
	t.Helper()
	statsEngine(t)
	r := &statementRecorder{}
	statementRecorders.add(r)
	t.Cleanup(func() { statementRecorders.remove(r) })
	return r.take
}

// statementRecorders is the one hook on the shared handle; it hands each
// statement to the recorders registered at the time.
var statementRecorders = &recorderHook{}

type recorderHook struct {
	mu        sync.Mutex
	recorders []*statementRecorder
}

func (h *recorderHook) add(r *statementRecorder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recorders = append(h.recorders, r)
}

func (h *recorderHook) remove(r *statementRecorder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, x := range h.recorders {
		if x == r {
			h.recorders = append(h.recorders[:i], h.recorders[i+1:]...)
			return
		}
	}
}

func (h *recorderHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *recorderHook) AfterQuery(_ context.Context, ev *bun.QueryEvent) {
	if !strings.Contains(ev.Query, "store_sales_") {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recorders {
		r.record(ev.Query)
	}
}

type statementRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *statementRecorder) record(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, q)
}

func (r *statementRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.seen
	r.seen = nil
	return out
}

// shopRow writes the store's shop row, live, as the mirror writes it: a raw
// insert of a v1:shopify:shop row under the connector actor, with its prices
// including tax. The row is deleted when the test ends.
func shopRow(t *testing.T, storeID, currency, zone string, development bool) {
	t.Helper()
	writeShopRow(t, storeID, map[string]any{
		"currencyCode": currency, "ianaTimezone": zone, "taxesIncluded": true,
		"plan": map[string]any{"partnerDevelopment": development},
	})
}

// writeShopRow writes a version of the store's one shop row with these fields
// over its storeId, gid, deleted false and name.
func writeShopRow(t *testing.T, storeID string, fields map[string]any) {
	t.Helper()
	eng, db := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	gid := "gid://shopify/Shop/1"
	rowID := shopify.MirrorRowID(storeID, gid)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:shop' AND id = ?`, "v1:shopify:shop:"+rowID)
	})
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	row := map[string]any{"storeId": storeID, "gid": gid, "deleted": false, "name": "Shop " + storeID, "updatedAt": stamp, "syncedAt": stamp}
	for k, v := range fields {
		row[k] = v
	}
	payload, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	connector := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "shopify"))
	if _, err := eng.Execute(connector, fmt.Sprintf(`insert("v1:shopify:shop", id=%s, payload=%s)`, langparser.QuoteString(rowID), payload)); err != nil {
		t.Fatalf("write the shop row of %s: %v", storeID, err)
	}
}

// answerVerdict sets the integration's verdictOf to read this reply, as the
// mirror's builtin would answer it, through the same reading the default
// makes; a nil reply is a verdict that cannot be read.
func answerVerdict(i *Integration, reply map[string]any) {
	i.verdictOf = func(context.Context, string) (storeVerdict, error) {
		if reply == nil {
			return storeVerdict{}, errors.New("the verdict could not be read")
		}
		return verdictFromReply(reply)
	}
}

// seededStores is every store a test of this package made with newStore and
// has not yet cleaned up. THE HARNESS RUNS AGAINST ITS OWN STORES: the
// database is shared, and a store another package's test left behind --
// one whose verdict cannot be read, whose walk fails -- would otherwise be
// walked by every run here and fail this package's tests. statsEngine sets
// onlyStoresForTests to its has. Only a test file sets that variable, which
// TestNoProductionCodeSetsOnlyStoresForTests holds (rollup.go). The tests of
// this package run one at a time, so the set is the running test's stores.
var seededStores = &storeSet{ids: map[string]bool{}}

type storeSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (s *storeSet) add(id string)    { s.mu.Lock(); s.ids[id] = true; s.mu.Unlock() }
func (s *storeSet) remove(id string) { s.mu.Lock(); delete(s.ids, id); s.mu.Unlock() }
func (s *storeSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[id]
}

// foreignStore writes a store as another package's test would leave one: a
// store row this package's tests did not seed. It is deleted when the test
// ends.
func foreignStore(t *testing.T) string {
	t.Helper()
	eng, db := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	storeID := fmt.Sprintf("dbtest-foreign-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM store_sales_store WHERE store_id = ?`, storeID)
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = ?`, "v1:shopify:store:"+storeID)
	})
	call := fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s, adminTokenRef: "test-admin", apiVersion: "2026-07")`,
		langparser.QuoteString(storeID), langparser.QuoteString(storeID+".myshopify.com"))
	if _, err := eng.Execute(ownerCtx(), call); err != nil {
		t.Fatalf("write the foreign store %s: %v", storeID, err)
	}
	return storeID
}

// captureLogs gives i a logger that keeps every line at Debug and above, and
// answers a function that hands back the lines written so far, each as the
// JSON the handler wrote.
func captureLogs(i *Integration) func() []string {
	w := &lineBuffer{}
	i.logger = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return w.lines
}

type lineBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *lineBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(string(b.buf), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---- Task 13: a run, and the store's rows read back ----

// run is one run of the rollup by i under its principal, at internal origin,
// as the scheduled automation makes it: the report it answers, and its error.
// A run that stood down answers no report, which fails the test.
func run(t *testing.T, i *Integration) (RollupReport, error) {
	t.Helper()
	nodes, err := i.handleRollup(auth.ContextWithInternalOrigin(principalCtx(t, "storeStatisticsRollupSweep")), nil, 0)
	if err != nil {
		return RollupReport{}, err
	}
	if len(nodes) != 1 {
		t.Fatalf("the run answered %d nodes, want its report", len(nodes))
	}
	var rep RollupReport
	if err := json.Unmarshal(nodes[0].Payload, &rep); err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	return rep, nil
}

// salesRows reads every row of the store in a store_sales_ table, each as
// Postgres renders it in JSON: a NUMERIC as its own digits (a json.Number,
// so its scale is kept), a time as an RFC 3339 string, null as nil. The rows
// are keyed by the column named, or by key columns joined with "/".
func salesRows(t *testing.T, table, storeID string, keys ...string) map[string]map[string]any {
	t.Helper()
	_, db := statsEngine(t)
	rows, err := db.QueryContext(context.Background(), "SELECT to_jsonb(x)::text FROM "+table+" x WHERE store_id = ?", storeID)
	if err != nil {
		t.Fatalf("read the store's %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		row := map[string]any{}
		if err := dec.Decode(&row); err != nil {
			t.Fatal(err)
		}
		var key []string
		for _, k := range keys {
			key = append(key, fmt.Sprint(row[k]))
		}
		out[strings.Join(key, "/")] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func orderFactsOf(t *testing.T, storeID string) map[string]map[string]any {
	return salesRows(t, "store_sales_order_fact", storeID, "order_gid")
}

func lineFactsOf(t *testing.T, storeID string) map[string]map[string]any {
	return salesRows(t, "store_sales_line_fact", storeID, "line_gid")
}

func refundFactsOf(t *testing.T, storeID string) map[string]map[string]any {
	return salesRows(t, "store_sales_refund_fact", storeID, "refund_gid")
}

func refundLineFactsOf(t *testing.T, storeID string) map[string]map[string]any {
	return salesRows(t, "store_sales_refund_line_fact", storeID, "refund_gid", "position")
}

func watermarksOf(t *testing.T, storeID string) map[string]map[string]any {
	return salesRows(t, "store_sales_watermark", storeID, "source")
}

// ---- Task 16: the check ----

// rewriteInPlace changes the keys named in every stored version of a mirror
// row with one statement, and leaves each version's write time as it was. It
// is a test's stand-in for a customer's erasure, which rewrites a row's
// versions in place where no walk looks; Task 18 runs the mirror's own.
func rewriteInPlace(t *testing.T, concept, storeID, gid string, change map[string]any) {
	t.Helper()
	_, db := statsEngine(t)
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET payload = payload || ?::jsonb WHERE concept = ? AND id = ?`,
		string(raw), concept, concept+":"+shopify.MirrorRowID(storeID, gid))
	if err != nil {
		t.Fatalf("rewrite %s in place: %v", gid, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("rewrite %s in place changed no version", gid)
	}
}
