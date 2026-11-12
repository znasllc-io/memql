package campaigns

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// send_row_db_test.go -- the transactional row lane against a real engine and a
// real Postgres: the ledger row the organization guard admits for a person with
// no recipient row, the suppression it honours and the one it ignores, the
// replay that asks the resolver again, and the status read.
//
// Each test registers a concept of its own (registerTestRowAddress puts back
// what was there), whose resolver counts its calls.

// rowLane is one organization with a published transactional template, a
// sender, a worker, and a concept registered for the lane.
type rowLane struct {
	*digestNewsletter
	concept, template string
	resolver          *countingResolver
	address           string
}

func newRowLane(t *testing.T, name string) *rowLane {
	t.Helper()
	n := newDigestFixture(t).newEmptyNewsletter(t, name)
	l := &rowLane{digestNewsletter: n, concept: "v1:test:" + strings.ReplaceAll(n.suffix, "-", ""), template: n.suffix + "-lane-tpl",
		address: "applicant-" + n.suffix + "@example.com"}
	args := map[string]any{"templateId": l.template, "accountId": n.org, "name": "Application approved", "purpose": "transactional",
		"content": map[string]any{"subject": "Welcome, {{fields.companyName}}", "textBody": "Sign in: {{fields.signInUrl}}", "htmlBody": "<p>Sign in: {{fields.signInUrl}}</p>"}}
	nodes, err := n.worker.handleSaveTemplate(n.owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	args["expectedRevision"], args["action"] = decodeResult(t, nodes)["revision"], "publish"
	if _, err := n.worker.handleSaveTemplate(n.owner, args, 0); err != nil {
		t.Fatal(err)
	}
	l.resolver = &countingResolver{answer: func(call int) (RowMessage, error) {
		return RowMessage{Email: l.address, AccountID: n.org, Fields: map[string]string{
			"companyName": "Acme Widgets", "signInUrl": fmt.Sprintf("https://example.com/sign-in?t=minted-%d", call)}}, nil
	}}
	registerTestRowAddress(t, l.concept, l.resolver.resolve)
	return l
}

func (l *rowLane) args(request string) map[string]any {
	return map[string]any{"rowConcept": l.concept, "rowId": l.suffix + "-application", "accountId": l.org, "kind": "approved",
		"templateId": l.template, "requestId": l.suffix + "-" + request, "senderIdentityId": l.suffix + "-sender"}
}

func (l *rowLane) send(t *testing.T, request string) map[string]any {
	t.Helper()
	nodes, err := l.worker.handleSendRowMessage(auth.ContextWithInternalOrigin(l.owner), l.args(request), 0)
	if err != nil {
		t.Fatalf("sendRowMessage: %v", err)
	}
	return decodeResult(t, nodes)
}

// ledger is the newest version of the request's delivery row.
func (l *rowLane) ledger(t *testing.T, request string) map[string]any {
	t.Helper()
	row, err := l.worker.newsletterRow(l.system, "providerDeliveryById", "deliveryId", singleDeliveryRowID(l.suffix+"-"+request, rowRecipientID(l.concept, l.suffix+"-application")))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestAnOrganizationUnsubscribeDoesNotStopARowMessage(t *testing.T) {
	l := newRowLane(t, "lane-org-unsub")
	if err := l.store.RecordOrganizationSuppression(l.system, l.org, EmailDigest(l.address), "unsubscribed", "example.com", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := l.send(t, "org-unsub"); got["sent"] != true || l.sender.count() != 1 {
		t.Fatalf("result %v, %d sent: an organization unsubscribe stopped a message about the person's own request", got, l.sender.count())
	}
	row := l.ledger(t, "org-unsub")
	if str(row, "status") != "sent" || str(row, "rowConcept") != l.concept || str(row, "rowId") != l.suffix+"-application" || str(row, "rowKind") != "approved" {
		t.Fatalf("the ledger row: %v", row)
	}
}

func TestAClusterWideUnsubscribeDoesNotStopARowMessage(t *testing.T) {
	l := newRowLane(t, "lane-cluster-unsub")
	if err := l.store.RecordSuppression(l.system, EmailDigest(l.address), "unsubscribed", "example.com", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := l.send(t, "cluster-unsub"); got["sent"] != true || l.sender.count() != 1 {
		t.Fatalf("result %v, %d sent: a cluster unsubscribe stopped a message about the person's own request", got, l.sender.count())
	}
}

func TestAHardBounceSkipsARowMessage(t *testing.T) {
	for _, tc := range []struct{ reason, scope string }{{"hard_bounce", "cluster"}, {"complaint", "cluster"}, {"manual", "organization"}} {
		t.Run(tc.reason, func(t *testing.T) {
			l := newRowLane(t, "lane-"+strings.ReplaceAll(tc.reason, "_", ""))
			var err error
			if tc.scope == "cluster" {
				err = l.store.RecordSuppression(l.system, EmailDigest(l.address), tc.reason, "example.com", "", "")
			} else {
				err = l.store.RecordOrganizationSuppression(l.system, l.org, EmailDigest(l.address), tc.reason, "example.com", "", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := l.send(t, "blocked"); got["skipped"] != true || got["reason"] != tc.reason || l.sender.count() != 0 {
				t.Fatalf("result %v, %d sent", got, l.sender.count())
			} else if got["skipSentence"] != SkipSentence(tc.reason) || got["skipSentence"] == tc.reason {
				t.Fatalf("the skip answered the sentence %q, want the lane's words for %s", got["skipSentence"], tc.reason)
			}
			if row := l.ledger(t, "blocked"); str(row, "status") != "skipped" || str(row, "skipReason") != tc.reason {
				t.Fatalf("the ledger row: %v", row)
			}
			// THE STATUS READ SAYS THE SAME, from the ledger: the code, and beside it
			// the sentence a person reads.
			nodes, err := l.worker.handleRowMessageStatus(auth.ContextWithInternalOrigin(l.owner),
				map[string]any{"requestIds": []any{l.suffix + "-blocked"}, "accountId": l.org}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if status := decodeResult(t, nodes); status["status"] != "skipped" || status["skipReason"] != tc.reason ||
				status["skipSentence"] != SkipSentence(tc.reason) {
				t.Fatalf("the status read answered %v", status)
			}
		})
	}
}

func TestAReplayedRowSendAsksTheResolverAgainAndSendsItsFreshFields(t *testing.T) {
	l := newRowLane(t, "lane-replay")
	l.worker.limiter = newRateLimiter(1, l.worker.now)
	if !l.worker.allowSend() {
		t.Fatal("new limiter empty")
	}
	if got := l.send(t, "replay"); got["deferred"] != true || l.sender.count() != 0 {
		t.Fatalf("the first call did not defer: %v", got)
	}
	if l.resolver.count() != 0 {
		t.Fatalf("a deferred send asked the resolver %d times", l.resolver.count())
	}
	pending := l.ledger(t, "replay")
	if str(pending, "status") != "pending" || str(pending, "rowConcept") != l.concept || str(pending, "rowKind") != "approved" {
		t.Fatalf("the pending row: %v", pending)
	}
	later := time.Now().Add(2 * time.Minute)
	l.worker.now = func() time.Time { return later }
	l.worker.limiter = newRateLimiter(1000, l.worker.now)
	l.worker.retryDeferredSends(context.Background(), l.system)
	if l.resolver.count() != 1 || l.sender.count() != 1 {
		t.Fatalf("the replay asked the resolver %d times and sent %d; want once and one", l.resolver.count(), l.sender.count())
	}
	msg := l.sender.sent[0]
	if msg.To != l.address || !strings.Contains(msg.TextBody, "t=minted-1") {
		t.Fatalf("the replay did not carry the field answered when it was sent: %q", msg.TextBody)
	}
	if row := l.ledger(t, "replay"); str(row, "status") != "sent" {
		t.Fatalf("the ledger row after the replay: %v", row)
	}
	var withField int
	if err := l.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:campaigns:delivery' AND payload::text LIKE ?`, "%minted-%").Scan(&withField); err != nil {
		t.Fatal(err)
	}
	if withField != 0 {
		t.Fatalf("%d ledger rows carry the message's field", withField)
	}
	// Run again: nothing is due, nothing is asked.
	l.worker.retryDeferredSends(context.Background(), l.system)
	if l.resolver.count() != 1 || l.sender.count() != 1 {
		t.Fatal("a settled row send was replayed again")
	}
}

func TestASecondCallWithTheSameRequestIdSendsNothingAndAsksNoResolver(t *testing.T) {
	l := newRowLane(t, "lane-twice")
	if got := l.send(t, "twice"); got["sent"] != true {
		t.Fatalf("first: %v", got)
	}
	got := l.send(t, "twice")
	if got["replayed"] != true || l.sender.count() != 1 || l.resolver.count() != 1 {
		t.Fatalf("second: %v, %d sent, resolver asked %d times", got, l.sender.count(), l.resolver.count())
	}
}

func TestRowMessageStatusAnswersEachRequestId(t *testing.T) {
	l := newRowLane(t, "lane-status")
	if got := l.send(t, "status-sent"); got["sent"] != true {
		t.Fatalf("send: %v", got)
	}
	withdrawn := &countingResolver{answer: func(int) (RowMessage, error) {
		return RowMessage{}, RowWithdrawn{Reason: "the application was withdrawn"}
	}}
	registerTestRowAddress(t, l.concept, withdrawn.resolve)
	if got := l.send(t, "status-withdrawn"); got["skipped"] != true {
		t.Fatalf("withdrawn: %v", got)
	}
	ids := []any{l.suffix + "-status-sent", l.suffix + "-status-withdrawn", l.suffix + "-status-never"}
	nodes, err := l.worker.handleRowMessageStatus(auth.ContextWithInternalOrigin(l.owner), map[string]any{"requestIds": ids, "accountId": l.org}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("%d answers for 3 request ids", len(nodes))
	}
	// A WITHDRAWAL'S SENTENCE IS THE ROW'S OWN, answered as it was given; a
	// message that was not skipped has none.
	want := []struct{ status, reason, sentence string }{{"sent", "", ""},
		{"skipped", "the application was withdrawn", "the application was withdrawn"}, {"", "", ""}}
	for i, node := range nodes {
		got := decodeResult(t, nodes[i:i+1])
		if got["requestId"] != ids[i] || got["status"] != want[i].status || got["skipReason"] != want[i].reason ||
			got["skipSentence"] != want[i].sentence {
			t.Fatalf("answer %d: %v", i, got)
		}
		if i == 0 && str(got, "sentAt") == "" {
			t.Fatalf("a sent message has no sentAt: %v", got)
		}
		for key := range got {
			switch key {
			case "id", "requestId", "status", "skipReason", "skipSentence", "providerStatus", "sentAt":
			default:
				t.Fatalf("the status reply carries %q: %v", key, got)
			}
		}
		if strings.Contains(string(node.Payload), l.address) {
			t.Fatalf("the status reply names the address: %s", node.Payload)
		}
	}
}

// The status read names its organization: another organization's request id
// answers nothing, exactly as an unknown one does.
func TestRowMessageStatusAnswersOnlyItsOrganization(t *testing.T) {
	l := newRowLane(t, "lane-status-org")
	if got := l.send(t, "status-org"); got["sent"] != true {
		t.Fatalf("send: %v", got)
	}
	ask := func(account string) map[string]any {
		t.Helper()
		nodes, err := l.worker.handleRowMessageStatus(auth.ContextWithInternalOrigin(l.owner), map[string]any{"requestIds": []any{l.suffix + "-status-org"}, "accountId": account}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return decodeResult(t, nodes)
	}
	if got := ask(l.suffix + "-another-org"); got["status"] != "" || got["sentAt"] != "" {
		t.Fatalf("another organization's account read the message: %v", got)
	}
	if got := ask(l.org); got["status"] != "sent" {
		t.Fatalf("the organization's own account: %v", got)
	}
	if got := ask("v1:accounts:account:" + l.org); got["status"] != "sent" {
		t.Fatalf("the organization's canonical account: %v", got)
	}
}

// A block on an address is never weakened by a later unsubscribe (ruling 43):
// an unsubscribe written over a bounce, a complaint or an operator's block
// leaves the row's reason as it was, at either scope. A blocking reason written
// over an unsubscribe replaces it.
func TestABlockOnAnAddressIsNeverWeakenedByALaterUnsubscribe(t *testing.T) {
	l := newRowLane(t, "lane-sticky")
	digest := EmailDigest(l.address)
	cluster := func(reason string) {
		t.Helper()
		if err := l.store.RecordSuppression(l.system, digest, reason, "example.com", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	clusterReason := func() string {
		t.Helper()
		sup, found, err := l.store.SuppressionByDigest(l.system, digest)
		if err != nil || !found {
			t.Fatalf("the cluster row: %v %v", found, err)
		}
		return sup.Reason
	}
	cluster("hard_bounce")
	cluster("unsubscribed")
	if got := clusterReason(); got != "hard_bounce" {
		t.Fatalf("a hard bounce then an unsubscribe reads %q", got)
	}
	if sup, blocked, err := l.store.BlockingSuppression(l.system, l.org, digest); err != nil || !blocked || sup.Reason != "hard_bounce" {
		t.Fatalf("BlockingSuppression: %+v %v %v", sup, blocked, err)
	}
	if got := l.send(t, "sticky"); got["skipped"] != true || got["reason"] != "hard_bounce" || l.sender.count() != 0 {
		t.Fatalf("a lane send to a bounced address that later unsubscribed: %v, %d sent", got, l.sender.count())
	}
	// An operator's suppress with reason unsubscribed, over a complaint.
	cluster("complaint")
	admin := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: l.user, Role: auth.RoleOwner}))
	if _, err := l.worker.handleSuppress(admin, map[string]any{"email": l.address, "reason": "unsubscribed"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := clusterReason(); got != "complaint" {
		t.Fatalf("a complaint then an operator's unsubscribed reads %q", got)
	}

	// The organization's row, in the same order.
	org := func(reason string) {
		t.Helper()
		if err := l.store.RecordOrganizationSuppression(l.system, l.org, digest, reason, "example.com", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	orgReason := func() string {
		t.Helper()
		row, err := l.store.organizationSuppression(l.system, l.org, digest)
		if err != nil || row == nil {
			t.Fatalf("the organization row: %v %v", row, err)
		}
		return str(row, "reason")
	}
	org("manual")
	org("unsubscribed")
	if got := orgReason(); got != "manual" {
		t.Fatalf("an operator's block then an unsubscribe reads %q at the organization", got)
	}

	// An unsubscribe then a hard bounce: the bounce replaces it.
	other := "unsub-first-" + l.suffix + "@example.com"
	if err := l.store.RecordSuppression(l.system, EmailDigest(other), "unsubscribed", "example.com", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.store.RecordSuppression(l.system, EmailDigest(other), "hard_bounce", "example.com", "", ""); err != nil {
		t.Fatal(err)
	}
	if sup, _, _ := l.store.SuppressionByDigest(l.system, EmailDigest(other)); sup.Reason != "hard_bounce" {
		t.Fatalf("an unsubscribe then a hard bounce reads %q", sup.Reason)
	}
}
