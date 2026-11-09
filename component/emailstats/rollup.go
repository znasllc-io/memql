package emailstats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// rollup.go -- the walk that fills email_stat_fact (the 2026-11-08 statistics
// design, section 5 and D10). builtin emailStatisticsRollup runs it every five
// minutes on the cron leader, under its own maintenance principal
// (plugin.go, handleRollup).
//
// # Each source, from its watermark
//
// Three sources -- deliveries, engagement events, consent events -- each walked
// by its own @serverOnly query, in row.createdAt order, from that source's
// watermark less five minutes (watermarkOverlap), or from four hundred days
// back when it has none. The engine answers each row at its latest version, so
// a delivery whose outcome changed since the last run is read again and adds
// what it newly says (facts.go). Each page's facts are written in one
// transaction with the watermark moved to the page's newest row, so the
// watermark never passes a fact that was not written, and a run that dies
// part way -- a replica stopping, the cron lead moving to another node --
// leaves the next run to carry on from the last page that landed.
//
// # Why the overlap
//
// row.createdAt is stamped by the writing node's clock, and a row commits
// after it is stamped. A row written now by a replica whose clock is a minute
// behind, or by a transaction that took a while to commit, carries a time
// before a watermark another run has already moved past. Reading five minutes
// behind the watermark picks such rows up; reading them twice costs nothing,
// because every fact is keyed and every write is ON CONFLICT DO NOTHING.
// Further behind than five minutes is missed, which is the bound
// (TestTheWatermarkOverlapCatchesALateWrite).
//
// # Never past the clock
//
// The opposite skew is worse, so the watermark is capped at the run's own
// clock. A row dated AHEAD -- a writer whose clock runs fast, a version time
// somebody got wrong -- would otherwise carry the watermark with it, every row
// written afterwards with a true time would sort before the watermark less five
// minutes, and the source would stop being counted, silently, until the clock
// caught up. Capped, the row is counted and read again on each run until the
// clock passes it, which costs nothing. A watermark already ahead of the clock
// -- which, with the cap, only a database restored from elsewhere or a run
// from before it could leave -- cannot say how far the walk really read, so it
// is dropped and the source is walked again from four hundred days back, as on
// a first run; the keyed facts make the re-read count nothing twice
// (TestARowDatedAheadOfTheClockDoesNotHoldTheWalk).
//
// # Bounded runs that always advance
//
// A run reads at most maxPagesPerRun pages of a source past its watermark, so
// a first run on a cluster with history is a backfill spread over runs rather
// than a run that never ends. Pages a run only reads AGAIN, inside the
// overlap, do not count against that bound: counting them would let a burst
// of more than forty pages inside five minutes hold every later run on the
// same rows, re-reading them and never reaching what came after. Those pages
// are bounded too, by maxPagesReadPerRun, and a source that exceeds it, a
// page with no way to continue, or a cursor that comes back unchanged is an
// error for that source, never a quiet stop or a loop
// (TestTheFirstRunWalksAtMostFortyPagesPerSource).
//
// # Through the engine, never SQL over the graph
//
// The walk reads campaign rows only through named queries. Hand-written SQL
// over MemoryNodes is the measured hazard the design rules out (section 2):
// the latest-version collapse, the canonical id spellings and the row
// admission all live in the engine, and a statement here that re-derived them
// would be one more place to get them wrong. What is plain SQL is this
// package's own two tables, which nothing else reads or writes.
//
// # Multi-node
//
// The run executes on whichever node holds the cron lead, and a lead that
// moves mid-run can briefly leave two runs walking at once. Nothing here
// needs a lock for that: the facts are keyed, and a watermark only ever moves
// forward (GREATEST), so two runs over the same rows write each fact once and
// leave the watermark where the further of them got.

// Engine is the narrow engine surface the rollup needs: the seam every Go
// component in this tree uses to ask a named query.
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// RollupReport says what one run did.
type RollupReport struct {
	// Rows is how many rows each source's walk read, the overlap's included.
	Rows map[string]int `json:"rows"`
	// Facts is how many facts the run newly recorded; a fact already there
	// is not counted again.
	Facts int `json:"facts"`
	// Deleted is how many facts retention removed.
	Deleted int64 `json:"deleted"`
	// Behind names the sources whose walk stopped at its page bound with
	// rows still to read: a backfill under way, which the next run continues.
	Behind []string `json:"behind,omitempty"`
}

const (
	// watermarkOverlap is how far behind its watermark each walk starts; see
	// the file comment.
	watermarkOverlap = 5 * time.Minute

	// retentionDays is how long a fact is kept (D4), and how far back a
	// source's first walk starts.
	retentionDays = 400

	// maxPagesPerRun bounds the pages a run reads past a source's watermark.
	maxPagesPerRun = 40

	// maxPagesReadPerRun bounds every page a run reads of one source, the
	// overlap's included. Far past any rate this product writes at: a source
	// that reaches it has more rows inside five minutes than ten runs' worth,
	// and the run says so rather than reading on.
	maxPagesReadPerRun = 10 * maxPagesPerRun

	// lookupBatch is the most ids one attribution lookup names.
	lookupBatch = 500

	// maxLookupPages bounds the pages of one lookup batch. A batch of
	// lookupBatch ids reads at most that many rows, one page and an empty
	// continuation at most; anything more is an engine answering a question
	// it was not asked.
	maxLookupPages = 4

	// statementTimeout bounds each statement on this package's own tables.
	statementTimeout = "30s"
)

// walkPageSize is how many rows one page of a walk reads: the queries' own
// `paginate 500`, the engine's per-query row cap. A variable so a test can
// walk a few rows across many pages.
var walkPageSize = 500

// The sources' names: each keys its watermark and its count in the report.
const (
	sourceDelivery   = "delivery"
	sourceEngagement = "engagementEvent"
	sourceConsent    = "consentEvent"
)

// source is one concept the rollup walks.
type source struct {
	name string
	// query is the walk's statement: a format taking the quoted since instant
	// and the page size. The construct name is written out in the literal, so
	// test/dslconformance sees this file call each @serverOnly query.
	query string
	facts func(row map[string]any) []Fact
	// refs notes what a row's facts are attributed through; nil when they are
	// attributed through nothing.
	refs func(row map[string]any, refs pageRefs)
}

var sources = []source{
	{name: sourceDelivery, query: "paginate(query deliveriesWrittenSince(since: %s), %d)", facts: deliveryFacts, refs: deliveryRefs},
	{name: sourceEngagement, query: "paginate(query engagementEventsWrittenSince(since: %s), %d)", facts: engagementFacts},
	{name: sourceConsent, query: "paginate(query consentEventsWrittenSince(since: %s), %d)", facts: consentFacts, refs: consentRefs},
}

// The attribution lookups, formats taking the JSON list of ids and the page
// size.
const (
	journeyLookup  = "paginate(query journeyEnrollmentsByIds(ids: %s), %d)"
	audienceLookup = "paginate(query recipientAudiencesByIds(ids: %s), %d)"
)

// sourceFailure is one source's walk failing. The other sources still run.
type sourceFailure struct {
	Source string
	Err    error
}

func (f *sourceFailure) Error() string { return fmt.Sprintf("%s: %v", f.Source, f.Err) }
func (f *sourceFailure) Unwrap() error { return f.Err }

// runRollup walks every source once and then applies retention.
//
// ONE SOURCE'S FAILURE COSTS ONLY ITSELF. The sources are independent, so a
// failed walk is recorded and the next source still runs, and retention runs
// after them all; the joined error says which failed, and the next run retries
// each from its own watermark.
func runRollup(ctx context.Context, engine Engine, db *bun.DB, now time.Time) (RollupReport, error) {
	rep := RollupReport{Rows: map[string]int{}}
	if engine == nil {
		return rep, errors.New("this node has no engine handle, so the email statistics cannot be read from the campaign rows")
	}
	if db == nil {
		return rep, errors.New("this node has no database handle, so the email statistics cannot be written")
	}
	// Internal origin, because the five queries are @serverOnly: the
	// principal reads every organization's rows, and nothing on the wire may.
	// A fresh read, because the result cache must not answer a walk with a
	// page from before the rows it is looking for were written. A roots-only
	// read, because every shape the walk reads projects the row's own fields:
	// left to expand, each delivery would also fetch its recipient, rule,
	// campaign and organization, two statements apiece, uncached under the
	// fresh read -- measured at 608 statements and 2.3 s for a page of 107
	// deliveries, against 2 statements and 52 ms roots-only.
	ctx = auth.ContextWithInternalOrigin(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(ctx)))
	horizon := now.UTC().Add(-retentionDays * 24 * time.Hour)

	var failures []error
	for _, src := range sources {
		w, err := walkSource(ctx, engine, db, src, now.UTC(), horizon)
		rep.Rows[src.name] = w.rows
		rep.Facts += w.facts
		if w.behind {
			rep.Behind = append(rep.Behind, src.name)
		}
		if err != nil {
			failures = append(failures, &sourceFailure{Source: src.name, Err: err})
		}
	}
	deleted, err := deleteExpired(ctx, db, horizon)
	rep.Deleted = deleted
	if err != nil {
		failures = append(failures, fmt.Errorf("retention: %w", err))
	}
	return rep, errors.Join(failures...)
}

// walked is what one source's walk did.
type walked struct {
	rows, facts int
	behind      bool
}

// walkSource reads one source from its watermark, page by page, writing each
// page's facts and moving the watermark past it before reading the next.
func walkSource(ctx context.Context, engine Engine, db *bun.DB, src source, now, horizon time.Time) (walked, error) {
	var w walked
	from, found, err := readWatermark(ctx, db, src.name)
	if err != nil {
		return w, err
	}
	// A watermark ahead of the clock is dropped, and the walk starts over as
	// a first run: see "Never past the clock" above.
	if found && from.After(now) {
		if err := dropWatermarkAhead(ctx, db, src.name, now); err != nil {
			return w, err
		}
		from, found = time.Time{}, false
	}
	// Never further back than retention keeps: a row older than the horizon
	// gives facts the same run would delete.
	since := horizon
	if found {
		if s := from.Add(-watermarkOverlap); s.After(since) {
			since = s
		}
	}
	query := fmt.Sprintf(src.query, langparser.QuoteString(since.UTC().Format(time.RFC3339Nano)), walkPageSize)

	cursor := ""
	for counted, read := 0, 0; ; {
		if read >= maxPagesReadPerRun {
			return w, fmt.Errorf("read %d pages without passing the rows written in the five minutes before the watermark (%s); the walk stops here rather than reading on", read, from.UTC().Format(time.RFC3339))
		}
		res, err := engine.Execute(memql.ContextWithCursor(ctx, cursor), query)
		if err != nil {
			return w, fmt.Errorf("read a page: %w", err)
		}
		// A page that says there are more rows and offers no way to reach them
		// is refused, never read as the end of the walk: the run would report
		// itself caught up having read less than the rows there are.
		next, err := memql.ResultPageCursor(res)
		if err != nil {
			return w, err
		}
		read++

		page := memql.MaterializeRows(res)
		w.rows += len(page)
		refs := newPageRefs()
		var facts []Fact
		var newest time.Time
		for _, row := range page {
			if at, ok := instantOf(row["createdAt"]); ok && at.After(newest) {
				newest = at
			}
			facts = append(facts, src.facts(row)...)
			if src.refs != nil {
				src.refs(row, refs)
			}
		}
		facts = uniqueFacts(facts)
		if err := attribute(ctx, engine, facts, refs.enrollmentOf, refs.audienceOf); err != nil {
			return w, err
		}
		through := newest
		if through.After(now) {
			through = now
		}
		n, err := recordPage(ctx, db, src.name, facts, through)
		if err != nil {
			return w, err
		}
		w.facts += n
		// A page that only re-reads rows at or before the watermark the run
		// started from does not count: see "Bounded runs" above.
		if !found || newest.After(from) {
			counted++
		}

		if next == "" {
			return w, nil
		}
		if next == cursor {
			return w, errors.New("the walk did not advance past a page: the engine answered the same cursor it was asked with")
		}
		if counted >= maxPagesPerRun {
			w.behind = true
			return w, nil
		}
		cursor = next
	}
}

// attribute fills JourneyID and AudienceID (D18). enrollmentOf names, by a
// delivery fact's SourceID, the journey enrollment the delivery's request id
// names; audienceOf names, by a subscription change's SourceID, the recipient
// it is about. Both are read through the engine in batches -- two lookups per
// page at most, each of at most lookupBatch ids -- under the walk's context.
//
// A record the lookup does not find leaves its facts unattributed: a send
// whose request names no enrollment still counts in its organization's
// totals, under no journey.
func attribute(ctx context.Context, engine Engine, facts []Fact, enrollmentOf, audienceOf map[string]string) error {
	journeys, err := lookup(ctx, engine, journeyLookup, enrollmentOf, "journeyId")
	if err != nil {
		return fmt.Errorf("read the journeys of the page's journey sends: %w", err)
	}
	audiences, err := lookup(ctx, engine, audienceLookup, audienceOf, "audienceId")
	if err != nil {
		return fmt.Errorf("read the audiences of the page's subscription changes: %w", err)
	}
	for i := range facts {
		f := &facts[i]
		switch f.Kind {
		case kindSent, kindDelivered, kindUndelivered, kindFailed, kindSkipped, kindSuppressed:
			if enrollment, ok := enrollmentOf[f.SourceID]; ok {
				f.JourneyID = journeys[enrollment]
			}
		case kindSubscribe, kindUnsubscribe:
			if recipient, ok := audienceOf[f.SourceID]; ok {
				f.AudienceID = audiences[recipient]
			}
		}
	}
	return nil
}

// lookup answers field for each distinct record the refs name, keyed by the
// record's bare id.
func lookup(ctx context.Context, engine Engine, format string, refs map[string]string, field string) (map[string]string, error) {
	out := map[string]string{}
	ids := lookupIDs(refs)
	for start := 0; start < len(ids); start += lookupBatch {
		batch := ids[start:min(start+lookupBatch, len(ids))]
		raw, err := json.Marshal(batch)
		if err != nil {
			return nil, err
		}
		query := fmt.Sprintf(format, raw, lookupBatch)
		cursor := ""
		for page := 0; ; page++ {
			if page >= maxLookupPages {
				return nil, fmt.Errorf("a lookup of %d ids answered more than %d pages", len(batch), maxLookupPages)
			}
			res, err := engine.Execute(memql.ContextWithCursor(ctx, cursor), query)
			if err != nil {
				return nil, err
			}
			next, err := memql.ResultPageCursor(res)
			if err != nil {
				return nil, err
			}
			for _, row := range memql.MaterializeRows(res) {
				if id := bareText(row, "id"); id != "" {
					out[id] = bareText(row, field)
				}
			}
			if next == "" {
				break
			}
			if next == cursor {
				return nil, errors.New("a lookup did not advance past a page")
			}
			cursor = next
		}
	}
	return out, nil
}

// lookupIDs are the distinct ids the refs name, sorted, leaving out any that is
// not a bare id. The engine refuses a whole lookup over one id it cannot
// resolve, so a malformed reference on one row would otherwise fail its page
// on every run; dropping it costs only that row's attribution.
func lookupIDs(refs map[string]string) []string {
	seen := make(map[string]bool, len(refs))
	ids := make([]string, 0, len(refs))
	for _, id := range refs {
		if id == "" || seen[id] || strings.ContainsAny(id, ": \t\r\n\"") {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// factRow is one email_stat_fact row. day is written as the 'YYYY-MM-DD' text
// of the fact's UTC day, never as an instant: Postgres casts a timestamp to a
// date in the session's time zone, and the day must be UTC whatever that is.
type factRow struct {
	bun.BaseModel `bun:"table:email_stat_fact"`
	Kind          string `bun:"kind"`
	SourceID      string `bun:"source_id"`
	AccountID     string `bun:"account_id"`
	CampaignID    string `bun:"campaign_id"`
	RuleID        string `bun:"rule_id"`
	JourneyID     string `bun:"journey_id"`
	AudienceID    string `bun:"audience_id"`
	Day           string `bun:"day"`
}

// recordPage writes one page's facts and moves the source's watermark to
// through, the page's newest row capped at the clock, in one transaction, and
// answers how many facts were new. The watermark only ever moves forward.
func recordPage(ctx context.Context, db *bun.DB, source string, facts []Fact, through time.Time) (int, error) {
	var inserted int64
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '"+statementTimeout+"'"); err != nil {
			return fmt.Errorf("bound the page's statements: %w", err)
		}
		if len(facts) > 0 {
			rows := make([]factRow, 0, len(facts))
			for _, f := range facts {
				rows = append(rows, factRow{
					Kind: f.Kind, SourceID: f.SourceID, AccountID: f.AccountID, CampaignID: f.CampaignID,
					RuleID: f.RuleID, JourneyID: f.JourneyID, AudienceID: f.AudienceID,
					Day: f.Day.UTC().Format(time.DateOnly),
				})
			}
			res, err := tx.NewInsert().Model(&rows).On("CONFLICT (kind, source_id) DO NOTHING").Exec(ctx)
			if err != nil {
				return fmt.Errorf("record the page's facts: %w", err)
			}
			inserted, _ = res.RowsAffected()
		}
		if through.IsZero() {
			return nil
		}
		if _, err := tx.NewRaw(`INSERT INTO email_stat_watermark (source, through) VALUES (?, ?)
			ON CONFLICT (source) DO UPDATE SET through = GREATEST(email_stat_watermark.through, EXCLUDED.through)`,
			source, through.UTC()).Exec(ctx); err != nil {
			return fmt.Errorf("move the watermark: %w", err)
		}
		return nil
	})
	return int(inserted), err
}

// readWatermark answers a source's watermark, and false when it has none.
func readWatermark(ctx context.Context, db *bun.DB, source string) (time.Time, bool, error) {
	var through time.Time
	err := db.NewRaw(`SELECT through FROM email_stat_watermark WHERE source = ?`, source).Scan(ctx, &through)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the watermark: %w", err)
	}
	return through, true, nil
}

// dropWatermarkAhead removes a source's watermark when it is ahead of now. The
// condition is in the statement, so a watermark another run has just written
// at a true time is kept.
func dropWatermarkAhead(ctx context.Context, db *bun.DB, source string, now time.Time) error {
	if _, err := db.NewRaw(`DELETE FROM email_stat_watermark WHERE source = ? AND through > ?`, source, now.UTC()).Exec(ctx); err != nil {
		return fmt.Errorf("drop a watermark ahead of the clock: %w", err)
	}
	return nil
}

// deleteExpired removes the facts dated before the horizon's UTC day.
func deleteExpired(ctx context.Context, db *bun.DB, horizon time.Time) (int64, error) {
	var deleted int64
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '"+statementTimeout+"'"); err != nil {
			return err
		}
		res, err := tx.NewRaw(`DELETE FROM email_stat_fact WHERE day < ?::date`, horizon.UTC().Format(time.DateOnly)).Exec(ctx)
		if err != nil {
			return err
		}
		deleted, _ = res.RowsAffected()
		return nil
	})
	return deleted, err
}
