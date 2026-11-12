package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// statistics.go -- builtin storeStatistics: one store's sales figures, for a
// holder of the sales part, asked through a storefront site that serves the
// store.
//
// # Two locks on one door
//
// The construct declares @requiresCapability("execute",
// "app:deployables/sales"), and the engine judges it in the organization of
// the site `siteId` names (organizationCapabilityTarget). The handler does not
// rely on it: a construct of another source can be bound to this executor
// without the declaration, and a construct of the same name in another domain
// can leave the engine's gate unresolved, and either would reach this handler
// with no gate at all (statistics_gate_db_test.go mounts both). So
// handleStatistics holds the
// whole gate again, in Go, before anything of a store is read:
//
//  1. the period parses (statsperiod.Parse's own sentence refuses it);
//  2. the site is read through `query siteById` under the CALLER's own
//     context -- the context this handler was handed, never walkContext --
//     so the site's own row authorization decides whether this caller may
//     see it;
//  3. the caller holds the part for the organization that row names, asked
//     of the engine's RequirementHeldFor, the one judgement the engine's own
//     gate makes once it knows the construct's requirement and the target's
//     account; the handler asks it and nothing of its own;
//  4. it is a shopify_storefront and not systemOwned;
//  5. the store asked for is its bound or its preview store, compared bare.
//
// Before all of them, a call made under borrowed authority is refused: the
// shopper surface runs a declared read as the site owner's borrowed actor
// (auth.ContextWithUserActor, which marks the actor RoleStandIn), and revenue
// is never answered to a storefront's visitor in its owner's name. A page's
// call always carries the person's own session.
//
// Any of 2 to 5 failing answers one sentence, errNotServed: "no such site",
// "not yours", "you do not hold the part" and "not that store" are one
// answer, so a refusal says nothing about what exists.
//
// # Where the caller's authority ends
//
// Steps 1 to 5 run on the caller's context and read nothing but the site.
// Only after them is store_sales_* read, in one readTx on the component's own
// handle, by the bare id of a store step 5 proved is the site's. That read
// runs under the component's authority: a holder of the part cannot read a
// store row, an order or a fact. The only thing a caller names that reaches
// it is the store id, and the store id is accepted only when it equals the
// bound or preview store of a site the caller could read and holds the part
// for; no argument widens it.
//
// The read never takes internal origin: a read with internal origin would
// pass @requiresCapability and the site's row authorization for whoever
// asked. Nothing in this file or statistics_payload.go calls walkContext.

// statisticsConcept is the concept of the one node the read answers with.
// Not a DSL concept: the answer rides a synthetic node, as the email and
// website statistics answers do.
const statisticsConcept = "integration:storeStats:statistics"

// salesPart is the part the read requires.
const salesPart = "app:deployables/sales"

// errNotServed is the read's one refusal.
var errNotServed = errors.New("This deployable does not serve that store for you.")

// The states the frame answers of its own, beside the verdict's words: the
// store's facts are being deleted, or walked again from the start.
const (
	statePurging    = "purging"
	stateRebuilding = "rebuilding"
)

// The destinations, as the answer spells them.
const (
	destinationProduction = "production"
	destinationTesting    = "testing"
	destinationBoth       = "both"
)

// requirementJudge is what the handler asks the engine of the caller: the
// engine's own judgement, never a rule of this package's. *memql.MemQLEngine
// is one. An engine that is not one admits nobody.
type requirementJudge interface {
	RequirementHeldFor(ctx context.Context, account string, required memql.CapabilityRequirement) bool
}

// salesRequirement is the construct's own declaration, as the engine holds it.
var salesRequirement = memql.CapabilityRequirement{Verb: auth.VerbExecute, Resource: salesPart}

type statisticsRequest struct {
	SiteID, StoreID, Destination string // StoreID bare; Destination one of production, testing, both
	Period                       statsperiod.Period
}

// handleStatistics makes the gate's checks (the file's comment), builds the
// request and answers one node with id `statistics`.
func (i *Integration) handleStatistics(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if borrowedAuthority(ctx) {
		return nil, errNotServed
	}
	period, err := statsperiod.Parse(argText(args, "period"))
	if err != nil {
		return nil, err
	}
	req, ok := i.servedRequest(ctx, argText(args, "siteId"), argText(args, "storeId"))
	if !ok {
		return nil, errNotServed
	}
	req.Period = period

	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	now := time.Now()
	if i.now != nil {
		now = i.now()
	}
	stats, err := readStatistics(ctx, db, req, now)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(stats.Payload())
	if err != nil {
		return nil, fmt.Errorf("store statistics: encode the answer: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "statistics",
		Concept:   statisticsConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: now.UTC(),
		Payload:   payload,
	}}, nil
}

// servedRequest is steps 2 to 5 of the gate. ok is false for every refusal,
// whatever its cause; the request carries the bare ids and the destination.
func (i *Integration) servedRequest(ctx context.Context, siteID, storeID string) (statisticsRequest, bool) {
	siteID, storeID = memql.BareShortId(siteID), memql.BareShortId(storeID)
	if siteID == "" || storeID == "" || i.engine == nil {
		return statisticsRequest{}, false
	}
	// 2. The site, under the caller's own context.
	res, err := i.engine.Execute(ctx, "query siteById(siteId: "+langparser.QuoteString(siteID)+")")
	if err != nil {
		return statisticsRequest{}, false
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		return statisticsRequest{}, false
	}
	site := rowFields(rows[0])
	// 3. The part, in the site's own organization.
	if !i.holdsTheSalesPart(ctx, rowText(site, "accountId")) {
		return statisticsRequest{}, false
	}
	// 4. A storefront, and not the platform's own site.
	if rowText(site, "kind") != "shopify_storefront" {
		return statisticsRequest{}, false
	}
	if systemOwned, _ := site["systemOwned"].(bool); systemOwned {
		return statisticsRequest{}, false
	}
	// 5. The site's own store.
	bound := memql.BareShortId(bindingStore(site, "binding")) == storeID
	preview := memql.BareShortId(bindingStore(site, "previewBinding")) == storeID
	req := statisticsRequest{SiteID: siteID, StoreID: storeID}
	switch {
	case bound && preview:
		req.Destination = destinationBoth
	case bound:
		req.Destination = destinationProduction
	case preview:
		req.Destination = destinationTesting
	default:
		return statisticsRequest{}, false
	}
	return req, true
}

// holdsTheSalesPart asks the engine whether the caller holds the sales part
// for a site of this organization: RequirementHeldFor, the judgement the
// engine's own gate makes for a target-bound construct
// (refuseOrganizationActionCapability), so a question added to that gate is
// asked here too.
func (i *Integration) holdsTheSalesPart(ctx context.Context, account string) bool {
	judge, ok := i.engine.(requirementJudge)
	return ok && judge.RequirementHeldFor(ctx, account, salesRequirement)
}

// borrowedAuthority reports whether the call carries a stand-in actor rather
// than the person's own session: the shopper surface's borrowed authority,
// and work restored for its owner, both marked RoleStandIn by component/auth.
func borrowedAuthority(ctx context.Context) bool {
	ac, ok := auth.AccessFromContext(ctx)
	return ok && ac != nil && ac.RoleStandIn
}

// readStatistics is the read under the component's own authority, in one
// readTx: the frame, and from Task 18 the figures after it.
func readStatistics(ctx context.Context, db *bun.DB, req statisticsRequest, now time.Time) (Statistics, error) {
	var stats Statistics
	err := readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		stats, err = readFrame(ctx, tx, req, now)
		// The seam: the figures are read here, after the frame and in the
		// same snapshot (readFigures, the next task).
		return err
	})
	if err != nil {
		return Statistics{}, err
	}
	return stats, nil
}

// frameRow is the part of a store's store_sales_store row the frame reads,
// scanned by column name from a query on the table itself (no bun model: the
// table is a side table of this package, never the row table).
type frameRow struct {
	StoreDomain       string          `bun:"store_domain"`
	StoreName         string          `bun:"store_name"`
	Currency          string          `bun:"currency"`
	Timezone          string          `bun:"timezone"`
	Purging           bool            `bun:"purging"`
	TaxesIncluded     sql.NullBool    `bun:"taxes_included"`
	Development       sql.NullBool    `bun:"development"`
	AllOrdersReadable bool            `bun:"all_orders_readable"`
	HistorySince      sql.NullTime    `bun:"history_since"`
	VerdictState      string          `bun:"verdict_state"`
	VerdictReasons    json.RawMessage `bun:"verdict_reasons,type:jsonb"`
	VerdictSince      sql.NullTime    `bun:"verdict_since"`
	RecordingBlocked  bool            `bun:"recording_blocked"`
	LastClearAt       sql.NullTime    `bun:"last_clear_at"`
	LastChangeAt      sql.NullTime    `bun:"last_change_at"`
	RebuildingSince   sql.NullTime    `bun:"rebuilding_since"`
}

// readFrame reads the store's store_sales_store row and cuts the period in
// its zone. A store the walk holds no row for is answered as starting: its
// ids, its destination, and every other key null. Figures are left null for
// Task 18.
func readFrame(ctx context.Context, tx bun.Tx, req statisticsRequest, now time.Time) (Statistics, error) {
	stats := Statistics{
		StoreID: req.StoreID, SiteID: req.SiteID, Destination: req.Destination, Period: req.Period, Loc: time.UTC,
		Floors: map[string]bool{}, Series: []Bucket{}, ProductsBySales: []Product{}, ProductsByUnits: []Product{},
		OtherCurrencies: []CurrencyCount{}, Recording: Recording{Reasons: []Reason{}},
	}
	var row frameRow
	err := tx.NewSelect().Table("store_sales_store").
		Column("store_domain", "store_name", "currency", "timezone", "taxes_included", "development",
			"all_orders_readable", "purging", "history_since", "verdict_state", "verdict_reasons", "verdict_since",
			"recording_blocked", "last_clear_at", "last_change_at", "rebuilding_since").
		Where("store_id = ?", req.StoreID).Limit(1).Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		stats.Recording.State = "starting"
		return stats, nil
	}
	if err != nil {
		return Statistics{}, fmt.Errorf("read the statistics of store %s: %w", req.StoreID, err)
	}

	stats.StoreName, stats.StoreDomain, stats.Currency = nonEmpty(row.StoreName), nonEmpty(row.StoreDomain), nonEmpty(row.Currency)
	stats.Timezone = nonEmpty(row.Timezone)
	stats.PeriodZone = "UTC"
	if loc := storeZone(row.Timezone); loc != nil {
		stats.Loc = loc
		stats.PeriodZone = strings.TrimSpace(row.Timezone)
	}
	stats.TaxesIncluded, stats.Development = nullBool(row.TaxesIncluded), nullBool(row.Development)
	older := !row.AllOrdersReadable
	stats.OlderOrderChangesNotRecorded = &older

	bounds := statsperiod.ForIn(req.Period, now, stats.Loc)
	stats.Bounds = &bounds
	for _, at := range bounds.Starts() {
		stats.Series = append(stats.Series, Bucket{At: at})
	}

	stats.HistorySince, stats.LastChangeAt = nullTime(row.HistorySince), nullTime(row.LastChangeAt)
	rebuilding := row.RebuildingSince.Valid
	stats.Rebuilding = &rebuilding

	stats.Recording.State = row.VerdictState
	stats.Recording.Since = nullTime(row.VerdictSince)
	if reasons, err := storedReasons(row.VerdictReasons); err == nil {
		stats.Recording.Reasons = reasons
	} else {
		return Statistics{}, fmt.Errorf("read the recording reasons of store %s: %w", req.StoreID, err)
	}
	// Blocked is what the walk decided and kept, and the two states the
	// mirror gives no reason for. The read re-derives no classification.
	stats.Recording.Blocked = row.RecordingBlocked || row.VerdictState == "paused" || row.VerdictState == "not connected"
	if stats.Recording.Blocked {
		stats.AsOf = nullTime(row.LastClearAt)
	}
	// A store whose facts are being deleted, or walked again from the start,
	// has no figure to give, current or as of any moment, whatever its
	// verdict: the state says which, the verdict's reasons stay listed. A
	// purge comes first, because a rebuild sets both and deletes the facts
	// before the store is walked again.
	switch {
	case row.Purging:
		stats.Recording.State, stats.Recording.Blocked, stats.AsOf = statePurging, true, nil
	case row.RebuildingSince.Valid:
		stats.Recording.State, stats.Recording.Blocked, stats.AsOf = stateRebuilding, true, nil
	}
	return stats, nil
}

// storeZone loads a store's zone, or answers nil when it has none or one the
// Go runtime cannot load: the period is then cut in UTC, and the answer's
// periodZone says so. "Local" is the machine's zone, never a store's.
func storeZone(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}

// storedReasons reads verdict_reasons, keeping each reason's code and
// sentence and nothing else.
func storedReasons(raw json.RawMessage) ([]Reason, error) {
	out := []Reason{}
	if len(raw) == 0 {
		return out, nil
	}
	var stored []Reason
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, errors.New("verdict_reasons is not a list of reasons")
	}
	return append(out, stored...), nil
}

func nonEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func nullBool(b sql.NullBool) *bool {
	if !b.Valid {
		return nil
	}
	v := b.Bool
	return &v
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// rowFields is a row's own fields: the payload a materialized row carries,
// or the row itself when it is already flat.
func rowFields(row map[string]any) map[string]any {
	if p, ok := row["payload"].(map[string]any); ok {
		return p
	}
	return row
}

func rowText(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return strings.TrimSpace(s)
}

func bindingStore(site map[string]any, key string) string {
	b, _ := site[key].(map[string]any)
	return rowText(b, "storeId")
}

func argText(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}
