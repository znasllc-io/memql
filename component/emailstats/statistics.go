package emailstats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// statistics.go -- the emailStatistics read (docs/superpowers/specs/
// 2026-11-08-site-and-email-statistics-design.md, section 7, D4, D11 and
// D18): a period of email figures for the organizations a caller reaches,
// counted from the facts the rollup records (rollup.go). The Email Overview
// reads it for one organization, or for every organization the person reaches.
//
// # Narrowed by the caller's grant, never by what they name (D11)
//
// email_stat_fact is this package's own table, outside the parser and the
// row-authz filter path, so nothing upstream narrows a read of it: the gate is
// here, and it is the caller's grant -- MemQLEngine.ResolveAccessGrant, built
// on the row gate's own resolution of which organizations an actor reaches.
// allowedAccounts intersects the organizations a caller names with it. One
// they do not reach is dropped, not refused, so a client's member who names
// another client gets exactly the answer of somebody who reaches no
// organization: every figure null and no measuredSince, whether the named
// organization exists or not and whatever it sent. A grant that could not be
// resolved is empty, and so is the grant of an engine handle that cannot
// resolve one (plugin.go): both reach nothing, never everything.
//
// The facts older than organization attribution carry an empty account_id and
// belong to no organization anybody could name, so staff reading every
// organization is the one answer they are counted in (section 5).
//
// # Absent is never zero
//
// measuredSince is the first day the facts table holds anything, for the
// whole cluster, because the rollup walks every organization's mail at once:
// its first fact is where its record of anybody's mail begins. A bucket that
// ends on or before that day is null -- nothing was recorded then, for anyone
// -- and with no fact at all every figure is null. From that day on, a bucket
// with no fact in scope is a measured zero: nothing was sent. A rate with no
// campaign mail to be a share of is null.
//
// # Two ways to count an open (D18)
//
// The open and click rates are of the CAMPAIGN mail sent in a range, by
// send-day cohort: of the campaign sends dated in it, the share whose message
// has an open_unique (click_unique) fact on any day, the range's own or a
// later one. Rule and journey mail is left out of both sides because it is not
// tracked, and counting it as sent would lower a rate with messages nobody
// could have seen opened. A campaign's row and an organization's row count
// their unique opens and clicks the same way, so a row's rates are its unique
// opens and clicks over its campaign sends.
//
// The totals and the series count activity instead, each fact on its own day:
// an open is an open event in the range, and a unique open is the first open
// of a message, whenever the message was sent. So the totals' unique opens can
// count a message sent the day before the period, which the period's open
// rate leaves out, and a bucket's unique opens are not its rate's numerator.
//
// # Consent figures stay out of send figures
//
// A consent fact carries the campaign its event named, so a campaign's row is
// counted from sent, open_unique and click_unique facts alone. Unsubscribes,
// bounces and complaints are totals, series and audience figures, never a
// campaign's. Delivered and undelivered are counted as recorded and are not a
// split of the sends: a delivery whose provider's verdict changed between two
// readings has both (facts.go).
//
// # Twelve months have no previous period (D4)
//
// See core/statsperiod: the thirteen months before them are past the 400 days
// the facts are kept. Previous is every figure null, PreviousSeries is nil,
// and the payload says null for both.
//
// # One snapshot
//
// Every statement of a read runs in one read-only REPEATABLE READ transaction,
// so the totals, the series and the lists agree with each other while the
// rollup writes a page of facts underneath.

// emailPeriods are the periods the read offers: the website read's less 24
// hours, because a fact is dated by its UTC day and has no hour.
var emailPeriods = []statsperiod.Period{
	statsperiod.Last7Days, statsperiod.Last30Days, statsperiod.Last90Days, statsperiod.Last12Months,
}

// statisticsConcept is the concept of the one node the read answers with. Not
// a DSL concept: nothing stores it, and the client reads the node's payload.
const statisticsConcept = "integration:emailStats:statistics"

// campaignsKept is how many campaigns a read lists (section 7).
const campaignsKept = 50

// The kinds of automation a send is attributed to (D18), as
// AutomationFigures.Kind spells them.
const (
	automationRule    = "rule"
	automationJourney = "journey"
)

// GrantResolver answers which organizations the caller reaches. The engine is
// one (memql.MemQLEngine.ResolveAccessGrant, built on the row gate's own
// resolution); the plug-in asserts it on its engine handle, and a handle that
// is not one narrows every read to nothing.
type GrantResolver interface {
	ResolveAccessGrant(ctx context.Context) memql.AccessGrant
}

// EmailTotals are a range's figures; nil is "not measured".
type EmailTotals struct {
	Sent, Delivered, Undelivered, Failed, Skipped, Suppressed *int64
	// Opens and Clicks are the events in the range; UniqueOpens and
	// UniqueClicks the messages first opened or clicked in it, whenever they
	// were sent.
	Opens, Clicks, UniqueOpens, UniqueClicks      *int64
	Unsubscribes, Bounces, Complaints, Subscribes *int64
	// OpenRate and ClickRate are of the campaign mail sent in the range, by
	// cohort; nil when none was sent.
	OpenRate, ClickRate *float64
}

// EmailPoint is one bucket of a series, starting At.
type EmailPoint struct {
	At time.Time
	// The bucket's figures, each counted as EmailTotals counts it.
	Sent, UniqueOpens, UniqueClicks, Unsubscribes, Subscribes, Bounces, Complaints, Failed, Suppressed *int64
	// OpenRate and ClickRate are of the campaign mail sent in the bucket,
	// opened or clicked on any day.
	OpenRate, ClickRate *float64
}

// CampaignFigures is one campaign's sends in the period, or one
// organization's (the Accounts rows, ID the organization). UniqueOpens and
// UniqueClicks are of those sends' messages, on any day; OpenRate and
// ClickRate are them over the campaign sends, nil for an organization that
// sent no campaign mail.
type CampaignFigures struct {
	ID                              string
	Sent, UniqueOpens, UniqueClicks int64
	OpenRate, ClickRate             *float64
}

// AutomationFigures is one rule's or journey's mail in the period; Kind is
// automationRule or automationJourney.
type AutomationFigures struct {
	ID, Kind                 string
	Sent, Failed, Suppressed int64
}

// AudienceFigures is one audience's subscription changes in the period; Net is
// Subscribes - Unsubscribes.
type AudienceFigures struct {
	ID                            string
	Subscribes, Unsubscribes, Net int64
}

// EmailStatistics is one read's answer.
type EmailStatistics struct {
	Period statsperiod.Period
	// Bucket is statsperiod's: day or month.
	Bucket string
	// Start and End are the period; PrevStart and PrevEnd the one before it,
	// zero when HasPrevious is false.
	Start, End, PrevStart, PrevEnd time.Time
	HasPrevious                    bool
	// MeasuredSince is the cluster's first day with a fact; nil when it has
	// none, or when the caller reaches no organization.
	MeasuredSince *time.Time
	Totals        EmailTotals
	// Previous is the previous period's figures, every one nil without one.
	Previous EmailTotals
	// Series has one point per bucket, oldest first, and PreviousSeries the
	// previous period's, aligned by offset; nil without a previous period.
	Series, PreviousSeries []EmailPoint
	// Campaigns are the campaignsKept campaigns with sends in the period, most
	// unique opens first, then most unique clicks, most sent, and by id.
	Campaigns []CampaignFigures
	// Automations are the rules and journeys with a send, a failure or a
	// suppression in the period, most sent first.
	Automations []AutomationFigures
	// Audiences are the audiences with subscription changes in the period,
	// the largest net change first.
	Audiences []AudienceFigures
	// Accounts are the organizations with sends in the period, most sent
	// first, listed only when the read covers more than one organization.
	Accounts []CampaignFigures
}

// allowedAccounts is the scope a read covers: the organizations, as bare ids,
// and whether it is every organization, the unattributed facts included.
//
//   - Staff (EveryAccount) who name nothing reach everything: every
//     organization, and the facts older than organization attribution, which
//     belong to none.
//   - Staff who name organizations get those.
//   - Anyone else gets the organizations they name that are in their grant,
//     or their whole grant when they name none. One outside it is dropped,
//     not refused (D11).
//
// WHETHER ORGANIZATIONS WERE NAMED is the length of the list the caller sent,
// blanks included (namedIDsArg), the website read's rule: a named list with
// no usable id names no organization and reaches nothing, where reading it as
// "none named" would hand staff every organization -- under the label of the
// one they asked about -- and a member their whole grant.
func allowedAccounts(grant memql.AccessGrant, requested []string) (ids []string, everything bool) {
	named := len(requested) > 0
	if grant.EveryAccount && !named {
		return nil, true
	}
	// Staff reach any organization they name; anyone else reaches their
	// grant's. AccountIDs is empty for staff, and means "all" there.
	reach := map[string]bool{}
	for _, id := range grant.AccountIDs {
		if bare := memql.BareShortId(id); bare != "" {
			reach[bare] = true
		}
	}
	wanted := grant.AccountIDs
	if named {
		wanted = requested
	}
	seen := map[string]bool{}
	for _, id := range wanted {
		bare := memql.BareShortId(id)
		if bare == "" || seen[bare] || (!grant.EveryAccount && !reach[bare]) {
			continue
		}
		seen[bare] = true
		ids = append(ids, bare)
	}
	sort.Strings(ids)
	return ids, false
}

// orgScope is what a read covers, after allowedAccounts.
type orgScope struct {
	ids        []string
	everything bool
}

func (s orgScope) empty() bool { return !s.everything && len(s.ids) == 0 }

// several reports whether more than one organization is in scope, which is
// when a read lists each one (Accounts): every organization, or more than one
// by name.
func (s orgScope) several() bool { return s.everything || len(s.ids) > 1 }

// filter is the scope as a predicate on the account column named, and its
// arguments. Everything is no predicate at all, so the facts with an empty
// account_id are in it.
func (s orgScope) filter(column string) (string, []any) {
	if s.everything {
		return "TRUE", nil
	}
	return column + " IN (?)", []any{bun.In(s.ids)}
}

// readStatistics answers one read for the caller whose grant this is.
// requested is the accountIds argument as namedIDsArg decodes it. A period the
// read does not offer is refused; anything the caller does not reach is
// dropped.
func readStatistics(ctx context.Context, db *bun.DB, grant memql.AccessGrant, requested []string, p statsperiod.Period, now time.Time) (EmailStatistics, error) {
	period, err := statsperiod.Parse(string(p), emailPeriods...)
	if err != nil {
		return EmailStatistics{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	b := statsperiod.For(period, now)
	out := emptyStatistics(period, b)

	ids, everything := allowedAccounts(grant, requested)
	scope := orgScope{ids: ids, everything: everything}
	if scope.empty() {
		// The caller reaches none of what they named, or nothing at all: the
		// bounds and every figure null, and nothing is read -- not even
		// measuredSince, which is a fact about the cluster's mail and not
		// about anything they reach.
		return out, nil
	}
	if db == nil {
		return EmailStatistics{}, errors.New("this node has no database handle, so it cannot read email statistics")
	}
	err = statisticsTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		return fillStatistics(ctx, tx, scope, b, &out)
	})
	if err != nil {
		return EmailStatistics{}, fmt.Errorf("read the email statistics: %w", err)
	}
	return out, nil
}

// emptyStatistics is the answer before anything is read: the bounds, a null
// point for every bucket, and empty lists.
func emptyStatistics(p statsperiod.Period, b statsperiod.Bounds) EmailStatistics {
	s := EmailStatistics{
		Period: p, Bucket: b.Bucket,
		Start: b.Start, End: b.End, PrevStart: b.PrevStart, PrevEnd: b.PrevEnd, HasPrevious: b.HasPrevious,
		Series:      nullPoints(b.Starts()),
		Campaigns:   []CampaignFigures{},
		Automations: []AutomationFigures{},
		Audiences:   []AudienceFigures{},
		Accounts:    []CampaignFigures{},
	}
	if b.HasPrevious {
		s.PreviousSeries = nullPoints(b.PreviousStarts())
	}
	return s
}

func nullPoints(starts []time.Time) []EmailPoint {
	out := make([]EmailPoint, len(starts))
	for i, at := range starts {
		out[i] = EmailPoint{At: at}
	}
	return out
}

// statisticsTx runs one read in a read-only REPEATABLE READ transaction whose
// statements are bounded by statementTimeout, the rollup's own bound on this
// package's tables.
func statisticsTx(ctx context.Context, db *bun.DB, fn func(context.Context, bun.Tx) error) error {
	opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	return db.RunInTx(ctx, opts, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '"+statementTimeout+"'"); err != nil {
			return fmt.Errorf("bound the statements: %w", err)
		}
		return fn(ctx, tx)
	})
}

// fillStatistics reads the facts in scope into out.
func fillStatistics(ctx context.Context, tx bun.Tx, scope orgScope, b statsperiod.Bounds, out *EmailStatistics) error {
	since, err := readMeasuredSince(ctx, tx)
	if err != nil {
		return err
	}
	if since == nil {
		// No fact at all: nothing was ever recorded, and every figure stays
		// null.
		return nil
	}
	out.MeasuredSince = since

	// The previous period ends where the current one starts, so one read of
	// the days covers both.
	from := b.Start
	if b.HasPrevious {
		from = b.PrevStart
	}
	days, err := readDays(ctx, tx, scope, from, b.End)
	if err != nil {
		return err
	}
	out.Totals = days.totals(b.Start, b.End, *since)
	out.Series = days.series(b, b.Starts(), *since)
	if b.HasPrevious {
		out.Previous = days.totals(b.PrevStart, b.PrevEnd, *since)
		out.PreviousSeries = days.series(b, b.PreviousStarts(), *since)
	}

	if out.Campaigns, err = readCampaigns(ctx, tx, scope, b.Start, b.End); err != nil {
		return err
	}
	if out.Automations, err = readAutomations(ctx, tx, scope, b.Start, b.End); err != nil {
		return err
	}
	if out.Audiences, err = readAudiences(ctx, tx, scope, b.Start, b.End); err != nil {
		return err
	}
	if scope.several() {
		if out.Accounts, err = readAccounts(ctx, tx, scope, b.Start, b.End); err != nil {
			return err
		}
	}
	return nil
}

// readMeasuredSince reads the cluster's first day with a fact, nil when the
// table holds none. Every organization's: see "Absent is never zero" above.
func readMeasuredSince(ctx context.Context, tx bun.Tx) (*time.Time, error) {
	var row struct {
		First *string `bun:"first_day"`
	}
	if err := tx.NewRaw(`SELECT to_char(min(day)::timestamp, 'YYYY-MM-DD') AS first_day FROM email_stat_fact`).Scan(ctx, &row); err != nil {
		return nil, fmt.Errorf("read the first recorded day: %w", err)
	}
	if row.First == nil {
		return nil, nil
	}
	d, err := time.Parse(time.DateOnly, *row.First)
	if err != nil {
		return nil, fmt.Errorf("read the first recorded day %q: %w", *row.First, err)
	}
	return &d, nil
}

// cohort is a set of campaign sends and how many of their messages were
// opened, and clicked, at least once.
type cohort struct{ sent, opened, clicked int64 }

// dayFacts are a range's facts counted per UTC day, keyed YYYY-MM-DD: every
// kind on its own day, and each day's campaign sends as a cohort.
type dayFacts struct {
	kinds   map[string]map[string]int64
	cohorts map[string]cohort
}

// cohortJoin joins each send s to the first open (o) and the first click (cl)
// of its message, on any day. open_unique and click_unique are keyed by the
// delivery (facts.go), so a send meets at most one of each, and a count of
// the joined ids is a count of messages.
func cohortJoin() string {
	return fmt.Sprintf(`
  LEFT JOIN email_stat_fact o ON o.kind = '%s' AND o.source_id = s.source_id
  LEFT JOIN email_stat_fact cl ON cl.kind = '%s' AND cl.source_id = s.source_id`, kindOpenUnique, kindClickUnique)
}

// readDays counts the facts in scope over [from, to) by day: each kind, and
// each send day's campaign cohort. Days are 'YYYY-MM-DD' dates on both sides,
// never instants, so no session time zone moves one (rollup.go, factRow).
func readDays(ctx context.Context, tx bun.Tx, scope orgScope, from, to time.Time) (dayFacts, error) {
	out := dayFacts{kinds: map[string]map[string]int64{}, cohorts: map[string]cohort{}}

	where, args := scope.filter("account_id")
	var kinds []struct {
		Day  string `bun:"on_day"`
		Kind string `bun:"kind"`
		N    int64  `bun:"n"`
	}
	q := `
SELECT to_char(day::timestamp, 'YYYY-MM-DD') AS on_day, kind, count(*) AS n
  FROM email_stat_fact
 WHERE ` + where + ` AND day >= ?::date AND day < ?::date
 GROUP BY day, kind`
	if err := tx.NewRaw(q, append(args, dateArg(from), dateArg(to))...).Scan(ctx, &kinds); err != nil {
		return dayFacts{}, fmt.Errorf("count the facts by day: %w", err)
	}
	for _, r := range kinds {
		if out.kinds[r.Day] == nil {
			out.kinds[r.Day] = map[string]int64{}
		}
		out.kinds[r.Day][r.Kind] += r.N
	}

	where, args = scope.filter("s.account_id")
	var cohorts []struct {
		Day     string `bun:"on_day"`
		Sent    int64  `bun:"sent"`
		Opened  int64  `bun:"opened"`
		Clicked int64  `bun:"clicked"`
	}
	q = fmt.Sprintf(`
SELECT to_char(s.day::timestamp, 'YYYY-MM-DD') AS on_day, count(*) AS sent,
       count(o.source_id) AS opened, count(cl.source_id) AS clicked
  FROM email_stat_fact s%s
 WHERE s.kind = '%s' AND s.campaign_id <> '' AND %s AND s.day >= ?::date AND s.day < ?::date
 GROUP BY s.day`, cohortJoin(), kindSent, where)
	if err := tx.NewRaw(q, append(args, dateArg(from), dateArg(to))...).Scan(ctx, &cohorts); err != nil {
		return dayFacts{}, fmt.Errorf("count the campaign sends opened and clicked by day: %w", err)
	}
	for _, r := range cohorts {
		out.cohorts[r.Day] = cohort{sent: r.Sent, opened: r.Opened, clicked: r.Clicked}
	}
	return out, nil
}

// sum adds up the days in [from, to), which start at UTC midnight.
func (d dayFacts) sum(from, to time.Time) (map[string]int64, cohort) {
	kinds := map[string]int64{}
	var c cohort
	for day := from.UTC(); day.Before(to); day = day.AddDate(0, 0, 1) {
		key := day.Format(time.DateOnly)
		for kind, n := range d.kinds[key] {
			kinds[kind] += n
		}
		dc := d.cohorts[key]
		c.sent += dc.sent
		c.opened += dc.opened
		c.clicked += dc.clicked
	}
	return kinds, c
}

// totals are [from, to)'s figures: every one nil when the range ends on or
// before the first recorded day, and a count -- zero included -- otherwise.
func (d dayFacts) totals(from, to, since time.Time) EmailTotals {
	if !to.After(since) {
		return EmailTotals{}
	}
	k, c := d.sum(from, to)
	return EmailTotals{
		Sent: figure(k[kindSent]), Delivered: figure(k[kindDelivered]), Undelivered: figure(k[kindUndelivered]),
		Failed: figure(k[kindFailed]), Skipped: figure(k[kindSkipped]), Suppressed: figure(k[kindSuppressed]),
		Opens: figure(k[kindOpen]), Clicks: figure(k[kindClick]),
		UniqueOpens: figure(k[kindOpenUnique]), UniqueClicks: figure(k[kindClickUnique]),
		Unsubscribes: figure(k[kindUnsubscribe]), Bounces: figure(k[kindBounce]),
		Complaints: figure(k[kindComplaint]), Subscribes: figure(k[kindSubscribe]),
		OpenRate: shareOf(c.opened, c.sent), ClickRate: shareOf(c.clicked, c.sent),
	}
}

// series lays a period's days onto its buckets, each bucket by totals' rule.
func (d dayFacts) series(b statsperiod.Bounds, starts []time.Time, since time.Time) []EmailPoint {
	out := nullPoints(starts)
	for i, at := range starts {
		end := b.Next(at)
		if !end.After(since) {
			continue
		}
		k, c := d.sum(at, end)
		out[i] = EmailPoint{
			At: at, Sent: figure(k[kindSent]),
			UniqueOpens: figure(k[kindOpenUnique]), UniqueClicks: figure(k[kindClickUnique]),
			Unsubscribes: figure(k[kindUnsubscribe]), Subscribes: figure(k[kindSubscribe]),
			Bounces: figure(k[kindBounce]), Complaints: figure(k[kindComplaint]),
			Failed: figure(k[kindFailed]), Suppressed: figure(k[kindSuppressed]),
			OpenRate: shareOf(c.opened, c.sent), ClickRate: shareOf(c.clicked, c.sent),
		}
	}
	return out
}

// sendRow is one campaign's or organization's sends in a range, with the
// campaign sends among them and how many of those were opened and clicked.
type sendRow struct {
	ID           string `bun:"id"`
	Sent         int64  `bun:"sent"`
	CampaignSent int64  `bun:"campaign_sent"`
	UniqueOpens  int64  `bun:"unique_opens"`
	UniqueClicks int64  `bun:"unique_clicks"`
}

func figuresOf(rows []sendRow) []CampaignFigures {
	out := make([]CampaignFigures, 0, len(rows))
	for _, r := range rows {
		out = append(out, CampaignFigures{
			ID: r.ID, Sent: r.Sent, UniqueOpens: r.UniqueOpens, UniqueClicks: r.UniqueClicks,
			OpenRate: shareOf(r.UniqueOpens, r.CampaignSent), ClickRate: shareOf(r.UniqueClicks, r.CampaignSent),
		})
	}
	return out
}

// readCampaigns reads the campaignsKept campaigns with sends in [from, to).
// The ties after unique opens and unique clicks are broken by sends and then
// by id in byte order (COLLATE "C"), so the cut is the same on every
// database.
func readCampaigns(ctx context.Context, tx bun.Tx, scope orgScope, from, to time.Time) ([]CampaignFigures, error) {
	where, args := scope.filter("s.account_id")
	q := fmt.Sprintf(`
SELECT s.campaign_id AS id, count(*) AS sent, count(*) AS campaign_sent,
       count(o.source_id) AS unique_opens, count(cl.source_id) AS unique_clicks
  FROM email_stat_fact s%s
 WHERE s.kind = '%s' AND s.campaign_id <> '' AND %s AND s.day >= ?::date AND s.day < ?::date
 GROUP BY s.campaign_id
 ORDER BY unique_opens DESC, unique_clicks DESC, sent DESC, s.campaign_id COLLATE "C"
 LIMIT %d`, cohortJoin(), kindSent, where, campaignsKept)
	var rows []sendRow
	if err := tx.NewRaw(q, append(args, dateArg(from), dateArg(to))...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("read the campaigns: %w", err)
	}
	return figuresOf(rows), nil
}

// readAccounts reads each organization's sends in [from, to), most sent
// first. Every send counts in Sent, as in the totals; the rates are of its
// campaign sends. The facts with no organization are no organization's row.
func readAccounts(ctx context.Context, tx bun.Tx, scope orgScope, from, to time.Time) ([]CampaignFigures, error) {
	where, args := scope.filter("s.account_id")
	q := fmt.Sprintf(`
SELECT s.account_id AS id, count(*) AS sent,
       count(*) FILTER (WHERE s.campaign_id <> '') AS campaign_sent,
       count(o.source_id) FILTER (WHERE s.campaign_id <> '') AS unique_opens,
       count(cl.source_id) FILTER (WHERE s.campaign_id <> '') AS unique_clicks
  FROM email_stat_fact s%s
 WHERE s.kind = '%s' AND s.account_id <> '' AND %s AND s.day >= ?::date AND s.day < ?::date
 GROUP BY s.account_id
 ORDER BY sent DESC, unique_opens DESC, s.account_id COLLATE "C"`, cohortJoin(), kindSent, where)
	var rows []sendRow
	if err := tx.NewRaw(q, append(args, dateArg(from), dateArg(to))...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("read each organization's sends: %w", err)
	}
	return figuresOf(rows), nil
}

// readAutomations reads every rule and journey with a send, a failure or a
// suppression in [from, to). A send attributed to both a rule and a journey
// counts under each.
func readAutomations(ctx context.Context, tx bun.Tx, scope orgScope, from, to time.Time) ([]AutomationFigures, error) {
	where, scopeArgs := scope.filter("account_id")
	part := func(automation, column string) string {
		return fmt.Sprintf(`
SELECT '%[1]s' AS automation, %[2]s AS id,
       count(*) FILTER (WHERE kind = '%[3]s') AS sent,
       count(*) FILTER (WHERE kind = '%[4]s') AS failed,
       count(*) FILTER (WHERE kind = '%[5]s') AS suppressed
  FROM email_stat_fact
 WHERE %[2]s <> '' AND kind IN ('%[3]s', '%[4]s', '%[5]s') AND %[6]s
   AND day >= ?::date AND day < ?::date
 GROUP BY %[2]s`, automation, column, kindSent, kindFailed, kindSuppressed, where)
	}
	q := part(automationRule, "rule_id") + "\nUNION ALL" + part(automationJourney, "journey_id")
	var args []any
	for range 2 {
		args = append(args, scopeArgs...)
		args = append(args, dateArg(from), dateArg(to))
	}
	var rows []struct {
		Automation string `bun:"automation"`
		ID         string `bun:"id"`
		Sent       int64  `bun:"sent"`
		Failed     int64  `bun:"failed"`
		Suppressed int64  `bun:"suppressed"`
	}
	if err := tx.NewRaw(q, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("read the rules and journeys: %w", err)
	}
	out := make([]AutomationFigures, 0, len(rows))
	for _, r := range rows {
		out = append(out, AutomationFigures{ID: r.ID, Kind: r.Automation, Sent: r.Sent, Failed: r.Failed, Suppressed: r.Suppressed})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Sent != b.Sent {
			return a.Sent > b.Sent
		}
		if a.Failed != b.Failed {
			return a.Failed > b.Failed
		}
		if a.Suppressed != b.Suppressed {
			return a.Suppressed > b.Suppressed
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID < b.ID
	})
	return out, nil
}

// readAudiences reads every audience with a subscribe or an unsubscribe in
// [from, to). A subscription change whose recipient the rollup could not
// place is in the totals and in no audience's row.
func readAudiences(ctx context.Context, tx bun.Tx, scope orgScope, from, to time.Time) ([]AudienceFigures, error) {
	where, args := scope.filter("account_id")
	q := fmt.Sprintf(`
SELECT audience_id AS id,
       count(*) FILTER (WHERE kind = '%[1]s') AS subscribes,
       count(*) FILTER (WHERE kind = '%[2]s') AS unsubscribes
  FROM email_stat_fact
 WHERE audience_id <> '' AND kind IN ('%[1]s', '%[2]s') AND %[3]s AND day >= ?::date AND day < ?::date
 GROUP BY audience_id`, kindSubscribe, kindUnsubscribe, where)
	var rows []struct {
		ID           string `bun:"id"`
		Subscribes   int64  `bun:"subscribes"`
		Unsubscribes int64  `bun:"unsubscribes"`
	}
	if err := tx.NewRaw(q, append(args, dateArg(from), dateArg(to))...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("read the audiences: %w", err)
	}
	out := make([]AudienceFigures, 0, len(rows))
	for _, r := range rows {
		out = append(out, AudienceFigures{ID: r.ID, Subscribes: r.Subscribes, Unsubscribes: r.Unsubscribes, Net: r.Subscribes - r.Unsubscribes})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Net != b.Net {
			return a.Net > b.Net
		}
		if a.Subscribes != b.Subscribes {
			return a.Subscribes > b.Subscribes
		}
		return a.ID < b.ID
	})
	return out, nil
}

// figure is a measured count.
func figure(n int64) *int64 { return &n }

// shareOf is part / whole, nil when there is no whole to be a share of.
func shareOf(part, whole int64) *float64 {
	if whole <= 0 {
		return nil
	}
	f := float64(part) / float64(whole)
	return &f
}

// dateArg is a UTC day as the 'YYYY-MM-DD' text a ?::date placeholder takes.
func dateArg(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// Payload is the answer as the node carries it, which the OS parses
// (parseEmailStatistics): lowerCamel keys, instants in RFC3339, measuredSince
// as YYYY-MM-DD, and every figure nothing measured PRESENT AND NULL, so a
// client can tell "not measured" from a key it does not know -- the website
// read's payload (component/sitetraffic, Statistics.Payload). Without a
// previous period, previous, previousSeries, prevStart and prevEnd are null.
func (s EmailStatistics) Payload() map[string]any {
	p := map[string]any{
		"period":         string(s.Period),
		"bucket":         s.Bucket,
		"start":          instant(s.Start),
		"end":            instant(s.End),
		"hasPrevious":    s.HasPrevious,
		"prevStart":      nil,
		"prevEnd":        nil,
		"measuredSince":  nil,
		"totals":         s.Totals.payload(),
		"previous":       nil,
		"series":         pointsPayload(s.Series),
		"previousSeries": nil,
		"campaigns":      figuresPayload(s.Campaigns),
		"automations":    automationsPayload(s.Automations),
		"audiences":      audiencesPayload(s.Audiences),
		"accounts":       figuresPayload(s.Accounts),
	}
	if s.HasPrevious {
		p["prevStart"], p["prevEnd"] = instant(s.PrevStart), instant(s.PrevEnd)
		p["previous"] = s.Previous.payload()
		p["previousSeries"] = pointsPayload(s.PreviousSeries)
	}
	if s.MeasuredSince != nil {
		p["measuredSince"] = s.MeasuredSince.UTC().Format(time.DateOnly)
	}
	return p
}

func (t EmailTotals) payload() map[string]any {
	return map[string]any{
		"sent": countOrNull(t.Sent), "delivered": countOrNull(t.Delivered),
		"undelivered": countOrNull(t.Undelivered), "failed": countOrNull(t.Failed),
		"skipped": countOrNull(t.Skipped), "suppressed": countOrNull(t.Suppressed),
		"opens": countOrNull(t.Opens), "clicks": countOrNull(t.Clicks),
		"uniqueOpens": countOrNull(t.UniqueOpens), "uniqueClicks": countOrNull(t.UniqueClicks),
		"unsubscribes": countOrNull(t.Unsubscribes), "bounces": countOrNull(t.Bounces),
		"complaints": countOrNull(t.Complaints), "subscribes": countOrNull(t.Subscribes),
		"openRate": rateOrNull(t.OpenRate), "clickRate": rateOrNull(t.ClickRate),
	}
}

func pointsPayload(points []EmailPoint) []any {
	out := make([]any, len(points))
	for i, p := range points {
		out[i] = map[string]any{
			"at":   instant(p.At),
			"sent": countOrNull(p.Sent), "uniqueOpens": countOrNull(p.UniqueOpens),
			"uniqueClicks": countOrNull(p.UniqueClicks), "unsubscribes": countOrNull(p.Unsubscribes),
			"subscribes": countOrNull(p.Subscribes), "bounces": countOrNull(p.Bounces),
			"complaints": countOrNull(p.Complaints), "failed": countOrNull(p.Failed),
			"suppressed": countOrNull(p.Suppressed),
			"openRate":   rateOrNull(p.OpenRate), "clickRate": rateOrNull(p.ClickRate),
		}
	}
	return out
}

func figuresPayload(rows []CampaignFigures) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = map[string]any{
			"id": r.ID, "sent": r.Sent, "uniqueOpens": r.UniqueOpens, "uniqueClicks": r.UniqueClicks,
			"openRate": rateOrNull(r.OpenRate), "clickRate": rateOrNull(r.ClickRate),
		}
	}
	return out
}

func automationsPayload(rows []AutomationFigures) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = map[string]any{"id": r.ID, "kind": r.Kind, "sent": r.Sent, "failed": r.Failed, "suppressed": r.Suppressed}
	}
	return out
}

func audiencesPayload(rows []AudienceFigures) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = map[string]any{"id": r.ID, "subscribes": r.Subscribes, "unsubscribes": r.Unsubscribes, "net": r.Net}
	}
	return out
}

// countOrNull and rateOrNull spell an unmeasured figure as an untyped nil,
// which JSON writes as null.
func countOrNull(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func rateOrNull(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// instant renders a bound in RFC3339, UTC; a zero time is no bound at all.
func instant(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
