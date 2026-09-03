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

// readableSites narrows the caller's ids to the ones the engine hands back
// under their own actor.
//
// ONE READ FOR THE COMMON CASE. `sitesAll` answers with every deployable this
// caller may read -- their own, or all of them for a cluster owner -- so a
// list asking about twenty of them costs one query rather than twenty. An id
// that read does not carry is asked for individually through `siteById`, which
// is what covers an ARCHIVED deployable: `sitesAll` excludes those by design,
// and an archived deployable's traffic is exactly what somebody deciding
// whether to restore it wants to see.
func (r *Reader) readableSites(ctx context.Context, ids []string) ([]string, error) {
	want := make(map[string]struct{}, len(ids))
	ordered := make([]string, 0, len(ids))
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
	if len(ordered) == 0 {
		return nil, nil
	}

	allowed := make(map[string]struct{}, len(ordered))
	res, err := r.engine.Execute(ctx, "query sitesAll()")
	if err != nil {
		return nil, fmt.Errorf("read the deployables you may see: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		id, _ := row["id"].(string)
		id = memql.BareShortId(strings.TrimSpace(id))
		if _, wanted := want[id]; wanted {
			allowed[id] = struct{}{}
		}
	}

	lookups := 0
	for _, id := range ordered {
		if _, ok := allowed[id]; ok {
			continue
		}
		if lookups >= maxIndividualLookups {
			// See the constant. The remaining unknown ids are treated as
			// unreadable, which is the same answer they get for being
			// unreadable -- no rows, indistinguishable from a deployable
			// with no traffic.
			break
		}
		lookups++
		// Not in the caller's active list. It may still be one of theirs and
		// archived, which sitesAll excludes, so ask directly. A miss here is
		// a deployable this caller may not read, and it is dropped.
		one, err := r.engine.Execute(ctx, fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(id)))
		if err != nil {
			return nil, fmt.Errorf("read deployable %q: %w", id, err)
		}
		if len(memql.MaterializeRows(one)) > 0 {
			allowed[id] = struct{}{}
		}
	}

	out := make([]string, 0, len(allowed))
	for id := range allowed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}
