package campaigns

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func organizationOptOut() UnsubscribePayload {
	return UnsubscribePayload{OwnerUserID: testOwner, RecipientID: "r-1", CampaignID: testCampaign,
		AccountID: "client-a", EmailDigest: EmailDigest("leaving@example.test")}
}

func TestUnsubscribeBindsOrganizationAndMailboxEvenAfterRecipientChanges(t *testing.T) {
	for _, changed := range []string{"unchanged", "organization", "email", "deleted"} {
		t.Run(changed, func(t *testing.T) {
			e := unsubscribeFixture()
			e.roster[0]["accountId"] = "client-a"
			switch changed {
			case "organization":
				e.roster[0]["accountId"] = "client-b"
			case "email":
				e.roster[0]["email"] = "replacement@example.test"
			case "deleted":
				e.roster = nil
			}
			token, err := MintUnsubscribeToken(testSecret, organizationOptOut())
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			handlerFor(e).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(token), nil))
			writes := e.mutations("recordOrganizationSuppression")
			if rr.Code != 200 || len(writes) != 1 || argOf(writes[0].query, "accountId") != "client-a" || argOf(writes[0].query, "emailDigest") != organizationOptOut().EmailDigest {
				t.Fatalf("lost signed opt-out scope: status=%d writes=%v", rr.Code, writes)
			}
			if len(e.mutations("recordSuppression")) != 0 {
				t.Fatal("client opt-out became a cluster-wide block")
			}
			converged := len(e.mutations("setRecipientSubscription"))
			if (changed == "unchanged" && converged != 1) || (changed != "unchanged" && converged != 0) {
				t.Fatalf("changed recipient convergence: %d", converged)
			}
		})
	}
}

func TestUnsubscribeScopeCannotBeSwappedInTransit(t *testing.T) {
	token, err := MintUnsubscribeToken(testSecret, organizationOptOut())
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{2, 3, 4, 5, 6} {
		parts := strings.Split(token, ".")
		parts[index] = base64.RawURLEncoding.EncodeToString([]byte("replacement"))
		if _, err := ParseUnsubscribeToken([]string{testSecret}, strings.Join(parts, ".")); err == nil {
			t.Fatalf("accepted changed field %d", index)
		}
	}
}

func TestAlreadyMailedU2LinkStillUnsubscribesItsOrganization(t *testing.T) {
	values := []string{"u2", UnsubscribeKeyID(testSecret)}
	for _, v := range []string{testOwner, "r-1", testCampaign} {
		values = append(values, base64.RawURLEncoding.EncodeToString([]byte(v)))
	}
	body := strings.Join(values, ".")
	token := body + "." + base64.RawURLEncoding.EncodeToString(unsubscribeTag(testSecret, body))
	e := unsubscribeFixture()
	e.roster[0]["accountId"] = "client-a"
	rr := httptest.NewRecorder()
	handlerFor(e).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(token), nil))
	if rr.Code != 200 || len(e.mutations("recordOrganizationSuppression")) != 1 || len(e.mutations("recordSuppression")) != 0 {
		t.Fatal("already-mailed link lost its opt-out")
	}
}

type failedOrganizationSuppressionEngine struct {
	*fakeEngine
	write bool
}

func (e failedOrganizationSuppressionEngine) Execute(ctx context.Context, q string) (any, error) {
	prefix := "query suppressionForOrganization"
	if e.write {
		prefix = "mutation recordOrganizationSuppression"
	}
	if strings.HasPrefix(q, prefix) {
		return nil, fmt.Errorf("storage unavailable")
	}
	return e.fakeEngine.Execute(ctx, q)
}

func TestFailedUnsubscribeDoesNotClaimSuccess(t *testing.T) {
	token, err := MintUnsubscribeToken(testSecret, organizationOptOut())
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	handlerFor(failedOrganizationSuppressionEngine{unsubscribeFixture(), true}).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(token), nil))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" || strings.Contains(rr.Body.String(), "<h1>Unsubscribed") {
		t.Fatalf("false confirmation: %d %s", rr.Code, rr.Body.String())
	}
}

func TestOrganizationOptOutBlocksAllItsAudiencesButNotAnotherClient(t *testing.T) {
	for _, account := range []string{"client-a", "client-b"} {
		for _, audience := range []string{"newsletter", "offers"} {
			e := organizationSendEngine()
			e.template["accountId"], e.roster[0]["accountId"], e.emailRules[testEmailRule]["accountId"], e.senderIdentities["sender"]["accountId"] = account, account, account, account
			e.roster[0]["audienceId"] = audience
			digest := EmailDigest("person@example.test")
			e.suppression = map[string]map[string]any{organizationSuppressionID("client-a", digest): {"accountId": "client-a", "reason": "unsubscribed"}}
			sender := &recordingSender{}
			w := newTestWorker(t, e, sender)
			args := sendToRecipientArgs()
			args["senderIdentityId"] = "sender"
			if _, err := w.handleSendToRecipient(importCtx(), args, 0); err != nil {
				t.Fatal(err)
			}
			if (account == "client-a" && sender.count() != 0) || (account == "client-b" && sender.count() != 1) {
				t.Fatalf("wrong suppression scope: %s/%s sent=%d", account, audience, sender.count())
			}
		}
	}
}

func TestRuleSendSignsItsOrganizationWithoutAFalseCampaignRelationship(t *testing.T) {
	e := organizationSendEngine()
	sender := &recordingSender{}
	w := newTestWorker(t, e, sender)
	args := sendToRecipientArgs()
	args["senderIdentityId"] = "sender"
	if _, err := w.handleSendToRecipient(importCtx(), args, 0); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(strings.Trim(sender.sent[0].Headers[headerListUnsubscribe], "<>"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := ParseUnsubscribeToken(w.cfg.UnsubscribeKeys(), link.Query().Get("token"))
	if err != nil || payload.AccountID != "client-a" || payload.EmailDigest != EmailDigest("person@example.test") || payload.CampaignID != "" {
		t.Fatalf("wrong unsubscribe identity: %+v %v", payload, err)
	}
}

func TestOrganizationSuppressionLookupFailurePreventsMail(t *testing.T) {
	e := failedOrganizationSuppressionEngine{organizationSendEngine(), false}
	sender := &recordingSender{}
	w := newTestWorker(t, e, sender)
	args := sendToRecipientArgs()
	args["senderIdentityId"] = "sender"
	if _, err := w.handleSendToRecipient(importCtx(), args, 0); err == nil || sender.count() != 0 {
		t.Fatal("unread organization suppression treated as empty")
	}
	stop, err := w.processRecipient(context.Background(), context.Background(), importCtx(), &SendJob{}, Campaign{AccountID: "client-a"}, Template{AccountID: "client-a"}, resolvedIdentity{}, batchItem{recipient: Recipient{AccountID: "client-a", Email: "person@example.test"}})
	if !stop || err != nil || sender.count() != 0 {
		t.Fatal("queued worker ignored an unread organization suppression")
	}
}
