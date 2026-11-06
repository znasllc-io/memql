package campaigns

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// recipients_test.go -- an address is in an audience once, and confirming
// always goes through confirmation. The operator's add and the operator's
// subscription change are builtins because the raw writes are @serverOnly: a
// client cannot reach the write without passing the checks.

const alreadyHere = "This address is already in this audience."

func (n *digestNewsletter) addPerson(t *testing.T, id, email string, extra map[string]any) error {
	t.Helper()
	args := map[string]any{"recipientId": n.suffix + "-" + id, "audienceId": n.audience, "email": email, "accountId": n.org, "source": "manual"}
	for k, v := range extra {
		args[k] = v
	}
	_, err := n.worker.handleAddRecipient(n.owner, args, 0)
	return err
}

func (n *digestNewsletter) requireConfirmation(t *testing.T) {
	t.Helper()
	n.execute(t, n.owner, call("mutation", "setAudienceConfirmation", arg{"audienceId", n.audience}, arg{"confirmation", "required"}))
}

func TestAddingAnAddressAlreadyInTheAudienceIsRefused(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "add-twice")
	if err := n.addPerson(t, "first", "a@b.test", nil); err != nil {
		t.Fatal(err)
	}
	err := n.addPerson(t, "second", " A@B.test ", nil)
	if err == nil || !strings.Contains(err.Error(), alreadyHere) {
		t.Fatalf("the same address under another spelling: %v", err)
	}
	if got := len(n.roster(t)); got != 1 {
		t.Fatalf("the audience holds %d rows, want one", got)
	}
	// A retry of the very same add (same recipient id) is not a second person.
	if err := n.addPerson(t, "first", "a@b.test", nil); err != nil {
		t.Fatalf("a retry of the same add: %v", err)
	}
	if got := len(n.roster(t)); got != 1 {
		t.Fatalf("the retry left %d rows, want one", got)
	}
	// The raw write is not reachable by a client, whatever it passes.
	_, rawErr := n.engine.Execute(n.owner, call("mutation", "addRecipient", arg{"recipientId", n.suffix + "-raw"}, arg{"audienceId", n.audience}, arg{"accountId", n.org}, arg{"email", "raw@b.test"}))
	if rawErr == nil || !strings.Contains(rawErr.Error(), "server-only") {
		t.Fatalf("a client reached the raw addRecipient write, or was refused for another reason: %v", rawErr)
	}
	_, rawErr = n.engine.Execute(n.owner, call("mutation", "setRecipientSubscription", arg{"recipientId", n.suffix + "-first"}, arg{"subscriptionStatus", "unsubscribed"}))
	if rawErr == nil || !strings.Contains(rawErr.Error(), "server-only") {
		t.Fatalf("a client reached the raw setRecipientSubscription write: %v", rawErr)
	}
	// The builtin, called through the engine as a client calls it, is registered
	// and refuses the same duplicate with the same sentence.
	if err = n.engine.RegisterIntegration(n.worker); err != nil {
		t.Fatal(err)
	}
	_, err = n.engine.Execute(n.owner, call("builtin", "campaignAddRecipient", arg{"recipientId", n.suffix + "-via-engine"}, arg{"audienceId", n.audience}, arg{"email", "A@b.test"}))
	if err == nil || !strings.Contains(err.Error(), alreadyHere) {
		t.Fatalf("builtin campaignAddRecipient through the engine: %v", err)
	}
	if _, err = n.engine.Execute(n.owner, call("builtin", "campaignAddRecipient", arg{"recipientId", n.suffix + "-via-engine"}, arg{"audienceId", n.audience}, arg{"email", "engine@b.test"})); err != nil {
		t.Fatalf("builtin campaignAddRecipient through the engine: %v", err)
	}
	if got := len(n.roster(t)); got != 2 {
		t.Fatalf("the audience holds %d rows, want two", got)
	}
}

// Two operators adding one address at the same moment, on two replicas, leave one row.
func TestTwoConcurrentAddsOfOneAddressLeaveOneRow(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "add-race")
	second := n.replica(t)
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, w := range []*Worker{n.worker, second} {
		wg.Add(1)
		go func(i int, w *Worker) {
			defer wg.Done()
			_, results[i] = w.handleAddRecipient(n.owner, map[string]any{"recipientId": fmt.Sprintf("%s-race-%d", n.suffix, i), "audienceId": n.audience, "email": "race@b.test"}, 0)
		}(i, w)
	}
	wg.Wait()
	refused := 0
	for _, err := range results {
		if err != nil {
			if !strings.Contains(err.Error(), alreadyHere) && !strings.Contains(err.Error(), "Try again") {
				t.Fatalf("unexpected refusal: %v", err)
			}
			refused++
		}
	}
	if got := len(n.roster(t)); got != 1 || refused > 1 {
		t.Fatalf("%d rows after two simultaneous adds (%d refused), want one", got, refused)
	}
}

func TestAddingToAConfirmFirstAudienceDefaultsToPending(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "add-pending")
	n.requireConfirmation(t)
	if err := n.addPerson(t, "plain", "plain@b.test", nil); err != nil {
		t.Fatal(err)
	}
	if got := storedRecipient(t, n.store, n.owner, n.suffix+"-plain"); str(got, "subscriptionStatus") != "pending" || str(got, "confirmedAt") != "" {
		t.Fatalf("added without a status: %v", got)
	}
	// Saying "subscribed" without saying they confirmed does not skip confirmation.
	if err := n.addPerson(t, "claim", "claim@b.test", map[string]any{"subscriptionStatus": "subscribed"}); err == nil || err.Error() != confirmFirstSentence {
		t.Fatalf("added as subscribed with no confirmation: %v", err)
	}
	if len(n.roster(t)) != 1 {
		t.Fatal("the refused add wrote a row")
	}
	// Saying they already confirmed records it.
	if err := n.addPerson(t, "done", "done@b.test", map[string]any{"subscriptionStatus": "subscribed", "alreadyConfirmed": true}); err != nil {
		t.Fatal(err)
	}
	got := storedRecipient(t, n.store, n.owner, n.suffix+"-done")
	if str(got, "subscriptionStatus") != "subscribed" || str(got, "confirmedAt") == "" {
		t.Fatalf("added as already confirmed: %v", got)
	}
	system := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(n.system))
	rows, err := n.store.rows(system, call("query", "audienceConfirmationsSince", arg{"audienceId", n.audience}, arg{"since", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}))
	if err != nil || len(rows) != 1 || bare(str(rows[0], "id")) != n.suffix+"-done" {
		t.Fatalf("automations triggered on confirmation see %v, %v", rows, err)
	}
	// Nothing about a mailbox that is not confirm-first changes.
	m := newDigestFixture(t).newEmptyNewsletter(t, "add-open")
	if err := m.addPerson(t, "open", "open@b.test", nil); err != nil {
		t.Fatal(err)
	}
	if got := storedRecipient(t, m.store, m.owner, m.suffix+"-open"); str(got, "subscriptionStatus") != "subscribed" {
		t.Fatalf("added to an audience that needs no confirmation: %v", got)
	}
}

// confirmJourney activates an automation that mails a confirmation to everyone who
// joins and waits for them to confirm, and returns its id.
func (n *digestNewsletter) confirmJourney(t *testing.T) string {
	t.Helper()
	template := n.suffix + "-tpl"
	rows, err := n.store.rows(n.owner, call("query", "templateById", arg{"templateId", template}))
	if err != nil {
		t.Fatal(err)
	}
	if err = n.worker.newsletterWrite(n.owner, "updateTemplate", map[string]any{"templateId": template, "name": "Confirm", "subject": "Confirm", "textBody": "Confirm: {{confirmUrl}}", "htmlBody": "<p><a href=\"{{confirmUrl}}\">Confirm</a></p>", "status": "ready", "versionTime": memql.VersionTimeAfter(templateTime(rows[0]["createdAt"]), time.Now()).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	wired := NewWorker(n.worker.store.engine, nil, nil, quietLogger(), gatePoolFor(t))
	n.worker.journeyGate = wired.journeyGate
	journeyID := n.suffix + "-journey"
	result, err := n.worker.handleSaveJourney(n.owner, map[string]any{"journeyId": journeyID, "accountId": n.org, "name": "Confirm membership", "trigger": map[string]any{"kind": "joined", "audienceId": n.audience}, "steps": []map[string]any{{"id": "confirm", "kind": "confirm", "templateId": template, "senderIdentityId": n.suffix + "-sender", "timeoutDays": 7}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.worker.handleSetJourneyState(n.owner, map[string]any{"journeyId": journeyID, "action": "activate", "expectedRevision": decodeResult(t, result)["revision"]}, 0); err != nil {
		t.Fatal(err)
	}
	return journeyID
}

func TestMarkingSomeoneConfirmedStartsTheirConfirmedAutomations(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "mark-confirmed")
	n.requireConfirmation(t)
	journeyID := n.confirmJourney(t)
	id := n.suffix + "-wait"
	if err := n.addPerson(t, "wait", "wait@b.test", nil); err != nil {
		t.Fatal(err)
	}
	// The automation mails the confirmation and the person's run waits for it.
	if err := n.worker.runJourney(context.Background(), n.system, map[string]any{"id": journeyID}); err != nil {
		t.Fatal(err)
	}
	pending := storedRecipient(t, n.store, n.owner, id)
	enrollmentID := journeyEnrollmentID(journeyID, id, templateTime(pending["joinedAt"]))
	waiting, err := n.worker.newsletterRow(n.system, "journeyEnrollmentById", "enrollmentId", enrollmentID)
	if err != nil || waiting == nil || str(waiting, "status") != "waiting" {
		t.Fatalf("the person's run is not waiting for confirmation: %v %v", waiting, err)
	}
	// Without saying they confirmed, nothing changes.
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "subscribed"}, 0); err == nil || err.Error() != confirmFirstSentence {
		t.Fatalf("subscribed with no confirmation: %v", err)
	}
	if got := storedRecipient(t, n.store, n.owner, id); str(got, "subscriptionStatus") != "pending" || str(got, "confirmedAt") != "" {
		t.Fatalf("a refused change wrote: %v", got)
	}
	before := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	n.worker.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "subscribed", "alreadyConfirmed": true}, 0); err != nil {
		t.Fatal(err)
	}
	got := storedRecipient(t, n.store, n.owner, id)
	if str(got, "subscriptionStatus") != "subscribed" || str(got, "confirmedAt") == "" {
		t.Fatalf("marked confirmed: %v", got)
	}
	system := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(n.system))
	rows, err := n.store.rows(system, call("query", "audienceConfirmationsSince", arg{"audienceId", n.audience}, arg{"since", before}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("automations triggered on confirmation see %v, %v", rows, err)
	}
	joined := templateTime(got["joinedAt"])
	if ok, err := n.store.consentExists(n.owner, sha256Hex("operator-confirm\x00"+id+"\x00"+joined.UTC().Format(time.RFC3339Nano))); err != nil || !ok {
		t.Fatalf("no consent event for the person: %v %v", ok, err)
	}
	// The waiting run was woken: its next time moved from the timeout to the confirmation.
	woken, err := n.worker.newsletterRow(n.system, "journeyEnrollmentById", "enrollmentId", enrollmentID)
	if err != nil || woken == nil || !templateTime(woken["nextAt"]).Before(templateTime(waiting["nextAt"])) {
		t.Fatalf("the waiting run was not woken: before %v after %v (%v)", waiting["nextAt"], woken["nextAt"], err)
	}
	// Honouring an unsubscribe by hand still works through the same builtin.
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "unsubscribed"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := storedRecipient(t, n.store, n.owner, id); str(got, "subscriptionStatus") != "unsubscribed" {
		t.Fatalf("unsubscribed by hand: %v", got)
	}
	// Someone who unsubscribed cannot be made subscribed by hand, confirmed or not.
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "subscribed", "alreadyConfirmed": true}, 0); err == nil || err.Error() != blockedSentence {
		t.Fatalf("an unsubscribed person was made subscribed: %v", err)
	}
	if got := storedRecipient(t, n.store, n.owner, id); str(got, "subscriptionStatus") != "unsubscribed" {
		t.Fatalf("a refused change wrote: %v", got)
	}
}

func TestAnOperatorCannotConfirmASuppressedAddress(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "confirm-suppressed")
	n.requireConfirmation(t)
	if err := n.addPerson(t, "blocked", "blocked@b.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := n.store.RecordOrganizationSuppression(n.owner, n.org, EmailDigest("blocked@b.test"), "unsubscribed", "example.test", "", ""); err != nil {
		t.Fatal(err)
	}
	id := n.suffix + "-blocked"
	_, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "subscribed", "alreadyConfirmed": true}, 0)
	if err == nil || err.Error() != blockedSentence {
		t.Fatalf("a suppressed address was marked confirmed: %v", err)
	}
	if got := storedRecipient(t, n.store, n.owner, id); str(got, "subscriptionStatus") != "pending" || str(got, "confirmedAt") != "" {
		t.Fatalf("a refused confirmation wrote: %v", got)
	}
	if err := n.addPerson(t, "blocked2", "Blocked@b.test", nil); err == nil || !strings.Contains(err.Error(), alreadyHere) {
		t.Fatalf("duplicate of a suppressed address: %v", err)
	}
	// Adding someone as already confirmed works while nothing blocks the address...
	if err := n.addPerson(t, "blocked3", "other@b.test", map[string]any{"subscriptionStatus": "subscribed", "alreadyConfirmed": true}); err != nil {
		t.Fatal(err)
	}
	// ...and is refused, writing nothing, once the address is suppressed.
	if err := n.store.RecordOrganizationSuppression(n.owner, n.org, EmailDigest("fresh-blocked@b.test"), "unsubscribed", "example.test", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := n.addPerson(t, "blocked4", "fresh-blocked@b.test", map[string]any{"subscriptionStatus": "subscribed", "alreadyConfirmed": true}); err == nil || err.Error() != blockedSentence {
		t.Fatalf("a suppressed address was added as confirmed: %v", err)
	}
	if rows, _ := n.store.rows(memql.ContextWithFreshRead(n.owner), call("query", "recipientById", arg{"recipientId", n.suffix + "-blocked4"})); len(rows) != 0 {
		t.Fatal("the refused add wrote a row")
	}
}

func TestConfirmingByHandErrorsWhenThePersonIsGoneOrChanged(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "confirm-gone")
	n.requireConfirmation(t)
	if err := n.worker.confirmByHand(n.owner, Recipient{ID: n.suffix + "-nobody", AccountID: n.org, Email: "nobody@b.test", SubscriptionStatus: "pending"}); err == nil {
		t.Fatal("confirming someone who does not exist reported success")
	}
	if err := n.addPerson(t, "moved", "moved@b.test", nil); err != nil {
		t.Fatal(err)
	}
	stale := Recipient{ID: n.suffix + "-moved", AccountID: n.org, Email: "moved@b.test", SubscriptionStatus: "bounced"}
	if err := n.worker.confirmByHand(n.owner, stale); err == nil {
		t.Fatal("confirming from a stale view reported success")
	}
	if got := storedRecipient(t, n.store, n.owner, n.suffix+"-moved"); str(got, "subscriptionStatus") != "pending" {
		t.Fatalf("a refused confirmation wrote: %v", got)
	}
}

func TestRejoiningAfterUnsubscribeIsNotADuplicate(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "rejoin-dup")
	address := "back@b.test"
	if err := n.addPerson(t, "back", address, nil); err != nil {
		t.Fatal(err)
	}
	rid := n.suffix + "-back"
	h := NewEmailLinkHandler(deliveryDBEngine{n.engine}, Config{UnsubscribeSecret: testSecret, UnsubscribeBaseURL: "https://example.test"}, quietLogger(), gatePoolFor(t))
	token := emailPageToken(t, LinkPreferences, LinkPayload{OwnerUserID: n.user, RecipientID: rid, AccountID: n.org, EmailDigest: EmailDigest(address)})
	if rr := pageRequest(h, "POST", PreferencesPath, token, "unsubscribe"); rr.Code != 200 {
		t.Fatalf("unsubscribe: %d", rr.Code)
	}
	if rr := pageRequest(h, "POST", PreferencesPath, token, "subscribe"); rr.Code != 200 || strings.Contains(rr.Body.String(), alreadyHere) {
		t.Fatalf("subscribe again was refused: %d %s", rr.Code, rr.Body.String())
	}
	if got := len(n.roster(t)); got != 1 {
		t.Fatalf("subscribe again left %d rows, want one", got)
	}
}

// TestImportSkipsAddressesAlreadyInTheAudience is the import's half of the rule: the same address under another spelling is a duplicate, is not written, and is counted.
func TestImportSkipsAddressesAlreadyInTheAudience(t *testing.T) {
	engine := &fakeEngine{roster: []map[string]any{
		recipientRow("r-existing", " A@B.test ", "subscribed"),
	}}
	w := importWorker(t, engine, "email\na@b.test\nnew@b.test\n")
	got := runImport(t, w, importCtx())
	if got["added"] != float64(1) || got["duplicates"] != float64(1) {
		t.Fatalf("counts = %+v, want added 1 / duplicates 1", got)
	}
	if wroteContaining(engine, "mutation addRecipient", "a@b.test") {
		t.Fatal("the address already in the audience was written again")
	}
}
