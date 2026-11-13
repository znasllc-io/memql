package emailstats

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// statistics_scale_test.go -- opt-in evidence of what a staff read costs at
// scale, in the way of component/memql/os_list_performance_test.go.
//
// It writes about 2.1 million facts across 50 organizations and 400 days, as
// a cluster a year into sending would hold, and times the staff reads of 90
// days and of 12 months -- the reads that cover every organization and so
// the most facts -- on a handle carrying the request pool's 8 s server
// statement timeout (component/database, sessionConnParams): the limit the
// plug-in's reads run under, which the read no longer raises. A statement
// past it fails the read, and the test with it. The facts are deterministic
// and removed afterwards; the run writes and deletes two million rows, so it
// belongs on a disposable database:
//
//	MEMQL_EMAILSTATS_SCALE_EVIDENCE_FILE=/tmp/email-scale.json \
//	  MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=... \
//	  go test -count=1 -p=1 github.com/znasllc-io/memql/component/emailstats/ -run TestStaffReadsAtScaleEvidence

// scalePrefix opens every source id the evidence writes, so its removal is
// exact.
const scalePrefix = "scale-"

// scaleFacts writes the evidence's facts, dated back from today: 1.1 million
// sends (seven in ten campaign mail, two rule and one journey), their
// providers' verdicts, a quarter of the campaign messages first opened and a
// twentieth first clicked, open and click events, failures, skips and
// suppressions, and subscription changes, bounces and complaints.
func scaleFacts(t *testing.T, db *bun.DB, today time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	day := dateArg(today)
	// A delivery's organization, attribution and day are functions of its
	// number, so every fact about it agrees with its send.
	delivery := `'` + scalePrefix + `d-' || i, 'scale-org-' || (i % 50),
		CASE WHEN i % 10 < 7 THEN 'scale-camp-' || (i % 50) || '-' || (i % 20) ELSE '' END,
		CASE WHEN i % 10 IN (7, 8) THEN 'scale-rule-' || (i % 50) || '-' || (i % 5) ELSE '' END,
		CASE WHEN i % 10 = 9 THEN 'scale-journey-' || (i % 50) || '-' || (i % 3) ELSE '' END, ''`
	// The series is bigint because the spreading products pass int4's range,
	// and each offset is cast back to int, the type date arithmetic takes.
	sendDay := `(?::date - ((i * 7919) % 400)::int)`
	event := func(kind, tag string, n int, audience bool) string {
		aud := "''"
		if audience {
			aud = `'scale-aud-' || (i % 50) || '-' || (i % 5)`
		}
		return fmt.Sprintf(`SELECT '%s', '%s%s-' || i, 'scale-org-' || (i %% 50), 'scale-camp-' || (i %% 50) || '-' || (i %% 20), '', '', %s,
			(?::date - ((i * 104729) %% 400)::int) FROM generate_series(1::bigint, %d) AS i`, kind, scalePrefix, tag, aud, n)
	}
	statements := []string{
		`SELECT 'sent', ` + delivery + `, ` + sendDay + ` FROM generate_series(1::bigint, 1100000) AS i`,
		`SELECT 'delivered', ` + delivery + `, ` + sendDay + ` FROM generate_series(1::bigint, 600000) AS i`,
		`SELECT 'undelivered', ` + delivery + `, ` + sendDay + ` FROM generate_series(600001::bigint, 620000) AS i`,
		`SELECT 'open_unique', ` + delivery + `, LEAST(?::date, ` + sendDay + ` + (i % 3)::int) FROM generate_series(1::bigint, 1100000) AS i WHERE i % 10 < 7 AND i % 4 = 0`,
		`SELECT 'click_unique', ` + delivery + `, LEAST(?::date, ` + sendDay + ` + (i % 3)::int) FROM generate_series(1::bigint, 1100000) AS i WHERE i % 20 = 0`,
		event("open", "o", 100000, false),
		event("click", "c", 20000, false),
		event("failed", "f", 15000, false),
		event("skipped", "s", 10000, false),
		event("suppressed", "s", 5000, false),
		event("subscribe", "sub", 10000, true),
		event("unsubscribe", "uns", 5000, true),
		event("bounce", "b", 3000, false),
		event("complaint", "x", 1000, false),
	}
	for _, s := range statements {
		args := []any{day}
		if c := countPlaceholders(s); c == 2 {
			args = append(args, day)
		}
		q := `INSERT INTO email_stat_fact (kind, source_id, account_id, campaign_id, rule_id, journey_id, audience_id, day) ` + s
		if _, err := db.NewRaw(q, args...).Exec(ctx); err != nil {
			t.Fatalf("write the evidence's facts (%s): %v", s[:40], err)
		}
	}
	if _, err := db.ExecContext(ctx, `ANALYZE email_stat_fact`); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.NewRaw(`SELECT count(*) FROM email_stat_fact WHERE source_id LIKE ?`, scalePrefix+"%").Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countPlaceholders(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '?' {
			n++
		}
	}
	return n
}

// poolHandle is a handle with the request pool's session limits: the server's
// 8 s statement timeout, under pgdriver's own 10 s read deadline.
func poolHandle(t *testing.T) *bun.DB {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"statement_timeout": "8000"}),
	)), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type scaleTiming struct {
	Read        string    `json:"read"`
	Period      string    `json:"period"`
	Runs        []float64 `json:"runsMs"`
	MedianMs    float64   `json:"medianMs"`
	StatementMs []float64 `json:"slowestStatementMs,omitempty"`
}

func TestStaffReadsAtScaleEvidence(t *testing.T) {
	output := os.Getenv("MEMQL_EMAILSTATS_SCALE_EVIDENCE_FILE")
	if output == "" {
		t.Skip("opt-in: set MEMQL_EMAILSTATS_SCALE_EVIDENCE_FILE against a disposable database")
	}
	_, shared := statsEngine(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if _, err := shared.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, scalePrefix+"%").Exec(ctx); err != nil {
			t.Errorf("remove the evidence's facts: %v", err)
		}
		walkFrom(t, shared, time.Now())
	})
	now := time.Now().UTC()
	began := time.Now()
	facts := scaleFacts(t, shared, dayOf(now))
	written := time.Since(began)
	walkFrom(t, shared, now)

	pool := poolHandle(t)
	var timings []scaleTiming
	read := func(who string, grant scaleGrant, p statsperiod.Period) {
		t.Helper()
		timing := scaleTiming{Read: who, Period: string(p)}
		for run := 0; run < 5; run++ {
			start := time.Now()
			got, err := readStatistics(ctx, pool, grant.grant, grant.requested, p, now, fakeGrants{grant.grant}, nil)
			if err != nil {
				t.Fatalf("%s %s: %v", who, p, err)
			}
			timing.Runs = append(timing.Runs, float64(time.Since(start).Microseconds())/1000)
			if got.Totals.Sent == nil || *got.Totals.Sent == 0 {
				t.Fatalf("%s %s read no sends: %s", who, p, totalsLine(got.Totals))
			}
		}
		sorted := append([]float64{}, timing.Runs...)
		sort.Float64s(sorted)
		timing.MedianMs = sorted[len(sorted)/2]
		timing.StatementMs = slowestStatements(t, pool, grant, p, now)
		timings = append(timings, timing)
	}
	read("staff, every organization", scaleGrant{grant: staff()}, statsperiod.Last90Days)
	read("staff, every organization", scaleGrant{grant: staff()}, statsperiod.Last12Months)
	read("member of one organization", scaleGrant{grant: memberOf("scale-org-7")}, statsperiod.Last12Months)

	evidence := map[string]any{
		"facts": facts, "organizations": 50, "days": 400, "writtenIn": written.String(),
		"statementTimeout": "8s (the request pool's)", "reads": timings,
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", raw)
}

type scaleGrant struct {
	grant     memql.AccessGrant
	requested []string
}

// slowestStatements times each of the read's statements once, in the read's
// own transaction and order, so the evidence says which one the time goes to:
// measuredSince, the walks, the days, the campaigns, the automations, the
// audiences and the organizations.
func slowestStatements(t *testing.T, db *bun.DB, g scaleGrant, p statsperiod.Period, now time.Time) []float64 {
	t.Helper()
	ids, everything := allowedAccounts(g.grant, g.requested)
	scope := orgScope{ids: ids, allAccounts: everything}
	b := statsperiod.For(p, now)
	from := b.Start
	if b.HasPrevious {
		from = b.PrevStart
	}
	var ms []float64
	timed := func(fn func(ctx context.Context, tx bun.Tx) error) func(ctx context.Context, tx bun.Tx) error {
		return func(ctx context.Context, tx bun.Tx) error {
			start := time.Now()
			err := fn(ctx, tx)
			ms = append(ms, float64(time.Since(start).Microseconds())/1000)
			return err
		}
	}
	steps := []func(ctx context.Context, tx bun.Tx) error{
		timed(func(ctx context.Context, tx bun.Tx) error { _, err := readMeasuredSince(ctx, tx); return err }),
		timed(func(ctx context.Context, tx bun.Tx) error { _, err := readCoverage(ctx, tx, now, now); return err }),
		timed(func(ctx context.Context, tx bun.Tx) error {
			_, err := readDays(ctx, tx, scope, from, b.End)
			return err
		}),
		timed(func(ctx context.Context, tx bun.Tx) error {
			_, err := readCampaigns(ctx, tx, scope, b.Start, b.End)
			return err
		}),
		timed(func(ctx context.Context, tx bun.Tx) error {
			_, err := readAutomations(ctx, tx, scope, b.Start, b.End)
			return err
		}),
		timed(func(ctx context.Context, tx bun.Tx) error {
			_, err := readAudiences(ctx, tx, scope, b.Start, b.End)
			return err
		}),
		timed(func(ctx context.Context, tx bun.Tx) error {
			_, err := readAccounts(ctx, tx, scope, b.Start, b.End)
			return err
		}),
	}
	err := statisticsTx(context.Background(), db, func(ctx context.Context, tx bun.Tx) error {
		if everything {
			start := time.Now()
			if err := tx.NewRaw(`SELECT DISTINCT account_id FROM email_stat_fact ORDER BY account_id LIMIT ?`, maxStatisticsAccounts+1).Scan(ctx, &scope.ids); err != nil {
				return err
			}
			if len(scope.ids) > maxStatisticsAccounts {
				return fmt.Errorf("statistics account bound exceeded")
			}
			ms = append(ms, float64(time.Since(start).Microseconds())/1000)
		}
		for _, step := range steps {
			if err := step(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("time the statements of %s: %v", p, err)
	}
	return ms
}
