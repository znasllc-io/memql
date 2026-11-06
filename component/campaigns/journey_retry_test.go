package campaigns

import (
	"context"
	"github.com/znasllc-io/memql/component/auth"
	"net/url"
	"strings"
	"testing"
	"time"

	"errors"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// journey_retry_test.go -- an automation run that failed is started again by the
// person who runs the automation, and only ever sends what the drain would have
// sent: once, and not to someone who has left.

// unconfirmable makes the next drain fail the member's send as an unconfirmable
// one: the transport keeps no record, so the enrollment fails at once and its
// step's receipt stays "attempting".
func (a *automation) unconfirmable(t *testing.T) *recordingSender {
	t.Helper()
	broken := &recordingSender{fail: func(int) error { return errors.New("connection reset by peer") }}
	a.worker.resolve = func() email.Sender { return broken }
	a.drain(t)
	row := a.enrollment(t, a.member)
	if str(row, "status") != "failed" {
		t.Fatalf("the send did not fail the enrollment: %v", row)
	}
	if got := str(a.receipt(t, "welcome"), "status"); got != receiptAttempting {
		t.Fatalf("the failed send's receipt is %q, want it left attempting", got)
	}
	a.worker.resolve = func() email.Sender { return a.azure }
	return broken
}

func (a *automation) retry(t *testing.T, ids ...any) int {
	t.Helper()
	args := map[string]any{"journeyId": a.journeyID}
	if ids != nil {
		args["enrollmentIds"] = ids
	}
	nodes, err := a.worker.handleRetryEnrollments(a.owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	return integer(decodeResult(t, nodes), "retried")
}

func TestRetryEnrollmentsResetsFailedOnesAndTheySend(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-retry", oneEmail)
	a.unconfirmable(t)
	// A run that is not failed is left alone: naming it retries nothing.
	if got := a.retry(t, "no-such-enrollment"); got != 0 {
		t.Fatalf("retried %d enrollments that were not named", got)
	}
	if got := a.retry(t); got != 1 {
		t.Fatalf("retried %d, want 1", got)
	}
	row := a.enrollment(t, a.member)
	if str(row, "status") != "active" || integer(row, "attempts") != 0 || str(row, "lastError") != "" || !templateTime(row["nextAt"]).Equal(a.now) {
		t.Fatalf("the retried enrollment is %v, want it active, due now, with no attempt or error", row)
	}
	if got := str(a.receipt(t, "welcome"), "status"); got != receiptFailed {
		t.Fatalf("the unconfirmed attempt's receipt is %q, want failed so the retry submits", got)
	}
	a.drain(t)
	a.sentOnceAndAdvanced(t, 1)
	if got := str(a.receipt(t, "welcome"), "status"); got != receiptComplete {
		t.Fatalf("the receipt is %q after the send", got)
	}
	// Nothing is failed any more.
	if got := a.retry(t); got != 0 {
		t.Fatalf("retried %d with nothing failed", got)
	}
	a.drain(t)
	if a.azure.submissions() != 1 {
		t.Fatal("a second retry sent again")
	}
}

// A receipt that is complete is an email that went. Retrying an enrollment never
// resets it, so the drain replays it and the person is not mailed twice.
func TestRetryEnrollmentsNeverResetsACompleteReceipt(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-retry-complete", oneEmail)
	a.drain(t)
	a.sentOnceAndAdvanced(t, 1)
	row := a.enrollment(t, a.member)
	if err := a.worker.newsletterWrite(a.system, "advanceJourneyEnrollment", map[string]any{"enrollmentId": a.member, "stepId": "welcome", "stepIndex": 0, "nextAt": a.now.Format(time.RFC3339Nano), "status": "failed", "lastError": "forced", "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), a.now).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if got := a.retry(t); got != 1 {
		t.Fatalf("retried %d, want 1", got)
	}
	if got := str(a.receipt(t, "welcome"), "status"); got != receiptComplete {
		t.Fatalf("a complete receipt became %q", got)
	}
	a.drain(t)
	if a.azure.submissions() != 1 || a.azure.count() != 1 {
		t.Fatalf("Azure was asked %d times and accepted %d, want the one email only", a.azure.submissions(), a.azure.count())
	}
}

func TestRetryEnrollmentsDoesNotSendToAnUnsubscribedPerson(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-retry-left", oneEmail)
	a.unconfirmable(t)
	if err := a.store.SetRecipientSubscription(a.owner, a.suffix+"-member", "unsubscribed", a.now); err != nil {
		t.Fatal(err)
	}
	a.retry(t)
	a.drain(t)
	row := a.enrollment(t, a.member)
	if str(row, "status") != "exited" || str(row, "exitReason") != "unsubscribed" || a.azure.submissions() != 0 {
		t.Fatalf("a person who left is %v after %d submissions, want them exited and nothing sent", row, a.azure.submissions())
	}
}

func TestRetryEnrollmentsRefusesAnotherOrganizationsJourney(t *testing.T) {
	f := newDigestFixture(t)
	a := newAutomation(t, f, "journey-retry-mine", oneEmail)
	a.unconfirmable(t)
	stranger := a.suffix + "-stranger"
	outsider := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: stranger, Role: auth.RoleWriter}), &auth.TokenInfo{Subject: stranger})
	if _, err := a.worker.handleRetryEnrollments(outsider, map[string]any{"journeyId": a.journeyID}, 0); err == nil {
		t.Fatal("another organization retried this organization's automation")
	}
	if row := a.enrollment(t, a.member); str(row, "status") != "failed" {
		t.Fatalf("the refused retry changed the enrollment: %v", row)
	}
}

func TestSigningUpAgainAfterAFailedEnrollmentEnrollsAgain(t *testing.T) {
	f := newDigestFixture(t)
	a := newAutomation(t, f, "journey-resignup", confirmFirst)
	binding, err := a.worker.newsletterRow(a.owner, "newsletterForSite", "siteId", a.suffix+"-site")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := a.worker.handleConfigureNewsletter(a.owner, map[string]any{"siteId": str(binding, "siteId"), "audienceId": a.audience, "consentText": str(binding, "consentText"), "enabled": true, "expectedRevision": templateTime(binding["createdAt"]).Format(time.RFC3339Nano)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.args["consentRevision"] = decodeResult(t, saved)["revision"]
	if _, err = a.worker.handleSubscribe(a.owner, a.args, 0); err != nil {
		t.Fatal(err)
	}
	roster, err := a.store.Roster(memql.ContextWithFreshRead(a.owner), a.audience)
	if err != nil {
		t.Fatal(err)
	}
	var first Recipient
	for _, r := range roster {
		if r.Email == "subscriber@example.test" {
			first = r
		}
	}
	if first.ID == "" || first.SubscriptionStatus != "pending" {
		t.Fatalf("the signup is %+v", first)
	}
	// The confirmation cannot be confirmed as sent: the enrollment fails.
	broken := &recordingSender{fail: func(int) error { return errors.New("connection reset by peer") }}
	a.worker.resolve = func() email.Sender { return broken }
	a.drain(t)
	failed := a.enrollment(t, journeyEnrollmentID(a.journeyID, first.ID, first.JoinedAt))
	if str(failed, "status") != "failed" {
		t.Fatalf("the enrollment is %v, want it failed", failed)
	}
	a.worker.resolve = func() email.Sender { return a.azure }
	a.args["submissionId"] = "signing-up-again"
	if _, err = a.worker.handleSubscribe(a.owner, a.args, 0); err != nil {
		t.Fatal(err)
	}
	roster, _ = a.store.Roster(memql.ContextWithFreshRead(a.owner), a.audience)
	var again Recipient
	for _, r := range roster {
		if r.ID == first.ID {
			again = r
		}
	}
	if !again.JoinedAt.After(first.JoinedAt) || again.SubscriptionStatus != "pending" {
		t.Fatalf("signing up again left the membership %+v, want a renewed one after %s", again, first.JoinedAt)
	}
	a.later(time.Minute)
	a.drain(t)
	fresh := a.enrollment(t, journeyEnrollmentID(a.journeyID, again.ID, again.JoinedAt))
	if str(fresh, "status") != "waiting" || str(fresh, "stepId") != "confirm" {
		t.Fatalf("the new enrollment is %v, want it waiting for the confirmation", fresh)
	}
}

func TestALateConfirmationWhilePausedIsSentAfterResume(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-late-paused", confirmFirst)
	a.drain(t)
	if row := a.enrollment(t, a.member); str(row, "status") != "waiting" {
		t.Fatalf("the confirmation step is %v", row)
	}
	a.azure.mu.Lock()
	body := a.azure.sent[0].TextBody
	a.azure.mu.Unlock()
	var token string
	for _, word := range strings.Fields(body) {
		if strings.Contains(word, ConfirmPath+"?token=") {
			u, err := url.Parse(word)
			if err != nil {
				t.Fatal(err)
			}
			token = u.Query().Get("token")
		}
	}
	if token == "" {
		t.Fatal("the confirmation has no link")
	}
	a.later(8 * 24 * time.Hour)
	a.drain(t)
	if row := a.enrollment(t, a.member); str(row, "status") != "exited" || str(row, "exitReason") != "not confirmed" {
		t.Fatalf("the unconfirmed run is %v", row)
	}
	a.setJourneyState(t, "pause")
	page := NewEmailLinkHandler(deliveryDBEngine{a.engine}, a.worker.cfg, quietLogger(), gatePoolFor(t))
	page.now = func() time.Time { return a.now }
	if rr := pageRequest(page, "POST", ConfirmPath, token, ""); rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	row := a.enrollment(t, a.member)
	if str(row, "status") != "active" || str(row, "stepId") != "welcome" {
		t.Fatalf("a late confirmation while paused left %v, want the run reopened at its next step", row)
	}
	a.drain(t)
	if a.azure.count() != 1 {
		t.Fatalf("a paused automation sent: %d messages", a.azure.count())
	}
	a.setJourneyState(t, "activate")
	a.later(time.Minute)
	a.drain(t)
	if a.azure.count() != 2 {
		t.Fatalf("after resuming %d messages were sent, want the confirmation and the welcome", a.azure.count())
	}
}
