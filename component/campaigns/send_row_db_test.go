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
			}
			if row := l.ledger(t, "blocked"); str(row, "status") != "skipped" || str(row, "skipReason") != tc.reason {
				t.Fatalf("the ledger row: %v", row)
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
	nodes, err := l.worker.handleRowMessageStatus(auth.ContextWithInternalOrigin(l.owner), map[string]any{"requestIds": ids}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("%d answers for 3 request ids", len(nodes))
	}
	want := []struct{ status, reason string }{{"sent", ""}, {"skipped", "the application was withdrawn"}, {"", ""}}
	for i, node := range nodes {
		got := decodeResult(t, nodes[i:i+1])
		if got["requestId"] != ids[i] || got["status"] != want[i].status || got["skipReason"] != want[i].reason {
			t.Fatalf("answer %d: %v", i, got)
		}
		if i == 0 && str(got, "sentAt") == "" {
			t.Fatalf("a sent message has no sentAt: %v", got)
		}
		for key := range got {
			switch key {
			case "id", "requestId", "status", "skipReason", "providerStatus", "sentAt":
			default:
				t.Fatalf("the status reply carries %q: %v", key, got)
			}
		}
		if strings.Contains(string(node.Payload), l.address) {
			t.Fatalf("the status reply names the address: %s", node.Payload)
		}
	}
}
