package shopify

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"slices"
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
	langast "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// statistics_reads_authz_db_test.go -- the reads the store statistics walk
// asks, against a real engine over a real Postgres.
//
// # Two locks, and each is shown to hold alone
//
// These reads cross every organization's stores: the walk reads every store's
// mirrored orders, lines, refunds, shop and store rows, and keys them by store.
// So who may call them is the whole question, and two locks answer it:
//
//   - @serverOnly: a client-origin call is refused before anything is read,
//     whoever the actor is. A cluster owner signed in to a browser is refused.
//   - `actor.isClusterOwner == true` in the filter: an internal-origin caller
//     who is not a cluster owner reads zero rows.
//
// The non-owner is chosen per concept as the caller row authorization would
// ADMIT were the predicate gone -- the connector's own actor on the mirror
// concepts (their @origin names it) and a developer on v1:shopify:store (its
// read floor is developer) -- so the zero rows are the predicate's and not the
// tier's. A reader rank would read zero rows with or without the predicate,
// and prove nothing about it.
//
// Each refusal stands beside the reachable positive: a cluster owner with
// internal origin reads the seeded row. Without it zero rows reads the same
// for a refused read and a seed that never landed.

// The rows the statistics fixtures write, on authorityTestStoreId.
const (
	statsOrderGid  = "gid://shopify/Order/9101"
	statsLineGid   = "gid://shopify/LineItem/9101"
	statsRefundGid = "gid://shopify/Refund/9101"
	statsShopGid   = "gid://shopify/Shop/9101"
	statsShopName  = "Statistics Probe Shop"
)

// statsMirrorConcepts are the mirror concepts the statistics fixtures write.
var statsMirrorConcepts = []string{"order", "orderLineItem", "refund", "shop"}

// seedStatisticsRows writes the store and one row of each mirror concept the
// walk reads, through the engine's own store mutation and the connector's own
// mirror write path.
func seedStatisticsRows(t *testing.T, eng *memql.MemQLEngine, db *sql.DB) *Connector {
	t.Helper()
	seedStore(t, eng, db)
	for _, concept := range statsMirrorConcepts {
		cleanMirrored(t, db, concept)
	}
	c := &Connector{engine: eng}
	writeMirrored(t, c, "order", map[string]any{
		"id": statsOrderGid, "name": "#9101", "email": "buyer@example.com", "currencyCode": "USD",
		"updatedAt": "2026-11-01T00:00:00Z", "processedAt": "2026-11-01T00:00:00Z", "createdAt": "2026-11-01T00:00:00Z",
		"lineItems": map[string]any{"nodes": []any{map[string]any{
			"id": statsLineGid, "name": "probe line", "title": "Probe", "quantity": 2, "currentQuantity": 2,
		}}},
		"refunds": []any{map[string]any{
			"id": statsRefundGid, "createdAt": "2026-11-02T00:00:00Z", "updatedAt": "2026-11-02T00:00:00Z",
		}},
	})
	writeMirrored(t, c, "shop", map[string]any{
		"id": statsShopGid, "name": statsShopName, "currencyCode": "USD", "ianaTimezone": "Etc/UTC",
		"updatedAt": "2026-11-01T00:00:00Z",
	})
	return c
}

// developerCtx is a developer, signed in: below a cluster owner, and the rank
// v1:shopify:store's read floor admits.
func developerCtx() context.Context {
	return actorCtx("dbtest-statistics-developer", auth.RoleDeveloper)
}

// statsSince is a watermark an hour back: every row a test writes is after it.
func statsSince() string {
	return time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
}

// statisticsRead is one of the seven reads, called for the seeded store.
type statisticsRead struct {
	name string
	args map[string]any
	// reader is the internal-origin caller who is not a cluster owner and
	// whom row authorization admits to the concept.
	reader context.Context
	// seeded reports whether a row is the one the fixtures wrote.
	seeded func(map[string]any) bool
	// admitted is a read of the same concept with no cluster-owner
	// predicate, which the reader is expected to reach the seeded row
	// through: the premise that row authorization admits the reader, so the
	// zero rows of the statistics read are the predicate's. admittedRow
	// reports whether a row it answers is the seeded one.
	admitted    string
	admittedRow func(map[string]any) bool
}

func (r statisticsRead) call() string { return renderCall(r.name, r.args) }

// isStoreRow reports whether a row id is the seeded store's, in either form.
func isStoreRow(row map[string]any) bool {
	id := mapString(row, "id")
	return id == authorityTestStoreId || strings.HasSuffix(id, ":"+authorityTestStoreId)
}

func statisticsReads() []statisticsRead {
	since := statsSince()
	connector := connectorContext(context.Background())
	developer := auth.ContextWithInternalOrigin(developerCtx())
	byGid := func(gid string) func(map[string]any) bool {
		return func(row map[string]any) bool { return mapString(row, "gid") == gid }
	}
	walk := map[string]any{"storeId": authorityTestStoreId, "since": since}
	// The mirror concepts' generated reads all carry the cluster-owner
	// predicate too, so the predicate-free read of a mirror concept is the
	// engine's plain concept read: what row authorization alone lets through.
	mirror := func(concept string) string {
		return `paginate(concept==v1:shopify:` + concept + ` && payload.storeId=="` + authorityTestStoreId + `", 250)`
	}
	mirrorRow := func(gid string) func(map[string]any) bool {
		return func(row map[string]any) bool {
			return strings.HasSuffix(mapString(row, "id"), ":"+MirrorRowID(authorityTestStoreId, gid))
		}
	}
	// storeById is the tree's own read of a store, with no cluster-owner
	// predicate: v1:shopify:store's tier admits a developer.
	storeById := renderCall("storeById", map[string]any{"storeId": authorityTestStoreId})
	return []statisticsRead{
		{name: "storeStatisticsOrdersChanged", args: walk, reader: connector, seeded: byGid(statsOrderGid),
			admitted: mirror("order"), admittedRow: mirrorRow(statsOrderGid)},
		{name: "storeStatisticsLinesChanged", args: walk, reader: connector, seeded: byGid(statsLineGid),
			admitted: mirror("orderLineItem"), admittedRow: mirrorRow(statsLineGid)},
		{name: "storeStatisticsRefundsChanged", args: walk, reader: connector, seeded: byGid(statsRefundGid),
			admitted: mirror("refund"), admittedRow: mirrorRow(statsRefundGid)},
		{name: "storeStatisticsOrderBuyers", args: walk, reader: connector, seeded: byGid(statsOrderGid),
			admitted: mirror("order"), admittedRow: mirrorRow(statsOrderGid)},
		{name: "storeStatisticsShop", args: map[string]any{"storeId": authorityTestStoreId}, reader: connector,
			seeded:   func(row map[string]any) bool { return mapString(row, "name") == statsShopName },
			admitted: mirror("shop"), admittedRow: mirrorRow(statsShopGid)},
		{name: "storeStatisticsStores", reader: developer, seeded: isStoreRow,
			admitted: storeById, admittedRow: isStoreRow},
		{name: "storeStatisticsStoreAt", args: map[string]any{"storeId": authorityTestStoreId, "at": time.Now().UTC().Add(time.Minute).Format(time.RFC3339)},
			reader: developer, seeded: isStoreRow, admitted: storeById, admittedRow: isStoreRow},
	}
}

// readAll pages a read to its end through the engine's cursor, as the walk
// will: storeStatisticsStores answers every store in the shared database,
// oldest first, so the seeded one may not be on the first page.
func readAll(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, call string) []map[string]any {
	t.Helper()
	var out []map[string]any
	cursor := ""
	for page := 0; page < 100; page++ {
		pageCtx := ctx
		if cursor != "" {
			pageCtx = memql.ContextWithCursor(ctx, cursor)
		}
		res, err := eng.Execute(pageCtx, call)
		if err != nil {
			t.Fatalf("%s: %v", call, err)
		}
		rows := memql.MaterializeRows(res)
		out = append(out, rows...)
		next := ""
		if meta := res.GetMeta(); meta != nil {
			next = strings.TrimSpace(meta.Cursor)
		}
		if next == "" || next == cursor || len(rows) == 0 {
			return out
		}
		cursor = next
	}
	t.Fatalf("%s did not end within 100 pages", call)
	return nil
}

func TestTheStatisticsReadsAnswerNothingToAReaderAndRowsToAClusterOwner(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	seedStatisticsRows(t, eng, db)

	for _, read := range statisticsReads() {
		t.Run(read.name, func(t *testing.T) {
			// THE REACHABLE POSITIVE: a cluster owner with internal origin,
			// the walk's own standing, reads the seeded row.
			rows := readAll(t, eng, seedCtx(), read.call())
			if !slices.ContainsFunc(rows, read.seeded) {
				t.Fatalf("a cluster owner with internal origin read %d rows of %s and not the seeded one, "+
					"so nothing below measures a lock: %v", len(rows), read.name, rows)
			}

			// THE PREMISE: row authorization admits the reader to the
			// concept, so what it reads below is the predicate's answer and
			// not the tier's.
			if admitted := readAll(t, eng, read.reader, read.admitted); !slices.ContainsFunc(admitted, read.admittedRow) {
				t.Fatalf("the reader of %s does not reach the seeded row through %s (%d rows), so row authorization "+
					"refuses it and the zero rows below would prove nothing about the predicate", read.name, read.admitted, len(admitted))
			}

			// THE PREDICATE: an internal-origin caller who is not a cluster
			// owner, and whom row authorization admits, reads nothing.
			res, err := eng.Execute(read.reader, read.call())
			if err != nil {
				t.Fatalf("%s for an internal-origin caller who is not a cluster owner: %v; want zero rows and no error", read.name, err)
			}
			if got := memql.MaterializeRows(res); len(got) != 0 {
				t.Errorf("%s answered %d rows to an internal-origin caller who is not a cluster owner; want 0: "+
					"the actor.isClusterOwner predicate is the lock, and it did not hold", read.name, len(got))
			}

			// THE ANNOTATION: a cluster owner from a client is refused before
			// anything is read.
			_, err = eng.Execute(auth.ContextWithClientOrigin(ownerCtx()), read.call())
			if err == nil || !strings.Contains(err.Error(), "server-only") {
				t.Errorf("%s from a client, as a cluster owner: err = %v; want it refused as server-only", read.name, err)
			}
		})
	}
}

// A changed-read answers a row the mirror has retired: the walk needs the
// tombstone to retire the fact the row made. Pinned per walk, since a walk
// that filtered `deleted` would leave a refunded-away order counted forever.
func TestAChangedReadAnswersATombstoneToo(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	c := seedStatisticsRows(t, eng, db)
	now := time.Now()
	for concept, gid := range map[string]string{"order": statsOrderGid, "orderLineItem": statsLineGid, "refund": statsRefundGid} {
		if err := c.writeMirror(context.Background(), tombstone(concept, authorityTestStoreId, gid, now)); err != nil {
			t.Fatalf("retire the %s %s: %v", concept, gid, err)
		}
	}
	for _, read := range statisticsReads()[:4] {
		t.Run(read.name, func(t *testing.T) {
			rows := readAll(t, eng, seedCtx(), read.call())
			i := slices.IndexFunc(rows, read.seeded)
			if i < 0 {
				t.Fatalf("%s did not answer the retired row; the walk could never retire its fact: %v", read.name, rows)
			}
			if deleted, _ := rows[i]["deleted"].(bool); !deleted {
				t.Errorf("%s answered the retired row with deleted %v; want true: %v", read.name, rows[i]["deleted"], rows[i])
			}
		})
	}
}

// The six shapes, as the walk is to receive them: the fields, in order.
var statisticsShapes = map[string]struct {
	concept string
	fields  []string
}{
	"storeStatisticsOrder": {"order", []string{"row.id", "row.createdAt", "storeId", "gid", "syncedAt", "deleted", "processedAt",
		"sourceCreatedAt", "cancelledAt", "test", "taxesIncluded", "currencyCode", "customerGid", "email", "subtotalLineItemsQuantity",
		"totalShippingPriceSet", "totalTaxSet", "originalTotalDutiesSet", "currentSubtotalPriceSet", "totalDiscountsSet", "totalRefundedSet"}},
	"storeStatisticsLine": {"orderLineItem", []string{"row.id", "row.createdAt", "storeId", "gid", "parentGid", "deleted", "isGiftCard",
		"productGid", "title", "quantity", "currentQuantity", "originalUnitPriceSet", "discountAllocations"}},
	"storeStatisticsRefund": {"refund", []string{"row.id", "row.createdAt", "storeId", "gid", "parentGid", "syncedAt", "deleted",
		"processedAt", "sourceCreatedAt", "totalRefundedSet", "duties", "refundLineItems", "refundShippingLines", "orderAdjustments"}},
	"storeStatisticsBuyer": {"order", []string{"row.id", "row.createdAt", "storeId", "gid", "deleted", "email"}},
	"storeStatisticsShop": {"shop", []string{"row.id", "row.createdAt", "storeId", "deleted", "name", "currencyCode", "ianaTimezone",
		"taxesIncluded", "plan"}},
	"storeStatisticsStore": {"store", []string{"row.id", "domain", "name", "isDevelopment", "scopesGranted", "uninstalledAt",
		"redactedAt", "health"}},
}

// The query each shape is read through.
var statisticsQueryShapes = map[string]string{
	"storeStatisticsOrdersChanged":  "storeStatisticsOrder",
	"storeStatisticsLinesChanged":   "storeStatisticsLine",
	"storeStatisticsRefundsChanged": "storeStatisticsRefund",
	"storeStatisticsOrderBuyers":    "storeStatisticsBuyer",
	"storeStatisticsShop":           "storeStatisticsShop",
	"storeStatisticsStores":         "storeStatisticsStore",
	"storeStatisticsStoreAt":        "storeStatisticsStore",
}

var statisticsTree struct {
	once    sync.Once
	shapes  map[string]*langast.ShapeDecl
	shapeOf map[string]string
	err     error
}

// loadStatisticsTree reads the six shapes and the seven queries out of the
// embedded DSL tree, through dslimports.Load -- the pipeline the engine's
// loader is built on -- so the declarations are judged, not a copy of them.
func loadStatisticsTree(t *testing.T) (map[string]*langast.ShapeDecl, map[string]string) {
	t.Helper()
	s := &statisticsTree
	s.once.Do(func() {
		tree, err := dslimports.Load(memqldsl.Tree())
		if err != nil {
			s.err = err
			return
		}
		s.shapes, s.shapeOf = map[string]*langast.ShapeDecl{}, map[string]string{}
		for path, file := range tree.Files {
			if file == nil || !strings.HasPrefix(path, "shopify/overlay/") {
				continue
			}
			for _, def := range file.Definitions {
				switch d := def.(type) {
				case *langast.ShapeDecl:
					if _, ok := statisticsShapes[d.Name]; ok {
						s.shapes[d.Name] = d
					}
				case *langast.FunctionDef:
					if shaped, ok := d.Body.(*langast.ShapeExpr); ok {
						if _, ok := statisticsQueryShapes[d.Name]; ok {
							s.shapeOf[d.Name] = shaped.TemplateName
						}
					}
				}
			}
		}
	})
	if s.err != nil {
		t.Fatalf("dslimports.Load: %v -- the embedded DSL tree did not load, so the statistics shapes cannot be read", s.err)
	}
	return s.shapes, s.shapeOf
}

// The walk reads every organization's orders, so a shape carries what a figure
// needs and nothing else: no person's name, address or phone, no account, and
// the buyer's email only on the two order shapes, where a returning buyer is
// counted. The one name projected is the shop's own and the store's own.
func TestTheStatisticsShapesProjectExactlyTheirFields(t *testing.T) {
	shapes, shapeOf := loadStatisticsTree(t)
	for name, want := range statisticsShapes {
		decl, ok := shapes[name]
		if !ok {
			t.Errorf("dsl/shopify/overlay declares no shape %s", name)
			continue
		}
		if decl.SignatureConcept != want.concept {
			t.Errorf("shape %s is bound to %q, want %q", name, decl.SignatureConcept, want.concept)
		}
		got := make([]string, 0, len(decl.Paths))
		for _, p := range decl.Paths {
			got = append(got, strings.TrimSpace(p))
		}
		if !slices.Equal(got, want.fields) {
			t.Errorf("shape %s projects\n  %v\nwant exactly, in order,\n  %v", name, got, want.fields)
		}
		for _, field := range got {
			lower := strings.ToLower(field)
			switch {
			case field == "accountId":
				t.Errorf("shape %s projects accountId; the account plays no part in a figure", name)
			case field == "email" && want.concept != "order":
				t.Errorf("shape %s projects email; only the two order shapes may", name)
			case strings.Contains(lower, "address") || strings.Contains(lower, "phone"):
				t.Errorf("shape %s projects %s: no address and no phone reaches the walk", name, field)
			case strings.Contains(lower, "name") && !(field == "name" && (want.concept == "shop" || want.concept == "store")):
				t.Errorf("shape %s projects %s: the one name projected is the shop's or the store's own", name, field)
			}
		}
	}
	for query, shape := range statisticsQueryShapes {
		if got := shapeOf[query]; got != shape {
			t.Errorf("query %s reads through shape %q, want %q", query, got, shape)
		}
	}
}

// storeStatisticsStoreAt answers the store row as it stood at an instant: the
// walk asks earlier and earlier instants to find when a store first existed,
// since nothing on the row says so.
func TestTheStoreRowIsAnsweredAsOfAnInstantAndNotBeforeItWasWritten(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	seedStore(t, eng, db)
	// The instants are the store's own write times, read back exactly: an
	// at is sent with its fractions of a second, so the test needs no wait
	// between the writes for the instants to fall apart.
	first := storeWrittenAt(t, eng)
	if _, err := eng.Execute(seedCtx(), renderCall("updateStore", map[string]any{"storeId": authorityTestStoreId, "name": "Renamed Probe"})); err != nil {
		t.Fatalf("rename the store: %v", err)
	}
	renamed := storeWrittenAt(t, eng)
	if !renamed.After(first) {
		t.Fatalf("the rename was written at %v, not after the first write at %v", renamed, first)
	}
	between, before := first, first.Add(-time.Microsecond)
	at := func(instant time.Time) []map[string]any {
		return readAll(t, eng, seedCtx(), renderCall("storeStatisticsStoreAt", map[string]any{
			"storeId": authorityTestStoreId, "at": instant.Format(time.RFC3339Nano),
		}))
	}
	if rows := at(renamed); len(rows) != 1 || mapString(rows[0], "name") != "Renamed Probe" {
		t.Errorf("as of the rename the store reads %v; want the one row, renamed", rows)
	}
	if rows := at(between); len(rows) != 1 || mapString(rows[0], "name") != "Authority Probe" {
		t.Errorf("as of its first write, before the rename, the store reads %v; want the one row with its older name", rows)
	}
	if rows := at(before); len(rows) != 0 {
		t.Errorf("as of a microsecond before the store was first written it reads %v; want no row", rows)
	}
}

// storeWrittenAt is the write time of the seeded store's newest version.
func storeWrittenAt(t *testing.T, eng *memql.MemQLEngine) time.Time {
	t.Helper()
	rows := readAll(t, eng, seedCtx(), renderCall("storeById", map[string]any{"storeId": authorityTestStoreId}))
	if len(rows) != 1 {
		t.Fatalf("storeById read %d rows of the seeded store, want 1", len(rows))
	}
	at, ok := rows[0]["createdAt"].(time.Time)
	if !ok {
		t.Fatalf("the store row's createdAt is %T %v, not a time", rows[0]["createdAt"], rows[0]["createdAt"])
	}
	return at.UTC()
}

// sqlRecorder keeps every SQL statement the engine sends.
type sqlRecorder struct {
	mu    sync.Mutex
	stmts []string
}

func (r *sqlRecorder) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context { return ctx }

func (r *sqlRecorder) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, event.Query)
}

func (r *sqlRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.stmts
	r.stmts = nil
	return out
}

// recordingAuthorityEngine is authorityEngine with every SQL statement kept.
func recordingAuthorityEngine(t *testing.T) (*memql.MemQLEngine, *sql.DB, *sqlRecorder) {
	t.Helper()
	dsn := dbtest.DSN()
	raw := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "shopify statistics reads", dsn, err)
		return nil, nil, nil
	}
	db := bun.NewDB(raw, pgdialect.New())
	rec := &sqlRecorder{}
	db.AddQueryHook(rec)
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	if _, err := eng.Execute(seedCtx(), `insert("v1:accounts:account", id="self", payload={"name":"Test cluster","status":"active","domainStatus":"unverified"})`); err != nil {
		t.Fatalf("seed the test cluster organization: %v", err)
	}
	return eng, raw, rec
}

// A walk since a watermark costs what was written since, not the concept's
// history: the engine collapses only the versions at or after the bound, and
// only when `row.createdAt >= args.since` is a top-level conjunct. The rule
// this pins is latestScanLowerBounds in component/memql/latest_scan_keys.go,
// which narrows the latest-key enumeration with that one clause.
//
// What is matched is kept to that clause: the enumeration is the subquery the
// engine aliases `latest_keys` (component/memql/executor_filter.go), so the
// text before the alias is the enumeration, and it must hold
// `"createdAt" >= '<since>`. The instant is matched to the second and its zone
// suffix is left out. Move the conjunct under an `||` with a real other arm
// and the enumeration loses it, and this fails.
func TestTheStatisticsWalksCollapseOnlyTheVersionsWrittenSince(t *testing.T) {
	eng, db, rec := recordingAuthorityEngine(t)
	if eng == nil {
		return
	}
	seedStatisticsRows(t, eng, db)
	sinceAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	bound := `"createdAt" >= '` + sinceAt.Format("2006-01-02 15:04:05")
	for name, concept := range map[string]string{
		"storeStatisticsOrdersChanged":  "order",
		"storeStatisticsLinesChanged":   "orderLineItem",
		"storeStatisticsRefundsChanged": "refund",
		"storeStatisticsOrderBuyers":    "order",
	} {
		t.Run(name, func(t *testing.T) {
			rec.take()
			if _, err := eng.Execute(seedCtx(), renderCall(name, map[string]any{
				"storeId": authorityTestStoreId, "since": sinceAt.Format(time.RFC3339),
			})); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			enumerations := 0
			for _, stmt := range rec.take() {
				end := strings.Index(stmt, "AS latest_keys")
				if end < 0 || !strings.Contains(stmt[:end], "'v1:shopify:"+concept+"'") {
					continue
				}
				enumerations++
				if keys := stmt[:end]; !strings.Contains(keys, bound) {
					t.Errorf("%s enumerated its latest keys without the watermark (%s), so a page costs the concept's "+
						"whole history:\n%s", name, bound, keys)
				}
			}
			if enumerations == 0 {
				t.Errorf("%s sent no latest-key enumeration of %s; the read no longer takes the path this test pins", name, concept)
			}
		})
	}
}
