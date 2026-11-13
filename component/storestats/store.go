package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/uptrace/bun"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/statsperiod"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// store.go -- what the walk keeps about each store, and what it decides for
// each store at the start of its turn (storeState, set to keepStore).
//
// # What is kept
//
// store_sales_store holds the store's own name, domain, currency and zone,
// whether all its orders are readable, the first day its history covers, and
// the store's verdict as the statistics read it. Nothing of a buyer and
// nothing of an order: an error or a log line here names the store and a
// field, never a value read from an order.
//
// # Reads first, writes after
//
// keepStore reads what it needs from the engine -- the shop row and the
// verdict -- before its transaction begins, and writes after, in one writeTx
// with anyGeneration: no engine call is made while the lock is held (tx.go,
// "Nothing of the engine inside"). Every read is made from Go, under
// walkContext, never as an automation step, and never with a blank store id.
//
// # A store whose data was removed
//
// A store whose row says redactedAt (D17) gets no new store_sales_store row.
// One it still has is marked the way a rebuild marks it (rebuildStore, which
// raises the generation, so no page read before it is written after it), its
// facts are deleted in batches (purgeStore), and once none is left its row,
// its watermarks and its check row go too (forgetStore). keepStore answers
// outcomePurging until then, and outcomeRemoved after.
//
// # Purging first
//
// A store whose purging is set has its facts deleted before anything else is
// done for it, at most purgeBudget transactions a run, each deleting at most
// deleteBatch rows of each fact table: a delete of a whole store in one
// transaction would hold the write lock, and every other store's walk, for as
// long as it took. Nothing is folded for the store while purging is set, so
// no batch can delete a fact written after the rebuild.

// storeStatsDefinition is the definition the facts are made under. A store
// whose stored definition is lower is rebuilt; a new row has 0, so a store's
// first fill is a rebuild with nothing to delete.
const storeStatsDefinition = 1

// deleteBatch is the rows a batched delete takes of one table in one
// transaction; a variable so a test can lower it.
var deleteBatch = 5000

// purgeBudget is the transactions one run spends deleting one store's facts;
// a variable so a test can lower it.
var purgeBudget = 20

// allOrdersScope is the scope without which Shopify lists only a store's last
// sixty days of orders: the test the mirror makes in stampOrdersFrom.
const allOrdersScope = "read_all_orders"

// statsTable is one table the store statistics keep, and how a removal
// deletes a store's rows from it.
type statsTable struct {
	name    string
	batched bool // a fact table: deleted deleteBatch rows a transaction, before any other
}

// storeSalesTables is THE ONE LIST of the tables the store statistics keep. A
// removal deletes a store's rows from each of them: the batched ones (the
// facts) a batch at a time (purgeBatch), then the others, in this order, the
// store's own row last (forgetStore). A table the migrations add joins this
// list: TestEveryStoreSalesTableIsOnThePurgeList holds the migrations to it.
var storeSalesTables = []statsTable{
	{"store_sales_order_fact", true},
	{"store_sales_line_fact", true},
	{"store_sales_refund_fact", true},
	{"store_sales_refund_line_fact", true},
	{"store_sales_unreadable", true}, // the rows that could not be read: as many as a store's facts, at worst
	{"store_sales_watermark", false},
	{"store_sales_check", false},
	{"store_sales_store", false},
}

// statsTableNames is the name of every table on storeSalesTables, in order.
func statsTableNames() []string {
	out := make([]string, 0, len(storeSalesTables))
	for _, t := range storeSalesTables {
		out = append(out, t.name)
	}
	return out
}

// factTables are the batched tables of storeSalesTables: the four that hold
// a store's facts, and its records of the rows that could not be read.
var factTables = func() []string {
	var out []string
	for _, t := range storeSalesTables {
		if t.batched {
			out = append(out, t.name)
		}
	}
	return out
}()

// verdictStaleAfter is how long after the last run that read a store's
// verdict a verdict that cannot be read changes nothing. A rollout of the
// engine's nodes can make the read fail for some minutes; past this the store
// is recorded as blocked, since nothing then confirms its figures are current.
// The read uses the same bound for a stopped walk. With many stores whose
// turns fall further apart, answering figures as of their walk is intentional.
const verdictStaleAfter = 15 * time.Minute

// The reasons this package adds to a store's kept reasons. Each has a code of
// its own, outside the mirror's, so nothing that keys or deduplicates reasons
// by code takes one for the other or for a mirror reason.
const (
	verdictUnreadableCode     = "statistics_verdict_unreadable"
	verdictUnreadableSentence = "The store's recording state could not be read."
	notRecordedCode           = "statistics_not_recorded"
	notRecordedSentence       = "Part of what the figures need is not recorded for this store."
)

// verdictReason is one reason of a store's verdict as the statistics keep it.
type verdictReason struct {
	Code, Sentence string
	InTheWay       bool // shopify.VerdictReasonFromPayload(<its element>).StandsInTheWayOf(walkedConcepts)
}

// storeVerdict is a store's verdict as the statistics read it.
type storeVerdict struct {
	State   string
	Since   time.Time
	Reasons []verdictReason
	// Outside is whether a walked concept is outside what the store records,
	// or the reply does not say (a reply from an older node, with no
	// outsideConcepts): what is not known is never taken for recorded.
	Outside bool
}

// blocked is ruling R1's one rule: the store is paused or not connected, a
// reason stands in the way of the walked concepts, or a walked concept is
// outside what the store records.
func (v storeVerdict) blocked() bool {
	if v.State == shopify.VerdictPaused || v.State == shopify.VerdictNotConnected || v.Outside {
		return true
	}
	for _, r := range v.Reasons {
		if r.InTheWay {
			return true
		}
	}
	return false
}

// known reports whether the state is one of the five this build knows.
func (v storeVerdict) known() bool {
	switch v.State {
	case shopify.VerdictRecording, shopify.VerdictStarting, shopify.VerdictPaused, shopify.VerdictNeedsAttention, shopify.VerdictNotConnected:
		return true
	}
	return false
}

// clear reports whether a run under this verdict is a clear run: not blocked,
// and recording or needing attention only for reasons that stand in the way
// of nothing the walk reads. starting, and any word this does not know, is
// neither clear nor blocked.
func (v storeVerdict) clear() bool {
	return !v.blocked() && (v.State == shopify.VerdictRecording || v.State == shopify.VerdictNeedsAttention)
}

// askVerdict is verdictOf's default: the mirror's builtin shopifyStoreVerdict,
// asked under walkContext.
func (i *Integration) askVerdict(ctx context.Context, storeID string) (storeVerdict, error) {
	if storeID == "" {
		return storeVerdict{}, errors.New("a verdict is never asked for a blank store id")
	}
	if i.engine == nil {
		return storeVerdict{}, errors.New("this node has no engine handle")
	}
	call, err := langparser.RenderCall("shopifyStoreVerdict", map[string]any{"storeId": storeID})
	if err != nil {
		return storeVerdict{}, err
	}
	res, err := i.engine.Execute(walkContext(ctx), "builtin "+call)
	if err != nil {
		return storeVerdict{}, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		return storeVerdict{}, fmt.Errorf("the verdict answered %d rows, want one", len(rows))
	}
	return verdictFromReply(rows[0])
}

// verdictFromReply reads the verdict builtin's reply. A reply whose state,
// since or reasons cannot be read is an error: the verdict cannot be read,
// and is never taken for one that says nothing is wrong.
func verdictFromReply(reply map[string]any) (storeVerdict, error) {
	state, ok := reply["state"].(string)
	if !ok {
		return storeVerdict{}, errors.New("the verdict's state is not a word")
	}
	since, ok := instantOf(reply["since"])
	if !ok {
		return storeVerdict{}, errors.New("the verdict's since is not a time")
	}
	var list []any
	switch rs := reply["reasons"].(type) {
	case []any:
		list = rs
	case []map[string]any:
		for _, m := range rs {
			list = append(list, m)
		}
	default:
		return storeVerdict{}, errors.New("the verdict's reasons are not a list")
	}
	v := storeVerdict{State: state, Since: since, Reasons: []verdictReason{}}
	for n, el := range list {
		m, ok := el.(map[string]any)
		if !ok {
			return storeVerdict{}, fmt.Errorf("the verdict's reason %d is not an object", n)
		}
		code, _ := m["code"].(string)
		sentence, _ := m["sentence"].(string)
		v.Reasons = append(v.Reasons, verdictReason{
			Code: code, Sentence: sentence,
			InTheWay: shopify.VerdictReasonFromPayload(m).StandsInTheWayOf(walkedConcepts),
		})
	}
	outside, known := shopify.OutsideWhatTheStoreRecords(reply, walkedConcepts)
	v.Outside = outside || !known
	if arriving, known := shopify.HistoryArrivingFor(reply, walkedConcepts); (arriving || !known) && (v.State == shopify.VerdictRecording || v.State == shopify.VerdictNeedsAttention) {
		v.State = shopify.VerdictStarting
	}
	return v, nil
}

// shopFacts is what a live shop row gives the store's row.
type shopFacts struct {
	name, currency, zone string
	taxesIncluded        *bool
	development          *bool // plan.partnerDevelopment
}

// readShop reads the store's newest shop row through storeStatisticsShop and
// answers it when it is live; nil when there is none, or the newest is retired.
func (i *Integration) readShop(ctx context.Context, storeID string) (*shopFacts, error) {
	if storeID == "" {
		return nil, errors.New("the shop row is never read for a blank store id")
	}
	res, err := i.engine.Execute(walkContext(ctx), fmt.Sprintf(shopRead, langparser.QuoteString(storeID)))
	if err != nil {
		return nil, fmt.Errorf("read the shop row: %w", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	if deleted, _ := row["deleted"].(bool); deleted {
		return nil, nil
	}
	s := &shopFacts{}
	s.name, _ = row["name"].(string)
	s.currency, _ = row["currencyCode"].(string)
	s.zone, _ = row["ianaTimezone"].(string)
	if b, ok := row["taxesIncluded"].(bool); ok {
		s.taxesIncluded = &b
	}
	if plan, ok := row["plan"].(map[string]any); ok {
		if b, ok := plan["partnerDevelopment"].(bool); ok {
			s.development = &b
		}
	}
	return s, nil
}

// storeDataRemoved reports whether the store row says its data was removed:
// redactedAt holds anything but nothing. Whether that is a readable time is
// keepStore's to say.
func storeDataRemoved(store map[string]any) bool {
	switch v := store["redactedAt"].(type) {
	case nil:
		return false
	case string:
		return v != ""
	case time.Time:
		return !v.IsZero()
	}
	return true
}

// keptFields is what keepStoreRow writes, read before the transaction.
type keptFields struct {
	domain, name     string
	shop             *shopFacts // nil: no live shop row, so currency, zone and tax are left as they are
	development      *bool
	allOrders        bool
	ordersFrom       string // health.ordersFrom, a day; "" when there is none
	refundDetailFrom *time.Time
	verdict          storeVerdict
	verdictRead      bool
}

// keepStore is what storeState is set to. See the file's comment.
func (i *Integration) keepStore(ctx context.Context, store map[string]any, now time.Time) (string, error) {
	id, _ := store["id"].(string)
	if id == "" {
		return "", errors.New("a store with a blank id is not kept")
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	if db == nil {
		return "", errors.New("this node has no database handle, so the store statistics cannot be written")
	}

	if storeDataRemoved(store) {
		if _, ok := instantOf(store["redactedAt"]); !ok {
			return "", errors.New("the store row's redactedAt is set and is not a time")
		}
		return i.removeStore(ctx, db, id, now)
	}

	// The store's row is written first, purging or not, so its verdict and
	// settings stay current while its facts are deleted. Whether it is
	// purging is read under the lock, in the same transaction.
	k, err := i.readKept(ctx, store, id)
	if err != nil {
		return "", err
	}
	var rebuilt, purging, rebuilding, ended bool
	if err := writeTx(ctx, db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		if err := ensureStore(ctx, tx, id); err != nil {
			return err
		}
		var definition int
		var zone string
		var readAt sql.NullTime
		if err := tx.NewSelect().Table("store_sales_store").
			Column("definition", "timezone", "purging", "verdict_read_at").ColumnExpr("rebuilding_since IS NOT NULL").
			Where("store_id = ?", id).Scan(ctx, &definition, &zone, &purging, &readAt, &rebuilding); err != nil {
			return fmt.Errorf("read the store's row: %w", err)
		}
		if k.shop != nil {
			zone = k.shop.zone
		}
		since, err := historySince(ctx, tx, id, k.ordersFrom, i.zoneOf(id, zone))
		if err != nil {
			return err
		}
		if err := keepStoreRow(ctx, tx, id, k, since, readAt, now); err != nil {
			return err
		}
		if definition < storeStatsDefinition {
			rebuilt, purging = true, true
			return rebuildStore(ctx, tx, id, now)
		}
		if !purging && rebuilding {
			var err error
			ended, err = clearRebuilding(ctx, tx, id, now)
			return err
		}
		return nil
	}); err != nil {
		return "", err
	}
	if !purging {
		if ended {
			// Its check begins once the store is walked (rollup.go, rollStore).
			return outcomeRebuildEnded, nil
		}
		return "", nil
	}
	// A rebuild sets purging: the store's facts go before it is walked again.
	done, err := purgeStore(ctx, i, id, purgeBudget)
	if err != nil {
		return "", err
	}
	if !done {
		if rebuilt {
			i.logger.Info("Store statistics: a store's rebuild began; its facts are being deleted before it is walked again",
				"component", "storestats", "storeId", id)
		}
		return outcomePurging, nil
	}
	// The run that finishes a rebuild's purge reports the rebuild.
	if rebuilt || rebuilding {
		return outcomeRebuilt, nil
	}
	return "", nil
}

// readKept reads, before any transaction, what the store's row is written
// from: the store row the run listed, its shop row and its verdict.
func (i *Integration) readKept(ctx context.Context, store map[string]any, id string) (keptFields, error) {
	k := keptFields{}
	k.domain, _ = store["domain"].(string)
	k.name, _ = store["name"].(string)
	if scopes, ok := store["scopesGranted"].([]any); ok {
		for _, s := range scopes {
			if s == allOrdersScope {
				k.allOrders = true
			}
		}
	} else if scopes, ok := store["scopesGranted"].([]string); ok {
		for _, s := range scopes {
			if s == allOrdersScope {
				k.allOrders = true
			}
		}
	}
	if health, ok := store["health"].(map[string]any); ok {
		// The stamp is the beginning of a completed scan, not its earliest order.
		// A connection without all-order access can reach sixty days back from it.
		// Missing or malformed bookkeeping leaves the reachability unknown.
		if !k.allOrders {
			if began, ok := instantOf(health["refundDetailFrom"]); ok {
				floor := began.Add(-60 * 24 * time.Hour)
				k.refundDetailFrom = &floor
			}
		}
		switch v := health["ordersFrom"].(type) {
		case nil:
		case string:
			k.ordersFrom = v
		default:
			return k, errors.New("the store's health.ordersFrom is not a day")
		}
	}

	shop, err := i.readShop(ctx, id)
	if err != nil {
		return k, err
	}
	k.shop = shop
	if shop != nil && k.name == "" {
		k.name = shop.name
	}
	switch {
	case shop != nil && shop.development != nil:
		k.development = shop.development
	default:
		if b, ok := store["isDevelopment"].(bool); ok {
			k.development = &b
		}
	}

	if i.verdictOf == nil {
		err = errors.New("this node has no way to read a verdict")
	} else {
		k.verdict, err = i.verdictOf(ctx, id)
	}
	if err != nil {
		if i.noted.change("verdict:"+id, "unreadable") {
			i.logger.Warn("Store statistics: a store's verdict could not be read; its recorded state is kept for a while, then blocked",
				"component", "storestats", "storeId", id, "error", err)
		}
		k.verdict = storeVerdict{}
	} else {
		k.verdictRead = true
		if i.noted.change("verdict:"+id, "") {
			i.logger.Info("Store statistics: a store's verdict is read again",
				"component", "storestats", "storeId", id)
		}
	}
	return k, nil
}

// conditions remembers, per process, what is wrong with each store that is
// logged when it begins and when it ends, never on every run.
type conditions struct {
	mu sync.Mutex
	m  map[string]string
}

// change records the condition of key ("" for none) and reports whether it
// differs from the one recorded: the line to write when it begins or ends.
// A condition that was never recorded and is none is no change.
func (c *conditions) change(key, now string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]string{}
	}
	was := c.m[key]
	if now == "" {
		delete(c.m, key)
	} else {
		c.m[key] = now
	}
	return was != now
}

// zoneOf loads the zone the store's days are cut in: UTC when it has none, or
// one this runtime cannot load.
func (i *Integration) zoneOf(storeID, zone string) *time.Location {
	loc := time.UTC
	bad := ""
	if zone != "" {
		var err error
		if loc, err = time.LoadLocation(zone); err != nil {
			loc, bad = time.UTC, zone
		}
	}
	if i.noted.change("zone:"+storeID, bad) {
		if bad != "" {
			i.logger.Warn("Store statistics: a store's time zone could not be loaded; its days are cut in UTC",
				"component", "storestats", "storeId", storeID, "timezone", zone)
		} else {
			i.logger.Info("Store statistics: a store's time zone loads again",
				"component", "storestats", "storeId", storeID, "timezone", zone)
		}
	}
	return loc
}

// historySince is the store-local midnight of the first recorded day: of
// health.ordersFrom when the store has one, else of the day of its oldest
// order fact that is not deleted; nil when it has neither. A day that cannot
// be read is the store's stated problem, never the beginning of time.
func historySince(ctx context.Context, tx bun.Tx, storeID, ordersFrom string, loc *time.Location) (*time.Time, error) {
	if ordersFrom != "" {
		day, err := time.Parse(time.DateOnly, ordersFrom)
		if err != nil {
			return nil, errors.New("the store's health.ordersFrom is not a day")
		}
		t := statsperiod.StartOfDay(day.Year(), day.Month(), day.Day(), loc)
		return &t, nil
	}
	var oldest sql.NullTime
	if err := tx.NewSelect().Table("store_sales_order_fact").ColumnExpr("min(ordered_at)").
		Where("store_id = ? AND NOT deleted", storeID).Scan(ctx, &oldest); err != nil {
		return nil, fmt.Errorf("read the store's oldest order: %w", err)
	}
	if !oldest.Valid {
		return nil, nil
	}
	y, m, d := oldest.Time.In(loc).Date()
	t := statsperiod.StartOfDay(y, m, d, loc)
	return &t, nil
}

// keepStoreRow writes the store's row from what was read. See the brief's
// rules in the file's comment and in the design (D4, D16). readAt is the
// last run that read the store's verdict, as the row holds it.
//
// A VERDICT THAT CANNOT BE READ changes nothing of what the row says of it
// while readAt is within verdictStaleAfter; past that, or when it was never
// read, the store is blocked with verdictUnreadableSentence, until the first
// run that reads it and finds nothing in the way. A state this build does not
// know is blocked with the same sentence at once: it was read, and cannot be
// interpreted.
func keepStoreRow(ctx context.Context, tx bun.Tx, storeID string, k keptFields, historySince *time.Time, readAt sql.NullTime, now time.Time) error {
	q := tx.NewUpdate().TableExpr("store_sales_store").
		Set("store_domain = ?", k.domain).
		Set("store_name = ?", k.name).
		Set("development = ?", k.development).
		Set("all_orders_readable = ?", k.allOrders).
		Set("refund_detail_from = ?", k.refundDetailFrom).
		Set("history_since = ?", historySince).
		Where("store_id = ?", storeID)
	if k.shop != nil {
		q = q.Set("currency = ?", k.shop.currency).
			Set("timezone = ?", k.shop.zone).
			Set("taxes_included = ?", k.shop.taxesIncluded)
	}

	type keptReason struct {
		Code     string `json:"code"`
		Sentence string `json:"sentence"`
	}
	reasons := []keptReason{}
	var verdictSince *time.Time
	state, blocked, clear := "", true, false
	switch {
	case k.verdictRead:
		for _, r := range k.verdict.Reasons {
			reasons = append(reasons, keptReason{r.Code, r.Sentence})
		}
		if k.verdict.Outside {
			reasons = append(reasons, keptReason{notRecordedCode, notRecordedSentence})
		}
		if !k.verdict.known() {
			reasons = append(reasons, keptReason{verdictUnreadableCode, verdictUnreadableSentence})
		}
		s := k.verdict.Since.UTC()
		state, verdictSince = k.verdict.State, &s
		blocked = !k.verdict.known() || k.verdict.blocked()
		clear = k.verdict.known() && k.verdict.clear()
		q = q.Set("verdict_read_at = ?", now.UTC())
	case readAt.Valid && now.Sub(readAt.Time) < verdictStaleAfter:
		// Not read this run, and read a moment ago: nothing of it changes.
		_, err := q.Exec(ctx)
		if err != nil {
			return fmt.Errorf("keep the store's row: %w", err)
		}
		return nil
	default:
		reasons = append(reasons, keptReason{verdictUnreadableCode, verdictUnreadableSentence})
	}
	reasonsJSON, err := json.Marshal(reasons)
	if err != nil {
		return fmt.Errorf("encode the verdict's reasons: %w", err)
	}
	q = q.Set("verdict_state = ?", state).
		Set("verdict_reasons = ?::jsonb", string(reasonsJSON)).
		Set("verdict_since = ?", verdictSince).
		Set("recording_blocked = ?", blocked)
	if clear {
		q = q.Set("last_clear_at = ?", now.UTC())
	}
	if _, err := q.Exec(ctx); err != nil {
		return fmt.Errorf("keep the store's row: %w", err)
	}
	return nil
}

// rebuildStore marks a store for a rebuild inside tx: it draws the store a
// new generation from the one sequence, sets purging and rebuilding_since,
// clears walked_through, sets definition, drops the wholesale pass it would
// otherwise resume (its cursor, and the width it learned) while keeping the
// store's wholesale settings, and deletes the store's watermarks and its
// check row. It deletes no fact: purgeStore does, in batches.
func rebuildStore(ctx context.Context, tx bun.Tx, storeID string, now time.Time) error {
	if _, err := tx.NewUpdate().TableExpr("store_sales_store").
		Set("generation = nextval('store_sales_generation')").
		Set("wholesale_cursor = ''").
		Set("wholesale_window_seconds = 86400").
		Set("purging = true").
		Set("rebuilding_since = ?", now.UTC()).
		Set("walked_through = NULL").
		Set("caught_up = false").
		Set("definition = ?", storeStatsDefinition).
		Where("store_id = ?", storeID).
		Exec(ctx); err != nil {
		return fmt.Errorf("mark the store for a rebuild: %w", err)
	}
	for _, table := range []string{"store_sales_watermark", "store_sales_check"} {
		if _, err := tx.NewDelete().TableExpr(table).Where("store_id = ?", storeID).Exec(ctx); err != nil {
			return fmt.Errorf("clear the store's %s: %w", table, err)
		}
	}
	return nil
}

// storePurging reads whether the store's facts are being deleted. A store
// with no row is not.
func storePurging(ctx context.Context, tx bun.Tx, storeID string) (bool, error) {
	var purging bool
	err := tx.NewSelect().Table("store_sales_store").Column("purging").
		Where("store_id = ?", storeID).Scan(ctx, &purging)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read whether store %s is purging: %w", storeID, err)
	}
	return purging, nil
}

// purgeStore deletes a purging store's facts, deleteBatch rows of each fact
// table a transaction, at most budget transactions. done is true when no fact
// is left and purging was cleared, or another node cleared it.
func purgeStore(ctx context.Context, i *Integration, storeID string, budget int) (done bool, err error) {
	if storeID == "" {
		return false, errors.New("a purge is never made for a blank store id")
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	for n := 0; n < budget && !turnSpent(ctx); n++ {
		if err := writeTx(ctx, db, storeID, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
			var err error
			done, err = purgeBatch(ctx, tx, storeID)
			return err
		}); err != nil {
			return false, err
		}
		if done {
			return true, nil
		}
	}
	return false, nil
}

// purgeBatch is one transaction of a purge. It reads purging again under the
// lock and stops when it is not set (another node finished the purge, or the
// store has no row); otherwise it deletes up to deleteBatch rows of each fact
// table, and clears purging when every table had fewer left than that.
func purgeBatch(ctx context.Context, tx bun.Tx, storeID string) (done bool, err error) {
	purging, err := storePurging(ctx, tx, storeID)
	if err != nil {
		return false, err
	}
	if !purging {
		return true, nil
	}
	last := true
	for _, t := range storeSalesTables {
		if !t.batched {
			continue
		}
		table := t.name
		res, err := tx.NewDelete().TableExpr(table).
			Where("ctid IN (SELECT ctid FROM ? WHERE store_id = ? LIMIT ?)", bun.Ident(table), storeID, deleteBatch).
			Exec(ctx)
		if err != nil {
			return false, fmt.Errorf("delete a batch of the store's %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		if n >= int64(deleteBatch) {
			last = false
		}
	}
	if !last {
		return false, nil
	}
	if _, err := tx.NewUpdate().TableExpr("store_sales_store").Set("purging = false").
		Where("store_id = ?", storeID).Exec(ctx); err != nil {
		return false, fmt.Errorf("clear the store's purging: %w", err)
	}
	return true, nil
}

// removeStore is a turn of a store whose data was removed. See "A store whose
// data was removed". A store that has no row left is left: nothing is
// written, logged or reported of it again.
func (i *Integration) removeStore(ctx context.Context, db *bun.DB, id string, now time.Time) (string, error) {
	var exists, purging bool
	if err := readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewSelect().Table("store_sales_store").ColumnExpr("true").
			Where("store_id = ?", id).Scan(ctx, &exists); err != nil {
			return err
		}
		var err error
		purging, err = storePurging(ctx, tx, id)
		return err
	}); errors.Is(err, sql.ErrNoRows) {
		return outcomeLeft, nil
	} else if err != nil {
		return "", fmt.Errorf("read the row of store %s: %w", id, err)
	}
	// A store already purging -- for a rebuild, say -- already holds a new
	// generation and refuses every writer, so it is not marked again.
	if !purging {
		if err := writeTx(ctx, db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
			return rebuildStore(ctx, tx, id, now)
		}); err != nil {
			return "", err
		}
	}
	// The line is this process's, written at its first turn of the removal,
	// whether the removal marked the store or met a rebuild's purge. Another
	// node, or a new cron leader, that takes a turn of the same removal writes
	// it once more: a log line, nothing else.
	if i.noted.change("removal:"+id, "under way") {
		i.logger.Info("Store statistics: a store's data was removed; its statistics are being deleted",
			"component", "storestats", "storeId", id)
	}
	done, err := purgeStore(ctx, i, id, purgeBudget)
	if err != nil {
		return "", err
	}
	if !done {
		return outcomePurging, nil
	}
	gone := false
	if err := writeTx(ctx, db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		var err error
		gone, err = forgetStore(ctx, tx, id)
		return err
	}); err != nil {
		return "", err
	}
	if !gone {
		return outcomePurging, nil
	}
	i.noted.change("removal:"+id, "")
	i.logger.Info("Store statistics: the statistics of a store whose data was removed are deleted",
		"component", "storestats", "storeId", id)
	return outcomeRemoved, nil
}

// forgetStore deletes what is left of a store whose facts are gone: its rows
// in every table of storeSalesTables that is not batched, its own row last.
// A fact found left behind keeps the row and sets purging again, so the next
// run purges the store first; gone is false then.
func forgetStore(ctx context.Context, tx bun.Tx, storeID string) (gone bool, err error) {
	for _, t := range storeSalesTables {
		if !t.batched {
			continue
		}
		var left bool
		err := tx.NewSelect().TableExpr(t.name).ColumnExpr("true").Where("store_id = ?", storeID).Limit(1).Scan(ctx, &left)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read what is left of the store's %s: %w", t.name, err)
		}
		if _, err := tx.NewUpdate().TableExpr("store_sales_store").Set("purging = true").
			Where("store_id = ?", storeID).Exec(ctx); err != nil {
			return false, fmt.Errorf("set the store purging again: %w", err)
		}
		return false, nil
	}
	for _, t := range storeSalesTables {
		if t.batched {
			continue
		}
		if _, err := tx.NewDelete().TableExpr(t.name).Where("store_id = ?", storeID).Exec(ctx); err != nil {
			return false, fmt.Errorf("delete the store's %s: %w", t.name, err)
		}
	}
	return true, nil
}

// clearRebuilding ends a rebuild only after a complete turn reached every
// source's clock. The fact survives the spacing between turns and checks.
func clearRebuilding(ctx context.Context, tx bun.Tx, storeID string, now time.Time) (ended bool, err error) {
	res, err := tx.NewUpdate().TableExpr("store_sales_store").
		Set("rebuilding_since = NULL").
		Where("store_id = ? AND rebuilding_since IS NOT NULL AND NOT purging AND caught_up", storeID).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("end the store's rebuild: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
