package storestats

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// wholesale.go -- who is wholesale: an order is wholesale when its store has
// the wholesale pack enabled and its buyer holds granted wholesale access now.
// The pack knows both; this file asks it once a run, and judges a store's
// orders again when what it answers changes.
//
// # The pack's answer, once a run
//
// wholesaleOf is what the run's seam wholesaleFor is set to (rollup.go, "Who
// is wholesale"). It first asks this node whether the pack is loaded here,
// dsl.PackDomainDisabled: the loaders' own answer, set at boot from the
// v1:platform:packState rows over the pack's declared default, and what the
// module registry reports as loaded or mounted inert. The rows themselves are
// the cluster's wish and can differ from a node until it restarts; what
// matters here is whether the pack's queries exist on the node that runs the
// run. When the pack is off here no pack query is asked and no store is
// enabled.
//
// Otherwise it asks the pack's two statistics reads, settings and granted
// entitlements, with both forms of every store's id (`<id>` and
// `v1:shopify:store:<id>`: the pack's rows hold either, and the engine does
// not join the two), each paged to its end, and groups what they answer by
// the bare id. A store is enabled when it has a settings row; its granted set
// is the folded buyerEmail of its granted entitlements, a blank one left out.
// The two reads share one time budget, wholesaleReadBudget, asked before
// every page on the budgets' clock: a cluster's settings rows alone could
// fill any count of reads, so a count would not bound them. When it is spent
// the run is told so, with no value of a row, and does what it does for any
// wholesale error: every store's housekeeping runs and no store's facts are
// walked (rollup.go).
//
// # An order is judged when it is written, and again when the set changes
//
// The run hands each store's walk its state, and the fold judges every order
// it writes with it (wholesaleState.isWholesale): a blank email matches
// nothing, an entitlement with a blank address included, and neither does
// the marker an erasure leaves, since no granted address is the marker.
//
// An order written before its buyer was granted, or after the buyer was
// revoked, keeps the judgement it was written with until it is judged again.
// So a store's row keeps a digest of the granted set it was last judged
// against (grantedDigest), and a pass starts when an enabled store's digest
// differs from the kept one, or when the store becomes enabled -- its kept
// wholesale_enabled was false -- whatever its digest: orders written while
// it was not enabled were written retail. The pass walks the store's order
// buyers (storeStatisticsOrderBuyers) through walkSource from the store's
// floor, at most maxPagesPerRun pages a turn and asking the turn's budget
// before each, and writes, a page a transaction under the store's
// generation, each live order's judgement, its cursor and the width the walk
// learned; the last page empties the cursor. While the cursor is not empty
// the next turn carries on from it. A set that changes mid-pass starts the
// pass again. A rebuild drops a pass with the watermarks (store.go): the
// facts it refills are judged as they are written.
//
// A page's judgement is guarded by version, as the fold is: a fact the fold
// holds at a newer version than the row the page read was judged with that
// newer row, and is left; a tombstone and a deleted fact are passed over.
//
// A STORE THAT STOPS BEING ENABLED -- the pack off, or its settings gone --
// is kept not enabled, and keeps its digest, its cursor and every order's
// flag; the read answers no wholesale figure for it.
//
// # No address leaves memory
//
// The emails are compared in memory. No address, and nothing derived from
// one but the digest, reaches a table, an error or a log line: a page's
// transaction is handed order gids, versions and booleans.

const wholesaleDomain = "wholesale"

// wholesalePageSize is how many rows one page of the pack's reads holds: the
// queries' own `paginate 250`. A variable so a test pages a few rows across
// many pages.
var wholesalePageSize = 250

// canonicalStorePrefix makes a bare store id the other form the pack's rows
// may hold it in.
const canonicalStorePrefix = "v1:shopify:store:"

// buyersRead is the read the pass walks (rollup.go, changedReads).
const buyersRead = "storeStatisticsOrderBuyers"

// grantedDigest is the hex SHA-256 of the granted set: its folded emails,
// sorted, joined by "\n", a blank one left out. Never empty, so a store whose
// digest was never computed (an empty column) never matches one enabled with
// nobody granted.
func grantedDigest(granted map[string]bool) string {
	seen := map[string]bool{}
	emails := make([]string, 0, len(granted))
	for e, held := range granted {
		f := foldEmail(e)
		if !held || f == "" || seen[f] {
			continue
		}
		seen[f] = true
		emails = append(emails, f)
	}
	sort.Strings(emails)
	sum := sha256.Sum256([]byte(strings.Join(emails, "\n")))
	return hex.EncodeToString(sum[:])
}

// wholesaleOf answers each enabled store's state, keyed by its bare id; a
// store it does not name is not enabled. See "The pack's answer, once a run".
func (i *Integration) wholesaleOf(ctx context.Context, storeIDs []string) (map[string]wholesaleState, error) {
	if memqldsl.PackDomainDisabled(wholesaleDomain) {
		return nil, nil
	}
	wanted := map[string]bool{}
	var forms []string
	for _, raw := range storeIDs {
		id := memql.BareShortId(raw)
		if id == "" || wanted[id] {
			continue // a blank member would also match rows with no store id
		}
		wanted[id] = true
		forms = append(forms, langparser.QuoteString(id), langparser.QuoteString(canonicalStorePrefix+id))
	}
	if len(forms) == 0 {
		return nil, nil // the pack's reads refuse an empty list
	}
	if i.engine == nil {
		return nil, errors.New("this node has no engine handle, so the wholesale pack cannot be read")
	}
	ctx = walkContext(ctx)
	list := "[" + strings.Join(forms, ", ") + "]"
	ends := budgetNow().Add(wholesaleReadBudget)
	settings, err := readToEnd(ctx, i.engine, fmt.Sprintf(wholesaleSettingsRead, list, wholesalePageSize), "the wholesale settings", ends)
	if err != nil {
		return nil, err
	}
	granted, err := readToEnd(ctx, i.engine, fmt.Sprintf(wholesaleGrantedRead, list, wholesalePageSize), "the granted buyers", ends)
	if err != nil {
		return nil, err
	}
	states := map[string]wholesaleState{}
	for _, row := range settings {
		raw, _ := row["storeId"].(string)
		if id := memql.BareShortId(raw); wanted[id] {
			states[id] = wholesaleState{Enabled: true, Granted: map[string]bool{}}
		}
	}
	for _, row := range granted {
		raw, _ := row["storeId"].(string)
		st, ok := states[memql.BareShortId(raw)]
		if !ok || row["state"] != "granted" {
			continue
		}
		email, _ := row["buyerEmail"].(string)
		if folded := foldEmail(email); folded != "" {
			st.Granted[folded] = true
		}
	}
	for id, st := range states {
		st.Digest = grantedDigest(st.Granted)
		states[id] = st
	}
	return states, nil
}

// readToEnd reads every page of query, the reads' budget asked before each.
// Its errors name what was read, never a row.
func readToEnd(ctx context.Context, engine Engine, query, what string, ends time.Time) ([]map[string]any, error) {
	var rows []map[string]any
	cursor := ""
	for {
		if !budgetNow().Before(ends) {
			return nil, fmt.Errorf("the wholesale pack's reads did not end within their budget of %s, so no store's wholesale state is known this run", wholesaleReadBudget)
		}
		res, err := engine.Execute(memql.ContextWithCursor(ctx, cursor), query)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", what, err)
		}
		next, err := memql.ResultPageCursor(res)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", what, err)
		}
		rows = append(rows, memql.MaterializeRows(res)...)
		if next == "" {
			return rows, nil
		}
		if next == cursor {
			return nil, fmt.Errorf("read %s: the engine answered the same cursor it was asked with", what)
		}
		cursor = next
	}
}

// keptWholesale is where a store's wholesale judgement stands, as its row
// keeps it.
type keptWholesale struct {
	found, purging, enabled bool
	digest, cursor          string
	width                   time.Duration
}

func wholesaleKept(ctx context.Context, db *bun.DB, storeID string) (keptWholesale, error) {
	var k keptWholesale
	var seconds int64
	err := readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		return tx.NewSelect().Table("store_sales_store").
			Column("purging", "wholesale_enabled", "wholesale_digest", "wholesale_cursor", "wholesale_window_seconds").
			Where("store_id = ?", storeID).Scan(ctx, &k.purging, &k.enabled, &k.digest, &k.cursor, &seconds)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return keptWholesale{}, nil
	}
	if err != nil {
		return keptWholesale{}, fmt.Errorf("store %s: read where its wholesale judgement stands: %w", storeID, sanitized(err))
	}
	k.found = true
	k.width = time.Duration(seconds) * time.Second
	return k, nil
}

// judgedOrder is one order a pass page judges: plain values, made before the
// page's transaction begins.
type judgedOrder struct {
	gid       string
	version   time.Time
	wholesale bool
}

// judgeOrders is each order of the page judged, at its newest version in the
// page. A tombstone is passed over, and so is a row with no gid or write time
// or of another store.
func judgeOrders(storeID string, rows []map[string]any, st wholesaleState) []judgedOrder {
	type newest struct {
		judgedOrder
		gone bool
	}
	byGID := map[string]newest{}
	var order []string
	for _, row := range rows {
		gid, _ := row["gid"].(string)
		store, _ := row["storeId"].(string)
		at, ok := instantOf(row["createdAt"])
		if gid == "" || !ok || !sameStore(store, storeID) {
			continue
		}
		if held, seen := byGID[gid]; seen && !at.After(held.version) {
			continue
		} else if !seen {
			order = append(order, gid)
		}
		email, _ := row["email"].(string)
		byGID[gid] = newest{judgedOrder{gid: gid, version: at, wholesale: st.isWholesale(email)}, row["deleted"] == true}
	}
	out := make([]judgedOrder, 0, len(order))
	for _, gid := range order {
		if n := byGID[gid]; !n.gone {
			out = append(out, n.judgedOrder)
		}
	}
	return out
}

// wholesalePass is the wholesale part of a store's turn: it keeps a store
// that is not enabled so, starts a pass when one is due, and carries a pass
// under way on, at most budget pages. See "An order is judged when it is
// written, and again when the set changes".
func wholesalePass(ctx context.Context, i *Integration, storeID string, generation int64, st wholesaleState, clock func() time.Time, budget int) error {
	if strings.TrimSpace(storeID) == "" {
		return fmt.Errorf("judge a store's wholesale orders: %w", errNoStore)
	}
	// The turn's budget, before anything: a turn spent on its walk begins no
	// pass, and the next turn does what this one left (rollup.go, "A turn
	// has a budget"). walkSource asks it again before every page.
	if turnSpent(ctx) {
		return nil
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	if db == nil {
		return errors.New("this node has no database handle, so the store statistics cannot be written")
	}
	kept, err := wholesaleKept(ctx, db, storeID)
	if err != nil || !kept.found || kept.purging {
		return err
	}
	if !st.Enabled {
		if !kept.enabled {
			return nil
		}
		return passError(storeID, "keep the store not enabled for wholesale", writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			return keepWholesaleOff(ctx, tx, storeID)
		}))
	}
	digest := st.Digest
	if digest == "" {
		digest = grantedDigest(st.Granted)
	}
	starting := !kept.enabled || kept.digest != digest
	if !starting && kept.cursor == "" {
		return nil
	}
	floor, err := ensureFloor(ctx, i, storeID, clock())
	if err != nil {
		return err
	}
	w := walkWindow{Width: kept.width}
	if starting {
		if err := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			return startWholesalePass(ctx, tx, storeID, digest, floor)
		}); err != nil {
			return passError(storeID, "start the wholesale pass", err)
		}
		w = walkWindow{Through: floor, Width: defaultWindow}
	} else {
		through, err := time.Parse(time.RFC3339Nano, kept.cursor)
		if err != nil {
			return fmt.Errorf("store %s: the wholesale pass's cursor is not an instant", storeID)
		}
		w.Through = through.UTC()
	}

	stored := w
	next, _, err := walkSource(ctx, i.engine, buyersRead, storeID, floor, w, clock, budget, func(page walkPage) error {
		judged := judgeOrders(storeID, page.Rows, st)
		cursor := page.Through.UTC().Format(time.RFC3339Nano)
		if !page.CaughtUp.IsZero() {
			cursor = "" // the pass has read to the clock: it ends
		}
		if err := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			return wholesalePage(ctx, tx, storeID, judged, cursor, page.Width)
		}); err != nil {
			return passError(storeID, "judge a page of orders", err)
		}
		stored = walkWindow{Through: page.Through, Width: page.Width}
		return nil
	})
	if errors.Is(err, errRebuilt) {
		return errRebuilt
	}
	// A width the walk changed with no page handed on -- a page that
	// outlasted a read halves it -- is kept for the next turn, as the walk's
	// own sources keep theirs (fold.go, walkOneSource).
	if next.Through != stored.Through || next.Width != windowWidth(stored.Width) {
		cursor := next.Through.UTC().Format(time.RFC3339Nano)
		if kerr := writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
			return wholesalePage(ctx, tx, storeID, nil, cursor, next.Width)
		}); kerr != nil {
			if errors.Is(kerr, errRebuilt) {
				return errRebuilt
			}
			err = errors.Join(err, passError(storeID, "keep the wholesale pass's window", kerr))
		}
	}
	return err
}

// passError is a write of the pass that failed, named, with a refusal of the
// database reported by its SQLSTATE alone; errRebuilt is passed on as it is.
func passError(storeID, what string, err error) error {
	if err == nil || errors.Is(err, errRebuilt) {
		return err
	}
	return fmt.Errorf("store %s: %s: %w", storeID, what, sanitized(err))
}

// startWholesalePass starts the store's pass: its digest, enabled, its cursor
// at the floor and its window a day.
func startWholesalePass(ctx context.Context, tx bun.Tx, storeID, digest string, floor time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE store_sales_store SET wholesale_enabled = true, wholesale_digest = ?,
		wholesale_cursor = ?, wholesale_window_seconds = ? WHERE store_id = ?`,
		digest, floor.UTC().Format(time.RFC3339Nano), int64(defaultWindow/time.Second), storeID); err != nil {
		return fmt.Errorf("start the wholesale pass: %w", err)
	}
	return nil
}

// wholesalePage writes one page of a pass: each order's judgement, over a
// live fact held at the page row's version or an older one, and where the
// pass stands. An empty cursor ends the pass.
func wholesalePage(ctx context.Context, tx bun.Tx, storeID string, orders []judgedOrder, cursor string, width time.Duration) error {
	if len(orders) > 0 {
		gids := make([]string, len(orders))
		versions := make([]string, len(orders))
		flags := make([]string, len(orders))
		for n, o := range orders {
			gids[n] = o.gid
			versions[n] = o.version.UTC().Format(time.RFC3339Nano)
			flags[n] = fmt.Sprint(o.wholesale)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE store_sales_order_fact AS f SET wholesale = v.wholesale::boolean
			FROM unnest(?::text[], ?::text[], ?::text[]) AS v(order_gid, version, wholesale)
			WHERE f.store_id = ? AND f.order_gid = v.order_gid AND NOT f.deleted
				AND f.order_version <= v.version::timestamptz AND f.wholesale <> v.wholesale::boolean`,
			pgdialect.Array(gids), pgdialect.Array(versions), pgdialect.Array(flags), storeID); err != nil {
			return fmt.Errorf("judge the page's orders: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE store_sales_store SET wholesale_cursor = ?, wholesale_window_seconds = ? WHERE store_id = ?`,
		cursor, int64(windowWidth(width)/time.Second), storeID); err != nil {
		return fmt.Errorf("keep where the wholesale pass stands: %w", err)
	}
	return nil
}

// keepWholesaleOff keeps the store not enabled for wholesale, and nothing
// else: its digest, its cursor and its orders' flags stay.
func keepWholesaleOff(ctx context.Context, tx bun.Tx, storeID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE store_sales_store SET wholesale_enabled = false WHERE store_id = ?`, storeID); err != nil {
		return fmt.Errorf("keep the store not enabled for wholesale: %w", err)
	}
	return nil
}

// walkClock is the clock the walk and the pass read: the run's, or the time.
func (i *Integration) walkClock() func() time.Time {
	if i.now != nil {
		return i.now
	}
	return time.Now
}
