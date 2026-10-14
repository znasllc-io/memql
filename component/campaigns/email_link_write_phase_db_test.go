package campaigns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// The server may commit after a database client stops waiting. A public page
// must therefore retain its detached gate until each started write finishes.
func TestEmailLinkWritePhaseDB(t *testing.T) {
	f := newDigestFixture(t)
	for _, deadline := range []bool{false, true} {
		name := "VisitorLeavesDuringConsent"
		if deadline {
			name = "ConsentOutlastsRequestDeadline"
		}
		t.Run(name, func(t *testing.T) {
			n := f.newEmptyNewsletter(t, "page-consent-phase")
			rid, address := n.suffix+"-recipient", "phase@example.test"
			if err := n.store.AddRecipient(n.owner, rid, n.audience, address, "Reader", "signup", n.org, "pending", nil); err != nil {
				t.Fatal(err)
			}
			ctx, leave := context.WithCancel(context.Background())
			delay, during := 400*time.Millisecond, leave
			if deadline {
				leave()
				ctx, leave = context.WithTimeout(context.Background(), 2*time.Second)
				delay, during = 3*time.Second, nil
			}
			defer leave()
			recipientLocked := n.lockHeld(t, "campaign-journey:recipient:"+rid)
			mailboxLocked := n.lockHeld(t, "campaign-email-mailbox:"+organizationSuppressionID(n.org, EmailDigest(address)))
			engine := &straddlingWrite{Engine: deliveryDBEngine{n.engine}, prefix: "mutation recordConsentGrant", delay: delay, during: during,
				locked: func() bool { return recipientLocked() && mailboxLocked() }}
			h := NewEmailLinkHandler(engine, Config{UnsubscribeSecret: testSecret}, quietLogger(), gatePoolFor(t))
			p := LinkPayload{OwnerUserID: n.user, RecipientID: rid, AccountID: n.org, EmailDigest: EmailDigest(address), ExpiresAt: time.Now().Add(time.Hour).Unix()}
			token := emailPageToken(t, LinkConfirm, p)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, ConfirmPath+"?token="+url.QueryEscape(token), nil).WithContext(ctx))
			assertPageWriteCommittedUnderGate(t, engine)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("interrupted confirmation = %d, want retry", rr.Code)
			}
			if recipientLocked() || mailboxLocked() {
				t.Fatal("the completed request retained its gate")
			}
			if got := storedRecipient(t, n.store, n.owner, rid); str(got, "subscriptionStatus") != "pending" {
				t.Fatal("confirmation wrote after the request ended", got)
			}
			// Retrying the same token completes the interrupted operation without
			// writing another grant: the first one committed under the gate.
			h.store = NewStore(deliveryDBEngine{n.engine})
			if rr := pageRequest(h, http.MethodPost, ConfirmPath, token, ""); rr.Code != http.StatusOK {
				t.Fatalf("retry = %d %s", rr.Code, rr.Body.String())
			}
			var count int
			if err := n.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:campaigns:consentEvent' AND payload->>'recipientId' = ?`, "v1:campaigns:recipient:"+rid).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("retry produced %d consent versions, want one", count)
			}
		})
	}
	for _, status := range []string{"waiting", "exited"} {
		t.Run("VisitorLeavesDuringRecovery/"+status, func(t *testing.T) {
			n := f.newEmptyNewsletter(t, "page-recovery-phase")
			rid, jid, address := n.suffix+"-recipient", n.suffix+"-journey", "recovery@example.test"
			if err := n.store.AddRecipient(n.owner, rid, n.audience, address, "Reader", "signup", n.org, "subscribed", nil); err != nil {
				t.Fatal(err)
			}
			member, found, err := n.store.RecipientByID(n.owner, rid)
			if err != nil || !found {
				t.Fatalf("recipient: %v %v", found, err)
			}
			eid := journeyEnrollmentID(jid, rid, member.JoinedAt)
			write := func(name string, values map[string]any) {
				t.Helper()
				if err := n.worker.newsletterWrite(n.owner, name, values); err != nil {
					t.Fatal(err)
				}
			}
			at := time.Now().UTC()
			write("createJourney", map[string]any{"journeyId": jid, "accountId": n.org, "name": "Confirmation", "trigger": map[string]any{"kind": "joined", "audienceId": n.audience}, "steps": []map[string]any{{"id": "confirm", "kind": "confirm", "templateId": n.suffix + "-tpl", "senderIdentityId": n.suffix + "-sender", "timeoutDays": 7}, {"id": "next", "kind": "wait", "days": 1}}, "versionTime": at.Format(time.RFC3339Nano)})
			write("setJourneyStatus", map[string]any{"journeyId": jid, "status": "active", "activatedAt": at.Format(time.RFC3339Nano), "versionTime": at.Add(time.Second).Format(time.RFC3339Nano)})
			write("enrollJourneyRecipient", map[string]any{"enrollmentId": eid, "accountId": n.org, "journeyId": jid, "recipientId": rid, "emailDigest": EmailDigest(address), "stepId": "confirm", "enrolledAt": at.Format(time.RFC3339Nano), "triggeredAt": at.Format(time.RFC3339Nano)})
			write("advanceJourneyEnrollment", map[string]any{"enrollmentId": eid, "stepId": "confirm", "stepIndex": 0, "nextAt": at.Add(time.Hour).Format(time.RFC3339Nano), "status": status, "exitReason": "not confirmed", "versionTime": at.Add(time.Second).Format(time.RFC3339Nano)})
			ctx, leave := context.WithCancel(context.Background())
			defer leave()
			journeyLocked := n.lockHeld(t, "campaign-journey:journey:"+jid)
			recipientLocked := n.lockHeld(t, "campaign-journey:recipient:"+rid)
			enrollmentLocked := n.lockHeld(t, "campaign-journey:enrollment:"+eid)
			prefix := "mutation nudgeJourneyEnrollment"
			if status == "exited" {
				prefix = "mutation advanceJourneyEnrollment"
			}
			engine := &straddlingWrite{Engine: deliveryDBEngine{n.engine}, prefix: prefix, delay: 400 * time.Millisecond, during: leave,
				locked: func() bool { return journeyLocked() && recipientLocked() && enrollmentLocked() }}
			h := NewEmailLinkHandler(engine, Config{UnsubscribeSecret: testSecret}, quietLogger(), gatePoolFor(t))
			h.now = func() time.Time { return at }
			token := emailPageToken(t, LinkConfirm, LinkPayload{OwnerUserID: n.user, RecipientID: rid, AccountID: n.org, EmailDigest: EmailDigest(address), ExpiresAt: at.Add(time.Hour).Unix()})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, ConfirmPath+"?token="+url.QueryEscape(token), nil).WithContext(ctx))
			assertPageWriteCommittedUnderGate(t, engine)
			if rr.Code != http.StatusOK {
				t.Fatalf("completed recovery = %d %s", rr.Code, rr.Body.String())
			}
			if journeyLocked() || recipientLocked() || enrollmentLocked() {
				t.Fatal("recovery retained its gate after the write finished")
			}
			row, err := n.worker.newsletterRow(n.system, "journeyEnrollmentById", "enrollmentId", eid)
			if err != nil || !templateTime(row["nextAt"]).Equal(at) {
				t.Fatalf("recovery did not commit: %v %v", row, err)
			}
			if status == "exited" && (str(row, "status") != "active" || str(row, "stepId") != "next") {
				t.Fatalf("late confirmation did not resume after Confirm: %v", row)
			}
		})
	}
}

func assertPageWriteCommittedUnderGate(t *testing.T, engine *straddlingWrite) {
	t.Helper()
	engine.wg.Wait()
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.held) != 1 {
		t.Fatalf("the test reached %d delayed writes, want one", len(engine.held))
	}
	if !engine.held[0] {
		t.Error("the page released its gate before the database write committed")
	}
}
