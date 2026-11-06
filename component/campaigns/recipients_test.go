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

// The import holds the audience's lock for its whole batch and asks whether it
// still has it; a lost lock stops the batch before anything more is written.
func TestImportStopsWhenItsAudienceLockIsLost(t *testing.T) {
	engine := &fakeEngine{}
	w := importWorker(t, engine, "email\na@b.test\nc@d.test\n")
	taken := 0
	w.newsletterGate = func(_ context.Context, keys ...string) (gateHold, error) {
		taken++
		if len(keys) != 1 || keys[0] != newsletterLock("audience", testAudience) {
			t.Errorf("the import took %v, not the audience's lock", keys)
		}
		return lostEmailGate{}, nil
	}
	if _, err := w.handleImportRecipients(importCtx(), map[string]any{"audienceId": testAudience, "artifactId": testArtifact}, 0); err == nil || !strings.Contains(err.Error(), "lock was lost") {
		t.Fatalf("an import that lost its lock: %v", err)
	}
	if taken != 1 || wroteContaining(engine, "mutation addRecipient", "a@b.test") {
		t.Fatalf("lock taken %d times; rows were written without the lock", taken)
	}
}

// An operator never brings back someone who left. Moving them to pending first,
// and to subscribed from there, used to get round the refusal: the row ended
// subscribed and the next drain mailed them. Nothing moves a person out of
// unsubscribed, bounced or complained by hand; only the person can come back.
func TestAnOperatorCannotBringBackSomeoneWhoLeftThroughPending(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "left-pending", oneEmail)
	id := a.suffix + "-member"
	set := func(id, status string) error {
		_, err := a.worker.handleSetRecipientSubscription(a.owner, map[string]any{"recipientId": id, "subscriptionStatus": status}, 0)
		return err
	}
	if err := set(id, "unsubscribed"); err != nil {
		t.Fatal(err)
	}
	versions, _ := versionsOf(t, a.db, id)
	for _, status := range []string{"pending", "subscribed"} {
		if err := set(id, status); err == nil || err.Error() != blockedSentence {
			t.Fatalf("a person who unsubscribed was moved to %s by hand: %v", status, err)
		}
	}
	if got, _ := versionsOf(t, a.db, id); got != versions || str(storedRecipient(t, a.store, a.owner, id), "subscriptionStatus") != "unsubscribed" {
		t.Fatalf("a refused change wrote the person (%d versions, then %d): %v", versions, got, storedRecipient(t, a.store, a.owner, id))
	}
	// Saying it again is no change, and is not refused.
	if err := set(id, "unsubscribed"); err != nil {
		t.Fatalf("an unsubscribe of someone already unsubscribed: %v", err)
	}
	if got, _ := versionsOf(t, a.db, id); got != versions {
		t.Fatalf("an unsubscribe of someone already unsubscribed wrote them again (%d versions, then %d)", versions, got)
	}
	a.drain(t)
	if a.azure.submissions() != 0 {
		t.Fatal("the drain mailed a person who left")
	}
	// The provider's verdicts are the same: a bounce or a complaint stays what it is.
	for _, verdict := range []string{"bounced", "complained"} {
		gone := a.suffix + "-" + verdict
		if err := a.store.AddRecipient(a.owner, gone, a.audience, verdict+"@example.test", "", "manual", a.org, "subscribed", nil); err != nil {
			t.Fatal(err)
		}
		if err := a.store.SetRecipientSubscription(a.owner, gone, verdict, a.now); err != nil {
			t.Fatal(err)
		}
		for _, status := range []string{"pending", "subscribed", "unsubscribed"} {
			if err := set(gone, status); err == nil || err.Error() != blockedSentence {
				t.Fatalf("a person who %s was moved to %s by hand: %v", verdict, status, err)
			}
		}
		if got := str(storedRecipient(t, a.store, a.owner, gone), "subscriptionStatus"); got != verdict {
			t.Fatalf("a refused change left the person %s, want %s", got, verdict)
		}
	}
}

// An operator sets a person subscribed, pending or unsubscribed. The provider's
// verdicts, and anything else, are not theirs to write.
func TestAnOperatorSetsOnlySubscribedPendingOrUnsubscribed(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "set-states")
	if err := n.addPerson(t, "here", "here@b.test", nil); err != nil {
		t.Fatal(err)
	}
	id := n.suffix + "-here"
	for _, status := range []string{"bounced", "complained", "archived"} {
		_, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": status}, 0)
		if err == nil || err.Error() != operatorStatesSentence {
			t.Fatalf("an operator set a person %s: %v", status, err)
		}
	}
	if got := str(storedRecipient(t, n.store, n.owner, id), "subscriptionStatus"); got != "subscribed" {
		t.Fatalf("a refused change left the person %s", got)
	}
	for _, status := range []string{"pending", "subscribed", "unsubscribed"} {
		if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": status}, 0); err != nil {
			t.Fatalf("an operator set a person %s: %v", status, err)
		}
	}
}

// An add names the person it writes by id, and the write merges into a row that
// already has the id. An id that already names a person somewhere else -- another
// audience, another address, another organization -- is refused before anything
// is written, so nobody is moved, renamed or brought back by an add.
func TestAnAddNeverWritesOverAnotherPerson(t *testing.T) {
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "add-over")
	other := n.suffix + "-aud2"
	n.execute(t, n.owner, call("mutation", "createAudience", arg{"audienceId", other}, arg{"accountId", n.org}, arg{"name", "Other list"}))
	if err := n.addPerson(t, "left", "left@b.test", nil); err != nil {
		t.Fatal(err)
	}
	id := n.suffix + "-left"
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "unsubscribed"}, 0); err != nil {
		t.Fatal(err)
	}
	versions, _ := versionsOf(t, n.db, id)
	for _, c := range []struct{ name, audience, address string }{
		{"into another audience", other, "left@b.test"},
		{"into another audience under another address", other, "fresh@b.test"},
		{"under another address", n.audience, "fresh@b.test"},
	} {
		_, err := n.worker.handleAddRecipient(n.owner, map[string]any{"recipientId": id, "audienceId": c.audience, "email": c.address}, 0)
		if err == nil || err.Error() != idTakenSentence {
			t.Fatalf("an add %s with the id of a person who left: %v", c.name, err)
		}
	}
	row := storedRecipient(t, n.store, n.owner, id)
	if got, _ := versionsOf(t, n.db, id); got != versions || bare(str(row, "audienceId")) != n.audience || str(row, "subscriptionStatus") != "unsubscribed" || str(row, "email") != "left@b.test" {
		t.Fatalf("a refused add wrote the person (%d versions, then %d): %v", versions, got, row)
	}
	if roster, err := n.store.Roster(memql.ContextWithFreshRead(n.owner), other); err != nil || len(roster) != 0 {
		t.Fatalf("the other audience holds %v (%v), want nobody", roster, err)
	}
	// Another organization's person is not this operator's to write either.
	stranger := f.newDigestTenant(t, "add-over-stranger")
	theirs := stranger.suffix + "-theirs"
	if err := stranger.store.AddRecipient(stranger.owner, theirs, stranger.audience, "theirs@b.test", "", "manual", stranger.org, "subscribed", nil); err != nil {
		t.Fatal(err)
	}
	if err := stranger.store.SetRecipientSubscription(stranger.owner, theirs, "unsubscribed", time.Now()); err != nil {
		t.Fatal(err)
	}
	theirVersions, _ := versionsOf(t, n.db, theirs)
	_, err := n.worker.handleAddRecipient(n.owner, map[string]any{"recipientId": theirs, "audienceId": n.audience, "email": "theirs@b.test"}, 0)
	if err == nil || err.Error() != idTakenSentence {
		t.Fatalf("an add with another organization's person's id: %v", err)
	}
	if got, _ := versionsOf(t, n.db, theirs); got != theirVersions {
		t.Fatalf("a refused add wrote another organization's person (%d versions, then %d)", theirVersions, got)
	}
}

// A retry of an add -- the same id, the same audience, the same address -- is the
// person already added: it answers with them as they are and writes nothing, even
// if they have left since.
func TestARetriedAddAnswersWithThePersonAsTheyAre(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "add-retry")
	if err := n.addPerson(t, "again", "again@b.test", map[string]any{"subscriptionStatus": "pending"}); err != nil {
		t.Fatal(err)
	}
	id := n.suffix + "-again"
	retry := func(want string) {
		t.Helper()
		versions, _ := versionsOf(t, n.db, id)
		nodes, err := n.worker.handleAddRecipient(n.owner, map[string]any{"recipientId": id, "audienceId": n.audience, "email": " Again@B.test ", "subscriptionStatus": "subscribed"}, 0)
		if err != nil {
			t.Fatalf("a retry of the same add: %v", err)
		}
		if got := str(decodeResult(t, nodes), "subscriptionStatus"); got != want {
			t.Fatalf("the retry answered %q, want the person as they are (%s)", got, want)
		}
		if got, _ := versionsOf(t, n.db, id); got != versions {
			t.Fatalf("a retry wrote the person again (%d versions, then %d)", versions, got)
		}
		if got := str(storedRecipient(t, n.store, n.owner, id), "subscriptionStatus"); got != want {
			t.Fatalf("after the retry the person is %s, want %s", got, want)
		}
	}
	retry("pending")
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "unsubscribed"}, 0); err != nil {
		t.Fatal(err)
	}
	retry("unsubscribed")
}

// afterWrite runs then once, right after the first statement that begins with
// statement has been executed.
type afterWrite struct {
	Engine
	statement string
	then      func()
	once      sync.Once
}

func (e *afterWrite) Execute(ctx context.Context, statement string) (any, error) {
	result, err := e.Engine.Execute(ctx, statement)
	if strings.HasPrefix(statement, e.statement) {
		e.once.Do(e.then)
	}
	return result, err
}

// Someone the operator says already confirmed is written pending and then
// confirmed, both under the person's lock, which a drain takes before it sends to
// them: a drain that runs while they are being added sends them no confirmation.
// Should one get in all the same (the add's lock was lost on the way), the run it
// left waiting for a confirmation is woken once they are confirmed, as a
// confirmation by hand wakes it, and does not wait a week for its timeout.
func TestAddingSomeoneWhoAlreadyConfirmedNeverSendsThemAConfirmation(t *testing.T) {
	f := newDigestFixture(t)
	for _, c := range []struct {
		name     string
		lockLost bool
		sent     int
	}{
		{"a drain while they are added waits for them", false, 0},
		{"a run a drain left waiting anyway is woken", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := f.newEmptyNewsletter(t, "add-agreed-drain")
			n.requireConfirmation(t)
			journeyID := n.confirmJourney(t)
			wait := newsletterGateWait
			newsletterGateWait = 300 * time.Millisecond
			defer func() { newsletterGateWait = wait }()
			// Another node drains the automation the moment the person is written.
			other := newGatedWorker(t, n.engine, n.db, n.sender)
			other.journeyGate = NewWorker(other.store.engine, nil, nil, quietLogger(), gatePoolFor(t)).journeyGate
			if c.lockLost {
				other.newsletterGate = func(context.Context, ...string) (gateHold, error) { return openGate{}, nil }
			}
			drained := false
			actual := n.worker.store.engine
			n.worker.store.engine = &afterWrite{Engine: actual, statement: "mutation addRecipient", then: func() {
				drained = true
				// It may find the person held by the add, and try again on its next pass.
				_ = other.runJourney(context.Background(), n.system, map[string]any{"id": journeyID})
			}}
			err := n.addPerson(t, "agreed", "agreed@b.test", map[string]any{"subscriptionStatus": "subscribed", "alreadyConfirmed": true})
			n.worker.store.engine = actual
			if err != nil || !drained {
				t.Fatalf("the add (drained between: %v): %v", drained, err)
			}
			if err := n.worker.runJourney(context.Background(), n.system, map[string]any{"id": journeyID}); err != nil {
				t.Fatal(err)
			}
			if n.sender.count() != c.sent {
				t.Fatalf("a person who already confirmed was sent %d confirmation emails, want %d", n.sender.count(), c.sent)
			}
			id := n.suffix + "-agreed"
			person := storedRecipient(t, n.store, n.owner, id)
			if str(person, "subscriptionStatus") != "subscribed" || str(person, "confirmedAt") == "" {
				t.Fatalf("the person is %v, want them subscribed and confirmed", person)
			}
			run, err := n.worker.newsletterRow(n.system, "journeyEnrollmentById", "enrollmentId", journeyEnrollmentID(journeyID, id, templateTime(person["joinedAt"])))
			if err != nil || run == nil || str(run, "status") != "completed" {
				t.Fatalf("the person's run is %v (%v), want it past the confirmation and finished, not waiting for one", run, err)
			}
		})
	}
}

// The suppression list stands in front of every add as subscribed, whether or not
// the audience asks people to confirm: an address that opted out, or that the
// provider gave up on, is never added as a subscriber by hand.
func TestAnOperatorCannotAddASuppressedAddressAsSubscribedInAnyAudience(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "add-suppressed-open")
	if err := n.store.RecordOrganizationSuppression(n.owner, n.org, EmailDigest("gone@b.test"), "unsubscribed", "example.test", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"gone@b.test", " Gone@B.test "} {
		if err := n.addPerson(t, "gone", spelling, nil); err == nil || err.Error() != blockedSentence {
			t.Fatalf("a suppressed address (%q) was added as subscribed to an audience that asks for no confirmation: %v", spelling, err)
		}
	}
	if rows, _ := n.store.rows(memql.ContextWithFreshRead(n.owner), call("query", "recipientById", arg{"recipientId", n.suffix + "-gone"})); len(rows) != 0 {
		t.Fatal("the refused add wrote a row")
	}
	if err := n.addPerson(t, "fine", "fine@b.test", nil); err != nil {
		t.Fatalf("an address nothing blocks: %v", err)
	}
}

// Nobody who left, or whose address is suppressed, is subscribed by hand in any
// audience, including one that asks for no confirmation.
func TestAnOperatorCannotSubscribeSomeoneWhoLeftInAnyAudience(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "resubscribe-open")
	if err := n.addPerson(t, "left", "left@b.test", nil); err != nil {
		t.Fatal(err)
	}
	id := n.suffix + "-left"
	set := func(status string) error {
		_, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": status}, 0)
		return err
	}
	if err := set("unsubscribed"); err != nil {
		t.Fatal(err)
	}
	if err := set("subscribed"); err == nil || err.Error() != blockedSentence {
		t.Fatalf("an unsubscribed person was subscribed by hand: %v", err)
	}
	// A pending person whose address was suppressed since is refused too.
	if err := n.addPerson(t, "held", "held@b.test", map[string]any{"subscriptionStatus": "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := n.store.RecordOrganizationSuppression(n.owner, n.org, EmailDigest("held@b.test"), "unsubscribed", "example.test", "", ""); err != nil {
		t.Fatal(err)
	}
	_, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": n.suffix + "-held", "subscriptionStatus": "subscribed"}, 0)
	if err == nil || err.Error() != blockedSentence {
		t.Fatalf("a suppressed pending address was subscribed by hand: %v", err)
	}
	// A pending person with nothing against them is subscribed as before.
	if err := n.addPerson(t, "fine", "fine@b.test", map[string]any{"subscriptionStatus": "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": n.suffix + "-fine", "subscriptionStatus": "subscribed"}, 0); err != nil {
		t.Fatal(err)
	}
}
