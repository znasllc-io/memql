package emailstats

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// rollup_db_test.go -- the email facts rollup against a real database.
//
// The rows are written by the campaigns mutations the sending engine writes
// with, under internal origin and an owner actor, and read back through the
// engine the way the scheduled run reads them: through the five @serverOnly
// queries, under the automation's maintenance principal. A fake engine would
// assert its own rows; what is worth proving here is that the engine's own
// latest-version walk, its id spellings and its cursor give the facts the
// rules say.
//
// THE DATABASE IS SHARED, so every assertion is about the rows its own test
// wrote (each fixture is its own organization, named by a fresh suffix), and
// every test sets the watermarks it relies on before it runs rather than
// inheriting another test's.
//
// Postgres-gated. CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1,
// so a skip there is a failure rather than a green.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// statsEngine builds one engine over the real database per process, with this
// plug-in registered on it, or skips.
//
// ONE BOOT PER PROCESS, the sharedReadMergeEngine seam's rule (memql#4075): an
// engine boot loads the whole DSL tree. The Once records its own failure so
// every caller reports it rather than the first one swallowing it.
var statsEngineState struct {
	once    sync.Once
	eng     *memql.MemQLEngine
	db      *bun.DB
	pingErr error
	bootErr error
}

func statsEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	s := &statsEngineState
	s.once.Do(func() {
		if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
			s.pingErr = err
			return
		}
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		if err := db.PingContext(context.Background()); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			_ = db.Close()
			return
		}
		eng, err := memql.New(db)
		if err != nil {
			s.bootErr = fmt.Errorf("New: %w", err)
			_ = db.Close()
			return
		}
		eng.Logger = quietLogger()
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("Init: %w", err)
			_ = db.Close()
			return
		}
		// The plug-in as a node registers it, so `builtin emailStatisticsRollup()`
		// dispatches here exactly as the scheduled automation's run does.
		if err := eng.RegisterIntegration(NewIntegration(eng, func() *bun.DB { return db }, quietLogger())); err != nil {
			s.bootErr = fmt.Errorf("RegisterIntegration: %w", err)
			_ = db.Close()
			return
		}
		s.eng, s.db = eng, db
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "the email statistics facts", dbtest.DSN(), s.pingErr)
		return nil, nil
	}
	if s.bootErr != nil {
		t.Fatalf("the statistics engine failed to boot: %v", s.bootErr)
	}
	return s.eng, s.db
}

// principalCtx is the context the scheduled automation runs the builtin
// under: the cluster's maintenance principal for emailStatisticsRollupSweep,
// with the claims and token component/automations stamps beside it.
func principalCtx(t *testing.T) context.Context {
	t.Helper()
	actor := auth.MaintenanceActor("emailStatisticsRollupSweep")
	if actor == nil {
		t.Fatal("emailStatisticsRollupSweep is not on the maintenance list, so its automation runs as a reader and every run is refused")
	}
	claims := map[string]any{"sub": actor.UserId, "email": actor.UserId, "role": string(actor.Role)}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, actor)
}

// rollup runs one rollup at now and fails the test on an error.
func rollup(t *testing.T, eng Engine, db *bun.DB, now time.Time) RollupReport {
	t.Helper()
	rep, err := runRollup(principalCtx(t), eng, db, now)
	if err != nil {
		t.Fatalf("the rollup failed: %v", err)
	}
	return rep
}

// fixture is one organization of its own, with an audience, a template, a
// campaign and a rule, written by its owner the way the campaigns app writes
// them.
type fixture struct {
	t        *testing.T
	eng      *memql.MemQLEngine
	db       *bun.DB
	suffix   string
	user     string
	account  string
	audience string
	template string
	campaign string
	rule     string
	owner    context.Context
	made     int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	eng, db := statsEngine(t)
	f := &fixture{t: t, eng: eng, db: db, suffix: fmt.Sprintf("estat-%d", time.Now().UnixNano())}
	f.user, f.account, f.audience = f.suffix+"-user", f.suffix+"-org", f.suffix+"-aud"
	f.template, f.campaign, f.rule = f.suffix+"-tpl", f.suffix+"-camp", f.suffix+"-rule"

	system := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), auth.SystemActor("email-statistics-test")), &auth.TokenInfo{Subject: "email-statistics-test"})
	f.exec(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"displayName":"Statistics owner","primaryEmail":"%s@example.test","role":"owner","active":true})`, langparser.QuoteString(f.user), f.user))
	f.owner = auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: f.user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: f.user})

	f.mutate("createClientAccount", map[string]any{"accountId": f.account, "name": "Statistics organization"})
	f.mutate("createAudience", map[string]any{"audienceId": f.audience, "name": "Statistics audience", "accountId": f.account})
	f.mutate("createTemplate", map[string]any{
		"templateId": f.template, "name": "Statistics template", "subject": "Hello", "textBody": "Hello", "htmlBody": "Hello",
		"accountId": f.account, "versionTime": stamp(time.Now()),
	})
	f.mutate("createCampaign", map[string]any{"campaignId": f.campaign, "name": "Statistics campaign", "audienceId": f.audience, "templateId": f.template, "accountId": f.account})
	f.mutate("createEmailRule", map[string]any{
		"emailRuleId": f.rule, "name": "Statistics rule", "triggerConcept": "v1:campaigns:recipient", "eventKind": "created",
		"templateId": f.template, "recipientMode": "audience", "audienceId": f.audience, "accountId": f.account,
	})
	return f
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func dayString(t time.Time) string { return t.UTC().Format(time.DateOnly) }

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// exec runs one statement under internal origin, as the server's own writers
// run the campaigns mutations (most are @serverOnly).
func (f *fixture) exec(ctx context.Context, q string) {
	f.t.Helper()
	if _, err := f.eng.Execute(auth.ContextWithInternalOrigin(ctx), q); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *fixture) mutate(name string, args map[string]any) {
	f.t.Helper()
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(f.owner, "mutation "+call)
}

// audienceOf makes another audience in the fixture's organization.
func (f *fixture) newAudience() string {
	f.made++
	id := fmt.Sprintf("%s-aud-%d", f.suffix, f.made)
	f.mutate("createAudience", map[string]any{"audienceId": id, "name": "Another audience", "accountId": f.account})
	return id
}

// recipient adds one address to an audience and answers its id.
func (f *fixture) recipient(audience string) string {
	f.made++
	id := fmt.Sprintf("%s-rcp-%d", f.suffix, f.made)
	f.mutate("addRecipient", map[string]any{"recipientId": id, "audienceId": audience, "email": id + "@example.test", "accountId": f.account})
	return id
}

// deliver records one delivery to a recipient and answers its bare id. A
// recipient with more than one delivery must name each by its requestId.
func (f *fixture) deliver(recipient string, args map[string]any) string {
	f.t.Helper()
	full := map[string]any{"recipientId": recipient, "email": recipient + "@example.test", "accountId": f.account}
	for k, v := range args {
		full[k] = v
	}
	f.mutate("recordCampaignDelivery", full)
	request, _ := args["requestId"].(string)
	q := f.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("DISTINCT id").
		Where("concept = ?", "v1:campaigns:delivery").
		Where("payload->>'recipientId' IN (?)", bun.In([]string{recipient, "v1:campaigns:recipient:" + recipient}))
	if request != "" {
		q = q.Where("payload->>'requestId' = ?", request)
	} else {
		q = q.Where("COALESCE(payload->>'requestId', '') = ''")
	}
	var ids []string
	if err := q.Scan(context.Background(), &ids); err != nil {
		f.t.Fatalf("find the delivery to %s: %v", recipient, err)
	}
	if len(ids) != 1 {
		f.t.Fatalf("the delivery to %s (request %q) is %d rows, want 1: %v", recipient, request, len(ids), ids)
	}
	return memql.BareShortId(ids[0])
}

// providerReport records the provider's verdict on a delivery, as the
// feedback ingester does.
func (f *fixture) providerReport(delivery, status string, at time.Time) {
	f.mutate("recordProviderDelivery", map[string]any{
		"deliveryId": delivery, "providerStatus": status, "providerAt": stamp(at),
		"versionTime": stamp(time.Now().Add(time.Millisecond)),
	})
}

// engage records one open or click of a campaign delivery, as the tracking
// endpoint does, and answers the ids of every such event of that delivery.
func (f *fixture) engage(delivery, kind string, at time.Time) []string {
	f.t.Helper()
	f.mutate("recordEngagementEvent", map[string]any{"campaignId": f.campaign, "deliveryId": delivery, "kind": kind, "occurredAt": stamp(at), "accountId": f.account})
	var ids []string
	err := f.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("DISTINCT id").
		Where("concept = ?", "v1:campaigns:engagementEvent").
		Where("payload->>'deliveryId' IN (?)", bun.In([]string{delivery, "v1:campaigns:delivery:" + delivery})).
		Where("payload->>'kind' = ?", kind).
		Scan(context.Background(), &ids)
	if err != nil {
		f.t.Fatalf("find the %s events of %s: %v", kind, delivery, err)
	}
	for i := range ids {
		ids[i] = memql.BareShortId(ids[i])
	}
	return ids
}

// consent appends one consent event and answers its id. An empty recipient
// writes an event about an address with no membership.
func (f *fixture) consent(mutation, recipient string, at time.Time) string {
	f.made++
	id := fmt.Sprintf("%s-ce-%d", f.suffix, f.made)
	args := map[string]any{"eventId": id, "emailDigest": sha256Hex(id + "@example.test"), "source": "operator", "occurredAt": stamp(at), "accountId": f.account}
	if recipient != "" {
		args["recipientId"] = recipient
		args["emailDigest"] = sha256Hex(recipient + "@example.test")
	}
	if mutation == "recordConsentSuppress" {
		args["reason"] = "operator request"
	}
	f.mutate(mutation, args)
	return id
}

// journeyWithEnrollment makes an automation with one send step and enrolls a
// recipient in it, as the journey drain does, and answers the journey and the
// enrollment, whose id is a SHA-256 in hex like every enrollment's.
func (f *fixture) journeyWithEnrollment(recipient string) (journey, enrollment string) {
	f.made++
	journey = fmt.Sprintf("%s-journey-%d", f.suffix, f.made)
	enrollment = sha256Hex(journey + "|" + recipient)
	now := time.Now()
	f.mutate("createJourney", map[string]any{
		"journeyId": journey, "accountId": f.account, "name": "Welcome",
		"trigger":     map[string]any{"kind": "joined", "audienceId": f.audience},
		"steps":       []map[string]any{{"id": "welcome", "kind": "send", "templateId": f.template}},
		"versionTime": stamp(now),
	})
	f.mutate("enrollJourneyRecipient", map[string]any{
		"enrollmentId": enrollment, "accountId": f.account, "journeyId": journey, "recipientId": recipient,
		"emailDigest": sha256Hex(recipient + "@example.test"), "stepId": "welcome",
		"enrolledAt": stamp(now), "triggeredAt": stamp(now),
	})
	return journey, enrollment
}

// walkFrom sets every source's watermark to at, so the next run walks from
// five minutes before it -- the rows this test is about, and at most a few
// minutes of the tests before it.
func walkFrom(t *testing.T, db *bun.DB, at time.Time) {
	t.Helper()
	for _, s := range sources {
		if _, err := db.NewRaw(`INSERT INTO email_stat_watermark (source, through) VALUES (?, ?)
			ON CONFLICT (source) DO UPDATE SET through = EXCLUDED.through`, s.name, at.UTC()).Exec(context.Background()); err != nil {
			t.Fatalf("set the %s watermark: %v", s.name, err)
		}
	}
}

func watermarkOf(t *testing.T, db *bun.DB, source string) (time.Time, bool) {
	t.Helper()
	var through time.Time
	err := db.NewRaw(`SELECT through FROM email_stat_watermark WHERE source = ?`, source).Scan(context.Background(), &through)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read the %s watermark: %v", source, err)
	}
	return through, true
}

type storedFact struct {
	Kind       string `bun:"kind"`
	SourceID   string `bun:"source_id"`
	AccountID  string `bun:"account_id"`
	CampaignID string `bun:"campaign_id"`
	RuleID     string `bun:"rule_id"`
	JourneyID  string `bun:"journey_id"`
	AudienceID string `bun:"audience_id"`
	Day        string `bun:"day"`
}

// storedFacts answers the facts recorded for these sources, keyed kind|source.
func storedFacts(t *testing.T, db *bun.DB, sourceIDs ...string) map[string]storedFact {
	t.Helper()
	var rows []storedFact
	err := db.NewRaw(`SELECT kind, source_id, account_id, campaign_id, rule_id, journey_id, audience_id, to_char(day, 'YYYY-MM-DD') AS day
		FROM email_stat_fact WHERE source_id IN (?)`, bun.In(sourceIDs)).Scan(context.Background(), &rows)
	if err != nil {
		t.Fatalf("read the facts: %v", err)
	}
	out := make(map[string]storedFact, len(rows))
	for _, r := range rows {
		out[r.Kind+"|"+r.SourceID] = r
	}
	return out
}

func wantFact(t *testing.T, facts map[string]storedFact, kind, source string) storedFact {
	t.Helper()
	f, ok := facts[kind+"|"+source]
	if !ok {
		t.Errorf("no %s fact for %s", kind, source)
	}
	return f
}

func wantNoFact(t *testing.T, facts map[string]storedFact, kind, source string) {
	t.Helper()
	if f, ok := facts[kind+"|"+source]; ok {
		t.Errorf("a %s fact for %s was recorded (%+v), want none", kind, source, f)
	}
}

// Every rule of section 5, through the real mutations and the real walk.
func TestRollupRecordsFactsFromTheGraph(t *testing.T) {
	f := newFixture(t)
	start := time.Now().UTC().Truncate(time.Microsecond)
	walkFrom(t, f.db, start)

	r1, r2, r3, r4, r5 := f.recipient(f.audience), f.recipient(f.audience), f.recipient(f.audience), f.recipient(f.audience), f.recipient(f.audience)
	sentAt := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC).Add(-30 * time.Minute) // yesterday, 23:30
	sent := f.deliver(r1, map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(sentAt)})
	reported := f.deliver(r2, map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(start)})
	providerAt := start.Add(-time.Hour)
	f.providerReport(reported, "delivered", providerAt)
	failed := f.deliver(r3, map[string]any{"campaignId": f.campaign, "status": "failed", "lastError": "the transport refused it"})
	suppressed := f.deliver(r4, map[string]any{"campaignId": f.campaign, "status": "skipped", "skipReason": "hard_bounce"})
	unsubscribed := f.deliver(r5, map[string]any{"campaignId": f.campaign, "status": "skipped", "skipReason": "unsubscribed"})

	firstOpen, secondOpen, clickAt := start.Add(-50*time.Minute), start.Add(-20*time.Minute), start.Add(-10*time.Minute)
	f.engage(sent, "open", firstOpen)
	opens := f.engage(sent, "open", secondOpen)
	clicks := f.engage(sent, "click", clickAt)
	if len(opens) != 2 || len(clicks) != 1 {
		t.Fatalf("the tracking writes made %d opens and %d clicks, want 2 and 1", len(opens), len(clicks))
	}

	occurred := start.Add(-3 * time.Hour)
	withdraw := f.consent("recordConsentWithdraw", r1, occurred)
	grant := f.consent("recordConsentGrant", r2, occurred)
	bounce := f.consent("recordConsentBounce", r3, occurred)
	complaint := f.consent("recordConsentComplaint", r4, occurred)
	suppress := f.consent("recordConsentSuppress", r5, occurred)

	rep := rollup(t, f.eng, f.db, time.Now())
	if rep.Rows[sourceDelivery] < 5 || rep.Rows[sourceEngagement] < 3 || rep.Rows[sourceConsent] < 5 {
		t.Errorf("the run read %v rows, want at least this test's 5 deliveries, 3 engagement events and 5 consent events", rep.Rows)
	}
	// Seven from the deliveries (sent, sent and delivered, failed, skipped and
	// suppressed, skipped), five from the engagement (two opens and a click,
	// a unique open and a unique click) and four from the consent events.
	if rep.Facts < 16 {
		t.Errorf("the run recorded %d facts, want at least this test's 16", rep.Facts)
	}

	all := append([]string{sent, reported, failed, suppressed, unsubscribed, withdraw, grant, bounce, complaint, suppress}, append(opens, clicks...)...)
	facts := storedFacts(t, f.db, all...)

	s := wantFact(t, facts, kindSent, sent)
	if s.Day != dayString(sentAt) || s.AccountID != f.account || s.CampaignID != f.campaign {
		t.Errorf("the sent fact is %+v, want day %s in %s for %s", s, dayString(sentAt), f.account, f.campaign)
	}
	wantFact(t, facts, kindSent, reported)
	if d := wantFact(t, facts, kindDelivered, reported); d.Day != dayString(providerAt) {
		t.Errorf("the delivered fact is dated %s, want the provider's %s", d.Day, dayString(providerAt))
	}
	wantNoFact(t, facts, kindUndelivered, reported)
	wantFact(t, facts, kindFailed, failed)
	wantNoFact(t, facts, kindSent, failed)
	wantFact(t, facts, kindSkipped, suppressed)
	wantFact(t, facts, kindSuppressed, suppressed)
	wantFact(t, facts, kindSkipped, unsubscribed)
	wantNoFact(t, facts, kindSuppressed, unsubscribed)

	for _, id := range opens {
		wantFact(t, facts, kindOpen, id)
	}
	if u := wantFact(t, facts, kindOpenUnique, sent); u.Day != dayString(firstOpen) {
		t.Errorf("the unique open is dated %s, want the first open's %s", u.Day, dayString(firstOpen))
	}
	wantFact(t, facts, kindClick, clicks[0])
	if u := wantFact(t, facts, kindClickUnique, sent); u.CampaignID != f.campaign || u.AccountID != f.account {
		t.Errorf("the unique click is %+v, want it in %s for %s", u, f.account, f.campaign)
	}

	for kind, id := range map[string]string{kindUnsubscribe: withdraw, kindSubscribe: grant, kindBounce: bounce, kindComplaint: complaint} {
		if c := wantFact(t, facts, kind, id); c.Day != dayString(occurred) || c.AccountID != f.account {
			t.Errorf("the %s fact is %+v, want day %s in %s", kind, c, dayString(occurred), f.account)
		}
	}
	for key := range facts {
		if strings.HasSuffix(key, "|"+suppress) {
			t.Errorf("an operator's suppression was recorded as %s; it counts as nothing", key)
		}
	}
}

// A run that reads rows another run already folded counts none of them again,
// and a later version of a row adds only what it newly says.
func TestRerunsCountNothingTwice(t *testing.T) {
	f := newFixture(t)
	start := time.Now().UTC().Truncate(time.Microsecond)
	walkFrom(t, f.db, start)

	r1 := f.recipient(f.audience)
	sent := f.deliver(r1, map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(start)})
	f.engage(sent, "open", start.Add(-time.Hour))
	opens := f.engage(sent, "open", start.Add(-30*time.Minute))
	withdraw := f.consent("recordConsentWithdraw", r1, start)
	ids := append([]string{sent, withdraw}, opens...)

	rollup(t, f.eng, f.db, time.Now())
	first := storedFacts(t, f.db, ids...)
	if len(first) != 5 {
		t.Fatalf("the first run recorded %d facts for this test (%v), want sent, two opens, one unique open and an unsubscribe", len(first), first)
	}

	again := rollup(t, f.eng, f.db, time.Now())
	if again.Facts != 0 {
		t.Errorf("a run over rows already folded recorded %d facts, want none", again.Facts)
	}
	if second := storedFacts(t, f.db, ids...); fmt.Sprint(second) != fmt.Sprint(first) {
		t.Errorf("a rerun changed the facts:\n got %v\nwant %v", second, first)
	}

	// The provider's report is a new version of the same delivery: the walk
	// reads the delivery again, records the delivery it newly reports, and
	// keeps the one send it already counted, on its day.
	f.providerReport(sent, "delivered", start.Add(time.Minute))
	rollup(t, f.eng, f.db, time.Now())
	third := storedFacts(t, f.db, ids...)
	wantFact(t, third, kindDelivered, sent)
	if third[kindSent+"|"+sent] != first[kindSent+"|"+sent] {
		t.Errorf("the send changed from %+v to %+v when the provider reported", first[kindSent+"|"+sent], third[kindSent+"|"+sent])
	}
	if len(third) != 6 {
		t.Errorf("after the report there are %d facts (%v), want the five and one delivered", len(third), third)
	}
}

// The walk starts five minutes before the watermark, so a row written after a
// run but dated before its watermark -- a writer whose clock lags, a commit
// that landed late -- is still counted. A row dated further back than that is
// not: the overlap is the bound.
func TestTheWatermarkOverlapCatchesALateWrite(t *testing.T) {
	f := newFixture(t)
	start := time.Now().UTC().Truncate(time.Microsecond)
	walkFrom(t, f.db, start)

	early := f.deliver(f.recipient(f.audience), map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(start)})
	rollup(t, f.eng, f.db, time.Now())
	through, ok := watermarkOf(t, f.db, sourceDelivery)
	if !ok || through.Before(start) {
		t.Fatalf("the delivery watermark is %v (%v), want it at or after the run's newest row", through, ok)
	}

	lateAt := through.Add(-2 * time.Minute)
	late := f.deliver(f.recipient(f.audience), map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(lateAt), "versionTime": stamp(lateAt)})
	tooLateAt := through.Add(-watermarkOverlap - 5*time.Minute)
	tooLate := f.deliver(f.recipient(f.audience), map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(tooLateAt), "versionTime": stamp(tooLateAt)})
	rollup(t, f.eng, f.db, time.Now())

	facts := storedFacts(t, f.db, early, late, tooLate)
	wantFact(t, facts, kindSent, early)
	if l := wantFact(t, facts, kindSent, late); l.Day != dayString(lateAt) {
		t.Errorf("the late write is dated %s, want %s", l.Day, dayString(lateAt))
	}
	wantNoFact(t, facts, kindSent, tooLate)
	if after, _ := watermarkOf(t, f.db, sourceDelivery); after.Before(through) {
		t.Errorf("the watermark moved back from %v to %v", through, after)
	}
}

// A row dated ahead of the clock -- a writer whose clock runs fast, a version
// time somebody got wrong -- is counted, and does not carry the watermark with
// it: if it did, every row written after it with a true time would sort
// before the watermark less five minutes, and the walk would stop seeing them
// until the clock caught up. And a watermark already ahead of the clock says
// nothing true about how far the walk read, so the walk starts over.
func TestARowDatedAheadOfTheClockDoesNotHoldTheWalk(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	ahead := f.deliver(f.recipient(f.audience), map[string]any{
		"campaignId": f.campaign, "status": "sent", "sentAt": stamp(time.Now()),
		"versionTime": stamp(time.Now().Add(400 * 24 * time.Hour)),
	})
	rollup(t, f.eng, f.db, time.Now())
	if through, _ := watermarkOf(t, f.db, sourceDelivery); through.After(time.Now()) {
		t.Errorf("a row dated ahead of the clock carried the watermark to %v", through)
	}
	present := f.deliver(f.recipient(f.audience), map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(time.Now())})
	rollup(t, f.eng, f.db, time.Now())
	facts := storedFacts(t, f.db, ahead, present)
	wantFact(t, facts, kindSent, ahead)
	wantFact(t, facts, kindSent, present)

	// A stored watermark far ahead of the run's clock. The clock is put four
	// hundred days after start, so starting over walks from start: this
	// test's rows and nothing older in this database.
	start := time.Now().UTC().Truncate(time.Microsecond)
	restarted := f.deliver(f.recipient(f.audience), map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(time.Now())})
	now := start.Add(retentionDays * 24 * time.Hour)
	walkFrom(t, f.db, now.Add(retentionDays*24*time.Hour))
	rollup(t, f.eng, f.db, now)
	wantFact(t, storedFacts(t, f.db, restarted), kindSent, restarted)
	if through, ok := watermarkOf(t, f.db, sourceDelivery); !ok || through.After(now) {
		t.Errorf("after starting over the watermark is %v (%v), want one at or before the run's clock", through, ok)
	}
}

// The first run has no watermark and starts four hundred days back, so on a
// cluster with history it is a backfill: forty pages a source per run, and
// the next run carries on from where it stopped. Pages the next run only
// reads again inside the overlap do not count against its forty, or a burst
// of more than forty pages inside five minutes would hold the walk there for
// good.
func TestTheFirstRunWalksAtMostFortyPagesPerSource(t *testing.T) {
	f := newFixture(t)
	pageSize := walkPageSize
	walkPageSize = 2
	t.Cleanup(func() { walkPageSize = pageSize })

	recipient := f.recipient(f.audience)
	const extra = 5
	n := maxPagesPerRun*walkPageSize + extra
	start := time.Now().UTC().Truncate(time.Microsecond)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, f.deliver(recipient, map[string]any{
			"emailRuleId": f.rule, "requestId": fmt.Sprintf("%s-page-%03d", f.suffix, i),
			"status": "sent", "sentAt": stamp(time.Now()),
		}))
	}

	if _, err := f.db.NewRaw(`DELETE FROM email_stat_watermark`).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The first run's window opens four hundred days before now; a now four
	// hundred days after start opens it at start, so the walk meets this
	// test's rows first and nothing else this database holds. Its retention
	// pass, at that now, deletes every fact dated before today: they are this
	// package's own derived rows, and every test here writes the facts it reads.
	now := start.Add(retentionDays * 24 * time.Hour)

	first := rollup(t, f.eng, f.db, now)
	if got := first.Rows[sourceDelivery]; got != maxPagesPerRun*walkPageSize {
		t.Errorf("the first run read %d deliveries, want %d pages of %d", got, maxPagesPerRun, walkPageSize)
	}
	if !behind(first, sourceDelivery) {
		t.Errorf("the first run stopped at its page bound without saying the deliveries are behind: %+v", first)
	}
	if got := len(storedFacts(t, f.db, ids...)); got != maxPagesPerRun*walkPageSize {
		t.Errorf("the first run recorded %d sends, want %d", got, maxPagesPerRun*walkPageSize)
	}

	second := rollup(t, f.eng, f.db, now)
	if behind(second, sourceDelivery) {
		t.Errorf("the second run is still behind: %+v", second)
	}
	if got := len(storedFacts(t, f.db, ids...)); got != n {
		t.Errorf("after the second run %d of the %d sends are recorded, want every one", got, n)
	}
}

func behind(rep RollupReport, source string) bool {
	for _, s := range rep.Behind {
		if s == source {
			return true
		}
	}
	return false
}

// D18: a rule's send counts under its rule, and has no campaign.
func TestARuleSendIsAttributedToItsRule(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	sent := f.deliver(f.recipient(f.audience), map[string]any{"emailRuleId": f.rule, "status": "sent", "sentAt": stamp(time.Now())})
	failed := f.deliver(f.recipient(f.audience), map[string]any{"emailRuleId": f.rule, "status": "failed", "lastError": "the transport refused it"})
	rollup(t, f.eng, f.db, time.Now())

	facts := storedFacts(t, f.db, sent, failed)
	for kind, id := range map[string]string{kindSent: sent, kindFailed: failed} {
		if r := wantFact(t, facts, kind, id); r.RuleID != f.rule || r.CampaignID != "" || r.JourneyID != "" || r.AccountID != f.account {
			t.Errorf("the rule's %s fact is %+v, want rule %s in %s and no campaign or journey", kind, r, f.rule, f.account)
		}
	}
}

// D18: a journey's send names its enrollment in its request id, and counts
// under the enrollment's journey; a request id naming no enrollment counts
// under no journey.
func TestAJourneySendIsAttributedToItsJourney(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	member := f.recipient(f.audience)
	journey, enrollment := f.journeyWithEnrollment(member)
	sent := f.deliver(member, map[string]any{"requestId": "journey-" + enrollment + "-welcome", "templateId": f.template, "status": "sent", "sentAt": stamp(time.Now())})
	orphan := f.deliver(f.recipient(f.audience), map[string]any{"requestId": "journey-" + sha256Hex(f.suffix+"-nobody") + "-welcome", "status": "sent", "sentAt": stamp(time.Now())})
	rollup(t, f.eng, f.db, time.Now())

	facts := storedFacts(t, f.db, sent, orphan)
	if j := wantFact(t, facts, kindSent, sent); j.JourneyID != journey || j.RuleID != "" || j.CampaignID != "" {
		t.Errorf("the journey's send is %+v, want journey %s and no rule or campaign", j, journey)
	}
	if o := wantFact(t, facts, kindSent, orphan); o.JourneyID != "" {
		t.Errorf("a send whose request names no enrollment was attributed to journey %q", o.JourneyID)
	}
}

// D18: a subscribe or an unsubscribe counts under the audience of the
// recipient it is about; other consent events carry no audience.
func TestASubscribeIsAttributedToItsRecipientsAudience(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	other := f.newAudience()
	here, there := f.recipient(f.audience), f.recipient(other)
	grant := f.consent("recordConsentGrant", here, time.Now())
	withdraw := f.consent("recordConsentWithdraw", there, time.Now())
	bounce := f.consent("recordConsentBounce", here, time.Now())
	rollup(t, f.eng, f.db, time.Now())

	facts := storedFacts(t, f.db, grant, withdraw, bounce)
	if s := wantFact(t, facts, kindSubscribe, grant); s.AudienceID != f.audience {
		t.Errorf("the subscribe counts under audience %q, want its recipient's %s", s.AudienceID, f.audience)
	}
	if u := wantFact(t, facts, kindUnsubscribe, withdraw); u.AudienceID != other {
		t.Errorf("the unsubscribe counts under audience %q, want its recipient's %s", u.AudienceID, other)
	}
	if b := wantFact(t, facts, kindBounce, bounce); b.AudienceID != "" {
		t.Errorf("a bounce was attributed to audience %q; only a subscription change is", b.AudienceID)
	}
}

// A consent event about an address with no membership counts in its
// organization's totals and under no audience.
func TestAConsentEventWithNoRecipientCountsOnlyInTotals(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	withdraw := f.consent("recordConsentWithdraw", "", time.Now())
	rollup(t, f.eng, f.db, time.Now())

	u := wantFact(t, storedFacts(t, f.db, withdraw), kindUnsubscribe, withdraw)
	if u.AccountID != f.account || u.AudienceID != "" {
		t.Errorf("the unsubscribe is %+v, want it in %s under no audience", u, f.account)
	}
}

// scriptedEngine answers each walk query from pages keyed by the cursor it was
// asked with, so a test can make a page fail or a cursor repeat.
type scriptedEngine struct {
	pages map[string]map[string]scriptedPage
	// calls records, per call, the construct asked and how it was asked.
	calls []scriptedCall
}

type scriptedPage struct {
	rows []any
	next string
	err  error
}

type scriptedCall struct {
	construct                  string
	internal, fresh, rootsOnly bool
}

var queryName = regexp.MustCompile(`query ([A-Za-z0-9_]+)\(`)

func (e *scriptedEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	m := queryName.FindStringSubmatch(query)
	if m == nil {
		return nil, fmt.Errorf("unexpected statement %q", query)
	}
	e.calls = append(e.calls, scriptedCall{
		construct: m[1], internal: auth.OriginFromContext(ctx).IsInternal(),
		fresh: memql.FreshReadFromContext(ctx), rootsOnly: memql.RootsOnlyReadFromContext(ctx),
	})
	page, ok := e.pages[m[1]][memql.CursorFromContext(ctx)]
	if !ok {
		return memql.NewResultWithOutput([]any{}), nil
	}
	if page.err != nil {
		return nil, page.err
	}
	res := memql.NewResultWithOutput(page.rows)
	res.SetMeta(&memql.ResultMeta{Cursor: page.next, HasMore: page.next != ""})
	return res, nil
}

// The watermark moves only past pages whose facts are written: a page that
// fails leaves it at the page before, and its source's error does not stop
// the other sources. A cursor that comes back unchanged is an error, never a
// loop.
func TestAFailedPageLeavesTheWatermarkAtTheLastWrittenPage(t *testing.T) {
	_, db := statsEngine(t)
	suffix := fmt.Sprintf("scripted-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		// The last case leaves the watermarks ahead of the clock; put them
		// back, so a later run in this database reads what is written next.
		walkFrom(t, db, time.Now())
	})
	// Whole milliseconds, because the database keeps microseconds and the
	// watermark is compared with these instants exactly. Hours back, so every
	// instant here, the last case's included, is behind the clock.
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-3 * time.Hour)
	walkFrom(t, db, base)
	row := func(n int, at time.Time) any {
		return map[string]any{"id": fmt.Sprintf("v1:campaigns:delivery:%s-%d", suffix, n), "accountId": "org-x", "status": "sent", "sentAt": stamp(at), "createdAt": at}
	}
	t1, t2, t3 := base.Add(10*time.Minute), base.Add(20*time.Minute), base.Add(30*time.Minute)

	failing := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince": {
			"":       {rows: []any{row(1, t1), row(2, t2)}, next: "page-2"},
			"page-2": {err: errors.New("the engine went away")},
		},
		"consentEventsWrittenSince": {
			"": {rows: []any{map[string]any{"id": "v1:campaigns:consentEvent:" + suffix + "-c", "kind": "grant", "occurredAt": stamp(t3), "createdAt": t3}}},
		},
	}}
	rep, err := runRollup(principalCtx(t), failing, db, time.Now())
	if err == nil || !strings.Contains(err.Error(), sourceDelivery) {
		t.Fatalf("a failed page answered %v, want an error naming the %s source", err, sourceDelivery)
	}
	if rep.Rows[sourceDelivery] != 2 {
		t.Errorf("the run read %d deliveries before the failure, want 2", rep.Rows[sourceDelivery])
	}
	if through, _ := watermarkOf(t, db, sourceDelivery); !through.Equal(t2) {
		t.Errorf("the delivery watermark is %v, want the written page's newest row, %v", through, t2)
	}
	if through, _ := watermarkOf(t, db, sourceConsent); !through.Equal(t3) {
		t.Errorf("the consent watermark is %v, want %v: one source's failure must not stop another", through, t3)
	}
	facts := storedFacts(t, db, suffix+"-1", suffix+"-2", suffix+"-c")
	wantFact(t, facts, kindSent, suffix+"-1")
	wantFact(t, facts, kindSent, suffix+"-2")
	wantFact(t, facts, kindSubscribe, suffix+"-c")

	walkFrom(t, db, base)
	looping := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince": {
			"":     {rows: []any{row(3, t1)}, next: "same"},
			"same": {rows: []any{row(4, t2)}, next: "same"},
		},
	}}
	if _, err := runRollup(principalCtx(t), looping, db, time.Now()); err == nil || !strings.Contains(err.Error(), "advance") {
		t.Fatalf("a cursor that repeats answered %v, want an error saying the walk did not advance", err)
	}

	// A run that only reads rows older than the watermark -- the overlap, or
	// a slower run on another replica finishing behind a faster one -- leaves
	// the watermark where it was: it never moves back.
	ahead := t3.Add(time.Hour)
	walkFrom(t, db, ahead)
	behindOnly := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince": {"": {rows: []any{row(5, t1)}}},
	}}
	if _, err := runRollup(principalCtx(t), behindOnly, db, time.Now()); err != nil {
		t.Fatal(err)
	}
	if through, _ := watermarkOf(t, db, sourceDelivery); !through.Equal(ahead) {
		t.Errorf("a page older than the watermark moved it from %v to %v", ahead, through)
	}
}

// Every read the rollup makes, the three walks and the two lookups alike, is
// made with internal origin (the five queries are @serverOnly), as a fresh read
// (a cached page could predate the rows it is looking for) and roots-only
// (every shape it reads is a row's own fields; expanded, a page of deliveries
// costs two statements per relationship per row).
func TestEveryReadIsInternalFreshAndRootsOnly(t *testing.T) {
	_, db := statsEngine(t)
	suffix := fmt.Sprintf("context-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		walkFrom(t, db, time.Now())
	})
	at := time.Now().UTC().Add(-time.Minute)
	walkFrom(t, db, at)
	enrollment := sha256Hex(suffix)
	engine := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince": {"": {rows: []any{map[string]any{
			"id": "v1:campaigns:delivery:" + suffix + "-d", "status": "sent", "sentAt": stamp(at), "createdAt": at,
			"requestId": "journey-" + enrollment + "-welcome",
		}}}},
		"consentEventsWrittenSince": {"": {rows: []any{map[string]any{
			"id": "v1:campaigns:consentEvent:" + suffix + "-c", "kind": "grant", "occurredAt": stamp(at), "createdAt": at,
			"recipientId": "v1:campaigns:recipient:" + suffix + "-r",
		}}}},
		"journeyEnrollmentsByIds": {"": {rows: []any{map[string]any{
			"id": "v1:campaigns:journeyEnrollment:" + enrollment, "journeyId": "v1:campaigns:journey:" + suffix + "-j",
		}}}},
		"recipientAudiencesByIds": {"": {rows: []any{map[string]any{
			"id": "v1:campaigns:recipient:" + suffix + "-r", "audienceId": "v1:campaigns:audience:" + suffix + "-a",
		}}}},
	}}
	if _, err := runRollup(principalCtx(t), engine, db, time.Now()); err != nil {
		t.Fatal(err)
	}

	asked := map[string]bool{}
	for _, c := range engine.calls {
		asked[c.construct] = true
		if !c.internal || !c.fresh || !c.rootsOnly {
			t.Errorf("%s was read with internal origin %v, fresh %v, roots-only %v; want all three", c.construct, c.internal, c.fresh, c.rootsOnly)
		}
	}
	for _, construct := range []string{"deliveriesWrittenSince", "engagementEventsWrittenSince", "consentEventsWrittenSince", "journeyEnrollmentsByIds", "recipientAudiencesByIds"} {
		if !asked[construct] {
			t.Errorf("the run never read %s", construct)
		}
	}
	facts := storedFacts(t, db, suffix+"-d", suffix+"-c")
	if f := wantFact(t, facts, kindSent, suffix+"-d"); f.JourneyID != suffix+"-j" {
		t.Errorf("the journey send counts under journey %q, want %s-j", f.JourneyID, suffix)
	}
	if f := wantFact(t, facts, kindSubscribe, suffix+"-c"); f.AudienceID != suffix+"-a" {
		t.Errorf("the subscribe counts under audience %q, want %s-a", f.AudienceID, suffix)
	}
}

// recordingEngine answers from the real engine and keeps every row each
// construct answered, so a test can see what the walk was handed.
type recordingEngine struct {
	inner Engine
	rows  map[string][]map[string]any
}

func (e *recordingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	res, err := e.inner.Execute(ctx, query)
	if m := queryName.FindStringSubmatch(query); m != nil && err == nil {
		e.rows[m[1]] = append(e.rows[m[1]], memql.MaterializeRows(res)...)
	}
	return res, err
}

// The walk is handed what the facts read and nothing else. Least data is the
// rule for anything about a recipient: no address and no address digest,
// and no other field the facts do not use, reaches the rollup from any of the
// five reads -- so a field added to one of their shapes has to be argued here.
func TestTheWalkReadsNoAddressAndNothingTheFactsDoNotUse(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	member := f.recipient(f.audience)
	_, enrollment := f.journeyWithEnrollment(member)
	f.deliver(member, map[string]any{"requestId": "journey-" + enrollment + "-welcome", "status": "sent", "sentAt": stamp(time.Now())})
	sent := f.deliver(f.recipient(f.audience), map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(time.Now())})
	f.engage(sent, "click", time.Now())
	f.consent("recordConsentGrant", member, time.Now())

	recorder := &recordingEngine{inner: f.eng, rows: map[string][]map[string]any{}}
	if _, err := runRollup(principalCtx(t), recorder, f.db, time.Now()); err != nil {
		t.Fatal(err)
	}

	read := map[string][]string{
		"deliveriesWrittenSince":       {"id", "accountId", "campaignId", "emailRuleId", "requestId", "status", "skipReason", "sentAt", "providerStatus", "providerAt", "createdAt"},
		"engagementEventsWrittenSince": {"id", "accountId", "campaignId", "deliveryId", "kind", "occurredAt", "createdAt"},
		"consentEventsWrittenSince":    {"id", "accountId", "campaignId", "recipientId", "kind", "occurredAt", "createdAt"},
		"journeyEnrollmentsByIds":      {"id", "journeyId"},
		"recipientAudiencesByIds":      {"id", "audienceId"},
	}
	for construct, fields := range read {
		rows := recorder.rows[construct]
		if len(rows) == 0 {
			t.Errorf("%s answered no rows, so this test proves nothing about it", construct)
			continue
		}
		allowed := map[string]bool{}
		for _, field := range fields {
			allowed[field] = true
		}
		extra := map[string]bool{}
		for _, row := range rows {
			for key := range row {
				if !allowed[key] {
					extra[key] = true
				}
			}
		}
		for _, address := range []string{"email", "emailDigest"} {
			if extra[address] {
				t.Errorf("%s handed the walk the recipient's %s", construct, address)
			}
		}
		if len(extra) > 0 {
			keys := make([]string, 0, len(extra))
			for key := range extra {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			t.Errorf("%s handed the walk %v, which no fact reads", construct, keys)
		}
	}
}

// The builtin is the scheduled automation's and nobody else's: the same id
// with any other role, any other automation's principal, a cluster owner by
// hand and an ordinary user are all refused before the database is touched.
// The principal is admitted, through the engine, as the automation calls it.
func TestTheBuiltinRefusesAnyoneButItsMaintenancePrincipal(t *testing.T) {
	principal := auth.MaintenanceActor("emailStatisticsRollupSweep")
	if principal == nil {
		t.Fatal("emailStatisticsRollupSweep is not on the maintenance list, so its automation runs as a reader and every run is refused")
	}
	touched := false
	refusing := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, quietLogger())
	for name, ac := range map[string]*auth.AccessContext{
		"an ordinary user":              {UserId: "user-ordinary", Role: auth.RoleWriter},
		"a cluster owner by hand":       {UserId: "user-owner", Role: auth.RoleOwner},
		"another automation's":          auth.MaintenanceActor("siteStatisticsRollupSweep"),
		"the principal's id as reader":  {UserId: principal.UserId, Role: auth.RoleReader},
		"the ordinary automation actor": {UserId: "system:automation:emailStatisticsRollupSweep", Role: auth.RoleReader},
	} {
		nodes, err := refusing.handleRollup(auth.ContextWithAccess(context.Background(), ac), nil, 0)
		if err == nil || nodes != nil {
			t.Errorf("%s: got %d nodes and error %v, want a refusal", name, len(nodes), err)
		}
	}
	if nodes, err := refusing.handleRollup(context.Background(), nil, 0); err == nil || nodes != nil {
		t.Errorf("no caller at all: got %d nodes and error %v, want a refusal", len(nodes), err)
	}
	if touched {
		t.Error("a refused call reached for the database")
	}

	eng, db := statsEngine(t)
	// The check a node makes at startup: a builtin naming a capability its
	// registered plug-in does not offer fails boot.
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("the engine's boot-time executor audit refuses this wiring: %v", err)
	}
	walkFrom(t, db, time.Now())
	res, err := eng.Execute(auth.ContextWithInternalOrigin(principalCtx(t)), "builtin emailStatisticsRollup()")
	if err != nil {
		t.Fatalf("the maintenance principal was refused: %v", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("the builtin answered %d rows, want one report", len(rows))
	}
	raw, _ := json.Marshal(rows[0])
	if rows[0]["concept"] != rollupConcept || !strings.Contains(string(raw), `"facts"`) || !strings.Contains(string(raw), `"rows"`) {
		t.Errorf("the report is %s, want one %s row with rows and facts", raw, rollupConcept)
	}
}

// A second rollup while one runs in this process answers nothing and does
// nothing: a run slower than the schedule must not pile up behind itself.
func TestASecondRollupInTheSameProcessIsSkipped(t *testing.T) {
	touched := false
	i := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, quietLogger())
	i.rollupRunning.Store(true)
	nodes, err := i.handleRollup(principalCtx(t), nil, 0)
	if err != nil || nodes != nil || touched {
		t.Errorf("got %d nodes, error %v, database touched %v; want a quiet skip", len(nodes), err, touched)
	}
}

// The plug-in registers under the name its executor names, with the one
// capability the builtin calls.
func TestThePlugInOffersTheRollupItsBuiltinNames(t *testing.T) {
	var found bool
	for _, p := range memql.RegisteredPlugins() {
		if p.Name != "emailStats" {
			continue
		}
		found = true
		provider, err := p.Factory(memql.PluginContext{Logger: quietLogger()})
		if err != nil {
			t.Fatalf("the factory failed: %v", err)
		}
		if provider.IntegrationName() != "emailStats" {
			t.Errorf("the provider calls itself %q, want emailStats", provider.IntegrationName())
		}
		var names []string
		for _, c := range provider.Capabilities() {
			names = append(names, c.Name)
		}
		if strings.Join(names, ",") != "rollup" {
			t.Errorf("the capabilities are %v, want rollup", names)
		}
	}
	if !found {
		t.Fatal("no plug-in registered as emailStats, so integration.emailStats.rollup names nothing")
	}
}
