package sitetraffic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// The read half: a window of traffic for deployables the caller may read.
//
// # Authorization is the SITE's, and it is asked of the engine
//
// edge_request is a dedicated table, so it passes through neither the parser
// nor the row-authz filter path -- nothing the engine injects reaches it, and
// a hand-rolled read that gated nothing would hand one person's traffic to
// another. The gate is therefore explicit and it is the site's own: a caller
// may read a deployable's traffic exactly when they may read the deployable,
// which is `sitesAll` / `siteById` under THEIR actor and the composite
// owner-or-cluster-owner tier those carry. No second authorization model, and
// no rule of this package's own that could drift from the concept's.
//
// # An id the caller may not read is DROPPED, not refused
//
// A refusal naming the id would answer "does this deployable exist" for
// anybody who can spell an id. Dropping it gives the same answer as a
// deployable with no traffic -- no rows -- which is the honest one: as far as
// this caller is concerned there is nothing there.
//
// # An absent figure and a zero are different answers
//
// A window with no rows returns NO ROW for that site. The reader never
// synthesizes a zero-count row for a site it found nothing for: "nobody
// visited" and "we were not recording" send a person to two different places,
// and the client renders the absence as `unmeasured` in words.

// Bucket names the aggregate a read comes from.
const (
	Bucket1m = "1m"
	Bucket1h = "1h"
)

// maxSitesPerRead bounds one call. The list row asks for every deployable on
// screen, so the cap is generous; past it the call is REFUSED rather than
// truncated, because a silently short answer reads as "those deployables have
// no traffic".
const maxSitesPerRead = 200

// maxIndividualLookups bounds the per-id fallback below.
//
// `sitesAll` answers the common case in ONE read, and the fallback exists for
// exactly one situation: an ARCHIVED deployable, which that read excludes by
// design and whose traffic is what somebody deciding whether to restore it
// wants to see. That is a DETAIL surface asking about one id.
//
// Unbounded, it is also a way to make one call cost two hundred engine reads
// by naming two hundred ids nobody can read -- work a caller gets for free
// and the cluster pays for. Past this many unknown ids the rest are treated
// as unreadable, which is the answer they were overwhelmingly going to get
// anyway: a LIST never needs the fallback, because everything it shows is
// active and `sitesAll` already covered it.
const maxIndividualLookups = 16

// Reading is one answer: a bucket of one deployable's traffic, or -- in
// summary mode -- one deployable's whole window folded into a single row.
//
// windowEnd - windowStart says which: a bucket's span is the bucket size, and
// a summary's span is the window the caller asked for. That is the codeMetric
// convention, where a consumer reads the bucket size off the difference
// rather than off a second field.
type Reading struct {
	SiteId           string
	Bucket           string
	WindowStart      time.Time
	WindowEnd        time.Time
	RequestCount     int64
	ErrorCount       int64
	ClientErrorCount int64
	BytesTotal       int64
	LastServedAt     time.Time
}

// Query is one read.
type Query struct {
	// SiteIds are BARE site ids -- the client contract at every wire seam,
	// and what the edge stamped on the rows.
	SiteIds []string
	// Bucket is Bucket1m or Bucket1h.
	Bucket string
	// WindowStart is inclusive, WindowEnd exclusive. The caller aligns them
	// to the bucket; a half-open window is what makes two adjacent windows
	// add up rather than double-count their shared edge.
	WindowStart time.Time
	WindowEnd   time.Time
	// Summary folds the whole window into one row per deployable instead of
	// one row per bucket -- what a list row needs to say "last served" for
	// twenty deployables without pulling twenty series.
	Summary bool
}

// Engine is the narrow engine surface the reader needs: one method, the same
// seam every other Go component in this tree uses to ask a named query.
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// Reader answers a Query, gated by what the caller may read.
type Reader struct {
	db     *bun.DB
	engine Engine
}

// NewReader wires the two handles. Either may be nil, and a read then refuses
// with a sentence naming what is missing rather than panicking -- a node
// without a database is a real deployment, not a bug.
func NewReader(db *bun.DB, engine Engine) *Reader {
	return &Reader{db: db, engine: engine}
}

// Read answers the query for the sites the caller may read.
func (r *Reader) Read(ctx context.Context, q Query) ([]Reading, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("this node has no database handle, so it cannot read traffic")
	}
	if r.engine == nil {
		return nil, fmt.Errorf("this node has no engine handle, so it cannot check which deployables you may read")
	}

	allowed, err := r.readableSites(ctx, q.SiteIds)
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		// Every id was unreadable, or none was asked for. No rows: the same
		// answer a deployable with no traffic gives, which is what keeps this
		// from being an existence oracle.
		return nil, nil
	}

	if q.Summary {
		return r.readSummary(ctx, q, allowed)
	}
	return r.readBuckets(ctx, q, allowed)
}

func (q Query) validate() error {
	if len(q.SiteIds) == 0 {
		return fmt.Errorf("siteIds is required -- a traffic read is always about named deployables, never about every one of them")
	}
	if len(q.SiteIds) > maxSitesPerRead {
		return fmt.Errorf("a traffic read covers at most %d deployables at once; %d were asked for. Ask in pages rather than reading a short answer as an empty one", maxSitesPerRead, len(q.SiteIds))
	}
	if q.Bucket != Bucket1m && q.Bucket != Bucket1h {
		return fmt.Errorf("bucket must be %q or %q; got %q", Bucket1m, Bucket1h, q.Bucket)
	}
	if q.WindowStart.IsZero() || q.WindowEnd.IsZero() {
		return fmt.Errorf("windowStart and windowEnd are both required")
	}
	if !q.WindowEnd.After(q.WindowStart) {
		return fmt.Errorf("windowEnd must be after windowStart")
	}
	return nil
}

// aggregateFor names the relation a bucket reads from. The two names exist
// whether or not TimescaleDB is installed -- the migration creates a
// continuous aggregate with it and an ordinary view without -- so there is one
// query here rather than a branch.
func aggregateFor(bucket string) string {
	if bucket == Bucket1m {
		return "edge_request_1m"
	}
	return "edge_request_1h"
}

// bucketSpan is how long one bucket covers, used to fill windowEnd on a row.
func bucketSpan(bucket string) time.Duration {
	if bucket == Bucket1m {
		return time.Minute
	}
	return time.Hour
}

// aggregateRow is one row as the two relations shape it.
type aggregateRow struct {
	SiteId           string    `bun:"site_id"`
	WindowStart      time.Time `bun:"window_start"`
	RequestCount     int64     `bun:"request_count"`
	ErrorCount       int64     `bun:"error_count"`
	ClientErrorCount int64     `bun:"client_error_count"`
	BytesTotal       int64     `bun:"bytes_total"`
	LastServedAt     time.Time `bun:"last_served_at"`
}

func (r *Reader) readBuckets(ctx context.Context, q Query, allowed []string) ([]Reading, error) {
	var rows []aggregateRow
	err := r.db.NewSelect().
		ColumnExpr("site_id, window_start, request_count, error_count, client_error_count, bytes_total, last_served_at").
		TableExpr(aggregateFor(q.Bucket)).
		Where("site_id IN (?)", bun.In(allowed)).
		Where("window_start >= ?", q.WindowStart.UTC()).
		Where("window_start < ?", q.WindowEnd.UTC()).
		OrderExpr("site_id ASC, window_start ASC").
		Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", aggregateFor(q.Bucket), err)
	}

	span := bucketSpan(q.Bucket)
	out := make([]Reading, 0, len(rows))
	for _, row := range rows {
		out = append(out, Reading{
			SiteId:           row.SiteId,
			Bucket:           q.Bucket,
			WindowStart:      row.WindowStart.UTC(),
			WindowEnd:        row.WindowStart.UTC().Add(span),
			RequestCount:     row.RequestCount,
			ErrorCount:       row.ErrorCount,
			ClientErrorCount: row.ClientErrorCount,
			BytesTotal:       row.BytesTotal,
			LastServedAt:     row.LastServedAt.UTC(),
		})
	}
	return out, nil
}

// readSummary folds the window into one row per deployable.
//
// Summed IN THE DATABASE rather than in Go over the bucket rows, and that is
// the point of the mode: a list showing twenty deployables over a week would
// otherwise pull twenty series of a hundred and sixty-eight buckets to render
// twenty timestamps.
func (r *Reader) readSummary(ctx context.Context, q Query, allowed []string) ([]Reading, error) {
	var rows []aggregateRow
	err := r.db.NewSelect().
		ColumnExpr("site_id").
		ColumnExpr("MIN(window_start) AS window_start").
		ColumnExpr("SUM(request_count) AS request_count").
		ColumnExpr("SUM(error_count) AS error_count").
		ColumnExpr("SUM(client_error_count) AS client_error_count").
		ColumnExpr("SUM(bytes_total) AS bytes_total").
		ColumnExpr("MAX(last_served_at) AS last_served_at").
		TableExpr(aggregateFor(q.Bucket)).
		Where("site_id IN (?)", bun.In(allowed)).
		Where("window_start >= ?", q.WindowStart.UTC()).
		Where("window_start < ?", q.WindowEnd.UTC()).
		GroupExpr("site_id").
		OrderExpr("site_id ASC").
		Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("summarize %s: %w", aggregateFor(q.Bucket), err)
	}

	out := make([]Reading, 0, len(rows))
	for _, row := range rows {
		out = append(out, Reading{
			SiteId: row.SiteId,
			Bucket: q.Bucket,
			// THE WINDOW THE CALLER ASKED FOR, not the first bucket that
			// happened to carry a row. A summary whose start moved with the
			// data would make two deployables' summaries incomparable, and
			// the span is how a reader tells a summary from a bucket.
			WindowStart:      q.WindowStart.UTC(),
			WindowEnd:        q.WindowEnd.UTC(),
			RequestCount:     row.RequestCount,
			ErrorCount:       row.ErrorCount,
			ClientErrorCount: row.ClientErrorCount,
			BytesTotal:       row.BytesTotal,
			LastServedAt:     row.LastServedAt.UTC(),
		})
	}
	return out, nil
}

// readableSite is what the narrowing reads off a site row: its bare id, and the
// two facts the statistics read filters on.
type readableSite struct {
	id string
	// accountId is the organization the site belongs to, bare; empty when the
	// row names none (a system-owned site, or one older than organizations).
	accountId   string
	systemOwned bool
}

// readableSiteOf reads a sitesAll / siteById row. The engine answers an
// in-process caller with CANONICAL ids (`v1:platform:site:<id>`,
// `v1:accounts:account:<id>`), while the edge stamps and a client sends bare
// ones, so both are bared before anything compares them.
func readableSiteOf(row map[string]any) readableSite {
	id, _ := row["id"].(string)
	account, _ := row["accountId"].(string)
	owned, _ := row["systemOwned"].(bool)
	return readableSite{
		id:          memql.BareShortId(strings.TrimSpace(id)),
		accountId:   memql.BareShortId(strings.TrimSpace(account)),
		systemOwned: owned,
	}
}

// wantedSiteIds bares, trims and de-duplicates the caller's ids, in order. A
// blank id names no deployable and is dropped; whether a list was NAMED at all
// is the caller's question, answered before this (statisticsSites).
func wantedSiteIds(ids []string) (ordered []string, want map[string]struct{}) {
	want = make(map[string]struct{}, len(ids))
	ordered = make([]string, 0, len(ids))
	for _, id := range ids {
		id = memql.BareShortId(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if _, seen := want[id]; seen {
			continue
		}
		want[id] = struct{}{}
		ordered = append(ordered, id)
	}
	return ordered, want
}

// lookupSites asks `siteById` under the caller for each id, for the ids a
// list read did not carry, and answers the ones it found.
//
// This is what covers an ARCHIVED deployable: `sitesAll` excludes those by
// design, and an archived deployable's traffic is exactly what somebody
// deciding whether to restore it wants to see. A miss is a deployable this
// caller may not read, and it is dropped. At most maxIndividualLookups ids are
// asked; see that constant for why the rest are treated as unreadable.
func (r *Reader) lookupSites(ctx context.Context, ids []string) (map[string]readableSite, error) {
	found := make(map[string]readableSite)
	for i, id := range ids {
		if i >= maxIndividualLookups {
			// The remaining unknown ids are treated as unreadable, which is
			// the same answer they get for being unreadable -- no rows,
			// indistinguishable from a deployable with no traffic.
			break
		}
		one, err := r.engine.Execute(ctx, fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(id)))
		if err != nil {
			return nil, fmt.Errorf("read deployable %q: %w", id, err)
		}
		if rows := memql.MaterializeRows(one); len(rows) > 0 {
			site := readableSiteOf(rows[0])
			// Keyed by the id that was ASKED for, which siteById matched;
			// the row's own id is the same one, spelled canonically.
			site.id = id
			found[id] = site
		}
	}
	return found, nil
}

// readableSites narrows the caller's ids to the ones the engine hands back
// under their own actor. The traffic read's narrowing (Read).
//
// ONE READ FOR THE COMMON CASE. `sitesAll` answers with every deployable this
// caller may read -- their own, or all of them for a cluster owner -- so a
// list asking about twenty of them costs one query rather than twenty. An id
// that read does not carry is asked for individually (lookupSites), which is
// what covers an archived deployable.
//
// ONE PAGE OF `sitesAll`, the engine's per-query row cap: an id past it falls
// to the bounded lookups, so on a cluster where the caller reads more
// deployables than one page holds, a long list of ids can come back short.
// The statistics read does not use this; it walks every page
// (readableNamedSites).
func (r *Reader) readableSites(ctx context.Context, ids []string) ([]string, error) {
	ordered, want := wantedSiteIds(ids)
	if len(ordered) == 0 {
		return nil, nil
	}

	allowed := make(map[string]struct{}, len(ordered))
	res, err := r.engine.Execute(ctx, "query sitesAll()")
	if err != nil {
		return nil, fmt.Errorf("read the deployables you may see: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if site := readableSiteOf(row); site.id != "" {
			if _, wanted := want[site.id]; wanted {
				allowed[site.id] = struct{}{}
			}
		}
	}
	var missing []string
	for _, id := range ordered {
		if _, ok := allowed[id]; !ok {
			missing = append(missing, id)
		}
	}
	found, err := r.lookupSites(ctx, missing)
	if err != nil {
		return nil, err
	}
	for id := range found {
		allowed[id] = struct{}{}
	}

	out := make([]string, 0, len(allowed))
	for id := range allowed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// readableNamedSites is the statistics read's narrowing of named ids: the ids
// the caller may read, as rows, ordered by id, so a system-owned one can be
// dropped without a second read.
//
// EVERY PAGE OF `sitesAll` (walkReadableSites), the walk a read naming no
// deployables makes, and not readableSites' one page: a total over named
// deployables must not silently leave out the ones that sort past the
// engine's per-query cap. Only the ids the walk did not return -- an archived
// deployable's, or one the caller may not read -- fall to the bounded lookups.
func (r *Reader) readableNamedSites(ctx context.Context, ids []string) ([]readableSite, error) {
	ordered, want := wantedSiteIds(ids)
	if len(ordered) == 0 {
		return nil, nil
	}
	listed, err := r.walkReadableSites(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]readableSite, len(ordered))
	for _, site := range listed {
		if _, wanted := want[site.id]; wanted {
			allowed[site.id] = site
		}
	}
	var missing []string
	for _, id := range ordered {
		if _, ok := allowed[id]; !ok {
			missing = append(missing, id)
		}
	}
	found, err := r.lookupSites(ctx, missing)
	if err != nil {
		return nil, err
	}
	for id, site := range found {
		allowed[id] = site
	}

	out := make([]readableSite, 0, len(allowed))
	for _, site := range allowed {
		out = append(out, site)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// sitesPageSize is how many deployables one page of the every-deployable walk
// reads: the engine's own per-query row cap (MEMQL_MEMORY_ENGINE_MAX_RESULTS,
// 500 by default), so a cluster with fewer deployables than that answers in
// one read. A variable so a test can walk a small list across pages.
var sitesPageSize = 500

// maxSitePages bounds the walk: forty pages of 500 is 20,000 deployables. Past
// it the read is REFUSED rather than answered short, maxSitesPerRead's rule --
// a total that silently left deployables out would read as theirs having no
// traffic. The bound is far past any cluster this serves; it exists so a list
// that never ends cannot hold a read open.
const maxSitePages = 40

// walkReadableSites reads every page of `sitesAll` under the caller's own
// actor and answers each deployable on it once, in the list's order.
//
// `sitesAll` UNDER THE CALLER'S OWN ACTOR, and nothing else, for the reason
// readableSites gives: the site's own tier decides who reaches what, so a
// client's member reaches their organization's deployables and staff reach
// every organization's, and the statistics read has no rule of its own that
// could disagree with the list the Deployables app shows. Archived deployables
// are out, as they are from that list. EVERY PAGE OF IT, because staff on a
// large cluster may read more deployables than one page holds, and a total
// over the first page alone would be short without saying so.
//
// A list the walk cannot finish is REFUSED, never read short: a page that
// says there is more with no cursor to reach it, a cursor that does not
// advance, or more pages than maxSitePages. Both statistics paths walk this
// same list, so naming the deployables is no way round the bound.
func (r *Reader) walkReadableSites(ctx context.Context) ([]readableSite, error) {
	query := fmt.Sprintf("paginate(query sitesAll(), %d)", sitesPageSize)
	seen := map[string]struct{}{}
	var out []readableSite
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxSitePages {
			return nil, fmt.Errorf("you may read more than %d deployables, more than a statistics read covers; the read is refused rather than answered short", maxSitePages*sitesPageSize)
		}
		res, err := r.engine.Execute(memql.ContextWithCursor(ctx, cursor), query)
		if err != nil {
			return nil, fmt.Errorf("read the deployables you may see: %w", err)
		}
		for _, row := range memql.MaterializeRows(res) {
			site := readableSiteOf(row)
			if site.id == "" {
				continue
			}
			if _, dup := seen[site.id]; dup {
				continue
			}
			seen[site.id] = struct{}{}
			out = append(out, site)
		}
		next, more := "", false
		if res != nil && res.Meta != nil {
			next, more = strings.TrimSpace(res.Meta.Cursor), res.Meta.HasMore
		}
		if next == "" {
			if more {
				// A full page the engine could not continue. Refused rather
				// than read as the end of the list (pageActiveBundles' rule in
				// component/memql).
				return nil, fmt.Errorf("the list of deployables you may read has more rows and no cursor to reach them")
			}
			return out, nil
		}
		if next == cursor {
			return nil, fmt.Errorf("the list of deployables you may read did not advance past one page")
		}
		cursor = next
	}
}

// readableSitesForAccounts answers the deployables a read that names none of
// them covers: every one the caller may read (walkReadableSites), the
// cluster's own system-owned surfaces left out, kept to the named
// organizations when the caller named any.
//
// THE ORGANIZATIONS ONLY EVER NARROW. They filter the rows the caller was
// already given; an organization the caller does not belong to matches none
// of them, which is the empty answer an organization with no deployables
// gives, so naming one confirms nothing. WHETHER ANY WERE NAMED is the raw
// list's length, not the usable ids': a list holding only blank ids names no
// organization, so it keeps no deployable, and never turns the filter off. An
// organization whose id is blank is not a match for a deployable that names
// none either. System-owned sites are dropped whatever is named (D16): they
// are never recorded, and the console somebody reads a figure in is not one
// of their websites.
func (r *Reader) readableSitesForAccounts(ctx context.Context, accountIds []string) ([]string, error) {
	named := len(accountIds) > 0
	keep := make(map[string]struct{}, len(accountIds))
	for _, id := range accountIds {
		if bare := memql.BareShortId(strings.TrimSpace(id)); bare != "" {
			keep[bare] = struct{}{}
		}
	}
	listed, err := r.walkReadableSites(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, site := range listed {
		if site.systemOwned {
			continue
		}
		if named {
			if _, kept := keep[site.accountId]; !kept {
				continue
			}
		}
		out = append(out, site.id)
	}
	sort.Strings(out)
	return out, nil
}
