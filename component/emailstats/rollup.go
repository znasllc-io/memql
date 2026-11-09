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
	"github.com/uptrace/bun/driver/pgdriver"

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
// watermark less five minutes (watermarkOverlap), or when it has none from
// four hundred days back or the later start an earlier first walk left (below).
// The engine answers each row at its latest version, so
// a delivery whose outcome changed since the last run is read again and adds
// what it newly says (facts.go). The query's `row.createdAt >= since` is a
// top-level conjunct, so the engine collapses only the versions written since
// (latestScanLowerBounds, component/memql/latest_scan_keys.go): a run pays for
// what was written since its watermark, and only a first run pays for the
// history. Each page's facts are written in one transaction with the
// watermark moved to the page's newest row, so the watermark never passes a
// fact that was not written, and a run that dies part way -- a replica
// stopping, the cron lead moving to another node -- leaves the next run to
// carry on from the last page that landed.
//
// # Walked, nothing there
//
// The page a walk reads LAST moves the watermark further: to the clock as the
// walk asked for it, rows or none. Every row written before that instant had
// been read, or was a late commit inside the overlap the next run reads again
// (below), so the walk is through that instant. Without it a source with
// nothing new keeps the watermark of its last row -- days old on a quiet
// cluster, absent on one that never had a row -- and the statistics read,
// which counts a figure only where its sources have been walked
// (statistics.go, "Absent is never zero"), could not tell a source walked to
// the clock with nothing in it from one the rollup has not reached. A walk
// that stops at its page bound with rows still to read has not read what
// comes after, and keeps the newest row it wrote
// (TestAWalkThatReadsItsLastPageRecordsTheClockItAskedBy).
//
// # A first walk that cannot read its first page
//
// A first walk -- a source with no watermark -- starts four hundred days back
// (firstWalkFrom), and its first page collapses every version the source
// wrote since then (latestScanLowerBounds, component/memql). Measured at
// about 1.1 s per 100,000 versions, that page outlasts the request pool's 8 s
// statement timeout from roughly 700,000 versions, some 350,000 deliveries. A
// failed page writes no watermark, so every run would start over from four
// hundred days back, fail the same way, and the source's statistics would
// never begin.
//
// So when the FIRST page of a FIRST walk fails -- any failure of the engine's
// read of it, though the timeout is the one it exists for and is logged apart
// -- the next first walk starts half way from this one's start to the clock
// it asked the page by: 400 days, then 200, then 100, down to a floor of a day
// (firstWalkFloor), at a UTC midnight. The start is recorded in
// email_stat_walk_start, not in memory, so a run on another node after the
// cron lead moves carries on from it, and each move is a Warn naming the
// source and the start (plugin.go). A start only moves later (GREATEST): two
// replicas that both failed leave the later of their starts. Once a walk
// reads a page, the watermark takes over, and the start stays where it was
// as the beginning of that source's record: the statistics read counts
// nothing of the source before it (statistics_coverage.go), so the days the
// walk gave up read as not measured rather than as zero. Only the read of the
// page moves it, and not when the run's own context ended: what the page costs
// grows with the span, while attributing or writing a page, or a node
// stopping, does not (TestAFirstWalkThatCannotReadItsFirstPageStartsLater).
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
// The opposite skew is worse, so the watermark is capped at the clock. A row
// dated AHEAD -- a writer whose clock runs fast, a version time somebody got
// wrong -- would otherwise carry the watermark with it, every row written
// afterwards with a true time would sort before the watermark less five
// minutes, and the source would stop being counted, silently, until the clock
// caught up. Capped, the row is counted and read again on each run until the
// clock passes it, which costs nothing.
//
// The clock is the run's own `now` plus the time since the run began, so a run
// long enough to overlap the next one judges by the time it reads, not the time
// it started. A stored watermark further ahead of that clock than the overlap
// absorbs -- which only a database restored from elsewhere, or a leader whose
// clock ran fast, could leave -- cannot say how far the walk really read, so it
// is dropped and the source is walked again from four hundred days back, as on
// a first run; the keyed facts make the re-read count nothing twice. A
// watermark less far ahead is kept: it is another run's, written a moment ago
// by a node whose clock is a little ahead of this one, and dropping it would
// send this run back over the whole history for nothing
// (TestARowDatedAheadOfTheClockDoesNotHoldTheWalk,
// TestAWatermarkAnotherRunMovedPastThisRunsStartIsKept).
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
// # Test runs are not statistics
//
// A test run is a campaign of its own (component/campaigns, test_audience.go):
// testSourceCampaignId names the campaign it tests, it is sent to the
// organization's testing audience, and its deliveries, opens and clicks each
// name it. Counted, its sends would swell Sent and it would take a place among
// the campaigns the Overview ranks, where the campaigns feed, which leaves
// test runs out, has none to show for it. So a delivery, an open, a click or a
// consent event whose campaign is a test run makes no fact. Nor does a consent
// event about a recipient of an organization's testing audience: its people
// are the organization's own, there to receive tests, and their joining is
// not a subscriber's. A row left out is still read, and the watermark moves
// past it like any other.
//
// Both are asked of the engine for each page AFTER the page is read, one
// batched lookup each (campaignTestRunsByIds, testingAudiencesByIds), and only
// for what the page names:
//
//   - No window for a new test run. A test run's campaign row is written
//     before its send is started, so every row naming it is written after it,
//     and a lookup made after the page holding such a row always finds the
//     run. Asked once at the start of a run, a test created while the run
//     walked would be counted (TestATestRunCreatedDuringARunIsStillLeftOut).
//   - A campaign is asked about once a run: the answer is kept for the rest of
//     it, because a campaign is a test run or not from its first write.
//     testSourceCampaignId is accepted by one mutation, createCampaignTestRun,
//     which creates the campaign, and a write from outside the engine that
//     would set or clear it is refused (validateOrganizationSensitiveChanges,
//     component/memql).
//   - The testing audience is asked about on every page and never kept, since
//     an organization can choose another at any time. A consent event is left
//     out when its audience is the testing audience by the time the walk
//     reads it, which takes in a person who joined an audience before it was
//     chosen if the walk had not yet read their joining
//     (TestTheTestingAudiencesConsentEventsMakeNoFact). What the walk read
//     before the audience was chosen stays counted, as every fact does once
//     written (facts.go): people added to an audience that becomes the testing
//     audience after a run has read their joining count as its subscribers on
//     the day they joined. That is the one window left.
//
// (TestATestRunsSendsOpensAndClicksMakeNoFact.)
//
// # Through the engine, never SQL over the graph
//
// The walk reads campaign rows only through named queries. Hand-written SQL
// over MemoryNodes is the measured hazard the design rules out (section 2):
// the latest-version collapse, the canonical id spellings and the row
// admission all live in the engine, and a statement here that re-derived them
// would be one more place to get them wrong. What is plain SQL is this
// package's own tables -- the facts, the watermarks and the walks' starts --
// which nothing else reads or writes.
//
// # The request pool's statement timeout stands
//
// The rollup writes through the request pool (PluginContext.BunDB), whose
// backends carry an 8 s server statement_timeout under pgdriver's 10 s client
// read deadline (component/database, sessionConnParams and
// maintenance_session.go). Nothing here raises the server's limit: a
// statement allowed past the client's deadline would be cut off by the client
// and run on in the server, holding its locks, where at 8 s the server ends it
// first and says so. Its statements are one page's facts and watermark, and
// one day's retention, well inside that
// (TestTheRollupKeepsThePoolsStatementTimeout).
//
// # Multi-node
//
// The run executes on whichever node holds the cron lead, and a lead that
// moves mid-run can briefly leave two runs walking at once. Nothing here needs
// a lock for that: the facts are keyed, and a watermark write never moves it
// back (GREATEST), so two runs over the same rows write each fact once and
// leave the watermark where the further of them got. The one write that does
// lower a watermark is the drop above, of one further ahead of the clock than
// any run could have written.

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
	// watermarkOverlap is how far behind its watermark each walk starts, and
	// how far ahead of the clock a watermark may be before it is dropped; see
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
)

// retention is retentionDays as a duration: how long a fact is kept, which
// the run's delete is counted from, and the furthest back any walk starts. A
// constant: a test that moved it would move the delete, across a table other
// tests write to.
const retention = retentionDays * 24 * time.Hour

// firstWalkFrom is where a source's first walk starts when nothing has moved
// its start: retention back from the run's clock. A variable so a test can
// put a first walk at its own rows without moving the delete.
var firstWalkFrom = func(now time.Time) time.Time { return now.Add(-retention) }

// firstWalkFloor is the shortest span a first walk that cannot read its first
// page is moved to: see "A first walk that cannot read its first page".
const firstWalkFloor = 24 * time.Hour

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

// The lookups that attribute a page's facts, and the two that say which of its
// rows are a test's (see "Test runs are not statistics"): formats taking the
// JSON list of ids and the page size.
const (
	journeyLookup         = "paginate(query journeyEnrollmentsByIds(ids: %s), %d)"
	audienceLookup        = "paginate(query recipientAudiencesByIds(ids: %s), %d)"
	testRunLookup         = "paginate(query campaignTestRunsByIds(ids: %s), %d)"
	testingAudienceLookup = "paginate(query testingAudiencesByIds(ids: %s), %d)"
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
	// Internal origin, because the seven queries are @serverOnly: the
	// principal reads every organization's rows, and nothing on the wire may.
	// A fresh read, because the result cache must not answer a walk with a
	// page from before the rows it is looking for were written. A roots-only
	// read, because every shape the walk reads projects the row's own fields:
	// left to expand, each delivery would also fetch its recipient, rule,
	// campaign and organization, two statements apiece, uncached under the
	// fresh read -- measured at 608 statements and 2.3 s for a page of 107
	// deliveries, against 2 statements and 52 ms roots-only.
	ctx = auth.ContextWithInternalOrigin(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(ctx)))
	// The clock the walk reads by: the run's now, moved on by the time the run
	// has taken. See "Never past the clock" above.
	began := time.Now()
	clock := func() time.Time { return now.UTC().Add(time.Since(began)) }
	horizon := now.UTC().Add(-retention)
	firstFrom := firstWalkFrom(now.UTC())
	if firstFrom.Before(horizon) {
		firstFrom = horizon
	}

	// Which campaigns the run has found to be test runs, and which not: kept
	// for the run, since a campaign is one or the other from its first write.
	// See "Test runs are not statistics".
	testRuns := map[string]bool{}

	var failures []error
	for _, src := range sources {
		w, err := walkSource(ctx, engine, db, src, clock, horizon, firstFrom, testRuns)
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
// page's facts and moving the watermark past it before reading the next. A
// source with no watermark walks for the first time, from firstFrom or the
// later start an earlier first walk left (see "A first walk that cannot read
// its first page"). testRuns is the run's answers about test runs, which the
// walk adds to (attribute).
func walkSource(ctx context.Context, engine Engine, db *bun.DB, src source, clock func() time.Time, horizon, firstFrom time.Time, testRuns map[string]bool) (walked, error) {
	var w walked
	from, found, err := readWatermark(ctx, db, src.name)
	if err != nil {
		return w, err
	}
	// A watermark further ahead of the clock -- as it reads now, at the start
	// of this source's walk -- than the overlap absorbs is dropped, and the
	// walk starts over as a first run; one less far ahead is another run's
	// and is kept. See "Never past the clock" above.
	if tolerance := clock().Add(watermarkOverlap); found && from.After(tolerance) {
		if err := dropWatermarkAhead(ctx, db, src.name, tolerance); err != nil {
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
	} else {
		since = firstFrom
		start, moved, err := readWalkStart(ctx, db, src.name)
		if err != nil {
			return w, err
		}
		if moved && start.After(since) {
			since = start
		}
	}
	query := fmt.Sprintf(src.query, quoteInstant(since), walkPageSize)

	cursor := ""
	for counted, read := 0, 0; ; {
		if read >= maxPagesReadPerRun {
			return w, fmt.Errorf("read %d pages without passing the rows written in the five minutes before the watermark (%s); the walk stops here rather than reading on", read, from.UTC().Format(time.RFC3339))
		}
		// The clock as this page is asked for: if it is the last page, the
		// walk is through this instant. See "Walked, nothing there" above.
		askedAt := clock()
		res, err := engine.Execute(memql.ContextWithCursor(ctx, cursor), query)
		if err != nil && !found && read == 0 && ctx.Err() == nil {
			// The first page of a first walk: start the next run's first walk
			// later. See "A first walk that cannot read its first page". Not
			// when the run's own context ended -- the node stopping, the lead
			// moving -- which says nothing about how long the history is.
			next := laterStart(since, askedAt)
			if serr := recordWalkStart(ctx, db, src.name, next); serr != nil {
				return w, errors.Join(fmt.Errorf("read a page: %w", err), serr)
			}
			return w, &firstWalkMoved{Start: next, TimedOut: isStatementTimeout(err), Err: fmt.Errorf("read a page: %w", err)}
		}
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
		// Asked after the page is read: see "Test runs are not statistics".
		if facts, err = attribute(ctx, engine, uniqueFacts(facts), refs, testRuns); err != nil {
			return w, err
		}
		// Capped at the clock as it reads now: see "Never past the clock".
		through := newest
		if now := clock(); through.After(now) {
			through = now
		}
		// The last page is through the clock it was asked by, rows or none:
		// see "Walked, nothing there". askedAt is never past the clock, so the
		// cap above still holds.
		if next == "" && askedAt.After(through) {
			through = askedAt
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

// quoteInstant is an instant as a query argument.
func quoteInstant(t time.Time) string {
	return langparser.QuoteString(t.UTC().Format(time.RFC3339Nano))
}

// attribute fills JourneyID and AudienceID (D18) and answers the facts to
// record: the page's, less a test run's and the testing audience's (see "Test
// runs are not statistics"). refs.enrollmentOf names, by a delivery fact's
// SourceID, the journey enrollment the delivery's request id names;
// refs.recipientOf names, by a consent fact's SourceID, the recipient it is
// about. testRuns holds the run's answers so far, by campaign, true for a
// test run; the page's campaigns it has no answer for are asked about and
// added. Every lookup is read through the engine in batches -- four per page
// at most, each of at most lookupBatch ids, and none when the page names
// nothing to ask about -- under the walk's context.
//
// A JOURNEY ONLY IN ITS OWN ORGANIZATION. A request id is a string the sender
// chose -- an event automation names its own -- and the lookup runs as the
// cluster, so a request id spelling another organization's enrollment would
// otherwise count one organization's send under another's journey. A send is
// attributed to the enrollment's journey only when the enrollment's
// organization is the delivery's (TestAJourneySendIsAttributedOnlyInItsOwnOrganization).
// A subscription change is not checked the same way because its recipient is
// not a string anybody chose: it is a declared reference, which the campaigns
// write path checks against the event's own organization when the event is
// written.
//
// A record the lookup does not find leaves its facts unattributed: a send
// whose request names no enrollment still counts in its organization's
// totals, under no journey.
func attribute(ctx context.Context, engine Engine, facts []Fact, refs pageRefs, testRuns map[string]bool) ([]Fact, error) {
	journeys, err := lookup(ctx, engine, journeyLookup, refs.enrollmentOf)
	if err != nil {
		return nil, fmt.Errorf("read the journeys of the page's journey sends: %w", err)
	}
	recipients, err := lookup(ctx, engine, audienceLookup, refs.recipientOf)
	if err != nil {
		return nil, fmt.Errorf("read the audiences of the page's consent events: %w", err)
	}
	if err := askTestRuns(ctx, engine, facts, testRuns); err != nil {
		return nil, fmt.Errorf("read which of the page's campaigns are test runs: %w", err)
	}
	testing, err := testingAudiences(ctx, engine, recipients)
	if err != nil {
		return nil, fmt.Errorf("read which of the page's audiences are testing audiences: %w", err)
	}
	kept := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if testRuns[f.CampaignID] {
			continue
		}
		switch f.Kind {
		case kindSent, kindDelivered, kindUndelivered, kindFailed, kindSkipped, kindSuppressed:
			enrollmentID, named := refs.enrollmentOf[f.SourceID]
			enrollment, found := journeys[enrollmentID]
			if named && found {
				if account := bareText(enrollment, "accountId"); account != "" && account == f.AccountID {
					f.JourneyID = bareText(enrollment, "journeyId")
				}
			}
		case kindSubscribe, kindUnsubscribe, kindBounce, kindComplaint:
			audience := ""
			if recipientID, named := refs.recipientOf[f.SourceID]; named {
				audience = bareText(recipients[recipientID], "audienceId")
			}
			if testing[audience] {
				continue
			}
			// Only a subscription change is counted under an audience.
			if f.Kind == kindSubscribe || f.Kind == kindUnsubscribe {
				f.AudienceID = audience
			}
		}
		kept = append(kept, f)
	}
	return kept, nil
}

// askTestRuns asks which of the facts' campaigns the run has no answer for
// are test runs, and records the answer for each in testRuns: true for a test
// run, false for any other campaign, one the lookup does not find included.
// The lookup answers test runs alone (campaignTestRunsByIds).
func askTestRuns(ctx context.Context, engine Engine, facts []Fact, testRuns map[string]bool) error {
	unasked := map[string]string{}
	for _, f := range facts {
		if _, asked := testRuns[f.CampaignID]; f.CampaignID != "" && !asked {
			unasked[f.CampaignID] = f.CampaignID
		}
	}
	if len(unasked) == 0 {
		return nil
	}
	runs, err := lookup(ctx, engine, testRunLookup, unasked)
	if err != nil {
		return err
	}
	for id := range unasked {
		_, isRun := runs[id]
		testRuns[id] = isRun
	}
	return nil
}

// testingAudiences answers which of the audiences the page's recipients
// belong to are an organization's testing audience, as the settings stand
// now, after the page was read.
func testingAudiences(ctx context.Context, engine Engine, recipients map[string]map[string]any) (map[string]bool, error) {
	audiences := map[string]string{}
	for _, recipient := range recipients {
		if audience := bareText(recipient, "audienceId"); audience != "" {
			audiences[audience] = audience
		}
	}
	testing := map[string]bool{}
	if len(audiences) == 0 {
		return testing, nil
	}
	settings, err := lookup(ctx, engine, testingAudienceLookup, audiences)
	if err != nil {
		return nil, err
	}
	for _, setting := range settings {
		if audience := bareText(setting, "audienceId"); audience != "" {
			testing[audience] = true
		}
	}
	return testing, nil
}

// lookup answers the row of each distinct record the refs name, keyed by the
// record's bare id.
func lookup(ctx context.Context, engine Engine, format string, refs map[string]string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
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
					out[id] = row
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
// through -- the page's newest row, capped at the clock, or for the last page
// the clock it was asked by -- in one transaction, and answers how many facts
// were new. The write never moves the watermark back (GREATEST): a run
// finishing behind a faster one leaves it where the faster one got. Only
// dropWatermarkAhead lowers one. Its statements run under the request pool's
// own statement timeout: see "The request pool's statement timeout stands".
func recordPage(ctx context.Context, db *bun.DB, source string, facts []Fact, through time.Time) (int, error) {
	var inserted int64
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
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

// firstWalkMoved is a first walk whose first page failed, and where the next
// one starts: see "A first walk that cannot read its first page".
type firstWalkMoved struct {
	Start time.Time
	// TimedOut is whether the page ended at the statement timeout, which is
	// the failure the move exists for and is logged apart.
	TimedOut bool
	Err      error
}

func (m *firstWalkMoved) Error() string {
	return fmt.Sprintf("%v; the next first walk starts from %s", m.Err, m.Start.Format(time.DateOnly))
}

func (m *firstWalkMoved) Unwrap() error { return m.Err }

// laterStart is where the next first walk starts after one that could not
// read its first page from since: half way from since to the clock it asked
// by, no nearer the clock than firstWalkFloor, at the UTC midnight that day
// begins with, so the days from it on are walked whole -- and never before
// since, which the midnight could otherwise put it at a walk already near the
// floor.
func laterStart(since, askedAt time.Time) time.Time {
	next := since.Add(askedAt.Sub(since) / 2)
	if latest := askedAt.Add(-firstWalkFloor); next.After(latest) {
		next = latest
	}
	if next = dayOf(next); next.Before(since) {
		return since
	}
	return next
}

// isStatementTimeout reports whether a read ended at the server's statement
// timeout (SQLSTATE 57014): the driver's own error when the engine passes it
// on, and the text it renders when the engine passes on only the text.
func isStatementTimeout(err error) bool {
	var pgerr pgdriver.Error
	if errors.As(err, &pgerr) {
		return pgerr.StatementTimeout()
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "57014") || strings.Contains(text, "canceling statement due to statement timeout")
}

// readWalkStart answers where a source's first walk starts when an earlier
// first walk moved it, and false when none has.
func readWalkStart(ctx context.Context, db *bun.DB, source string) (time.Time, bool, error) {
	var start time.Time
	err := db.NewRaw(`SELECT starts_at FROM email_stat_walk_start WHERE source = ?`, source).Scan(ctx, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read where the first walk starts: %w", err)
	}
	return start, true, nil
}

// recordWalkStart records where a source's next first walk starts. It never
// moves a start back: two replicas whose first walks both failed leave the
// later of the starts they computed.
func recordWalkStart(ctx context.Context, db *bun.DB, source string, start time.Time) error {
	if _, err := db.NewRaw(`INSERT INTO email_stat_walk_start (source, starts_at) VALUES (?, ?)
		ON CONFLICT (source) DO UPDATE SET starts_at = GREATEST(email_stat_walk_start.starts_at, EXCLUDED.starts_at)`,
		source, start.UTC()).Exec(ctx); err != nil {
		return fmt.Errorf("record where the next first walk starts: %w", err)
	}
	return nil
}

// dropWatermarkAhead removes a source's watermark when it is past tolerance,
// the clock plus the overlap -- the one write that lowers a watermark. The
// condition is in the statement, so a watermark another run has just written
// at a true time is kept.
func dropWatermarkAhead(ctx context.Context, db *bun.DB, source string, tolerance time.Time) error {
	if _, err := db.NewRaw(`DELETE FROM email_stat_watermark WHERE source = ? AND through > ?`, source, tolerance.UTC()).Exec(ctx); err != nil {
		return fmt.Errorf("drop a watermark ahead of the clock: %w", err)
	}
	return nil
}

// deleteExpired removes the facts dated before the horizon's UTC day: one
// statement, under the request pool's own statement timeout (see "The request
// pool's statement timeout stands"). Each day it finds one day of facts past
// the horizon, so it stays small; a delete the timeout ends removes nothing,
// and the next run's retention pass deletes the same days.
func deleteExpired(ctx context.Context, db *bun.DB, horizon time.Time) (int64, error) {
	res, err := db.NewRaw(`DELETE FROM email_stat_fact WHERE day < ?::date`, horizon.UTC().Format(time.DateOnly)).Exec(ctx)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()
	return deleted, nil
}
