package emailstats

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// statistics_db_test.go -- the emailStatistics read against the real table.
//
// The figures are SQL's -- the counts by day, the cohort join on the delivery,
// the rankings -- so they are proved against Postgres, with the facts written
// DIRECTLY into email_stat_fact, as the rollup would have written them. The
// rollup is never run here: its retention pass deletes old facts across the
// shared table.
//
// THE TESTS OWN A BAND OF DATES, 2040 through 2042, and every fact they write
// is in it, under a source id that opens with statsSourcePrefix. The rollup
// tests date their facts by the real clock and their retention reaches back
// from it, so neither touches the band. Each test clears the band's facts --
// its own prefix, inside the band -- before and after it runs, because a read
// for staff covers every organization in a period, and a fact an interrupted
// run left behind would land in the next run's totals.
//
// MEASUREDSINCE IS THE CLUSTER'S, the first day the table holds anything, so on
// a database holding only a test's own facts it would fall inside the test's
// period. Every band test therefore writes an anchor fact on the band's first
// day, before every period it reads, so each bucket it reads is measured
// whatever else the table holds.
//
// SO ARE THE WATERMARKS. A figure is measured only where the rollup has walked
// its sources (statistics.go, "Absent is never zero"), so every band test sets
// the three watermarks to statsNow, and puts them back at the real clock when
// it ends, as the rollup tests do (walkFrom). The tests about measuredSince and
// the walks' coverage read a pair of tables of their own (emptyStatsTables).
//
// Postgres-gated: CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1.

// The engine is a GrantResolver, which is what the plug-in asserts on the
// handle it is given.
var _ GrantResolver = (*memql.MemQLEngine)(nil)

var (
	statsBandStart = time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	statsBandEnd   = time.Date(2043, 1, 1, 0, 0, 0, 0, time.UTC)
	// statsNow is the instant the reads are counted back from: far enough into
	// the band that twelve months back, and ninety days with the ninety before
	// them, all start after the anchor.
	statsNow = time.Date(2041, 6, 17, 15, 4, 5, 0, time.UTC)
)

// statsSourcePrefix opens every source id these tests write, so clearing the
// band removes only their facts.
const statsSourcePrefix = "estatsread-"

// statsFixture is one test's facts: its organizations, campaigns, rules and
// sources all carry a suffix of its own.
type statsFixture struct {
	t      *testing.T
	db     *bun.DB
	suffix string
	n      int
	// anyDay lets the fixture write outside the band: a clock fixture's facts
	// are dated by the real clock and removed by their prefix instead.
	anyDay bool
}

// newStatsFixture writes into the shared table, inside the band, with the
// anchor in place and every source walked through statsNow.
func newStatsFixture(t *testing.T) *statsFixture {
	t.Helper()
	_, db := statsEngine(t)
	clearStatsBand(t, db)
	t.Cleanup(func() { clearStatsBand(t, db) })
	walkFrom(t, db, statsNow)
	t.Cleanup(func() { walkFrom(t, db, time.Now()) })
	f := &statsFixture{t: t, db: db, suffix: fmt.Sprintf("%s%d", statsSourcePrefix, time.Now().UnixNano())}
	f.times(1, kindSkipped, statsBandStart, statFact{account: f.id("anchor")})
	return f
}

// newEmptyTableFixture writes into tables of its own that start empty
// (emptyStatsTables), with no anchor and every source walked through
// statsNow: the cluster it reads holds exactly the facts the test writes.
func newEmptyTableFixture(t *testing.T) *statsFixture {
	t.Helper()
	f := &statsFixture{t: t, db: emptyStatsTables(t), suffix: fmt.Sprintf("%s%d", statsSourcePrefix, time.Now().UnixNano())}
	for _, s := range sources {
		f.walked(s.name, statsNow)
	}
	return f
}

// newClockFixture writes into the shared table on days of the real clock, for
// a read the engine's own registered plug-in answers: it counts its period
// back from time.Now(), so the band cannot serve it. Its facts are removed by
// their prefix when the test ends.
func newClockFixture(t *testing.T) *statsFixture {
	t.Helper()
	_, db := statsEngine(t)
	f := &statsFixture{t: t, db: db, suffix: fmt.Sprintf("%s%d", statsSourcePrefix, time.Now().UnixNano()), anyDay: true}
	t.Cleanup(func() {
		if _, err := db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, f.suffix+"%").Exec(context.Background()); err != nil {
			t.Errorf("remove the clock fixture's facts: %v", err)
		}
	})
	return f
}

func clearStatsBand(t *testing.T, db *bun.DB) {
	t.Helper()
	if _, err := db.NewRaw(`DELETE FROM email_stat_fact WHERE day >= ?::date AND day < ?::date AND source_id LIKE ?`,
		dateArg(statsBandStart), dateArg(statsBandEnd), statsSourcePrefix+"%").Exec(context.Background()); err != nil {
		t.Fatalf("clear the statistics tests' band: %v", err)
	}
}

// walked records how far a source has been walked, which is what the read
// counts a figure as measured by (walked_through, as the rollup records it);
// neverWalked removes the record, as on a cluster the rollup has not reached.
// On a band fixture these move the shared watermarks, which the fixture puts
// back at the real clock when the test ends.
func (f *statsFixture) walked(source string, through time.Time) {
	f.t.Helper()
	if _, err := f.db.NewRaw(`INSERT INTO email_stat_watermark (source, walked_through) VALUES (?, ?)
		ON CONFLICT (source) DO UPDATE SET walked_through = EXCLUDED.walked_through`, source, through.UTC()).Exec(context.Background()); err != nil {
		f.t.Fatalf("record the %s walk through %s: %v", source, through, err)
	}
}

func (f *statsFixture) neverWalked(source string) {
	f.t.Helper()
	if _, err := f.db.NewRaw(`DELETE FROM email_stat_watermark WHERE source = ?`, source).Exec(context.Background()); err != nil {
		f.t.Fatalf("forget the %s walk: %v", source, err)
	}
}

// walksFrom records where a source's walk starts, as the rollup records it
// after a first walk that could not read its first page. Only on a fixture's
// own tables (emptyStatsTables): the shared table's starts move the rollup
// tests' first walks.
func (f *statsFixture) walksFrom(source string, start time.Time) {
	f.t.Helper()
	if _, err := f.db.NewRaw(`INSERT INTO email_stat_walk_start (source, starts_at) VALUES (?, ?)
		ON CONFLICT (source) DO UPDATE SET starts_at = EXCLUDED.starts_at`, source, start.UTC()).Exec(context.Background()); err != nil {
		f.t.Fatalf("record the %s walk start %s: %v", source, start, err)
	}
}

// emptyStatsTables is a database handle that sees an empty email_stat_fact,
// email_stat_watermark and email_stat_walk_start of its own: temporary tables
// of the same shape, indexes and keys included, on the handle's one
// connection, which hide the shared tables from that connection alone. It is
// how a test reads a cluster with no fact at all, one whose first fact it
// chose, or one whose walks it placed, on a database where other tests leave
// facts and move watermarks.
func emptyStatsTables(t *testing.T) *bun.DB {
	t.Helper()
	// Skips when no database is reachable, and leaves the schema migrated.
	statsEngine(t)
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	// ONE connection, kept for the test: a temporary table belongs to the
	// session that made it, so every statement must run on that session.
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
	sqldb.SetConnMaxLifetime(0)
	sqldb.SetConnMaxIdleTime(0)
	db := bun.NewDB(sqldb, pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, table := range []string{"email_stat_fact", "email_stat_watermark", "email_stat_walk_start"} {
		if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE `+table+` (LIKE `+table+` INCLUDING ALL)`); err != nil {
			t.Fatalf("make an empty %s: %v", table, err)
		}
		// The read names its tables bare, as it must; prove each name resolves
		// to the temporary table here, or this test would read the shared one
		// and pass or fail on other tests' rows.
		var persistence string
		if err := db.NewRaw(`SELECT relpersistence::text FROM pg_class WHERE oid = ?::regclass`, table).Scan(ctx, &persistence); err != nil || persistence != "t" {
			t.Fatalf("%s resolves to a table of persistence %q (%v), want the empty temporary one", table, persistence, err)
		}
	}
	return db
}

// statementRecorder keeps every statement a handle issues.
type statementRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *statementRecorder) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, e.Query)
	return ctx
}

func (r *statementRecorder) AfterQuery(context.Context, *bun.QueryEvent) {}

// recordedDB is a second handle on the test database, whose statements are
// kept.
func recordedDB(t *testing.T) (*bun.DB, *statementRecorder) {
	t.Helper()
	statsEngine(t)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	rec := &statementRecorder{}
	db.AddQueryHook(rec)
	return db, rec
}

// wantThePoolsTimeout fails when a recorded statement moves statement_timeout,
// and when none touched the table named: a recorder that saw nothing proves
// nothing.
func wantThePoolsTimeout(t *testing.T, what string, rec *statementRecorder, table string) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	touched := false
	for _, q := range rec.seen {
		if strings.Contains(strings.ToLower(q), "statement_timeout") {
			t.Errorf("%s moved the pool's statement timeout: %s", what, q)
		}
		touched = touched || strings.Contains(q, table)
	}
	if !touched {
		t.Errorf("%s issued no statement on %s through the recorded handle; the %d it issued prove nothing", what, table, len(rec.seen))
	}
}

// statFact is one email_stat_fact row as the rollup writes it; source is a
// fresh one when left empty.
type statFact struct {
	kind, source, account, campaign, rule, journey, audience string
	day                                                      time.Time
}

// id is a name of the fixture's own: an organization, a campaign, a rule.
func (f *statsFixture) id(name string) string { return f.suffix + "-" + name }

// write inserts facts, refusing any outside the band or under another prefix,
// and answers their sources.
func (f *statsFixture) write(facts ...statFact) []string {
	f.t.Helper()
	rows := make([]factRow, 0, len(facts))
	sources := make([]string, 0, len(facts))
	for _, x := range facts {
		if !f.anyDay && (x.day.Before(statsBandStart) || !x.day.Before(statsBandEnd)) {
			f.t.Fatalf("a %s fact on %s is outside the statistics tests' band [%s, %s)", x.kind, dateArg(x.day), dateArg(statsBandStart), dateArg(statsBandEnd))
		}
		if x.source == "" {
			f.n++
			x.source = fmt.Sprintf("%s-src-%d", f.suffix, f.n)
		}
		if !strings.HasPrefix(x.source, statsSourcePrefix) {
			f.t.Fatalf("source %q does not open with %q, so clearing the band would leave it behind", x.source, statsSourcePrefix)
		}
		rows = append(rows, factRow{
			Kind: x.kind, SourceID: x.source, AccountID: x.account, CampaignID: x.campaign,
			RuleID: x.rule, JourneyID: x.journey, AudienceID: x.audience, Day: dateArg(x.day),
		})
		sources = append(sources, x.source)
	}
	if len(rows) == 0 {
		return sources
	}
	if _, err := f.db.NewInsert().Model(&rows).Exec(context.Background()); err != nil {
		f.t.Fatalf("write %d facts: %v", len(rows), err)
	}
	return sources
}

// times writes n facts of one kind on a day, each about a record of its own,
// and answers the records; a send's is its delivery.
func (f *statsFixture) times(n int, kind string, on time.Time, x statFact) []string {
	f.t.Helper()
	facts := make([]statFact, n)
	for i := range facts {
		facts[i] = x
		facts[i].kind, facts[i].day = kind, on
	}
	return f.write(facts...)
}

// of writes one fact of a kind for each record, on a day: the first open or
// click of each delivered message, keyed by its delivery as the rollup keys
// it, or a suppression beside a skip.
func (f *statsFixture) of(kind string, sources []string, on time.Time, x statFact) {
	f.t.Helper()
	facts := make([]statFact, len(sources))
	for i, s := range sources {
		facts[i] = x
		facts[i].kind, facts[i].source, facts[i].day = kind, s, on
	}
	f.write(facts...)
}

// read answers readStatistics over the fixture's table at statsNow.
func (f *statsFixture) read(grant memql.AccessGrant, requested []string, p statsperiod.Period) EmailStatistics {
	f.t.Helper()
	s, err := readStatistics(context.Background(), f.db, grant, requested, p, statsNow)
	if err != nil {
		f.t.Fatalf("readStatistics(%+v, %q, %s): %v", grant, requested, p, err)
	}
	return s
}

func staff() memql.AccessGrant { return memql.AccessGrant{EveryAccount: true} }

func memberOf(accounts ...string) memql.AccessGrant { return memql.AccessGrant{AccountIDs: accounts} }

// fakeGrants answers one grant for every caller.
type fakeGrants struct{ grant memql.AccessGrant }

func (g fakeGrants) ResolveAccessGrant(context.Context) memql.AccessGrant { return g.grant }

// integration is the plug-in over the fixture's table at statsNow, resolving
// grants with the resolver given (nil: none).
func (f *statsFixture) integration(grants GrantResolver) *Integration {
	i := NewIntegration(nil, func() *bun.DB { return f.db }, quietLogger())
	i.grants = grants
	i.now = func() time.Time { return statsNow }
	return i
}

// call runs builtin emailStatistics through its handler and answers the
// payload as a client decodes it.
func call(t *testing.T, i *Integration, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	nodes, err := i.handleStatistics(ctx, args, 0)
	if err != nil {
		t.Fatalf("handleStatistics(%v): %v", args, err)
	}
	if len(nodes) != 1 || nodes[0].ID != "statistics" || nodes[0].Concept != statisticsConcept {
		t.Fatalf("handleStatistics(%v) answered %+v, want one node %q of %s", args, nodes, "statistics", statisticsConcept)
	}
	var p map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
		t.Fatalf("decode the payload: %v", err)
	}
	return p
}

// totalSent is the payload's totals.sent: a float64 from JSON, or nil.
func totalSent(p map[string]any) any {
	totals, _ := p["totals"].(map[string]any)
	return totals["sent"]
}

// callerCtx is an authenticated caller of a role.
func callerCtx(userId string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// joinOrganization makes an organization with its own group and puts userId
// in it, as a Member: one of the client's people, who reaches the
// organization only through the group, which is what the engine's grant reads.
//
// THE THREE ROWS ARE REMOVED WHEN THE TEST ENDS, every version of each. They
// are shared rows on a database every test in the lane reads, and fixture
// organizations left behind on every run accumulate in each staff reader's
// list of organizations until a test that walks that list stops passing.
func joinOrganization(t *testing.T, eng *memql.MemQLEngine, account, userId string) {
	t.Helper()
	_, db := statsEngine(t)
	group := account + "-group"
	membership := group + "-" + userId
	written := map[string]string{
		"v1:accounts:account":         account,
		"v1:identity:group":           group,
		"v1:identity:groupMembership": membership,
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for concept, id := range written {
			// Both spellings: the engine stores a row under its canonical id,
			// and a bare one costs nothing to name.
			ids := bun.In([]string{id, concept + ":" + id})
			if _, err := db.NewRaw(`DELETE FROM "MemoryNodes" WHERE concept = ? AND id IN (?)`, concept, ids).Exec(ctx); err != nil {
				t.Errorf("remove the fixture %s %s: %v", concept, id, err)
				continue
			}
			var left int
			if err := db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE concept = ? AND id IN (?)`, concept, ids).Scan(ctx, &left); err != nil || left != 0 {
				t.Errorf("the fixture %s %s still has %d rows after its removal (%v)", concept, id, left, err)
			}
		}
	})
	seed := auth.ContextWithInternalOrigin(callerCtx("email-statistics-seeder", auth.RoleOwner))
	for _, q := range []string{
		fmt.Sprintf(`insert("v1:accounts:account", id=%s, payload={"name": "Statistics organization", "status": "active", "domainStatus": "unverified"})`, langparser.QuoteString(account)),
		fmt.Sprintf(`mutation writeGroup(groupId: %s, name: "Statistics organization", kind: "account", accountId: %s, status: "active")`, langparser.QuoteString(group), langparser.QuoteString(account)),
		fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, accountId: %s, origin: "added", status: "active")`,
			langparser.QuoteString(membership), langparser.QuoteString(group), langparser.QuoteString(userId), langparser.QuoteString(account)),
	} {
		if _, err := eng.Execute(seed, q); err != nil {
			t.Fatalf("put %s in %s: %v", userId, account, err)
		}
	}
	// The rows are there under the ids the removal names, or it would remove
	// nothing and say so only by leaving them.
	for concept, id := range written {
		var n int
		if err := db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE concept = ? AND id IN (?)`, concept, bun.In([]string{id, concept + ":" + id})).Scan(context.Background(), &n); err != nil || n == 0 {
			t.Fatalf("the fixture %s %s was written under an id its removal does not name (%d rows, %v)", concept, id, n, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Rendering: every figure in one line, nil as "null", so a failure shows the
// whole row and a null can never pass for a zero.
// ---------------------------------------------------------------------------

func num(p *int64) string {
	if p == nil {
		return "null"
	}
	return strconv.FormatInt(*p, 10)
}

func rate(p *float64) string {
	if p == nil {
		return "null"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func totalsLine(x EmailTotals) string {
	return fmt.Sprintf("sent=%s delivered=%s undelivered=%s failed=%s skipped=%s suppressed=%s opens=%s clicks=%s uniqueOpens=%s uniqueClicks=%s unsubscribes=%s bounces=%s complaints=%s subscribes=%s openRate=%s clickRate=%s",
		num(x.Sent), num(x.Delivered), num(x.Undelivered), num(x.Failed), num(x.Skipped), num(x.Suppressed),
		num(x.Opens), num(x.Clicks), num(x.UniqueOpens), num(x.UniqueClicks),
		num(x.Unsubscribes), num(x.Bounces), num(x.Complaints), num(x.Subscribes), rate(x.OpenRate), rate(x.ClickRate))
}

func pointLine(p EmailPoint) string {
	return fmt.Sprintf("%s sent=%s uniqueOpens=%s uniqueClicks=%s unsubscribes=%s subscribes=%s bounces=%s complaints=%s failed=%s suppressed=%s openRate=%s clickRate=%s",
		p.At.UTC().Format(time.DateOnly), num(p.Sent), num(p.UniqueOpens), num(p.UniqueClicks), num(p.Unsubscribes), num(p.Subscribes),
		num(p.Bounces), num(p.Complaints), num(p.Failed), num(p.Suppressed), rate(p.OpenRate), rate(p.ClickRate))
}

// zeroPoint is a bucket that sent nothing, and nullPoint one nothing measured.
func zeroPoint(on string) string {
	return on + " sent=0 uniqueOpens=0 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=0 complaints=0 failed=0 suppressed=0 openRate=null clickRate=null"
}

func nullPoint(on string) string {
	return on + " sent=null uniqueOpens=null uniqueClicks=null unsubscribes=null subscribes=null bounces=null complaints=null failed=null suppressed=null openRate=null clickRate=null"
}

func figuresLine(rows []CampaignFigures) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("%s sent=%d uniqueOpens=%d uniqueClicks=%d openRate=%s clickRate=%s", r.ID, r.Sent, r.UniqueOpens, r.UniqueClicks, rate(r.OpenRate), rate(r.ClickRate))
	}
	return strings.Join(parts, "; ")
}

func automationsLine(rows []AutomationFigures) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("%s %s sent=%d failed=%d suppressed=%d", r.Kind, r.ID, r.Sent, r.Failed, r.Suppressed)
	}
	return strings.Join(parts, "; ")
}

func audiencesLine(rows []AudienceFigures) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("%s subscribes=%d unsubscribes=%d net=%d", r.ID, r.Subscribes, r.Unsubscribes, r.Net)
	}
	return strings.Join(parts, "; ")
}

func wantSeries(t *testing.T, what string, got []EmailPoint, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d points, want %d", what, len(got), len(want))
	}
	for i := 0; i < len(got) && i < len(want); i++ {
		if line := pointLine(got[i]); line != want[i] {
			t.Errorf("%s[%d]:\n  got  %s\n  want %s", what, i, line, want[i])
		}
	}
}

// wantNothing is the answer a caller gets for organizations they do not
// reach: the answer of somebody who reaches none, so it confirms nothing.
func wantNothing(t *testing.T, who string, s EmailStatistics) {
	t.Helper()
	none := totalsLine(EmailTotals{})
	if line := totalsLine(s.Totals); line != none {
		t.Errorf("%s: totals %s, want every figure null", who, line)
	}
	if line := totalsLine(s.Previous); line != none {
		t.Errorf("%s: previous %s, want every figure null", who, line)
	}
	if s.MeasuredSince != nil {
		t.Errorf("%s: measuredSince %s, want none", who, s.MeasuredSince.Format(time.DateOnly))
	}
	for i, p := range append(append([]EmailPoint{}, s.Series...), s.PreviousSeries...) {
		if line := pointLine(p); line != nullPoint(p.At.UTC().Format(time.DateOnly)) {
			t.Errorf("%s: point %d is %s, want null", who, i, line)
		}
	}
	if len(s.Campaigns)+len(s.Automations)+len(s.Audiences)+len(s.Accounts) != 0 {
		t.Errorf("%s: campaigns %s, automations %s, audiences %s, accounts %s; want none",
			who, figuresLine(s.Campaigns), automationsLine(s.Automations), audiencesLine(s.Audiences), figuresLine(s.Accounts))
	}
}

// wantNothingInPayload is wantNothing as the client reads it: every figure of
// the totals, the previous period and every point PRESENT AND NULL, and every
// list empty.
func wantNothingInPayload(t *testing.T, who string, p map[string]any) {
	t.Helper()
	if p["measuredSince"] != nil {
		t.Errorf("%s: measuredSince %v, want null", who, p["measuredSince"])
	}
	figures := []map[string]any{}
	for _, key := range []string{"totals", "previous"} {
		m, ok := p[key].(map[string]any)
		if !ok {
			t.Errorf("%s: %s is %v, want an object of null figures", who, key, p[key])
			continue
		}
		figures = append(figures, m)
	}
	for _, key := range []string{"series", "previousSeries"} {
		points, _ := p[key].([]any)
		for _, point := range points {
			m, _ := point.(map[string]any)
			delete(m, "at")
			figures = append(figures, m)
		}
	}
	for _, m := range figures {
		for name, v := range m {
			if v != nil {
				t.Errorf("%s: %s is %v, want null", who, name, v)
			}
		}
	}
	for _, key := range []string{"campaigns", "automations", "audiences", "accounts"} {
		if list, ok := p[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s: %s is %v, want an empty list", who, key, p[key])
		}
	}
}

// ---------------------------------------------------------------------------
// The tests.
// ---------------------------------------------------------------------------

// THE RATES ARE BY COHORT, THE TOTALS BY DAY (statistics.go, "Two ways to
// count an open"). 100 campaign messages go out in the week: 41 are first
// opened in it and one the day after it ends, and three are clicked. Two
// messages sent the week before are first opened in this one. So the week's
// open rate is 42 in 100 -- the late open counts, the earlier week's opens do
// not -- while its unique opens are the 43 first opens dated in it. The week
// before has its two sends, both opened: a rate of 1, though neither open
// happened in it.
func TestTotalsAndCohortRates(t *testing.T) {
	f := newStatsFixture(t)
	a := statFact{account: f.id("a"), campaign: f.id("camp")}
	sent := f.times(100, kindSent, day("2041-06-12"), a)
	f.of(kindOpenUnique, sent[:41], day("2041-06-13"), a)
	f.of(kindOpenUnique, sent[41:42], day("2041-06-20"), a)
	f.of(kindClickUnique, sent[:3], day("2041-06-14"), a)
	f.times(60, kindOpen, day("2041-06-13"), a)
	f.times(5, kindClick, day("2041-06-14"), a)
	f.of(kindDelivered, sent[:90], day("2041-06-12"), a)
	f.of(kindUndelivered, sent[90:94], day("2041-06-13"), a)
	f.times(2, kindFailed, day("2041-06-12"), a)
	f.of(kindSuppressed, f.times(3, kindSkipped, day("2041-06-12"), a)[:1], day("2041-06-12"), a)
	f.times(2, kindUnsubscribe, day("2041-06-15"), a)
	f.times(1, kindBounce, day("2041-06-15"), a)
	f.times(1, kindComplaint, day("2041-06-15"), a)
	f.times(7, kindSubscribe, day("2041-06-15"), a)
	f.of(kindOpenUnique, f.times(2, kindSent, day("2041-06-08"), a), day("2041-06-12"), a)

	got := f.read(memberOf(a.account), nil, statsperiod.Last7Days)

	if want := "sent=100 delivered=90 undelivered=4 failed=2 skipped=3 suppressed=1 opens=60 clicks=5 uniqueOpens=43 uniqueClicks=3 unsubscribes=2 bounces=1 complaints=1 subscribes=7 openRate=0.42 clickRate=0.03"; totalsLine(got.Totals) != want {
		t.Errorf("totals:\n  got  %s\n  want %s", totalsLine(got.Totals), want)
	}
	if want := "sent=2 delivered=0 undelivered=0 failed=0 skipped=0 suppressed=0 opens=0 clicks=0 uniqueOpens=0 uniqueClicks=0 unsubscribes=0 bounces=0 complaints=0 subscribes=0 openRate=1 clickRate=0"; totalsLine(got.Previous) != want {
		t.Errorf("previous:\n  got  %s\n  want %s", totalsLine(got.Previous), want)
	}
	if got.Period != statsperiod.Last7Days || got.Bucket != statsperiod.BucketDay || !got.HasPrevious ||
		!got.Start.Equal(day("2041-06-11")) || !got.End.Equal(day("2041-06-18")) ||
		!got.PrevStart.Equal(day("2041-06-04")) || !got.PrevEnd.Equal(day("2041-06-11")) {
		t.Errorf("bounds %s %s [%s, %s) previous %v [%s, %s), want 7d by day [2041-06-11, 2041-06-18) after [2041-06-04, 2041-06-11)",
			got.Period, got.Bucket, got.Start, got.End, got.HasPrevious, got.PrevStart, got.PrevEnd)
	}
	if got.MeasuredSince == nil || got.MeasuredSince.After(statsBandStart) {
		t.Errorf("measuredSince %v, want a day no later than the band's anchor", got.MeasuredSince)
	}

	// The payload carries the same figures, as the client decodes them.
	raw, err := json.Marshal(got.Payload())
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	totals, _ := p["totals"].(map[string]any)
	previous, _ := p["previous"].(map[string]any)
	if p["period"] != "7d" || p["bucket"] != "day" || p["start"] != "2041-06-11T00:00:00Z" || p["end"] != "2041-06-18T00:00:00Z" ||
		p["hasPrevious"] != true || p["prevStart"] != "2041-06-04T00:00:00Z" || p["prevEnd"] != "2041-06-11T00:00:00Z" {
		t.Errorf("payload bounds %v %v [%v, %v) previous %v [%v, %v)", p["period"], p["bucket"], p["start"], p["end"], p["hasPrevious"], p["prevStart"], p["prevEnd"])
	}
	if totals["sent"] != 100.0 || totals["uniqueOpens"] != 43.0 || totals["openRate"] != 0.42 || totals["clickRate"] != 0.03 || previous["openRate"] != 1.0 {
		t.Errorf("payload totals %v previous %v, want 100 sent, 43 unique opens, rates 0.42 and 0.03, and a previous open rate of 1", totals, previous)
	}
	if series, _ := p["series"].([]any); len(series) != 7 {
		t.Errorf("payload series has %d points, want 7", len(series))
	}
}

// D18: only campaign mail is tracked, so only campaign mail is in a rate. 100
// campaign messages with 42 opened, beside 50 rule and 20 journey messages,
// still open at 0.42; a day that sent only journey mail has no rate though
// opens are counted on it; and an organization that sent only rule mail has
// no rate at all, never a rate of zero.
func TestRatesCountCampaignMailOnly(t *testing.T) {
	f := newStatsFixture(t)
	a := statFact{account: f.id("a"), campaign: f.id("camp")}
	f.of(kindOpenUnique, f.times(100, kindSent, day("2041-06-12"), a)[:42], day("2041-06-13"), a)
	f.times(50, kindSent, day("2041-06-12"), statFact{account: a.account, rule: f.id("rule")})
	f.times(20, kindSent, day("2041-06-13"), statFact{account: a.account, journey: f.id("journey")})
	b := statFact{account: f.id("b"), rule: f.id("rule-b")}
	f.times(50, kindSent, day("2041-06-12"), b)

	got := f.read(memberOf(a.account), nil, statsperiod.Last7Days)
	if num(got.Totals.Sent) != "170" || rate(got.Totals.OpenRate) != "0.42" || rate(got.Totals.ClickRate) != "0" {
		t.Errorf("a: %s, want 170 sent and rates of 0.42 and 0: the rates are of the 100 campaign messages", totalsLine(got.Totals))
	}
	wantSeries(t, "a's twelfth and thirteenth", got.Series[1:3], []string{
		"2041-06-12 sent=150 uniqueOpens=0 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=0 complaints=0 failed=0 suppressed=0 openRate=0.42 clickRate=0",
		"2041-06-13 sent=20 uniqueOpens=42 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=0 complaints=0 failed=0 suppressed=0 openRate=null clickRate=null",
	})
	if line := figuresLine(got.Campaigns); line != a.campaign+" sent=100 uniqueOpens=42 uniqueClicks=0 openRate=0.42 clickRate=0" {
		t.Errorf("campaigns %s, want the one campaign with its 100 messages", line)
	}

	rules := f.read(memberOf(b.account), nil, statsperiod.Last7Days)
	if num(rules.Totals.Sent) != "50" || rules.Totals.OpenRate != nil || rules.Totals.ClickRate != nil || num(rules.Totals.UniqueOpens) != "0" {
		t.Errorf("b: %s, want 50 sent, 0 unique opens and no rates: it sent no campaign mail", totalsLine(rules.Totals))
	}
}

// A series has a point per bucket, oldest first, with the delivery problems
// beside the sends; each point's rates are its own send day's cohort, so the
// twelfth's opens, which happen on the fourteenth, are the twelfth's rate and
// the fourteenth's unique opens. Twelve months have thirteen monthly points
// and no previous period.
func TestSeriesByDayAndByMonth(t *testing.T) {
	f := newStatsFixture(t)
	a := statFact{account: f.id("a"), campaign: f.id("camp")}
	twelfth := f.times(10, kindSent, day("2041-06-12"), a)
	f.of(kindOpenUnique, twelfth[:4], day("2041-06-14"), a)
	f.of(kindClickUnique, twelfth[:2], day("2041-06-14"), a)
	f.times(1, kindBounce, day("2041-06-12"), a)
	f.times(3, kindFailed, day("2041-06-12"), a)
	f.of(kindSuppressed, f.times(1, kindSkipped, day("2041-06-12"), a), day("2041-06-12"), a)
	f.times(1, kindUnsubscribe, day("2041-06-12"), a)
	rule := statFact{account: a.account, rule: f.id("rule")}
	f.times(5, kindSent, day("2041-06-14"), rule)
	f.times(2, kindComplaint, day("2041-06-14"), rule)
	f.times(2, kindSubscribe, day("2041-06-14"), statFact{account: a.account, audience: f.id("aud")})
	f.of(kindOpenUnique, f.times(6, kindSent, day("2041-06-17"), a)[:3], day("2041-06-17"), a)
	f.times(1, kindFailed, day("2041-06-17"), a)
	f.of(kindOpenUnique, f.times(4, kindSent, day("2041-06-06"), a)[:1], day("2041-06-06"), a)
	f.times(1, kindBounce, day("2041-06-06"), a)
	f.of(kindOpenUnique, f.times(3, kindSent, day("2040-07-15"), a)[:1], day("2040-08-02"), a)
	f.times(1, kindBounce, day("2041-05-31"), a)

	week := f.read(memberOf(a.account), nil, statsperiod.Last7Days)
	wantSeries(t, "the week", week.Series, []string{
		zeroPoint("2041-06-11"),
		"2041-06-12 sent=10 uniqueOpens=0 uniqueClicks=0 unsubscribes=1 subscribes=0 bounces=1 complaints=0 failed=3 suppressed=1 openRate=0.4 clickRate=0.2",
		zeroPoint("2041-06-13"),
		"2041-06-14 sent=5 uniqueOpens=4 uniqueClicks=2 unsubscribes=0 subscribes=2 bounces=0 complaints=2 failed=0 suppressed=0 openRate=null clickRate=null",
		zeroPoint("2041-06-15"),
		zeroPoint("2041-06-16"),
		"2041-06-17 sent=6 uniqueOpens=3 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=0 complaints=0 failed=1 suppressed=0 openRate=0.5 clickRate=0",
	})
	wantSeries(t, "the week before", week.PreviousSeries, []string{
		zeroPoint("2041-06-04"),
		zeroPoint("2041-06-05"),
		"2041-06-06 sent=4 uniqueOpens=1 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=1 complaints=0 failed=0 suppressed=0 openRate=0.25 clickRate=0",
		zeroPoint("2041-06-07"),
		zeroPoint("2041-06-08"),
		zeroPoint("2041-06-09"),
		zeroPoint("2041-06-10"),
	})

	months := f.read(memberOf(a.account), nil, statsperiod.Last12Months)
	want := []string{
		zeroPoint("2040-06-01"),
		"2040-07-01 sent=3 uniqueOpens=0 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=0 complaints=0 failed=0 suppressed=0 openRate=0.3333333333333333 clickRate=0",
		"2040-08-01 sent=0 uniqueOpens=1 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=0 complaints=0 failed=0 suppressed=0 openRate=null clickRate=null",
	}
	for _, m := range []string{"2040-09-01", "2040-10-01", "2040-11-01", "2040-12-01", "2041-01-01", "2041-02-01", "2041-03-01", "2041-04-01"} {
		want = append(want, zeroPoint(m))
	}
	want = append(want,
		"2041-05-01 sent=0 uniqueOpens=0 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=1 complaints=0 failed=0 suppressed=0 openRate=null clickRate=null",
		"2041-06-01 sent=25 uniqueOpens=8 uniqueClicks=2 unsubscribes=1 subscribes=2 bounces=2 complaints=2 failed=4 suppressed=1 openRate=0.4 clickRate=0.1",
	)
	wantSeries(t, "twelve months", months.Series, want)
	if months.Bucket != statsperiod.BucketMonth || months.HasPrevious || months.PreviousSeries != nil ||
		totalsLine(months.Previous) != totalsLine(EmailTotals{}) || !months.PrevStart.IsZero() || !months.PrevEnd.IsZero() {
		t.Errorf("twelve months: bucket %s, hasPrevious %v, previous %s, %d previous points, previous [%v, %v); want months and no previous period",
			months.Bucket, months.HasPrevious, totalsLine(months.Previous), len(months.PreviousSeries), months.PrevStart, months.PrevEnd)
	}
	if num(months.Totals.Sent) != "28" || num(months.Totals.Bounces) != "3" {
		t.Errorf("twelve months' totals %s, want 28 sent and 3 bounces", totalsLine(months.Totals))
	}
	p := months.Payload()
	for _, key := range []string{"previous", "previousSeries", "prevStart", "prevEnd"} {
		if v, present := p[key]; !present || v != nil {
			t.Errorf("twelve months' payload %s = %v (present %v), want present and null", key, v, present)
		}
	}
	if p["hasPrevious"] != false {
		t.Errorf("twelve months' payload hasPrevious = %v, want false", p["hasPrevious"])
	}
}

// D18: one row per rule and per journey with a send, a failure or a
// suppression in the period -- a rule whose every send failed included --
// with nothing from the week before and nothing of another organization's.
func TestAutomationsListRulesAndJourneysWithFailedAndSuppressed(t *testing.T) {
	f := newStatsFixture(t)
	account := f.id("a")
	r1 := statFact{account: account, rule: f.id("rule-1")}
	r2 := statFact{account: account, rule: f.id("rule-2")}
	j1 := statFact{account: account, journey: f.id("journey-1")}
	f.times(5, kindSent, day("2041-06-12"), r1)
	f.times(2, kindFailed, day("2041-06-13"), r1)
	f.of(kindSuppressed, f.times(2, kindSkipped, day("2041-06-13"), r1)[:1], day("2041-06-13"), r1)
	f.times(7, kindSent, day("2041-06-05"), r1)
	f.times(3, kindFailed, day("2041-06-14"), r2)
	f.times(4, kindSent, day("2041-06-15"), j1)
	f.of(kindSuppressed, f.times(2, kindSkipped, day("2041-06-15"), j1), day("2041-06-15"), j1)
	f.times(1, kindSent, day("2041-06-15"), statFact{account: account, campaign: f.id("camp")})
	f.times(9, kindSent, day("2041-06-12"), statFact{account: f.id("b"), rule: f.id("rule-b")})

	got := f.read(memberOf(account), nil, statsperiod.Last7Days)
	want := strings.Join([]string{
		"rule " + r1.rule + " sent=5 failed=2 suppressed=1",
		"journey " + j1.journey + " sent=4 failed=0 suppressed=2",
		"rule " + r2.rule + " sent=0 failed=3 suppressed=0",
	}, "; ")
	if line := automationsLine(got.Automations); line != want {
		t.Errorf("automations:\n  got  %s\n  want %s", line, want)
	}
	if line := totalsLine(got.Totals); !strings.HasPrefix(line, "sent=10 delivered=0 undelivered=0 failed=5 skipped=4 suppressed=3 ") {
		t.Errorf("totals %s, want 10 sent, 5 failed, 4 skipped and 3 of them suppressed", line)
	}
	p := got.Payload()
	rows, _ := p["automations"].([]any)
	if first, _ := rows[0].(map[string]any); first["kind"] != "rule" || first["id"] != r1.rule || first["failed"] != int64(2) {
		t.Errorf("payload automations[0] = %v, want rule %s with 2 failed", rows[0], r1.rule)
	}
}

// D18: one row per audience with subscription changes in the period, with
// its net change, largest first. A change whose recipient the rollup could
// not place, and the bounces and complaints no audience carries, are totals
// only; and an unsubscribe naming a campaign is no send of the campaign's.
func TestAudiencesCarryTheirNetChange(t *testing.T) {
	f := newStatsFixture(t)
	account := f.id("a")
	u1 := statFact{account: account, audience: f.id("aud-1")}
	u2 := statFact{account: account, audience: f.id("aud-2")}
	u3 := statFact{account: account, audience: f.id("aud-3")}
	f.times(5, kindSubscribe, day("2041-06-12"), u1)
	f.times(2, kindUnsubscribe, day("2041-06-13"), u1)
	f.times(1, kindSubscribe, day("2041-06-12"), u2)
	f.times(4, kindUnsubscribe, day("2041-06-16"), u2)
	f.times(1, kindUnsubscribe, day("2041-06-14"), u3)
	f.times(3, kindSubscribe, day("2041-06-08"), statFact{account: account, audience: f.id("aud-4")})
	f.times(1, kindUnsubscribe, day("2041-06-14"), statFact{account: account, campaign: f.id("camp")})
	f.times(1, kindBounce, day("2041-06-14"), statFact{account: account})
	f.times(1, kindComplaint, day("2041-06-14"), statFact{account: account})

	got := f.read(memberOf(account), nil, statsperiod.Last7Days)
	want := strings.Join([]string{
		u1.audience + " subscribes=5 unsubscribes=2 net=3",
		u3.audience + " subscribes=0 unsubscribes=1 net=-1",
		u2.audience + " subscribes=1 unsubscribes=4 net=-3",
	}, "; ")
	if line := audiencesLine(got.Audiences); line != want {
		t.Errorf("audiences:\n  got  %s\n  want %s", line, want)
	}
	if num(got.Totals.Subscribes) != "6" || num(got.Totals.Unsubscribes) != "8" || num(got.Totals.Bounces) != "1" || num(got.Totals.Complaints) != "1" {
		t.Errorf("totals %s, want 6 subscribes, 8 unsubscribes, 1 bounce and 1 complaint", totalsLine(got.Totals))
	}
	if len(got.Campaigns) != 0 {
		t.Errorf("campaigns %s, want none: an unsubscribe naming a campaign is not one of its sends", figuresLine(got.Campaigns))
	}
	rows, _ := got.Payload()["audiences"].([]any)
	if last, _ := rows[2].(map[string]any); last["net"] != int64(-3) {
		t.Errorf("payload audiences[2] = %v, want a net of -3", rows[2])
	}
}

// Campaigns with sends in the period, by unique opens, then unique clicks,
// then sends and id; fifty of them. A campaign sent the week before is not
// listed though its opens are this week's, and consent facts naming a
// campaign are not its figures.
func TestCampaignsRankByUniqueOpensThenUniqueClicks(t *testing.T) {
	f := newStatsFixture(t)
	account := f.id("a")
	campaign := func(name string, sends, opened, clicked int) statFact {
		x := statFact{account: account, campaign: f.id(name)}
		ids := f.times(sends, kindSent, day("2041-06-12"), x)
		f.of(kindOpenUnique, ids[:opened], day("2041-06-13"), x)
		f.of(kindClickUnique, ids[:clicked], day("2041-06-13"), x)
		return x
	}
	c1 := campaign("c1", 10, 5, 1)
	c2 := campaign("c2", 20, 5, 3)
	c3 := campaign("c3", 8, 6, 0)
	c4 := statFact{account: account, campaign: f.id("c4")}
	f.of(kindOpenUnique, f.times(9, kindSent, day("2041-06-08"), c4), day("2041-06-12"), c4)
	f.times(3, kindUnsubscribe, day("2041-06-13"), c1)
	f.times(1, kindComplaint, day("2041-06-13"), c1)
	var extras []string
	for i := 1; i <= campaignsKept; i++ {
		x := statFact{account: account, campaign: f.id(fmt.Sprintf("x%02d", i))}
		f.times(1, kindSent, day("2041-06-14"), x)
		extras = append(extras, x.campaign)
	}

	got := f.read(memberOf(account), nil, statsperiod.Last7Days)
	if len(got.Campaigns) != campaignsKept {
		t.Fatalf("%d campaigns, want the %d kept", len(got.Campaigns), campaignsKept)
	}
	want := strings.Join([]string{
		c3.campaign + " sent=8 uniqueOpens=6 uniqueClicks=0 openRate=0.75 clickRate=0",
		c2.campaign + " sent=20 uniqueOpens=5 uniqueClicks=3 openRate=0.25 clickRate=0.15",
		c1.campaign + " sent=10 uniqueOpens=5 uniqueClicks=1 openRate=0.5 clickRate=0.1",
	}, "; ")
	if line := figuresLine(got.Campaigns[:3]); line != want {
		t.Errorf("the leading campaigns:\n  got  %s\n  want %s", line, want)
	}
	for i, row := range got.Campaigns[3:] {
		if row.ID != extras[i] {
			t.Errorf("campaign %d is %s, want %s: past the ranked ones, the fifty kept are by sends and then by id", i+3, row.ID, extras[i])
			break
		}
	}
	for _, row := range got.Campaigns {
		if row.ID == c4.campaign {
			t.Errorf("%s is listed, want it left out: it sent nothing in the period", c4.campaign)
		}
	}
}

// THE OWNER'S MOST IMPORTANT PROPERTY (D11). A member of organization a who
// names b gets what somebody who reaches nothing gets -- every figure null,
// no measuredSince -- never a refusal and never b's figures. A named list
// with no usable id names nothing, never everything; naming nobody reads a
// alone, and so does naming both. Asked of the handler, as the wire asks it,
// with a resolver that answers a; and a grant that resolved to nothing, or no
// resolver at all, reaches nothing.
func TestAMemberGetsOnlyTheirOrganization(t *testing.T) {
	f := newStatsFixture(t)
	a := statFact{account: f.id("a"), campaign: f.id("camp-a")}
	b := statFact{account: f.id("b"), campaign: f.id("camp-b")}
	f.of(kindOpenUnique, f.times(10, kindSent, day("2041-06-12"), a)[:3], day("2041-06-13"), a)
	f.of(kindOpenUnique, f.times(20, kindSent, day("2041-06-12"), b)[:8], day("2041-06-13"), b)
	f.times(4, kindUnsubscribe, day("2041-06-13"), b)
	member := f.integration(fakeGrants{memberOf(a.account)})
	ctx := callerCtx(f.id("member"), auth.RoleWriter)

	for name, args := range map[string]map[string]any{
		"b by id":                  {"accountIds": []any{b.account}},
		"b as one string":          {"accountIds": b.account},
		"a blank id":               {"accountIds": []any{""}},
		"a non-string id":          {"accountIds": []any{42}},
		"blank, null and a number": {"accountIds": []any{" ", nil, 3.5}},
	} {
		args["period"] = "7d"
		wantNothingInPayload(t, name, call(t, member, ctx, args))
	}
	for name, args := range map[string]map[string]any{
		"no accountIds":    {},
		"an empty list":    {"accountIds": []any{}},
		"a by id":          {"accountIds": []any{a.account}},
		"a and b":          {"accountIds": []any{a.account, b.account}},
		"a beside a blank": {"accountIds": []any{"", a.account}},
	} {
		args["period"] = "7d"
		p := call(t, member, ctx, args)
		if sent := totalSent(p); sent != 10.0 {
			t.Errorf("%s: %v sent, want a's 10", name, sent)
		}
		if accounts, ok := p["accounts"].([]any); !ok || len(accounts) != 0 {
			t.Errorf("%s: accounts %v, want an empty list: one organization is in scope", name, p["accounts"])
		}
	}
	wantNothing(t, "a member naming b", f.read(memberOf(a.account), []string{b.account}, statsperiod.Last7Days))
	if got := f.read(memberOf(a.account), []string{}, statsperiod.Last7Days); figuresLine(got.Campaigns) != a.campaign+" sent=10 uniqueOpens=3 uniqueClicks=0 openRate=0.3 clickRate=0" || num(got.Totals.Unsubscribes) != "0" {
		t.Errorf("a member naming none: campaigns %s, totals %s; want a's campaign alone and none of b's unsubscribes", figuresLine(got.Campaigns), totalsLine(got.Totals))
	}

	for name, i := range map[string]*Integration{
		"a grant that resolved to nothing": f.integration(fakeGrants{}),
		"no grant resolver":                f.integration(nil),
	} {
		wantNothingInPayload(t, name, call(t, i, ctx, map[string]any{"accountIds": []any{a.account}, "period": "7d"}))
		wantNothingInPayload(t, name+", naming none", call(t, i, ctx, map[string]any{"period": "7d"}))
	}
}

// Staff who name nothing reach every organization, and the facts older than
// organization attribution, which no organization's row holds; naming
// organizations narrows them to those. A named list with no usable id names
// nothing for staff too: asked of the handler, because the wire's raw
// argument is where "named" is decided, and of staff, who reach everything,
// so a widened read cannot hide.
func TestStaffGetEveryOrganizationAndTheUnattributed(t *testing.T) {
	f := newStatsFixture(t)
	a := statFact{account: f.id("a"), campaign: f.id("camp-a")}
	b := statFact{account: f.id("b"), campaign: f.id("camp-b")}
	old := statFact{campaign: f.id("camp-old")}
	f.times(3, kindSent, day("2041-06-12"), a)
	f.times(4, kindSent, day("2041-06-13"), b)
	f.times(5, kindSent, day("2041-06-14"), old)
	f.times(2, kindUnsubscribe, day("2041-06-14"), old)

	all := f.read(staff(), nil, statsperiod.Last7Days)
	if num(all.Totals.Sent) != "12" || num(all.Totals.Unsubscribes) != "2" {
		t.Errorf("every organization: %s, want 12 sent and 2 unsubscribes, the unattributed included", totalsLine(all.Totals))
	}
	if line := figuresLine(all.Campaigns); line != strings.Join([]string{
		old.campaign + " sent=5 uniqueOpens=0 uniqueClicks=0 openRate=0 clickRate=0",
		b.campaign + " sent=4 uniqueOpens=0 uniqueClicks=0 openRate=0 clickRate=0",
		a.campaign + " sent=3 uniqueOpens=0 uniqueClicks=0 openRate=0 clickRate=0",
	}, "; ") {
		t.Errorf("every organization's campaigns: %s", line)
	}
	if line := figuresLine(all.Accounts); line != strings.Join([]string{
		b.account + " sent=4 uniqueOpens=0 uniqueClicks=0 openRate=0 clickRate=0",
		a.account + " sent=3 uniqueOpens=0 uniqueClicks=0 openRate=0 clickRate=0",
	}, "; ") {
		t.Errorf("every organization's rows: %s, want b and a and no row for the unattributed", line)
	}

	one := f.read(staff(), []string{a.account}, statsperiod.Last7Days)
	if num(one.Totals.Sent) != "3" || len(one.Accounts) != 0 {
		t.Errorf("staff naming a: %s with rows %s, want a's 3 and no rows", totalsLine(one.Totals), figuresLine(one.Accounts))
	}
	two := f.read(staff(), []string{b.account, a.account}, statsperiod.Last7Days)
	if num(two.Totals.Sent) != "7" || len(two.Accounts) != 2 {
		t.Errorf("staff naming a and b: %s with rows %s, want 7 and two rows, without the unattributed", totalsLine(two.Totals), figuresLine(two.Accounts))
	}

	handler := f.integration(fakeGrants{staff()})
	ctx := callerCtx(f.id("developer"), auth.RoleDeveloper)
	for name, ids := range map[string]any{
		"a blank id":      []any{""},
		"a non-string id": []any{7},
		"one blank":       "  ",
	} {
		wantNothingInPayload(t, name, call(t, handler, ctx, map[string]any{"accountIds": ids, "period": "7d"}))
	}
	if sent := totalSent(call(t, handler, ctx, map[string]any{"period": "7d"})); sent != 12.0 {
		t.Errorf("staff naming none through the handler: %v sent, want 12", sent)
	}
}

// Accounts lists each organization only when more than one is in scope:
// every send is in its row's sent, as in the totals, and its rates are of its
// campaign mail -- none when it sent none.
func TestAccountsListOnlyWhenMoreThanOneIsInScope(t *testing.T) {
	f := newStatsFixture(t)
	a := statFact{account: f.id("a"), campaign: f.id("camp-a")}
	b := f.id("b")
	f.of(kindOpenUnique, f.times(10, kindSent, day("2041-06-12"), a)[:4], day("2041-06-13"), a)
	f.times(5, kindSent, day("2041-06-12"), statFact{account: a.account, rule: f.id("rule-a")})
	f.times(5, kindSent, day("2041-06-13"), statFact{account: b, rule: f.id("rule-b")})

	want := strings.Join([]string{
		a.account + " sent=15 uniqueOpens=4 uniqueClicks=0 openRate=0.4 clickRate=0",
		b + " sent=5 uniqueOpens=0 uniqueClicks=0 openRate=null clickRate=null",
	}, "; ")
	for name, s := range map[string]EmailStatistics{
		"a member of both naming none": f.read(memberOf(a.account, b), nil, statsperiod.Last7Days),
		"staff naming both":            f.read(staff(), []string{a.account, b}, statsperiod.Last7Days),
	} {
		if line := figuresLine(s.Accounts); line != want {
			t.Errorf("%s: rows\n  got  %s\n  want %s", name, line, want)
		}
	}
	for name, s := range map[string]EmailStatistics{
		"a member of both naming one": f.read(memberOf(a.account, b), []string{a.account}, statsperiod.Last7Days),
		"a member of one":             f.read(memberOf(a.account), nil, statsperiod.Last7Days),
		"staff naming one":            f.read(staff(), []string{b}, statsperiod.Last7Days),
	} {
		if len(s.Accounts) != 0 {
			t.Errorf("%s: rows %s, want none: one organization is in scope", name, figuresLine(s.Accounts))
		}
	}
	if list, ok := f.read(memberOf(a.account), nil, statsperiod.Last7Days).Payload()["accounts"].([]any); !ok || len(list) != 0 {
		t.Errorf("the payload's accounts for one organization is %v, want an empty list", list)
	}
}

// No fact at all is no measurement at all: every figure null, every point of
// every bucket null, nothing listed and no measuredSince -- for staff reading
// everything, over a month and over twelve.
func TestNoFactsMeansNothingMeasured(t *testing.T) {
	f := newEmptyTableFixture(t)
	for _, p := range []statsperiod.Period{statsperiod.Last30Days, statsperiod.Last12Months} {
		got := f.read(staff(), nil, p)
		wantNothing(t, string(p), got)
		b := statsperiod.For(p, statsNow)
		if len(got.Series) != b.Buckets {
			t.Errorf("%s: %d points, want %d", p, len(got.Series), b.Buckets)
		}
		for i, at := range b.Starts() {
			if i < len(got.Series) && !got.Series[i].At.Equal(at) {
				t.Errorf("%s: point %d starts %s, want %s", p, i, got.Series[i].At, at)
			}
		}
		if b.HasPrevious != (got.PreviousSeries != nil) || (b.HasPrevious && len(got.PreviousSeries) != b.Buckets) {
			t.Errorf("%s: %d previous points, want %d", p, len(got.PreviousSeries), b.Buckets)
		}
		payload := got.Payload()
		raw, _ := json.Marshal(payload)
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if b.HasPrevious {
			wantNothingInPayload(t, string(p)+" as the client reads it", decoded)
		}
		if totals, _ := decoded["totals"].(map[string]any); totals["sent"] != nil || totals["openRate"] != nil {
			t.Errorf("%s: payload totals %v, want null figures", p, totals)
		}
	}
}

// A bucket that ends on or before the cluster's first fact is null -- nothing
// was recorded then -- and one after it with no fact is a measured zero. The
// month that holds the first fact is measured though it began before it.
func TestBucketsBeforeTheFirstFactAreNullAndAfterItZero(t *testing.T) {
	f := newEmptyTableFixture(t)
	x := statFact{account: f.id("a"), campaign: f.id("camp")}
	f.of(kindOpenUnique, f.times(2, kindSent, day("2041-06-14"), x)[:1], day("2041-06-14"), x)

	week := f.read(staff(), nil, statsperiod.Last7Days)
	if week.MeasuredSince == nil || !week.MeasuredSince.Equal(day("2041-06-14")) {
		t.Fatalf("measuredSince %v, want 2041-06-14, the first fact", week.MeasuredSince)
	}
	first := "2041-06-14 sent=2 uniqueOpens=1 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=0 complaints=0 failed=0 suppressed=0 openRate=0.5 clickRate=0"
	wantSeries(t, "the week", week.Series, []string{
		nullPoint("2041-06-11"), nullPoint("2041-06-12"), nullPoint("2041-06-13"),
		first,
		zeroPoint("2041-06-15"), zeroPoint("2041-06-16"), zeroPoint("2041-06-17"),
	})
	if want := "sent=2 delivered=0 undelivered=0 failed=0 skipped=0 suppressed=0 opens=0 clicks=0 uniqueOpens=1 uniqueClicks=0 unsubscribes=0 bounces=0 complaints=0 subscribes=0 openRate=0.5 clickRate=0"; totalsLine(week.Totals) != want {
		t.Errorf("the week's totals:\n  got  %s\n  want %s", totalsLine(week.Totals), want)
	}
	if line := totalsLine(week.Previous); line != totalsLine(EmailTotals{}) {
		t.Errorf("the week before ends before the first fact, so nothing in it was measured; got %s", line)
	}
	for i, p := range week.PreviousSeries {
		if line := pointLine(p); line != nullPoint(p.At.Format(time.DateOnly)) {
			t.Errorf("the week before, point %d: %s, want null", i, line)
		}
	}

	months := f.read(staff(), nil, statsperiod.Last12Months)
	for i, p := range months.Series[:12] {
		if line := pointLine(p); line != nullPoint(p.At.Format(time.DateOnly)) {
			t.Errorf("twelve months, point %d: %s, want null: the month ends before the first fact", i, line)
		}
	}
	if line := pointLine(months.Series[12]); line != strings.Replace(first, "2041-06-14", "2041-06-01", 1) {
		t.Errorf("the month holding the first fact: %s, want it measured", line)
	}
}

// The resolver the plug-in takes from its engine is the engine's own grant:
// a member placed in organization a's group through the real membership rows
// reads a and nothing of b, staff read b, and somebody in no group reads
// nothing. Through the handler, with the engine as the GrantResolver.
func TestTheReadNarrowsUnderTheEnginesOwnGrant(t *testing.T) {
	eng, _ := statsEngine(t)
	f := newStatsFixture(t)
	a, b := f.id("org-a"), f.id("org-b")
	joinOrganization(t, eng, a, f.id("member"))
	f.times(10, kindSent, day("2041-06-12"), statFact{account: a, campaign: f.id("camp-a")})
	f.times(20, kindSent, day("2041-06-12"), statFact{account: b, campaign: f.id("camp-b")})

	i := NewIntegration(eng, func() *bun.DB { return f.db }, quietLogger())
	i.now = func() time.Time { return statsNow }
	if i.grants == nil {
		t.Fatal("NewIntegration did not take the engine as its grant resolver, so every read answers nothing")
	}
	member := callerCtx(f.id("member"), auth.RoleWriter)
	wantNothingInPayload(t, "a member naming b", call(t, i, member, map[string]any{"accountIds": []any{b}, "period": "7d"}))
	if sent := totalSent(call(t, i, member, map[string]any{"period": "7d"})); sent != 10.0 {
		t.Errorf("a member naming none: %v sent, want a's 10", sent)
	}
	if sent := totalSent(call(t, i, callerCtx(f.id("developer"), auth.RoleDeveloper), map[string]any{"accountIds": []any{b}, "period": "7d"})); sent != 20.0 {
		t.Errorf("staff naming b: %v sent, want b's 20", sent)
	}
	wantNothingInPayload(t, "somebody in no group naming a", call(t, i, callerCtx(f.id("stranger"), auth.RoleWriter), map[string]any{"accountIds": []any{a}, "period": "7d"}))
}

// The builtin is declared, dispatched and answered through the engine: one
// integration:emailStats:statistics row for anybody, and a period the read
// does not offer refused with a sentence naming the ones it does.
func TestTheStatisticsBuiltinAnswersOneNodeThroughTheEngine(t *testing.T) {
	eng, _ := statsEngine(t)
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("the engine's boot-time executor audit refuses this wiring: %v", err)
	}
	stranger := callerCtx(fmt.Sprintf("%snobody-%d", statsSourcePrefix, time.Now().UnixNano()), auth.RoleWriter)
	res, err := eng.Execute(stranger, `builtin emailStatistics(accountIds: ["`+statsSourcePrefix+`nowhere"], period: "30d")`)
	if err != nil {
		t.Fatalf("builtin emailStatistics: %v", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 || rows[0]["concept"] != statisticsConcept {
		t.Fatalf("the builtin answered %v, want one %s row", rows, statisticsConcept)
	}
	raw, _ := json.Marshal(rows[0])
	if !strings.Contains(string(raw), `"totals"`) || !strings.Contains(string(raw), `"previousSeries"`) {
		t.Errorf("the row is %s, want the statistics payload", raw)
	}

	for _, period := range []string{"24h", "1y"} {
		_, err := eng.Execute(stranger, `builtin emailStatistics(period: "`+period+`")`)
		if err == nil || !strings.Contains(err.Error(), "7d, 30d, 90d or 12m") {
			t.Errorf("period %q answered %v, want a refusal naming 7d, 30d, 90d or 12m", period, err)
		}
	}
}

// THE POOL'S STATEMENT TIMEOUT STANDS. The plug-in reads through the request
// pool, whose backends carry an 8 s server statement_timeout under pgdriver's
// 10 s client read deadline: a statement the read let run past the second
// would be cut off by the client and run on in the server, holding the read's
// snapshot. So no statement the read issues moves the server's limit.
func TestTheReadKeepsThePoolsStatementTimeout(t *testing.T) {
	f := newStatsFixture(t)
	a := statFact{account: f.id("a"), campaign: f.id("camp")}
	f.of(kindOpenUnique, f.times(3, kindSent, day("2041-06-12"), a)[:1], day("2041-06-13"), a)
	f.times(1, kindSubscribe, day("2041-06-13"), statFact{account: a.account, audience: f.id("aud")})
	db, rec := recordedDB(t)
	got, err := readStatistics(context.Background(), db, staff(), nil, statsperiod.Last7Days, statsNow)
	if err != nil {
		t.Fatal(err)
	}
	if num(got.Totals.Sent) == "null" || len(got.Campaigns) == 0 || len(got.Audiences) == 0 {
		t.Fatalf("the read answered %s with %d campaigns and %d audiences; it must read everything it reads for this to prove anything", totalsLine(got.Totals), len(got.Campaigns), len(got.Audiences))
	}
	wantThePoolsTimeout(t, "the read", rec, "email_stat_fact")
}

// firstRecorded writes the cluster's first fact, on a day before the periods a
// test reads, so measuredSince does not decide its buckets and the walks'
// coverage does. It belongs to an organization of its own, which no reader in
// these tests reaches but staff.
func (f *statsFixture) firstRecorded(on time.Time) {
	f.t.Helper()
	f.times(1, kindSkipped, on, statFact{account: f.id("first")})
}

// A figure is measured only where the walks of its sources have reached
// (statistics.go, "Absent is never zero"). The deliveries are walked through
// the read's clock and the engagement events only through the end of the
// fourteenth: the last three days' unique opens and rates are null, though
// their sends are measured, and the week's opens and rates are null with them.
// The subscribes are the consent walk's, which is current, and the campaigns
// list, which counts opens, is not listed for a week whose opens are not all
// read. The week before, which ends before the engagement watermark, is
// measured.
func TestAFigureIsNullWhereItsSourcesWalkHasNotReached(t *testing.T) {
	f := newEmptyTableFixture(t)
	f.firstRecorded(day("2041-06-01"))
	a := statFact{account: f.id("a"), campaign: f.id("camp")}
	aud := statFact{account: a.account, audience: f.id("aud")}
	week := []string{"2041-06-11", "2041-06-12", "2041-06-13", "2041-06-14", "2041-06-15", "2041-06-16", "2041-06-17"}
	for _, d := range week {
		f.of(kindOpenUnique, f.times(10, kindSent, day(d), a)[:4], day(d), a)
		f.times(1, kindSubscribe, day(d), aud)
	}
	f.walked(sourceEngagement, day("2041-06-15"))

	got := f.read(staff(), nil, statsperiod.Last7Days)
	var want []string
	for i, d := range week {
		if i < 4 {
			want = append(want, d+" sent=10 uniqueOpens=4 uniqueClicks=0 unsubscribes=0 subscribes=1 bounces=0 complaints=0 failed=0 suppressed=0 openRate=0.4 clickRate=0")
			continue
		}
		want = append(want, d+" sent=10 uniqueOpens=null uniqueClicks=null unsubscribes=0 subscribes=1 bounces=0 complaints=0 failed=0 suppressed=0 openRate=null clickRate=null")
	}
	wantSeries(t, "the week", got.Series, want)
	if want := "sent=70 delivered=0 undelivered=0 failed=0 skipped=0 suppressed=0 opens=null clicks=null uniqueOpens=null uniqueClicks=null unsubscribes=0 bounces=0 complaints=0 subscribes=7 openRate=null clickRate=null"; totalsLine(got.Totals) != want {
		t.Errorf("the week's totals:\n  got  %s\n  want %s", totalsLine(got.Totals), want)
	}
	if want := "sent=0 delivered=0 undelivered=0 failed=0 skipped=0 suppressed=0 opens=0 clicks=0 uniqueOpens=0 uniqueClicks=0 unsubscribes=0 bounces=0 complaints=0 subscribes=0 openRate=null clickRate=null"; totalsLine(got.Previous) != want {
		t.Errorf("the week before ends before the engagement watermark, so it is measured:\n  got  %s\n  want %s", totalsLine(got.Previous), want)
	}
	if len(got.Campaigns) != 0 || len(got.Accounts) != 0 {
		t.Errorf("campaigns %s and accounts %s are listed, want neither: their opens are not all read", figuresLine(got.Campaigns), figuresLine(got.Accounts))
	}
	if line := audiencesLine(got.Audiences); line != aud.audience+" subscribes=7 unsubscribes=0 net=7" {
		t.Errorf("audiences %s, want the one audience: its consent walk is current", line)
	}
}

// A healthy rollup's watermark is up to one run behind the read: the day under
// way and the week are measured with it, or every read between two runs would
// show today as missing. A watermark further behind than coverageGrace belongs
// to a rollup that has stopped, and the day under way, and the week that ends
// with it, are not measured; yesterday, which ended long before, still is.
func TestAWatermarkARunBehindStillCountsTheDayUnderWay(t *testing.T) {
	f := newEmptyTableFixture(t)
	f.firstRecorded(day("2041-06-01"))
	f.times(3, kindSent, day("2041-06-17"), statFact{account: f.id("a"), campaign: f.id("camp")})

	f.walked(sourceDelivery, statsNow.Add(-5*time.Minute))
	got := f.read(staff(), nil, statsperiod.Last7Days)
	if num(got.Series[6].Sent) != "3" || num(got.Totals.Sent) != "3" {
		t.Errorf("a watermark one run behind: today %s, the week %s; want both measured", pointLine(got.Series[6]), totalsLine(got.Totals))
	}

	f.walked(sourceDelivery, statsNow.Add(-coverageGrace-time.Minute))
	got = f.read(staff(), nil, statsperiod.Last7Days)
	if got.Series[6].Sent != nil || got.Totals.Sent != nil {
		t.Errorf("a watermark past the grace: today %s, the week %s; want both unmeasured", pointLine(got.Series[6]), totalsLine(got.Totals))
	}
	if num(got.Series[5].Sent) != "0" {
		t.Errorf("yesterday %s, want it measured: it ended long before the watermark", pointLine(got.Series[5]))
	}
}

// A FINISHED DAY NEEDS ITS SOURCES WALKED THROUGH ITS END. The grace is for the
// range under way, which a healthy rollup is always up to a run behind; a day
// that ended long before the read, walked to ten minutes short of its end -- a
// rollup that stopped in its last quarter hour -- is not measured, rather than
// measured and short while every bucket after it is null. The day before it,
// walked through, is measured.
func TestAFinishedDayIsMeasuredOnlyOnceItsSourcesAreWalkedThroughItsEnd(t *testing.T) {
	f := newEmptyTableFixture(t)
	f.firstRecorded(day("2041-06-01"))
	a := statFact{account: f.id("a"), campaign: f.id("camp")}
	f.times(3, kindSent, day("2041-06-14"), a)
	f.times(2, kindSent, day("2041-06-15"), a)
	f.walked(sourceDelivery, day("2041-06-16").Add(-10*time.Minute))

	got := f.read(staff(), nil, statsperiod.Last7Days)
	if p := got.Series[3]; num(p.Sent) != "3" {
		t.Errorf("the fourteenth, walked through: %s; want its 3 sends measured", pointLine(p))
	}
	if p := got.Series[4]; p.Sent != nil {
		t.Errorf("the fifteenth, walked to ten minutes before its end: %s; want its sends null", pointLine(p))
	}
	if got.Totals.Sent != nil {
		t.Errorf("the week, walked only part way: %s; want its sends null", totalsLine(got.Totals))
	}
}

// A source the rollup has never walked has nothing measured: every figure that
// depends on it is null, in every bucket and in both periods, and its list is
// not listed, while the figures of the walked sources are read as ever.
func TestASourceNeverWalkedLeavesWhatDependsOnItNull(t *testing.T) {
	f := newEmptyTableFixture(t)
	f.firstRecorded(day("2041-06-01"))
	a := statFact{account: f.id("a"), campaign: f.id("camp")}
	f.of(kindOpenUnique, f.times(10, kindSent, day("2041-06-12"), a)[:5], day("2041-06-13"), a)
	f.times(3, kindUnsubscribe, day("2041-06-13"), statFact{account: a.account, audience: f.id("aud")})
	f.neverWalked(sourceConsent)

	got := f.read(staff(), nil, statsperiod.Last7Days)
	for name, totals := range map[string]EmailTotals{"the week": got.Totals, "the week before": got.Previous} {
		if totals.Unsubscribes != nil || totals.Subscribes != nil || totals.Bounces != nil || totals.Complaints != nil {
			t.Errorf("%s: %s, want every consent figure null: the consent source was never walked", name, totalsLine(totals))
		}
	}
	if num(got.Totals.Sent) != "10" || rate(got.Totals.OpenRate) != "0.5" || num(got.Totals.UniqueOpens) != "5" {
		t.Errorf("the week: %s, want the walked sources' 10 sent and 5 opened", totalsLine(got.Totals))
	}
	for i, p := range append(append([]EmailPoint{}, got.Series...), got.PreviousSeries...) {
		if p.Unsubscribes != nil || p.Subscribes != nil || p.Bounces != nil || p.Complaints != nil {
			t.Errorf("point %d: %s, want its consent figures null", i, pointLine(p))
		}
		if p.Sent == nil {
			t.Errorf("point %d: %s, want its sends measured", i, pointLine(p))
		}
	}
	if len(got.Audiences) != 0 {
		t.Errorf("audiences %s, want none listed: the consent source was never walked", audiencesLine(got.Audiences))
	}
	if len(got.Campaigns) != 1 {
		t.Errorf("campaigns %s, want the one campaign: its sources are walked", figuresLine(got.Campaigns))
	}
}

// A source whose walk was started later -- a first walk that could not read
// its first page over a longer history (rollup.go) -- has nothing measured
// before its start: the deliveries walk from the fourteenth, so the days
// before have null sends and rates, while their opens, whose walk started at
// the horizon, are measured. The week and the week before both begin before
// the start, so their delivery figures and their rates are null, and the
// lists that carry sends are not listed.
func TestDaysBeforeASourcesWalkStartAreNull(t *testing.T) {
	f := newEmptyTableFixture(t)
	f.firstRecorded(day("2041-06-01"))
	a := statFact{account: f.id("a"), campaign: f.id("camp")}
	week := []string{"2041-06-11", "2041-06-12", "2041-06-13", "2041-06-14", "2041-06-15", "2041-06-16", "2041-06-17"}
	for _, d := range week {
		f.of(kindOpenUnique, f.times(10, kindSent, day(d), a)[:4], day(d), a)
	}
	f.times(2, kindSubscribe, day("2041-06-12"), statFact{account: a.account, audience: f.id("aud")})
	f.walksFrom(sourceDelivery, day("2041-06-14"))

	got := f.read(staff(), nil, statsperiod.Last7Days)
	var want []string
	for i, d := range week {
		subscribes := "0"
		if d == "2041-06-12" {
			subscribes = "2"
		}
		if i < 3 {
			want = append(want, d+" sent=null uniqueOpens=4 uniqueClicks=0 unsubscribes=0 subscribes="+subscribes+" bounces=0 complaints=0 failed=null suppressed=null openRate=null clickRate=null")
			continue
		}
		want = append(want, d+" sent=10 uniqueOpens=4 uniqueClicks=0 unsubscribes=0 subscribes=0 bounces=0 complaints=0 failed=0 suppressed=0 openRate=0.4 clickRate=0")
	}
	wantSeries(t, "the week", got.Series, want)
	for name, totals := range map[string]EmailTotals{"the week": got.Totals, "the week before": got.Previous} {
		if totals.Sent != nil || totals.OpenRate != nil || totals.Opens == nil || totals.Subscribes == nil {
			t.Errorf("%s: %s, want its delivery figures and rates null and its opens and subscribes measured", name, totalsLine(totals))
		}
	}
	if len(got.Campaigns)+len(got.Automations)+len(got.Accounts) != 0 || len(got.Audiences) != 1 {
		t.Errorf("campaigns %s, automations %s, accounts %s, audiences %s: want only the audiences, whose walk began before the week",
			figuresLine(got.Campaigns), automationsLine(got.Automations), figuresLine(got.Accounts), audiencesLine(got.Audiences))
	}
}

// A source walked with nothing in it is measured, and reads zero: the walk
// recorded how far it read though it found no row (rollup.go), which is what
// tells "walked, nothing there" from "never walked". Deliveries and nothing
// else: no open is a 0% open rate, not a missing one.
func TestASourceWalkedWithNoRowsReadsMeasuredZeros(t *testing.T) {
	f := newEmptyTableFixture(t)
	f.firstRecorded(day("2041-06-01"))
	f.times(10, kindSent, day("2041-06-12"), statFact{account: f.id("a"), campaign: f.id("camp")})

	got := f.read(staff(), nil, statsperiod.Last7Days)
	if want := "sent=10 delivered=0 undelivered=0 failed=0 skipped=0 suppressed=0 opens=0 clicks=0 uniqueOpens=0 uniqueClicks=0 unsubscribes=0 bounces=0 complaints=0 subscribes=0 openRate=0 clickRate=0"; totalsLine(got.Totals) != want {
		t.Errorf("the week:\n  got  %s\n  want %s", totalsLine(got.Totals), want)
	}
	for i, p := range got.Series {
		if p.UniqueOpens == nil || p.Subscribes == nil {
			t.Errorf("point %d: %s, want the walked sources' figures measured", i, pointLine(p))
		}
	}
}

// enginePayload asks the engine itself, as the wire does, and answers the one
// row's statistics payload.
func enginePayload(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, call string) map[string]any {
	t.Helper()
	res, err := eng.Execute(ctx, call)
	if err != nil {
		t.Fatalf("%s: %v", call, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 || rows[0]["concept"] != statisticsConcept {
		t.Fatalf("%s answered %v, want one %s row", call, rows, statisticsConcept)
	}
	if payload, ok := rows[0]["payload"].(map[string]any); ok {
		return payload
	}
	return rows[0]
}

// THE OWNER'S MOST IMPORTANT PROPERTY, through the engine's own registered
// plug-in, as the OS asks it: a member of organization a reads a's figures and
// nothing of b's, whether they name nobody or name b. The registered plug-in
// counts its period back from the real clock, so these facts are dated today
// rather than in the band, and every source is walked through now.
func TestAMemberReadsTheirOwnOrganizationThroughTheEngine(t *testing.T) {
	eng, db := statsEngine(t)
	f := newClockFixture(t)
	a, b, member := f.id("org-a"), f.id("org-b"), f.id("member")
	joinOrganization(t, eng, a, member)
	today := dayOf(time.Now())
	f.times(10, kindSent, today, statFact{account: a, campaign: f.id("camp-a")})
	f.times(20, kindSent, today, statFact{account: b, campaign: f.id("camp-b")})
	walkFrom(t, db, time.Now())
	t.Cleanup(func() { walkFrom(t, db, time.Now()) })

	ctx := callerCtx(member, auth.RoleWriter)
	own := enginePayload(t, eng, ctx, `builtin emailStatistics(period: "7d")`)
	if sent := totalSent(own); sent != 10.0 {
		t.Errorf("the member naming nobody: %v sent, want their organization's 10", sent)
	}
	if campaigns, _ := own["campaigns"].([]any); len(campaigns) != 1 {
		t.Errorf("the member naming nobody: campaigns %v, want their organization's one", own["campaigns"])
	}
	wantNothingInPayload(t, "the member naming the other organization",
		enginePayload(t, eng, ctx, `builtin emailStatistics(accountIds: [`+langparser.QuoteString(b)+`], period: "7d")`))
}
