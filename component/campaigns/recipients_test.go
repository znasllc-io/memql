package campaigns

import (
	"strings"
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
	// The raw write is not reachable by a client.
	if _, err := n.engine.Execute(n.owner, call("mutation", "addRecipient", arg{"recipientId", n.suffix + "-raw"}, arg{"audienceId", n.audience}, arg{"email", "a@b.test"})); err == nil {
		t.Fatal("a client reached the raw addRecipient write")
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
	if err := n.addPerson(t, "claim", "claim@b.test", map[string]any{"subscriptionStatus": "subscribed"}); err == nil {
		t.Fatal("added as subscribed with no confirmation")
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

func TestMarkingSomeoneConfirmedStartsTheirConfirmedAutomations(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "mark-confirmed")
	n.requireConfirmation(t)
	id := n.suffix + "-wait"
	if err := n.addPerson(t, "wait", "wait@b.test", nil); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "subscribed"}, 0); err != nil {
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
	// Honouring an unsubscribe by hand still works through the same builtin.
	if _, err := n.worker.handleSetRecipientSubscription(n.owner, map[string]any{"recipientId": id, "subscriptionStatus": "unsubscribed"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := storedRecipient(t, n.store, n.owner, id); str(got, "subscriptionStatus") != "unsubscribed" {
		t.Fatalf("unsubscribed by hand: %v", got)
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
