package emailstats

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
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
	"github.com/znasllc-io/memql/core/statsperiod"
)

// rollup_db_test.go -- the email facts rollup against a real database.
//
// The rows are written by the campaigns mutations the sending engine writes
// with, under internal origin and an owner actor, and read back through the
// engine the way the scheduled run reads them: through the seven @serverOnly
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
	actor := auth.MaintenanceActor(auth.AutomationOriginEngine, "emailStatisticsRollupSweep")
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
	f.removeAccountWhenDone()
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

// removeAccountWhenDone removes the fixture's organization, every version of
// it, when the test ends, as joinOrganization removes its own: an organization
// left behind on every run accumulates in every staff list of organizations
// on a database the lane shares. Measured: after a day of runs, 607 were left,
// and the account list -- capped at the engine's five hundred rows -- no
// longer reached the one component/memql's
// TestAccountCreationDefaultsPersistAcrossReplicas and
// TestAPageFullOfHigherRankedRowsDoesNotReadAsExhaustion look for. The
// fixture's other rows stay, as every other test's do.
func (f *fixture) removeAccountWhenDone() {
	f.t.Helper()
	const concept = "v1:accounts:account"
	ids := bun.In([]string{f.account, concept + ":" + f.account})
	count := func() int {
		var n int
		if err := f.db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE concept = ? AND id IN (?)`, concept, ids).Scan(context.Background(), &n); err != nil {
			f.t.Fatalf("count the fixture organization's rows: %v", err)
		}
		return n
	}
	// Under the ids the removal names, or it would remove nothing and say so
	// only by leaving it.
	if count() == 0 {
		f.t.Fatalf("the fixture organization %s was written under an id its removal does not name", f.account)
	}
	f.t.Cleanup(func() {
		if _, err := f.db.NewRaw(`DELETE FROM "MemoryNodes" WHERE concept = ? AND id IN (?)`, concept, ids).Exec(context.Background()); err != nil {
			f.t.Errorf("remove the fixture organization %s: %v", f.account, err)
			return
		}
		if left := count(); left != 0 {
			f.t.Errorf("the fixture organization %s still has %d rows after its removal", f.account, left)
		}
	})
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
	return f.engageIn(f.campaign, delivery, kind, at)
}

// engageIn records one open or click of a delivery of the named campaign, and
// answers the ids of every such event of that delivery.
func (f *fixture) engageIn(campaign, delivery, kind string, at time.Time) []string {
	f.t.Helper()
	f.mutate("recordEngagementEvent", map[string]any{"campaignId": campaign, "deliveryId": delivery, "kind": kind, "occurredAt": stamp(at), "accountId": f.account})
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
	return f.consentFrom(mutation, recipient, "", at)
}

// consentFrom appends one consent event a send of the named campaign gave
// rise to -- a bounce, a complaint, a one-click withdraw -- and answers its
// id. An empty campaign names none.
func (f *fixture) consentFrom(mutation, recipient, campaign string, at time.Time) string {
	f.made++
	id := fmt.Sprintf("%s-ce-%d", f.suffix, f.made)
	args := map[string]any{"eventId": id, "emailDigest": sha256Hex(id + "@example.test"), "source": "operator", "occurredAt": stamp(at), "accountId": f.account}
	if recipient != "" {
		args["recipientId"] = recipient
		args["emailDigest"] = sha256Hex(recipient + "@example.test")
	}
	if campaign != "" {
		args["campaignId"] = campaign
	}
	if mutation == "recordConsentSuppress" {
		args["reason"] = "operator request"
	}
	f.mutate(mutation, args)
	return id
}

// chooseTestingAudience makes an audience the organization's testing
// audience, as Email Settings does (component/campaigns, test_audience.go).
func (f *fixture) chooseTestingAudience(audience string) {
	f.mutate("saveCampaignTestSettings", map[string]any{
		"settingsId": sha256Hex(f.suffix + "-test-settings"), "accountId": f.account, "audienceId": audience,
		"versionTime": stamp(time.Now()),
	})
}

// testRun makes a test run of the fixture's campaign as a test send writes one
// (component/campaigns, handleTestAudienceSend): the testing audience chosen,
// then a campaign of its own, marked by testSourceCampaignId and addressed to
// that audience. It answers the run's bare id, which, like every test run's,
// is a SHA-256 in hex.
func (f *fixture) testRun(testing string) string {
	f.chooseTestingAudience(testing)
	f.made++
	run := sha256Hex(fmt.Sprintf("%s-test-run-%d", f.suffix, f.made))
	f.mutate("createCampaignTestRun", map[string]any{
		"campaignId": run, "testSourceCampaignId": f.campaign, "name": "Statistics campaign · Test",
		"accountId": f.account, "audienceId": testing, "templateId": f.template,
	})
	return run
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

// walkFrom sets every source's watermark to at, and records each walked
// through it, so the next run walks from five minutes before it -- the rows
// this test is about, and at most a few minutes of the tests before it. It
// clears where a stopped re-read got to and how far a bounded walk reads,
// which another test's walk may have left (rollup_recovery_db_test.go).
func walkFrom(t *testing.T, db *bun.DB, at time.Time) {
	t.Helper()
	for _, s := range sources {
		if _, err := db.NewRaw(`INSERT INTO email_stat_watermark (source, through, walked_through) VALUES (?, ?, ?)
			ON CONFLICT (source) DO UPDATE SET through = EXCLUDED.through, walked_through = EXCLUDED.walked_through,
				reread_from = NULL, read_until = NULL`,
			s.name, at.UTC(), at.UTC()).Exec(context.Background()); err != nil {
			t.Fatalf("set the %s watermark: %v", s.name, err)
		}
	}
}

// watermarkOf answers where a source's next walk resumes, and false when it
// has nowhere to resume from: never walked, or walked with no row yet.
func watermarkOf(t *testing.T, db *bun.DB, source string) (time.Time, bool) {
	t.Helper()
	var through sql.NullTime
	err := db.NewRaw(`SELECT through FROM email_stat_watermark WHERE source = ?`, source).Scan(context.Background(), &through)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read the %s watermark: %v", source, err)
	}
	return through.Time, through.Valid
}

// walkedOf answers how far a source has been walked for the statistics read
// (email_stat_watermark.walked_through), and false when it has not been.
func walkedOf(t *testing.T, db *bun.DB, source string) (time.Time, bool) {
	t.Helper()
	var at sql.NullTime
	err := db.NewRaw(`SELECT walked_through FROM email_stat_watermark WHERE source = ?`, source).Scan(context.Background(), &at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read how far the %s walk has gone: %v", source, err)
	}
	return at.Time, at.Valid
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
// until the clock caught up. And a watermark further ahead of the clock than
// the overlap says nothing true about how far the walk read, so the walk
// starts over.
//
// AN HOUR AHEAD, because the database is shared: every walk reads a row dated
// ahead of its clock, so a row dated a year ahead would be read by every later
// test's runs for a year. An hour later this one is behind every clock.
func TestARowDatedAheadOfTheClockDoesNotHoldTheWalk(t *testing.T) {
	f := newFixture(t)
	began := time.Now().UTC().Truncate(time.Millisecond)
	t.Cleanup(func() { walkFrom(t, f.db, time.Now()) })
	walkFrom(t, f.db, time.Now())

	aheadAt := time.Now().UTC().Add(time.Hour)
	ahead := f.deliver(f.recipient(f.audience), map[string]any{
		"campaignId": f.campaign, "status": "sent", "sentAt": stamp(aheadAt), "versionTime": stamp(aheadAt),
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

	// A stored watermark two hours ahead of the run's clock is dropped, and the
	// source walked again as a first walk -- from this test's own start here
	// (firstWalkFrom), so it reads this test's rows and not the history the
	// shared database holds, and moves no delete: retention, which the delete
	// is counted from, is not the seam.
	saved := firstWalkFrom
	firstWalkFrom = func(time.Time) time.Time { return began }
	t.Cleanup(func() { firstWalkFrom = saved })
	if _, err := f.db.NewRaw(`DELETE FROM email_stat_walk_start WHERE source = ?`, sourceDelivery).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := f.deliver(f.recipient(f.audience), map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(time.Now())})
	walkFrom(t, f.db, time.Now().Add(2*time.Hour))
	rollup(t, f.eng, f.db, time.Now())
	wantFact(t, storedFacts(t, f.db, restarted), kindSent, restarted)
	if through, ok := watermarkOf(t, f.db, sourceDelivery); !ok || through.After(time.Now()) {
		t.Errorf("after starting over the watermark is %v (%v), want one at or before the clock", through, ok)
	}
}

// A run long enough to overlap the next one -- a backfill, the lead moving to
// another node -- can find a watermark the other run moved past this run's own
// start. That watermark is the other run's, true by the time this one reads
// it, and is kept: dropping it would send this run back over the whole
// history, and leave the watermark behind where the other run got.
//
// TIME IS COMPRESSED. This run spends a fifth of a second on its deliveries,
// standing in for minutes of backfill, and meanwhile the other run leaves the
// engagement watermark a tenth of a second past this run's start plus the
// overlap. Judged by the run's start, that watermark is further ahead than the
// overlap absorbs; judged by the clock as this run reaches the engagement
// source, it is not. So the test fails if the drop is judged by the start, and
// fails if it is judged by the clock without the overlap.
func TestAWatermarkAnotherRunMovedPastThisRunsStartIsKept(t *testing.T) {
	_, db := statsEngine(t)
	t.Cleanup(func() { walkFrom(t, db, time.Now()) })
	runStart := time.Now().UTC().Truncate(time.Millisecond)
	walkFrom(t, db, runStart.Add(-time.Hour))

	const reading, past = 200 * time.Millisecond, 100 * time.Millisecond
	theirs := runStart.Add(watermarkOverlap + past)
	moved := false
	engine := &scriptedEngine{
		pages: map[string]map[string]scriptedPage{},
		before: func(construct, _ string) {
			if construct != "deliveriesWrittenSince" || moved {
				return
			}
			moved = true
			if _, err := db.NewRaw(`UPDATE email_stat_watermark SET through = ? WHERE source = ?`, theirs, sourceEngagement).Exec(context.Background()); err != nil {
				t.Error(err)
			}
			time.Sleep(reading)
		},
	}
	if _, err := runRollup(principalCtx(t), engine, db, runStart); err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("the delivery walk never ran, so nothing moved the engagement watermark")
	}
	if through, ok := watermarkOf(t, db, sourceEngagement); !ok || through.Before(theirs) {
		t.Errorf("the other run's engagement watermark %v was dropped or moved back: it is %v (%v)", theirs, through, ok)
	}
	asked := false
	for _, c := range engine.calls {
		if c.construct != "engagementEventsWrittenSince" {
			continue
		}
		asked = true
		if since, want := sinceOf(t, c.query), theirs.Add(-watermarkOverlap); !since.Equal(want) {
			t.Errorf("the engagement walk started at %v, want the other run's watermark less the overlap, %v", since, want)
		}
	}
	if !asked {
		t.Error("the run never read the engagement source")
	}
}

// THE LEADER'S CLOCK NEVER DECIDES WHAT THE NEXT WALK READS. Rows are stamped
// by their writers' clocks; the cron leader's clock may run fast. Here it runs
// six minutes fast. Had the walk resumed from that clock less the overlap, the
// next run would start a minute after the true time of this one, and a row
// that a writer with a true clock stamped half a minute after it -- written
// after it -- would never be read while its day read as measured. The walk
// resumes from the newest row it read, so the next run, on a leader whose
// clock is right, reads it.
//
// sinceEngine answers as the engine does, with the rows written since the
// walk's since and no others: the shared database holds rows another test
// dates an hour ahead, which a real walk reads and caps at the leader's clock,
// and which would decide this test.
func TestALeaderClockRunningFastStillReadsTheRowsWrittenAfterIt(t *testing.T) {
	_, db := statsEngine(t)
	suffix := fmt.Sprintf("fastleader-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		walkFrom(t, db, time.Now())
	})
	start := time.Now().UTC().Truncate(time.Millisecond)
	walkFrom(t, db, start.Add(-time.Minute))
	row := func(name string, at time.Time) map[string]any {
		return map[string]any{"id": "v1:campaigns:delivery:" + suffix + "-" + name, "accountId": "org-x", "status": "sent", "sentAt": stamp(at), "createdAt": at}
	}
	engine := &sinceEngine{t: t, rows: []map[string]any{row("before", start.Add(-30*time.Second))}}

	if _, err := runRollup(principalCtx(t), engine, db, start.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if through, _ := watermarkOf(t, db, sourceDelivery); through.After(start) {
		t.Errorf("after a run whose clock ran six minutes fast the walk resumes from %v, past every row it read", through)
	}

	engine.rows = append(engine.rows, row("after", start.Add(30*time.Second)))
	if _, err := runRollup(principalCtx(t), engine, db, start.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	facts := storedFacts(t, db, suffix+"-before", suffix+"-after")
	wantFact(t, facts, kindSent, suffix+"-before")
	wantFact(t, facts, kindSent, suffix+"-after")
}

// sinceEngine answers the delivery walk with every row written at or after its
// since, oldest first, in one page, and every other read with nothing.
type sinceEngine struct {
	t    *testing.T
	rows []map[string]any
}

func (e *sinceEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	if m := queryName.FindStringSubmatch(query); m == nil || m[1] != "deliveriesWrittenSince" {
		return memql.NewResultWithOutput([]any{}), nil
	}
	since := sinceOf(e.t, query)
	out := []any{}
	for _, r := range e.rows {
		if at, _ := r["createdAt"].(time.Time); !at.Before(since) {
			out = append(out, r)
		}
	}
	return memql.NewResultWithOutput(out), nil
}

var sinceArg = regexp.MustCompile(`since: "([^"]+)"`)

// sinceOf reads the instant a walk query starts at.
func sinceOf(t *testing.T, query string) time.Time {
	t.Helper()
	m := sinceArg.FindStringSubmatch(query)
	if m == nil {
		t.Fatalf("the walk query %q names no since", query)
	}
	since, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		t.Fatal(err)
	}
	return since
}

// A request id is a string the sender chose, and the enrollment lookup runs as
// the cluster: a send whose request id spells another organization's
// enrollment is not counted under that organization's journey.
func TestAJourneySendIsAttributedOnlyInItsOwnOrganization(t *testing.T) {
	mine, theirs := newFixture(t), newFixture(t)
	walkFrom(t, mine.db, time.Now())

	member := theirs.recipient(theirs.audience)
	_, enrollment := theirs.journeyWithEnrollment(member)
	stray := mine.deliver(mine.recipient(mine.audience), map[string]any{
		"requestId": "journey-" + enrollment + "-welcome", "status": "sent", "sentAt": stamp(time.Now()),
	})
	rollup(t, mine.eng, mine.db, time.Now())

	if s := wantFact(t, storedFacts(t, mine.db, stray), kindSent, stray); s.JourneyID != "" || s.AccountID != mine.account {
		t.Errorf("the send is %+v, want it in %s and under no journey: its request id names another organization's enrollment", s, mine.account)
	}
}

// The first run has no watermark and starts at the horizon, so on a cluster
// with history it is a backfill: forty pages a source per run, and the next
// run carries on from where it stopped. Pages the next run only reads again
// inside the overlap do not count against its forty, or a burst of more than
// forty pages inside five minutes would hold the walk there for good.
//
// ITS RANGE STARTS AT ITS OWN FIRST ROW, so no row another test left can
// change what it finds. The rows are dated three hours ahead and the first
// walk is put on the first of them (firstWalkFrom), with the run's clock an
// hour past them so none is capped at it. Behind them are the rows this
// package's other tests date by the clock or an hour ahead and the rows
// another package writes beside it in CI. A row another suite dates days
// ahead sorts after this test's rows, which the second run's check allows
// for. Retention, which the run's delete is counted from, stays at its four
// hundred days, so the run deletes nothing another test wrote.
//
// AND MORE THAN THE OVERLAP PAST ITS OWN EARLIER RUNS. Each run of this test
// writes its eighty-five rows three hours ahead of its own clock, so a run
// within five minutes of an earlier one -- a suite run again at once -- would
// find the earlier rows inside its second run's overlap. At two rows a page
// the overlap reads at most maxPagesReadPerRun pages, eight hundred rows,
// which ten such runs exceed; the walk then stops with an error, rightly, and
// the test failed for rows that are not its own. A run removes its rows when
// it ends, and in case one did not -- a process killed mid-test -- the rows
// start the overlap and a second past the newest an earlier run left.
func TestTheFirstRunWalksAtMostFortyPagesPerSource(t *testing.T) {
	f := newFixture(t)
	pageSize, saved := walkPageSize, firstWalkFrom
	walkPageSize = 2
	t.Cleanup(func() { walkPageSize, firstWalkFrom = pageSize, saved })

	recipient := f.recipient(f.audience)
	const extra = 5
	n := maxPagesPerRun*walkPageSize + extra
	first := time.Now().UTC().Truncate(time.Millisecond).Add(3 * time.Hour)
	if past := newestPageBoundRow(t, f.db).Add(watermarkOverlap + time.Second); past.After(first) {
		first = past.Truncate(time.Millisecond)
	}
	firstWalkFrom = func(time.Time) time.Time { return first }
	ids := make([]string, 0, n)
	// Its rows and their facts go when it ends. Dated ahead of the clock, the
	// rows would otherwise be read again by every rollup any test runs against
	// this database until the clock passes them (rollup.go, "Never past the
	// clock"): a few runs of the suite in an afternoon left five thousand.
	t.Cleanup(func() {
		ctx := context.Background()
		if len(ids) > 0 {
			_, _ = f.db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id IN (?)`, bun.In(ids)).Exec(ctx)
		}
		_, _ = f.db.NewRaw(`DELETE FROM "MemoryNodes" WHERE concept = ? AND payload->>'requestId' LIKE ?`,
			"v1:campaigns:delivery", f.suffix+"-page-%").Exec(ctx)
	})
	for i := 0; i < n; i++ {
		at := first.Add(time.Duration(i) * time.Millisecond)
		ids = append(ids, f.deliver(recipient, map[string]any{
			"emailRuleId": f.rule, "requestId": fmt.Sprintf("%s-page-%03d", f.suffix, i),
			"status": "sent", "sentAt": stamp(at), "versionTime": stamp(at),
		}))
	}

	// Every source walks for the first time, from the first row: no watermark,
	// and no start an earlier first walk moved.
	for _, q := range []string{`DELETE FROM email_stat_watermark`, `DELETE FROM email_stat_walk_start`} {
		if _, err := f.db.NewRaw(q).Exec(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { walkFrom(t, f.db, time.Now()) })
	now := first.Add(time.Hour)

	firstRun := rollup(t, f.eng, f.db, now)
	if got := firstRun.Rows[sourceDelivery]; got != maxPagesPerRun*walkPageSize {
		t.Errorf("the first run read %d deliveries, want %d pages of %d", got, maxPagesPerRun, walkPageSize)
	}
	if !behind(firstRun, sourceDelivery) {
		t.Errorf("the first run stopped at its page bound without saying the deliveries are behind: %+v", firstRun)
	}
	if got := len(storedFacts(t, f.db, ids...)); got != maxPagesPerRun*walkPageSize {
		t.Errorf("the first run recorded %d sends, want %d", got, maxPagesPerRun*walkPageSize)
	}

	// The second run reads the first run's eighty rows again, inside the
	// overlap, and carries on to the last five. Those eighty do not count
	// against its forty pages, so it can stop behind only after forty pages
	// past them: rows another suite dated days ahead sort after this test's
	// own, and are not this test's to count.
	second := rollup(t, f.eng, f.db, now)
	if got := len(storedFacts(t, f.db, ids...)); got != n {
		t.Errorf("after the second run %d of the %d sends are recorded, want every one", got, n)
	}
	if read := second.Rows[sourceDelivery]; behind(second, sourceDelivery) && read < 2*maxPagesPerRun*walkPageSize {
		t.Errorf("the second run stopped at its page bound having read %d rows, eighty of them again: the pages it only read again were counted: %+v", read, second)
	}
}

// newestPageBoundRow is the newest delivery an earlier run of
// TestTheFirstRunWalksAtMostFortyPagesPerSource wrote, by the request id it
// names its rows with, and the zero time when there is none.
func newestPageBoundRow(t *testing.T, db *bun.DB) time.Time {
	t.Helper()
	var newest sql.NullTime
	err := db.NewRaw(`SELECT max("createdAt") FROM "MemoryNodes" WHERE concept = ? AND payload->>'requestId' LIKE ?`,
		"v1:campaigns:delivery", "estat-%-page-%").Scan(context.Background(), &newest)
	if err != nil {
		t.Fatalf("find the rows an earlier run of this test wrote: %v", err)
	}
	return newest.Time.UTC()
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

// TEST RUNS ARE NOT STATISTICS. A test run is a campaign of its own, marked by
// testSourceCampaignId from the write that creates it and sent to the
// organization's testing audience, with its own deliveries, opens and clicks.
// Counted, its sends would swell Sent and it would take a place in Top
// campaigns, where the campaigns feed, which leaves test runs out, has no
// campaign to show for it. So nothing of it makes a fact -- its send and the
// provider's verdict, its opens and clicks, a consent event it gave rise to --
// while the campaign it tests, in the same organization, counts as before.
func TestATestRunsSendsOpensAndClicksMakeNoFact(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	testing := f.newAudience()
	run := f.testRun(testing)
	tester := f.recipient(testing)
	testSend := f.deliver(tester, map[string]any{"campaignId": run, "status": "sent", "sentAt": stamp(time.Now())})
	f.providerReport(testSend, "delivered", time.Now())
	testEvents := append(f.engageIn(run, testSend, "open", time.Now()), f.engageIn(run, testSend, "click", time.Now())...)
	// A one-click withdraw from a forwarded test message names the run and no
	// recipient, so only its campaign says it is a test's.
	testWithdraw := f.consentFrom("recordConsentWithdraw", "", run, time.Now())

	member := f.recipient(f.audience)
	liveSend := f.deliver(member, map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(time.Now())})
	f.providerReport(liveSend, "delivered", time.Now())
	liveEvents := append(f.engage(liveSend, "open", time.Now()), f.engage(liveSend, "click", time.Now())...)
	liveWithdraw := f.consentFrom("recordConsentWithdraw", "", f.campaign, time.Now())
	now := time.Now()
	rollup(t, f.eng, f.db, now)

	ids := append(append([]string{testSend, liveSend, testWithdraw, liveWithdraw}, testEvents...), liveEvents...)
	facts := storedFacts(t, f.db, ids...)
	for _, fact := range facts {
		if fact.CampaignID == run {
			t.Errorf("the test run made a fact: %+v", fact)
		}
	}
	for _, kind := range []string{kindSent, kindDelivered, kindOpenUnique, kindClickUnique} {
		wantNoFact(t, facts, kind, testSend)
		if fact := wantFact(t, facts, kind, liveSend); fact.CampaignID != f.campaign {
			t.Errorf("the campaign's %s counts under %q, want %s", kind, fact.CampaignID, f.campaign)
		}
	}
	if len(testEvents) != 2 || len(liveEvents) != 2 {
		t.Fatalf("the opens and clicks are %v and %v, want one of each per send", testEvents, liveEvents)
	}
	wantNoFact(t, facts, kindOpen, testEvents[0])
	wantNoFact(t, facts, kindClick, testEvents[1])
	wantFact(t, facts, kindOpen, liveEvents[0])
	wantFact(t, facts, kindClick, liveEvents[1])
	wantNoFact(t, facts, kindUnsubscribe, testWithdraw)
	wantFact(t, facts, kindUnsubscribe, liveWithdraw)

	// What the Overview shows: one send, and the campaign alone in Top
	// campaigns.
	got, err := readStatistics(context.Background(), f.db, staff(), []string{f.account}, statsperiod.Last7Days, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.Sent == nil || *got.Totals.Sent != 1 {
		t.Errorf("the organization's sends are %s, want the campaign's one", num(got.Totals.Sent))
	}
	if len(got.Campaigns) != 1 || got.Campaigns[0].ID != f.campaign {
		t.Errorf("Top campaigns is %s, want the campaign alone", figuresLine(got.Campaigns))
	}
}

// THE TESTING AUDIENCE IS NOT STATISTICS. The people in an organization's
// testing audience are its own staff, there to receive tests, not
// subscribers: their joining, leaving, bounces and complaints make no fact,
// while the same events in the organization's other audiences count as
// before. The audience is judged when the walk reads the event, so a person
// who joined it before it was chosen, whose joining the walk had not yet read,
// is left out too.
func TestTheTestingAudiencesConsentEventsMakeNoFact(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	testing := f.newAudience()
	early := f.recipient(testing)
	joinedEarly := f.consent("recordConsentGrant", early, time.Now())
	f.chooseTestingAudience(testing)
	tester := f.recipient(testing)
	joined := f.consent("recordConsentGrant", tester, time.Now())
	left := f.consent("recordConsentWithdraw", tester, time.Now())
	bounced := f.consent("recordConsentBounce", tester, time.Now())
	complained := f.consent("recordConsentComplaint", tester, time.Now())

	member := f.recipient(f.audience)
	subscribed := f.consent("recordConsentGrant", member, time.Now())
	memberBounced := f.consent("recordConsentBounce", member, time.Now())
	now := time.Now()
	rollup(t, f.eng, f.db, now)

	facts := storedFacts(t, f.db, joinedEarly, joined, left, bounced, complained, subscribed, memberBounced)
	wantNoFact(t, facts, kindSubscribe, joinedEarly)
	wantNoFact(t, facts, kindSubscribe, joined)
	wantNoFact(t, facts, kindUnsubscribe, left)
	wantNoFact(t, facts, kindBounce, bounced)
	wantNoFact(t, facts, kindComplaint, complained)
	if s := wantFact(t, facts, kindSubscribe, subscribed); s.AudienceID != f.audience {
		t.Errorf("the member's subscribe counts under audience %q, want %s", s.AudienceID, f.audience)
	}
	wantFact(t, facts, kindBounce, memberBounced)

	got, err := readStatistics(context.Background(), f.db, staff(), []string{f.account}, statsperiod.Last7Days, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.Subscribes == nil || *got.Totals.Subscribes != 1 {
		t.Errorf("the organization's new subscribers are %s, want the member alone", num(got.Totals.Subscribes))
	}
	if len(got.Audiences) != 1 || got.Audiences[0].ID != f.audience {
		t.Errorf("the audiences are %s, want the organization's own audience alone", audiencesLine(got.Audiences))
	}
}

// NO WINDOW FOR A NEW TEST RUN. A test run's campaign row is written before its
// send is started, so every delivery, open, click or consent event naming it
// is written after the row. The walk asks which of a page's campaigns are test
// runs AFTER reading the page, so the answer always knows the run: here the
// run comes into being only once the delivery page has been read, after the
// rollup began, and its send still makes no fact. Asked once at the start of
// the run, the answer would not know it.
func TestATestRunCreatedDuringARunIsStillLeftOut(t *testing.T) {
	_, db := statsEngine(t)
	suffix := fmt.Sprintf("newtest-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		walkFrom(t, db, time.Now())
	})
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	walkFrom(t, db, at.Add(-time.Minute))
	delivery := func(name, campaign string) any {
		return map[string]any{"id": "v1:campaigns:delivery:" + suffix + "-" + name, "accountId": "v1:accounts:account:" + suffix + "-org",
			"campaignId": "v1:campaigns:campaign:" + campaign, "status": "sent", "sentAt": stamp(at), "createdAt": at}
	}
	run := sha256Hex(suffix + "-run")
	engine := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince": {"": {rows: []any{delivery("test", run), delivery("live", suffix+"-camp")}}},
	}}
	engine.before = func(construct, _ string) {
		if construct == "deliveriesWrittenSince" {
			engine.pages["campaignTestRunsByIds"] = map[string]scriptedPage{"": {rows: []any{map[string]any{"id": "v1:campaigns:campaign:" + run}}}}
		}
	}
	if _, err := runRollup(principalCtx(t), engine, db, time.Now()); err != nil {
		t.Fatal(err)
	}
	facts := storedFacts(t, db, suffix+"-test", suffix+"-live")
	wantNoFact(t, facts, kindSent, suffix+"-test")
	wantFact(t, facts, kindSent, suffix+"-live")
}

// scriptedEngine answers each walk query from pages keyed by the cursor it was
// asked with, so a test can make a page fail or a cursor repeat.
type scriptedEngine struct {
	pages map[string]map[string]scriptedPage
	// before, when set, runs at the start of every call: what another run
	// does while this one is between two reads.
	before func(construct, cursor string)
	// calls records, per call, the construct asked and how it was asked.
	calls []scriptedCall
}

type scriptedPage struct {
	rows []any
	next string
	err  error
}

type scriptedCall struct {
	construct, query           string
	internal, fresh, rootsOnly bool
}

var queryName = regexp.MustCompile(`query ([A-Za-z0-9_]+)\(`)

func (e *scriptedEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	m := queryName.FindStringSubmatch(query)
	if m == nil {
		return nil, fmt.Errorf("unexpected statement %q", query)
	}
	if e.before != nil {
		e.before(m[1], memql.CursorFromContext(ctx))
	}
	e.calls = append(e.calls, scriptedCall{
		construct: m[1], query: query, internal: auth.OriginFromContext(ctx).IsInternal(),
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
	// The consent walk read its last page: it resumes from its one row, and is
	// walked through the clock it asked that page by
	// (TestAWalkThatReadsItsLastPageRecordsTheClockItAskedBy).
	if through, _ := watermarkOf(t, db, sourceConsent); !through.Equal(t3) {
		t.Errorf("the consent watermark is %v, want its row's %v: one source's failure must not stop another", through, t3)
	}
	if walked, _ := walkedOf(t, db, sourceConsent); walked.Before(t3) || walked.After(time.Now()) {
		t.Errorf("the consent walk is through %v, want it past its row's %v and not past the clock", walked, t3)
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
	// the watermark where it was: a write never moves it back. The run's clock
	// is a minute behind the watermark, as that slower replica's is, so the
	// clock its last page records is behind it too.
	ahead := t3.Add(time.Hour)
	walkFrom(t, db, ahead)
	behindOnly := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince": {"": {rows: []any{row(5, t1)}}},
	}}
	if _, err := runRollup(principalCtx(t), behindOnly, db, ahead.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if through, _ := watermarkOf(t, db, sourceDelivery); !through.Equal(ahead) {
		t.Errorf("a page older than the watermark moved it from %v to %v", ahead, through)
	}
}

// Every read the rollup makes, the three walks and the four lookups alike, is
// made with internal origin (the seven queries are @serverOnly), as a fresh read
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
			"requestId": "journey-" + enrollment + "-welcome", "accountId": "v1:accounts:account:" + suffix + "-org",
			"campaignId": "v1:campaigns:campaign:" + suffix + "-camp",
		}}}},
		"consentEventsWrittenSince": {"": {rows: []any{map[string]any{
			"id": "v1:campaigns:consentEvent:" + suffix + "-c", "kind": "grant", "occurredAt": stamp(at), "createdAt": at,
			"recipientId": "v1:campaigns:recipient:" + suffix + "-r",
		}}}},
		"journeyEnrollmentsByIds": {"": {rows: []any{map[string]any{
			"id": "v1:campaigns:journeyEnrollment:" + enrollment, "journeyId": "v1:campaigns:journey:" + suffix + "-j",
			"accountId": "v1:accounts:account:" + suffix + "-org",
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
	for _, construct := range []string{"deliveriesWrittenSince", "engagementEventsWrittenSince", "consentEventsWrittenSince", "journeyEnrollmentsByIds", "recipientAudiencesByIds", "campaignTestRunsByIds", "testingAudiencesByIds"} {
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
// seven reads -- so a field added to one of their shapes has to be argued here.
// A test run and the testing audience are there so the two lookups that leave
// them out answer rows too.
func TestTheWalkReadsNoAddressAndNothingTheFactsDoNotUse(t *testing.T) {
	f := newFixture(t)
	walkFrom(t, f.db, time.Now())

	member := f.recipient(f.audience)
	_, enrollment := f.journeyWithEnrollment(member)
	f.deliver(member, map[string]any{"requestId": "journey-" + enrollment + "-welcome", "status": "sent", "sentAt": stamp(time.Now())})
	sent := f.deliver(f.recipient(f.audience), map[string]any{"campaignId": f.campaign, "status": "sent", "sentAt": stamp(time.Now())})
	f.engage(sent, "click", time.Now())
	f.consent("recordConsentGrant", member, time.Now())
	testing := f.newAudience()
	run := f.testRun(testing)
	tester := f.recipient(testing)
	f.deliver(tester, map[string]any{"campaignId": run, "status": "sent", "sentAt": stamp(time.Now())})
	f.consent("recordConsentGrant", tester, time.Now())

	recorder := &recordingEngine{inner: f.eng, rows: map[string][]map[string]any{}}
	if _, err := runRollup(principalCtx(t), recorder, f.db, time.Now()); err != nil {
		t.Fatal(err)
	}

	read := map[string][]string{
		"deliveriesWrittenSince":       {"id", "accountId", "campaignId", "emailRuleId", "requestId", "status", "skipReason", "sentAt", "providerStatus", "providerAt", "createdAt"},
		"engagementEventsWrittenSince": {"id", "accountId", "campaignId", "deliveryId", "kind", "occurredAt", "createdAt"},
		"consentEventsWrittenSince":    {"id", "accountId", "campaignId", "recipientId", "kind", "occurredAt", "createdAt"},
		"journeyEnrollmentsByIds":      {"id", "accountId", "journeyId"},
		"recipientAudiencesByIds":      {"id", "audienceId"},
		"campaignTestRunsByIds":        {"id"},
		"testingAudiencesByIds":        {"id", "audienceId"},
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
	principal := auth.MaintenanceActor(auth.AutomationOriginEngine, "emailStatisticsRollupSweep")
	if principal == nil {
		t.Fatal("emailStatisticsRollupSweep is not on the maintenance list, so its automation runs as a reader and every run is refused")
	}
	touched := false
	refusing := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, quietLogger())
	for name, ac := range map[string]*auth.AccessContext{
		"an ordinary user":              {UserId: "user-ordinary", Role: auth.RoleWriter},
		"a cluster owner by hand":       {UserId: "user-owner", Role: auth.RoleOwner},
		"another automation's":          auth.MaintenanceActor(auth.AutomationOriginEngine, "siteStatisticsRollupSweep"),
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

// The plug-in registers under the name its executors name, with the two
// capabilities its builtins call: the rollup and the statistics read.
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
		if strings.Join(names, ",") != "rollup,statistics" {
			t.Errorf("the capabilities are %v, want rollup and statistics", names)
		}
	}
	if !found {
		t.Fatal("no plug-in registered as emailStats, so integration.emailStats.rollup names nothing")
	}
}

// THE POOL'S STATEMENT TIMEOUT STANDS for the rollup's own writes too: they
// run on the request pool, whose 8 s server limit sits under pgdriver's 10 s
// client read deadline, and a statement allowed past the second would be cut
// off by the client and run on in the server. No statement the rollup issues
// on this package's tables moves the server's limit.
func TestTheRollupKeepsThePoolsStatementTimeout(t *testing.T) {
	_, shared := statsEngine(t)
	suffix := fmt.Sprintf("timeout-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = shared.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		walkFrom(t, shared, time.Now())
	})
	at := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	walkFrom(t, shared, at.Add(-time.Minute))
	engine := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince": {"": {rows: []any{map[string]any{
			"id": "v1:campaigns:delivery:" + suffix, "accountId": "org-x", "status": "sent", "sentAt": stamp(at), "createdAt": at,
		}}}},
	}}
	db, rec := recordedDB(t)
	if _, err := runRollup(principalCtx(t), engine, db, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantFact(t, storedFacts(t, shared, suffix), kindSent, suffix)
	wantThePoolsTimeout(t, "the rollup", rec, "email_stat_fact")
}

// A WALK THAT READS ITS LAST PAGE IS WALKED THROUGH THE CLOCK IT ASKED BY, rows
// or none: every row written before that instant had been read or was inside
// the overlap the next run reads again. So a source with nothing new, or
// nothing at all, records how far it was walked, and the statistics read can
// tell "walked, nothing there" from "never walked" (statistics.go). Where the
// next walk resumes is still the newest row it read, never the clock: a
// source with nothing new keeps its old watermark, and one that never had a
// row has none. A walk that stops at its page bound with rows still to read is
// walked through the newest row it wrote, not the clock: it has not read what
// comes after.
func TestAWalkThatReadsItsLastPageRecordsTheClockItAskedBy(t *testing.T) {
	_, db := statsEngine(t)
	suffix := fmt.Sprintf("caughtup-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		walkFrom(t, db, time.Now())
	})
	ctx := context.Background()
	// Whole milliseconds: the database keeps microseconds, and the bound
	// below compares with these instants exactly.
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	walkFrom(t, db, base)
	if _, err := db.NewRaw(`DELETE FROM email_stat_watermark WHERE source = ?`, sourceConsent).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	row := func(n int, at time.Time) any {
		return map[string]any{"id": fmt.Sprintf("v1:campaigns:delivery:%s-%d", suffix, n), "accountId": "org-x", "status": "sent", "sentAt": stamp(at), "createdAt": at}
	}

	// Deliveries: one page, the last, with a row an hour old. Engagement: a
	// page with nothing new. Consent: never walked, and nothing there.
	began := time.Now().UTC().Truncate(time.Microsecond)
	caughtUp := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince": {"": {rows: []any{row(0, base.Add(10*time.Minute))}}},
	}}
	if _, err := runRollup(principalCtx(t), caughtUp, db, time.Now()); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().UTC()
	for _, source := range []string{sourceDelivery, sourceEngagement, sourceConsent} {
		walked, ok := walkedOf(t, db, source)
		if !ok {
			t.Errorf("%s is walked through nothing after a walk that read its last page: walked with nothing there reads as never walked", source)
			continue
		}
		if walked.Before(began) || walked.After(ended) {
			t.Errorf("the %s walk is through %v, want the clock it asked its last page by, within [%v, %v]", source, walked, began, ended)
		}
	}
	for source, want := range map[string]time.Time{sourceDelivery: base.Add(10 * time.Minute), sourceEngagement: base} {
		if through, ok := watermarkOf(t, db, source); !ok || !through.Equal(want) {
			t.Errorf("the %s walk resumes from %v (%v), want its newest row's time, %v: the clock never decides it", source, through, ok, want)
		}
	}
	if through, ok := watermarkOf(t, db, sourceConsent); ok {
		t.Errorf("a source walked with no row resumes from %v, want nowhere: it has no row to resume from", through)
	}

	// Forty-one pages of a row each: the run stops at forty, behind, through
	// the fortieth page's row.
	pages := map[string]scriptedPage{}
	cursor := ""
	var fortieth time.Time
	for i := 1; i <= maxPagesPerRun+1; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		next := fmt.Sprintf("page-%d", i+1)
		pages[cursor] = scriptedPage{rows: []any{row(i, at)}, next: next}
		if i == maxPagesPerRun {
			fortieth = at
		}
		cursor = next
	}
	walkFrom(t, db, base)
	rep, err := runRollup(principalCtx(t), &scriptedEngine{pages: map[string]map[string]scriptedPage{"deliveriesWrittenSince": pages}}, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !behind(rep, sourceDelivery) {
		t.Fatalf("the run read %d deliveries without stopping at its page bound: %+v", rep.Rows[sourceDelivery], rep)
	}
	if through, _ := watermarkOf(t, db, sourceDelivery); !through.Equal(fortieth) {
		t.Errorf("a walk stopped behind resumes from %v, want the newest row it wrote, %v", through, fortieth)
	}
	if walked, _ := walkedOf(t, db, sourceDelivery); !walked.Equal(fortieth) {
		t.Errorf("a walk stopped behind is walked through %v, want the newest row it wrote, %v", walked, fortieth)
	}
}

// HOW FAR A SOURCE IS WALKED IS NEVER PAST THE CLOCK EITHER. A leader whose
// clock ran an hour fast records its caught-up walks through its own clock;
// kept, that would let the read count as measured an hour no walk has read,
// and a rollup that stopped inside it would go unseen. A run on a leader
// whose clock is right forgets one further ahead than the overlap and records
// its own. Where the walk resumes is left as it was.
func TestAWalkedThroughFarAheadOfTheClockIsForgotten(t *testing.T) {
	_, db := statsEngine(t)
	t.Cleanup(func() { walkFrom(t, db, time.Now()) })
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	walkFrom(t, db, base)
	ahead := time.Now().UTC().Add(time.Hour)
	if _, err := db.NewRaw(`UPDATE email_stat_watermark SET walked_through = ? WHERE source = ?`, ahead, sourceEngagement).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	began := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := runRollup(principalCtx(t), &scriptedEngine{pages: map[string]map[string]scriptedPage{}}, db, time.Now()); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().UTC()
	if walked, ok := walkedOf(t, db, sourceEngagement); !ok || walked.Before(began) || walked.After(ended) {
		t.Errorf("a walk an hour ahead of the clock is now through %v (%v), want this run's clock, within [%v, %v]", walked, ok, began, ended)
	}
	if through, ok := watermarkOf(t, db, sourceEngagement); !ok || !through.Equal(base) {
		t.Errorf("the engagement walk resumes from %v (%v), want where it was, %v", through, ok, base)
	}
}

// ONE AHEAD BY LESS THAN THE OVERLAP IS KEPT. A walked-through instant a few
// minutes ahead of this run's clock is another run's, written a moment ago by
// a node whose clock is a little ahead of this one, as a watermark that close
// is (TestAWatermarkAnotherRunMovedPastThisRunsStartIsKept): forgetting it
// would have the read show the day under way as unmeasured between runs for
// nothing. Three minutes ahead survives the run; an hour ahead, in the same
// run, is forgotten.
func TestAWalkedThroughWithinTheOverlapIsKept(t *testing.T) {
	_, db := statsEngine(t)
	t.Cleanup(func() { walkFrom(t, db, time.Now()) })
	walkFrom(t, db, time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute))
	near := time.Now().UTC().Truncate(time.Microsecond).Add(watermarkOverlap - 2*time.Minute)
	far := time.Now().UTC().Add(time.Hour)
	for source, at := range map[string]time.Time{sourceConsent: near, sourceEngagement: far} {
		if _, err := db.NewRaw(`UPDATE email_stat_watermark SET walked_through = ? WHERE source = ?`, at, source).Exec(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runRollup(principalCtx(t), &scriptedEngine{pages: map[string]map[string]scriptedPage{}}, db, time.Now()); err != nil {
		t.Fatal(err)
	}
	if walked, ok := walkedOf(t, db, sourceConsent); !ok || !walked.Equal(near) {
		t.Errorf("a walk three minutes ahead of the clock is now through %v (%v), want it kept at %v", walked, ok, near)
	}
	if walked, ok := walkedOf(t, db, sourceEngagement); !ok || !walked.Before(far) {
		t.Errorf("a walk an hour ahead of the clock is still through %v (%v), want it forgotten", walked, ok)
	}
}

// MOVING A FIRST WALK'S START NEVER MOVES THE DELETE. A test puts a first
// walk at its own rows through firstWalkFrom, and retention -- how long a fact
// is kept, which the run's delete is counted from -- is a constant no test
// moves: a fact dated yesterday, which a first walk starting an hour ago is
// far past, survives the run. No test may delete facts it did not write.
func TestMovingAFirstWalksStartMovesNoDelete(t *testing.T) {
	_, db := statsEngine(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("seam-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(ctx)
		walkFrom(t, db, time.Now())
	})
	start := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	saved := firstWalkFrom
	firstWalkFrom = func(time.Time) time.Time { return start }
	t.Cleanup(func() { firstWalkFrom = saved })
	yesterday := dayOf(time.Now()).AddDate(0, 0, -1)
	if _, err := db.NewInsert().Model(&[]factRow{{Kind: kindSent, SourceID: suffix, AccountID: suffix + "-org", Day: dateArg(yesterday)}}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	walkFrom(t, db, time.Now())
	for _, q := range []string{`DELETE FROM email_stat_watermark WHERE source = ?`, `DELETE FROM email_stat_walk_start WHERE source = ?`} {
		if _, err := db.NewRaw(q, sourceDelivery).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	engine := &scriptedEngine{pages: map[string]map[string]scriptedPage{}}
	if _, err := runRollup(principalCtx(t), engine, db, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, c := range engine.calls {
		if c.construct == "deliveriesWrittenSince" {
			if since := sinceOf(t, c.query); !since.Equal(start) {
				t.Errorf("the first walk started at %v, want the start the test set, %v", since, start)
			}
		}
	}
	wantFact(t, storedFacts(t, db, suffix), kindSent, suffix)
}

// A START ONLY MOVES LATER. Two replicas whose first walks both failed, one
// from a start the other has already moved past, leave the later of the two:
// a lead that moves mid-run cannot send the walk back to a longer history.
func TestAWalkStartNeverMovesBack(t *testing.T) {
	_, db := statsEngine(t)
	ctx := context.Background()
	source := fmt.Sprintf("start-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_walk_start WHERE source = ?`, source).Exec(ctx)
	})
	later := dayOf(time.Now()).AddDate(0, 0, -100)
	for _, start := range []time.Time{later, later.AddDate(0, 0, -100)} {
		if err := recordWalkStart(ctx, db, source, start); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := walkStartOf(t, db, source); !ok || !got.Equal(later) {
		t.Errorf("the start is %v (%v), want the later one, %v", got, ok, later)
	}
}

// A PAGE TOO LONG FOR ITS DEADLINE IS KNOWN BY THE DRIVER'S OWN ANSWER, in the
// two forms it takes: the server ending the statement at its statement timeout
// (SQLSTATE 57014, "canceling statement due to statement timeout"), and, with
// that timeout off or at the driver's ten seconds or more, pgdriver's own read
// deadline ending the read (a net.Error whose Timeout() is true, on a read).
// Each is read off the driver's error, wrapped as the walk wraps it, flattened
// as an engine might, or off its text alone. NOT a statement cancelled on
// request -- pg_cancel_backend, also 57014 -- nor a refused or reset
// connection, nor an ended context: none says what the page costs.
func TestAPageTooLongForItsDeadlineIsToldApartFromAnyOtherFailure(t *testing.T) {
	_, db := statsEngine(t)
	timeout := driverStatementTimeout(t, db)
	deadline := driverReadDeadline(t)
	var ne net.Error
	if !errors.As(deadline, &ne) || !ne.Timeout() {
		t.Fatalf("pgdriver's read deadline answered %T (%v), want a net.Error whose Timeout() is true", deadline, deadline)
	}
	for name, err := range map[string]error{
		"the statement timeout":                timeout,
		"the statement timeout, wrapped":       fmt.Errorf("read a page: %w", timeout),
		"the statement timeout, as text":       errors.New(timeout.Error()),
		"the statement timeout, flattened":     fmt.Errorf("execute: %v", timeout),
		"the driver's read deadline":           deadline,
		"the driver's read deadline, wrapped":  fmt.Errorf("read a page: %w", deadline),
		"the driver's read deadline, as text":  errors.New(deadline.Error()),
		"the driver's read deadline, flatten.": fmt.Errorf("execute: %v", deadline),
	} {
		if !isPageTimeout(err) {
			t.Errorf("%s (%v) is not read as a page too long for its deadline", name, err)
		}
	}
	cancelled := driverCancelOnRequest(t, db)
	refused := driverConnectionRefused(t)
	for name, err := range map[string]error{
		"a statement cancelled on request":            cancelled,
		"a statement cancelled on request, wrapped":   fmt.Errorf("read a page: %w", cancelled),
		"a statement cancelled on request, as text":   errors.New(cancelled.Error()),
		"a refused connection":                        refused,
		"a refused connection, as text":               errors.New(refused.Error()),
		"a reset connection":                          errors.New("read tcp 10.0.0.5:5432: connection reset by peer"),
		"a dial that timed out":                       &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded},
		"a dial that timed out, as text":              errors.New("dial tcp 10.0.0.5:5432: i/o timeout"),
		"a write that timed out":                      &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded},
		"the run's context ending":                    fmt.Errorf("read a page: %w", context.Canceled),
		"the run's context reaching its own deadline": fmt.Errorf("read a page: %w", context.DeadlineExceeded),
	} {
		if isPageTimeout(err) {
			t.Errorf("%s (%v) is read as a page too long for its deadline", name, err)
		}
	}
}

// driverStatementTimeout is the error pgdriver answers for a statement the
// server ends at its statement timeout.
func driverStatementTimeout(t *testing.T, db *bun.DB) error {
	t.Helper()
	var timeout error
	_ = db.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '50ms'`); err != nil {
			t.Fatal(err)
		}
		_, timeout = tx.ExecContext(ctx, `SELECT pg_sleep(2)`)
		return timeout
	})
	if timeout == nil {
		t.Fatal("a two-second statement under a fifty-millisecond timeout did not fail")
	}
	return timeout
}

// driverReadDeadline is the error pgdriver answers for a statement that
// outlasts its own client read deadline, on a handle of its own with a short
// one and no server statement timeout: what a first page gets when
// MEMQL_DB_STATEMENT_TIMEOUT_MS is 0, or 10000 or more.
func driverReadDeadline(t *testing.T) error {
	t.Helper()
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithReadTimeout(200*time.Millisecond)))
	defer func() { _ = sqldb.Close() }()
	_, err := sqldb.ExecContext(context.Background(), `SELECT pg_sleep(2)`)
	if err == nil {
		t.Fatal("a two-second statement under a two-hundred-millisecond read deadline did not fail")
	}
	return err
}

// driverCancelOnRequest is the error pgdriver answers for a statement another
// session cancels, as an operator's pg_cancel_backend does: SQLSTATE 57014,
// like the statement timeout, with the run's own context live throughout.
func driverCancelOnRequest(t *testing.T, db *bun.DB) error {
	t.Helper()
	ctx := context.Background()
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.ExecContext(ctx, `SELECT pg_sleep(5)`)
		done <- err
	}()
	for waited := 0; ; waited++ {
		var cancelled bool
		if err := db.NewRaw(`SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE pid = ? AND state = 'active' AND query LIKE 'SELECT pg_sleep(5)%'`, pid).Scan(ctx, &cancelled); err == nil && cancelled {
			break
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		if waited > 200 {
			t.Fatal("the statement to cancel never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	err = <-done
	if err == nil {
		t.Fatal("a statement cancelled on request did not fail")
	}
	return err
}

// driverConnectionRefused is the error pgdriver answers when no server takes
// the connection: what a page gets while the database switches over.
func driverConnectionRefused(t *testing.T) error {
	t.Helper()
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithAddr("127.0.0.1:1"), pgdriver.WithUser("memql"), pgdriver.WithInsecure(true), pgdriver.WithTimeout(2*time.Second)))
	defer func() { _ = sqldb.Close() }()
	_, err := sqldb.ExecContext(context.Background(), `SELECT 1`)
	if err == nil {
		t.Fatal("a connection to a port nothing listens on did not fail")
	}
	return err
}

// A FIRST PAGE THAT OUTLASTS THE DRIVER'S READ DEADLINE STARTS LATER TOO. With
// the server's statement timeout off (MEMQL_DB_STATEMENT_TIMEOUT_MS=0), or at
// the driver's ten seconds or more, a first page too long for one read never
// answers 57014: pgdriver's own read deadline ends it first, as an i/o timeout
// on the read. Read as any other failure, the first walk would fail on every
// run and never move. It is the same signal -- the page costs more than a read
// may take -- so it moves the start the same way, while a refused connection,
// which says nothing about the page, moves nothing. The errors are the real
// driver's (driverReadDeadline, driverConnectionRefused), handed to the walk as
// the engine hands on a read's error.
func TestAFirstPageThatOutlastsTheDriversReadDeadlineStartsLater(t *testing.T) {
	_, db := statsEngine(t)
	ctx := context.Background()
	firstWalks := []string{sourceDelivery, sourceConsent}
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_walk_start WHERE source IN (?)`, bun.In(firstWalks)).Exec(ctx)
		walkFrom(t, db, time.Now())
	})
	deadline, refused := driverReadDeadline(t), driverConnectionRefused(t)
	now := dayOf(time.Now()).Add(12 * time.Hour)
	walkFrom(t, db, now)
	for _, s := range firstWalks {
		for _, q := range []string{`DELETE FROM email_stat_watermark WHERE source = ?`, `DELETE FROM email_stat_walk_start WHERE source = ?`} {
			if _, err := db.NewRaw(q, s).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	failing := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince":    {"": {err: deadline}},
		"consentEventsWrittenSince": {"": {err: refused}},
	}}
	if _, err := runRollup(principalCtx(t), failing, db, now); err == nil {
		t.Fatal("the run answered no error for two sources whose first page failed")
	}
	horizon := now.Add(-retention)
	if got, ok := walkStartOf(t, db, sourceDelivery); !ok || !got.Equal(dayOf(horizon.Add(now.Sub(horizon)/2))) {
		t.Errorf("after the driver's read deadline the delivery first walk starts at %v (%v), want half way, %v", got, ok, dayOf(horizon.Add(now.Sub(horizon)/2)))
	}
	if got, ok := walkStartOf(t, db, sourceConsent); ok {
		t.Errorf("a refused connection moved the consent first walk to %v", got)
	}
}

// walkStartOf is where a source's first walk starts (email_stat_walk_start),
// and false when nothing has moved it from four hundred days back.
func walkStartOf(t *testing.T, db *bun.DB, source string) (time.Time, bool) {
	t.Helper()
	var at time.Time
	err := db.NewRaw(`SELECT starts_at FROM email_stat_walk_start WHERE source = ?`, source).Scan(context.Background(), &at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read the %s walk start: %v", source, err)
	}
	return at, true
}

// A FIRST WALK THAT CANNOT READ ITS FIRST PAGE STARTS LATER. On a cluster with
// a long history the first page of a first walk reads every version written
// in four hundred days, and can outlast the request pool's statement timeout;
// a failed page writes no watermark, so every run would start over and fail
// the same way, and the source's statistics would never begin. Instead each
// timeout moves the next first walk's start half way to the clock, to the UTC
// day, durably, with a Warn naming the source and the start: four hundred
// days back, then two hundred, then one hundred. The next run that reads the
// page walks from there, and the read counts nothing before it for that
// source's figures, while a source walked from further back is measured there.
//
// ONLY THE TIMEOUT MOVES IT. Any other failure of that page says nothing about
// how long the history is: a connection reset while the database switches
// over, or a statement an operator cancels with pg_cancel_backend, which
// answers 57014 like the timeout but says "user request". Each leaves the
// start where it was, is logged as the run's failure, and the next run tries
// the same walk again. Moved on every failure, nine failing runs would bring a
// source down to its last day.
func TestAFirstWalkThatCannotReadItsFirstPageStartsLater(t *testing.T) {
	_, db := statsEngine(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("cliff-%d", time.Now().UnixNano())
	firstWalks := []string{sourceDelivery, sourceEngagement, sourceConsent}
	cancelledOnRequest := driverCancelOnRequest(t, db)
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(ctx)
		_, _ = db.NewRaw(`DELETE FROM email_stat_walk_start WHERE source IN (?)`, bun.In(firstWalks)).Exec(ctx)
		walkFrom(t, db, time.Now())
	})
	// Noon, so each half way lands mid-day: the day it moves the start to is
	// the same however long a run takes.
	now := dayOf(time.Now()).Add(12 * time.Hour)
	walkFrom(t, db, now)
	for _, s := range firstWalks {
		for _, q := range []string{`DELETE FROM email_stat_watermark WHERE source = ?`, `DELETE FROM email_stat_walk_start WHERE source = ?`} {
			if _, err := db.NewRaw(q, s).Exec(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	var logs bytes.Buffer
	runAt := func(engine Engine) error {
		t.Helper()
		i := NewIntegration(engine, func() *bun.DB { return db }, slog.New(slog.NewTextHandler(&logs, nil)))
		i.now = func() time.Time { return now }
		_, err := i.handleRollup(principalCtx(t), nil, 0)
		return err
	}

	failing := &scriptedEngine{pages: map[string]map[string]scriptedPage{
		"deliveriesWrittenSince":       {"": {err: errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE=57014)")}},
		"engagementEventsWrittenSince": {"": {err: cancelledOnRequest}},
		"consentEventsWrittenSince":    {"": {err: errors.New("read tcp 10.0.0.5:5432: connection reset by peer")}},
	}}
	horizon := now.Add(-retention)
	halfWay := dayOf(horizon.Add(now.Sub(horizon) / 2))
	threeQuarters := dayOf(halfWay.Add(now.Sub(halfWay) / 2))
	for n, want := range []time.Time{halfWay, threeQuarters} {
		if err := runAt(failing); err == nil {
			t.Fatalf("failure %d: the run answered no error for two sources whose first page failed", n+1)
		}
		if got, ok := walkStartOf(t, db, sourceDelivery); !ok || !got.Equal(want) {
			t.Errorf("after timeout %d the %s first walk starts at %v (%v), want %v", n+1, sourceDelivery, got, ok, want)
		}
		if got, ok := walkStartOf(t, db, sourceConsent); ok {
			t.Errorf("after failure %d the %s first walk starts at %v: a connection reset moved it", n+1, sourceConsent, got)
		}
		if got, ok := walkStartOf(t, db, sourceEngagement); ok {
			t.Errorf("after failure %d the %s first walk starts at %v: a statement cancelled on request moved it", n+1, sourceEngagement, got)
		}
		for _, s := range firstWalks {
			if through, ok := watermarkOf(t, db, s); ok {
				t.Errorf("a failed first page left a %s watermark at %v", s, through)
			}
		}
	}
	for _, line := range []string{
		fmt.Sprintf("email statistics for %s start from %s: the first walk over a longer history timed out", sourceDelivery, halfWay.Format(time.DateOnly)),
		fmt.Sprintf("email statistics for %s start from %s: the first walk over a longer history timed out", sourceDelivery, threeQuarters.Format(time.DateOnly)),
		"source=" + sourceConsent + " error=\"read a page: read tcp 10.0.0.5:5432: connection reset by peer\"",
		"source=" + sourceEngagement + " error=\"read a page: ERROR: canceling statement due to user request (SQLSTATE=57014)\"",
	} {
		if !strings.Contains(logs.String(), line) {
			t.Errorf("the log does not say %q:\n%s", line, logs.String())
		}
	}
	for _, s := range []string{sourceConsent, sourceEngagement} {
		if line := fmt.Sprintf("email statistics for %s start from", s); strings.Contains(logs.String(), line) {
			t.Errorf("the log says %q, for a failure that moved nothing:\n%s", line, logs.String())
		}
	}

	// A run whose own context ends during the first page -- the node
	// stopping, the lead moving -- moves nothing, even when that page meanwhile
	// ends at the statement timeout: the run is stopping, and the next run reads
	// the same page again, so skipping it costs nothing. It does not try to: a
	// write on the ended context would fail anyway, so the error is what shows
	// the attempt.
	stopping, stop := context.WithCancel(principalCtx(t))
	defer stop()
	cancelled := &scriptedEngine{
		pages: map[string]map[string]scriptedPage{"deliveriesWrittenSince": {"": {err: errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE=57014)")}}},
		before: func(construct, _ string) {
			if construct == "deliveriesWrittenSince" {
				stop()
			}
		},
	}
	if _, err := runRollup(stopping, cancelled, db, now); err == nil || strings.Contains(err.Error(), "record where the next first walk starts") {
		t.Errorf("a run stopped during the first page answered %v; want its failed read, and no attempt to move the start", err)
	}
	if got, _ := walkStartOf(t, db, sourceDelivery); !got.Equal(threeQuarters) {
		t.Errorf("a run stopped during the first page moved the delivery start to %v, want it left at %v", got, threeQuarters)
	}

	// The next run that reads the first page walks from where the failures
	// left the start, and is walked from then on.
	reading := &scriptedEngine{pages: map[string]map[string]scriptedPage{}}
	if err := runAt(reading); err != nil {
		t.Fatal(err)
	}
	walked := 0
	for _, c := range reading.calls {
		switch c.construct {
		case "deliveriesWrittenSince":
			walked++
			if since := sinceOf(t, c.query); !since.Equal(threeQuarters) {
				t.Errorf("the deliveries' first walk started at %v, want the start the timeouts left, %v", since, threeQuarters)
			}
		case "consentEventsWrittenSince", "engagementEventsWrittenSince":
			if since := sinceOf(t, c.query); !since.Equal(horizon) {
				t.Errorf("%s's first walk started at %v, want four hundred days back, %v: its failure moved nothing", c.construct, since, horizon)
			}
		}
	}
	if walked == 0 {
		t.Fatal("the run never read the deliveries")
	}
	// The walks found no rows, so there is nothing to resume from, but each is
	// walked through the clock it asked its last page by.
	for _, s := range firstWalks {
		if at, ok := walkedOf(t, db, s); !ok || at.Before(now) {
			t.Errorf("the %s walk that read its first page is walked through %v (%v), want the run's clock", s, at, ok)
		}
	}

	// Before the start nothing of the deliveries is measured: a month that
	// ends before it, or holds it, has null sends, though a fact is recorded
	// there. The engagement events were walked from four hundred days back,
	// so that month's opens are measured.
	earlier := threeQuarters.AddDate(0, 0, -45)
	if _, err := db.NewInsert().Model(&[]factRow{{Kind: kindSent, SourceID: suffix + "-old", AccountID: suffix + "-org", CampaignID: suffix + "-camp", Day: dateArg(earlier)}}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := readStatistics(ctx, db, staff(), nil, statsperiod.Last12Months, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.MeasuredSince == nil || got.MeasuredSince.After(earlier) {
		t.Fatalf("measuredSince is %v, want it on or before the earlier fact's %v for this to prove anything", got.MeasuredSince, earlier)
	}
	for _, p := range got.Series {
		end := p.At.AddDate(0, 1, 0)
		switch {
		case p.At.Before(threeQuarters):
			if p.Sent != nil {
				t.Errorf("the month of %s, which starts before the deliveries' walk, has %d sends; want null", p.At.Format(time.DateOnly), *p.Sent)
			}
			if end.After(earlier) && p.UniqueOpens == nil {
				t.Errorf("the month of %s has null opens; the engagement walk started at the horizon, so they are measured", p.At.Format(time.DateOnly))
			}
		default:
			if p.Sent == nil {
				t.Errorf("the month of %s, after the deliveries' walk began, has null sends; want them measured", p.At.Format(time.DateOnly))
			}
		}
	}
}
