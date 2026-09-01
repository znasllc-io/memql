package campaigns

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// identity_test.go -- the resolution order, the refusal split, and the three
// places the check has to happen (memql#4821).
//
// The tests worth reading twice are the REFUSAL ones. A missing or disabled
// identity has an obvious wrong answer -- fall back to the configured default
// sender -- which produces a send that looks entirely successful and mails a
// client's list from another client's mailbox. Nothing downstream could
// detect it: the delivery ledger, the counters and the campaign row would all
// be identical. So each refusal is pinned at each of the three sites.

func identityWorker(t *testing.T, engine *fakeEngine) *Worker {
	t.Helper()
	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.SendingIdentity = "default@example.test"
	return w
}

func campaignWithIdentity(identityID string) Campaign {
	return Campaign{
		ID: testCampaign, OwnerUserID: testOwner, Name: "August update",
		AudienceID: testAudience, TemplateID: testTemplate, Status: "sending",
		SenderIdentityID: identityID,
	}
}

// TestUnnamedIdentityResolvesToTheConfiguredDefault is the ordinary case and
// the one plurality had to stay additive for: a campaign written before
// identities existed names none, and must behave exactly as it did.
func TestUnnamedIdentityResolvesToTheConfiguredDefault(t *testing.T) {
	w := identityWorker(t, &fakeEngine{})

	got, refusal := w.resolveSendIdentity(context.Background(), campaignWithIdentity(""))
	if refusal.refused() {
		t.Fatalf("a campaign naming no identity was refused: %s", refusal.Reason)
	}
	if got.SendAs.Address != "" {
		t.Errorf("SendAs.Address = %q, want empty -- an unnamed identity must resolve to the "+
			"transport's own configured mailbox, not to an address this package invented", got.SendAs.Address)
	}
	if got.Label != "default@example.test" {
		t.Errorf("reputation label = %q, want the env-derived default", got.Label)
	}
}

// TestCampaignFromNameReachesTheHeaderWithoutAnIdentity is design D6's
// defect fix: fromName has been authored, stored and documented since
// memql#3323 and reached no header at all. A campaign that names no identity
// still has to carry its display name.
func TestCampaignFromNameReachesTheHeaderWithoutAnIdentity(t *testing.T) {
	w := identityWorker(t, &fakeEngine{})
	c := campaignWithIdentity("")
	c.FromName = "The MemQL team"

	got, _ := w.resolveSendIdentity(context.Background(), c)
	if got.SendAs.FromName != "The MemQL team" {
		t.Errorf("SendAs.FromName = %q, want the campaign's own fromName. A SendAs carrying only a "+
			"display name means 'the configured mailbox, under this name', which is exactly what a "+
			"campaign overriding fromName and nothing else asks for", got.SendAs.FromName)
	}
	if got.SendAs.Address != "" {
		t.Errorf("SendAs.Address = %q, want empty: a fromName override must not change WHICH mailbox sends", got.SendAs.Address)
	}
}

func TestNamedIdentityResolvesToItsMailbox(t *testing.T) {
	engine := &fakeEngine{senderIdentities: map[string]map[string]any{
		"si-acme": senderIdentityRow("si-acme", "News@Acme.test", "Acme News", "active"),
	}}
	w := identityWorker(t, engine)

	got, refusal := w.resolveSendIdentity(context.Background(), campaignWithIdentity("si-acme"))
	if refusal.refused() {
		t.Fatalf("refused: %s", refusal.Reason)
	}
	if got.SendAs.Address != "News@Acme.test" {
		t.Errorf("SendAs.Address = %q, want the identity's address VERBATIM -- the transport builds a "+
			"/users/{address} path from it and the mailbox is named as the operator declared it", got.SendAs.Address)
	}
	if got.SendAs.FromName != "Acme News" {
		t.Errorf("SendAs.FromName = %q, want the identity's fromName", got.SendAs.FromName)
	}
	// The KEY is normalized even though the address is not: two spellings of
	// one mailbox must not split its warming ladder in half.
	if got.Label != "news@acme.test" {
		t.Errorf("reputation label = %q, want the normalized lowercase address -- a ladder counted twice "+
			"under two spellings advances neither", got.Label)
	}
}

func TestCampaignFromNameOverridesTheIdentitys(t *testing.T) {
	engine := &fakeEngine{senderIdentities: map[string]map[string]any{
		"si-acme": senderIdentityRow("si-acme", "news@acme.test", "Acme News", "active"),
	}}
	w := identityWorker(t, engine)
	c := campaignWithIdentity("si-acme")
	c.FromName = "Acme Spring Sale"

	got, _ := w.resolveSendIdentity(context.Background(), c)
	if got.SendAs.FromName != "Acme Spring Sale" {
		t.Errorf("SendAs.FromName = %q, want the campaign's override: the campaign is the more specific "+
			"statement about this one send", got.SendAs.FromName)
	}
	if got.SendAs.Address != "news@acme.test" {
		t.Errorf("SendAs.Address = %q -- overriding the display name must not move the mailbox", got.SendAs.Address)
	}
}

func TestReplyToPrefersTheCampaignOverTheIdentity(t *testing.T) {
	identity := resolvedIdentity{ReplyTo: "identity@acme.test"}
	if got := replyToFor(Campaign{ReplyTo: "campaign@acme.test"}, identity); got != "campaign@acme.test" {
		t.Errorf("Reply-To = %q, want the campaign's own", got)
	}
	if got := replyToFor(Campaign{}, identity); got != "identity@acme.test" {
		t.Errorf("Reply-To = %q, want the identity's default when the campaign sets none", got)
	}
	if got := replyToFor(Campaign{}, resolvedIdentity{}); got != "" {
		t.Errorf("Reply-To = %q, want empty so the header is left off entirely", got)
	}
}

// --- the refusals -------------------------------------------------------

func TestMissingIdentityIsATerminalRefusal(t *testing.T) {
	w := identityWorker(t, &fakeEngine{})

	_, refusal := w.resolveSendIdentity(context.Background(), campaignWithIdentity("si-gone"))
	if !refusal.refused() {
		t.Fatal("a campaign naming an identity that does not resolve was ACCEPTED. The obvious wrong " +
			"answer here is a silent fallback to the configured default sender, which mails a client's " +
			"list from another client's mailbox and leaves the ledger, the counters and the campaign row " +
			"all looking correct")
	}
	if !refusal.Terminal {
		t.Errorf("a missing identity was classified as an ENVIRONMENT problem. Nothing about the cluster "+
			"changes it, so retrying is a campaign that spins forever without telling the operator what "+
			"to fix.\n  reason: %s", refusal.Reason)
	}
}

func TestDisabledIdentityIsATerminalRefusal(t *testing.T) {
	engine := &fakeEngine{senderIdentities: map[string]map[string]any{
		"si-retired": senderIdentityRow("si-retired", "old@acme.test", "Acme", "disabled"),
	}}
	w := identityWorker(t, engine)

	_, refusal := w.resolveSendIdentity(context.Background(), campaignWithIdentity("si-retired"))
	if !refusal.refused() || !refusal.Terminal {
		t.Fatalf("a disabled identity must be a terminal refusal, got refused=%v terminal=%v (%s)",
			refusal.refused(), refusal.Terminal, refusal.Reason)
	}
	if strings.Contains(refusal.Reason, "old@acme.test") {
		t.Errorf("the refusal echoes the full mailbox address into a string that will be stored on the "+
			"campaign row and logged: %s", refusal.Reason)
	}
}

// TestAnIdentityWithNoStatusIsActive covers the absent-default trap: a
// concept @default is never applied on insert, so a row written by a path
// that omitted `status` carries none. Reading that as "disabled" would refuse
// the campaign TERMINALLY -- an unrecoverable answer derived from a missing
// key.
func TestAnIdentityWithNoStatusIsActive(t *testing.T) {
	row := senderIdentityRow("si-acme", "news@acme.test", "Acme News", "")
	delete(row, "status")
	engine := &fakeEngine{senderIdentities: map[string]map[string]any{"si-acme": row}}
	w := identityWorker(t, engine)

	if _, refusal := w.resolveSendIdentity(context.Background(), campaignWithIdentity("si-acme")); refusal.refused() {
		t.Fatalf("an identity row with no status was refused: %s", refusal.Reason)
	}
}

func TestAnUnreadableIdentityIsAnEnvironmentRefusal(t *testing.T) {
	engine := &fakeEngine{identityReadErr: errors.New("engine not ready")}
	w := identityWorker(t, engine)

	_, refusal := w.resolveSendIdentity(context.Background(), campaignWithIdentity("si-acme"))
	if !refusal.refused() {
		t.Fatal("a failed identity read was treated as 'no identity named'")
	}
	if refusal.Terminal {
		t.Errorf("a failed READ was classified as an authoring problem, so one slow query destroys a "+
			"scheduled send the operator cannot recover without re-authoring it.\n  reason: %s", refusal.Reason)
	}
}

// --- the three sites ----------------------------------------------------

// TestPreflightRefusesADisabledIdentity is site one: the operator is looking
// at the screen.
func TestPreflightRefusesADisabledIdentity(t *testing.T) {
	engine := &fakeEngine{
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
		senderIdentities: map[string]map[string]any{
			"si-retired": senderIdentityRow("si-retired", "old@acme.test", "Acme", "disabled"),
		},
	}
	w := identityWorker(t, engine)

	_, err := w.preflight(context.Background(), "startSend", campaignWithIdentity("si-retired"))
	if err == nil {
		t.Fatal("preflight accepted a campaign naming a disabled identity")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("preflight refused for some other reason: %v", err)
	}
}

// TestFireTimePreflightRefusesADisabledIdentity is site two: hours have
// passed since the operator committed to a time, and the mailbox was retired
// in between.
func TestFireTimePreflightRefusesADisabledIdentity(t *testing.T) {
	engine := &fakeEngine{
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
		senderIdentities: map[string]map[string]any{
			"si-retired": senderIdentityRow("si-retired", "old@acme.test", "Acme", "disabled"),
		},
	}
	w := identityWorker(t, engine)

	reason, terminal := w.fireTimePreflight(context.Background(), campaignWithIdentity("si-retired"))
	if reason == "" {
		t.Fatal("the fire-time preflight accepted a campaign whose identity was retired after scheduling")
	}
	if !terminal {
		t.Errorf("a disabled identity must FAIL the scheduled campaign rather than retry forever: %s", reason)
	}
}

// TestFireTimePreflightWaitsOnAnUnreadableIdentity is the other half of the
// split at the same site: the campaign is fine, the cluster is not.
func TestFireTimePreflightWaitsOnAnUnreadableIdentity(t *testing.T) {
	engine := &fakeEngine{template: templateRow(), identityReadErr: errors.New("engine not ready")}
	w := identityWorker(t, engine)

	reason, terminal := w.fireTimePreflight(context.Background(), campaignWithIdentity("si-acme"))
	if reason == "" {
		t.Fatal("the fire-time preflight ignored an identity read failure")
	}
	if terminal {
		t.Errorf("an unreadable identity failed the scheduled campaign; a bad deploy must not force the "+
			"operator to re-author a schedule: %s", reason)
	}
}

// TestDrainPathRefusesADisabledIdentity is site three, and it is the one the
// other two cannot cover.
//
// campaignStartSend resolves the identity when the operator presses the
// button and then enqueues a job. Nothing between that moment and the drain
// re-reads it, and the scheduled path's fire-time check never runs on a job
// that was never scheduled. So an immediate start whose identity is retired
// while the job sits in the queue reaches the transport unless sendBatch
// checks -- which is exactly what this asserts, by driving the DRAIN with a
// job already queued and no start call in sight.
func TestDrainPathRefusesADisabledIdentity(t *testing.T) {
	campaign := campaignRow()
	campaign["senderIdentityId"] = "v1:campaigns:senderIdentity:si-retired"
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaign,
		template: templateRow(),
		roster: []map[string]any{
			recipientRow("r-1", "a@example.test", "subscribed"),
			recipientRow("r-2", "b@example.test", "subscribed"),
		},
		senderIdentities: map[string]map[string]any{
			"si-retired": senderIdentityRow("si-retired", "old@acme.test", "Acme", "disabled"),
		},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.cfg.SendingIdentity = "default@example.test"

	w.DrainOnce(context.Background())

	if sender.count() != 0 {
		t.Fatalf("the drain mailed %d recipients as the DEFAULT sender while the campaign named a "+
			"disabled identity. This is the failure the whole refusal exists for: every row downstream "+
			"would look correct", sender.count())
	}
	if !wroteContaining(engine, "mutation updateSendJob", `status: "failed"`) {
		t.Error("the send job was not failed. A disabled identity is an authoring problem, so the job " +
			"has to stop with the reason on the row rather than retry on every tick")
	}
	if !wroteContaining(engine, "mutation updateCampaignProgress", "disabled") {
		t.Error("the reason was not stamped on the CAMPAIGN row, which is the row the operator who " +
			"picked the mailbox is looking at")
	}
}

// TestTheSendKeysReputationOnTheResolvedIdentity is design D8 arriving:
// reputationWindow.sendingIdentity finally carries something other than the
// deployment's one label.
func TestTheSendKeysReputationOnTheResolvedIdentity(t *testing.T) {
	campaign := campaignRow()
	campaign["senderIdentityId"] = "v1:campaigns:senderIdentity:si-acme"
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaign,
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "someone@example.test", "subscribed")},
		senderIdentities: map[string]map[string]any{
			"si-acme": senderIdentityRow("si-acme", "News@Acme.test", "Acme News", "active"),
		},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.cfg.SendingIdentity = "default@example.test"

	w.DrainOnce(context.Background())

	if sender.count() != 1 {
		t.Fatalf("expected one message, got %d", sender.count())
	}
	if as := sender.identities()[0]; as.Address != "News@Acme.test" {
		t.Errorf("the message went out as %q, want the campaign's declared mailbox", as.Address)
	}
	if !wroteContaining(engine, "mutation recordReputationWindow", `sendingIdentity: "news@acme.test"`) {
		t.Errorf("the reputation counter was not keyed on the resolved identity. Before memql#4821 the "+
			"key was always the deployment's one label, which made sendingIdentity a dimension with a "+
			"single member.\ncalls:\n%s", strings.Join(callsWithPrefix(engine, "mutation recordReputationWindow"), "\n"))
	}
}

// TestWarmupEvaluatesEveryActiveIdentity pins the plural half of design D8:
// the ramp reads one state row per identity this process has mailed as, plus
// the configured default, and the process-wide limiter takes the SLOWEST.
func TestWarmupEvaluatesEveryActiveIdentity(t *testing.T) {
	engine := &fakeEngine{}
	w := identityWorker(t, engine)
	w.cfg.WarmupEnabled = true
	w.cfg.WarmupSteps = []int{5, 10, 25}
	w.noteActiveIdentity("news@acme.test")
	w.noteActiveIdentity("hello@beta.test")

	w.applyWarmup(context.Background(), time.Now().UTC())

	for _, identity := range []string{"default@example.test", "news@acme.test", "hello@beta.test"} {
		if !wroteContaining(engine, "mutation recordWarmupState", `sendingIdentity: "`+identity+`"`) {
			t.Errorf("no warming state was recorded for %q. warmupStateForIdentity was built for "+
				"plurality and received one member until identities existed; a ramp that still evaluates "+
				"only the default silently stops warming every declared mailbox", identity)
		}
	}
}

// --- local call inspection ----------------------------------------------
//
// Deliberately local rather than methods on fakeEngine: a helper hung off the
// shared fixture is one more thing every other file in the package inherits,
// and these two are only ever asked about writes this file makes.

func callsWithPrefix(engine *fakeEngine, prefix string) []string {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	var out []string
	for _, c := range engine.calls {
		if strings.HasPrefix(c.query, prefix) {
			out = append(out, c.query)
		}
	}
	return out
}

func wroteContaining(engine *fakeEngine, prefix, needle string) bool {
	for _, q := range callsWithPrefix(engine, prefix) {
		if strings.Contains(q, needle) {
			return true
		}
	}
	return false
}
