package campaigns

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

func TestEmailLinkPagesDB(t *testing.T) {
	f := newDigestFixture(t)
	t.Run("ConfirmingTwiceGrantsOnce", func(t *testing.T) {
		n := f.newEmptyNewsletter(t, "confirm-pages")
		rid := n.suffix + "-recipient"
		address := "confirmed@example.test"
		if err := n.store.AddRecipient(n.owner, rid, n.audience, address, "Reader", "signup", n.org, "pending", nil); err != nil {
			t.Fatal(err)
		}
		replica, _ := digestDBEngine(t)
		cfg := Config{UnsubscribeSecret: testSecret, UnsubscribeBaseURL: "https://example.test"}
		handlers := []*EmailLinkHandler{NewEmailLinkHandler(deliveryDBEngine{n.engine}, cfg, quietLogger(), gatePoolFor(t)), NewEmailLinkHandler(deliveryDBEngine{replica}, cfg, quietLogger(), gatePoolFor(t))}
		p := LinkPayload{OwnerUserID: n.user, RecipientID: rid, AccountID: n.org, EmailDigest: EmailDigest(address), ExpiresAt: time.Now().Add(time.Hour).Unix()}
		token := emailPageToken(t, LinkConfirm, p)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, h := range handlers {
			wg.Add(1)
			go func(h *EmailLinkHandler) {
				defer wg.Done()
				<-start
				rr := pageRequest(h, "POST", ConfirmPath, token, "")
				if rr.Code != 200 {
					t.Errorf("confirm status=%d %s", rr.Code, rr.Body.String())
				}
			}(h)
		}
		close(start)
		wg.Wait()
		if got := storedRecipient(t, n.store, n.owner, rid); str(got, "subscriptionStatus") != "subscribed" || str(got, "confirmedAt") == "" {
			t.Fatal(got)
		}
		eventID := sha256Hex(fmt.Sprintf("email-confirm\x00%s\x00%d", rid, p.ExpiresAt))
		var count int
		if err := n.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:campaigns:consentEvent' AND id = ?`, "v1:campaigns:consentEvent:"+eventID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%d grant versions, want one", count)
		}
	})
	t.Run("SubscribeAgainLiftsThenAnotherOptOutHolds", func(t *testing.T) {
		n := f.newEmptyNewsletter(t, "preferences-pages")
		rid := n.suffix + "-recipient"
		address := "preferences@example.test"
		digest := EmailDigest(address)
		if err := n.store.AddRecipient(n.owner, rid, n.audience, address, "Reader", "manual", n.org, "subscribed", nil); err != nil {
			t.Fatal(err)
		}
		h := NewEmailLinkHandler(deliveryDBEngine{n.engine}, Config{UnsubscribeSecret: testSecret, UnsubscribeBaseURL: "https://example.test"}, quietLogger(), gatePoolFor(t))
		token := emailPageToken(t, LinkPreferences, LinkPayload{OwnerUserID: n.user, RecipientID: rid, AccountID: n.org, EmailDigest: digest})
		apply := func(action string) {
			t.Helper()
			rr := pageRequest(h, "POST", PreferencesPath, token, action)
			if rr.Code != 200 {
				t.Fatalf("%s: %d %s", action, rr.Code, rr.Body.String())
			}
		}
		blocked := func(want bool) {
			t.Helper()
			_, found, err := n.store.SuppressionForSend(n.system, n.org, digest)
			if err != nil || found != want {
				t.Fatalf("blocked=%v want=%v err=%v", found, want, err)
			}
		}
		apply("unsubscribe")
		blocked(true)
		apply("subscribe")
		blocked(false)
		row, err := n.store.organizationSuppression(n.system, n.org, digest)
		if err != nil || str(row, "liftedAt") == "" {
			t.Fatal(row, err)
		}
		// The ordinary sending path, under an independently booted replica, must now
		// observe the lifted row rather than a stale cached organization opt-out.
		replica, db := digestDBEngine(t)
		sender := &recordingSender{}
		worker := newGatedWorker(t, replica, db, sender)
		args := map[string]any{"requestId": n.suffix + "-send", "templateId": n.suffix + "-tpl", "recipientId": rid, "senderIdentityId": n.suffix + "-sender"}
		if _, err := worker.handleSendToRecipient(n.owner, args, 0); err != nil {
			t.Fatal(err)
		}
		if sender.count() != 1 {
			t.Fatal("resubscribed recipient was not sent")
		}
		apply("unsubscribe")
		blocked(true)
		row, err = n.store.organizationSuppression(n.system, n.org, digest)
		if err != nil || str(row, "liftedAt") != "" {
			t.Fatal("new opt-out retained the old lift", row, err)
		}
		args["requestId"] = n.suffix + "-after-opt-out"
		if _, err := worker.handleSendToRecipient(n.owner, args, 0); err != nil {
			t.Fatal(err)
		}
		if sender.count() != 1 {
			t.Fatal("later opt-out did not stop sending")
		}
	})
	t.Run("OneClickSerializesWithAnotherMembershipSubscribingAgain", func(t *testing.T) {
		n := f.newEmptyNewsletter(t, "choice-race")
		address := "race@example.test"
		digest := EmailDigest(address)
		first, second := n.suffix+"-first", n.suffix+"-second"
		for _, rid := range []string{first, second} {
			if err := n.store.AddRecipient(n.owner, rid, n.audience, address, "Reader", "manual", n.org, "subscribed", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := n.store.RecordOrganizationSuppression(n.system, n.org, digest, "unsubscribed", "example.test", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := n.store.SetRecipientSubscription(n.owner, first, "unsubscribed", time.Now()); err != nil {
			t.Fatal(err)
		}
		entered, resume := make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-resume:
			default:
				close(resume)
			}
		}()
		cfg := Config{UnsubscribeSecret: testSecret, UnsubscribeBaseURL: "https://example.test"}
		page := NewEmailLinkHandler(pausedEmailGrant{Engine: deliveryDBEngine{n.engine}, entered: entered, resume: resume}, cfg, quietLogger(), gatePoolFor(t))
		replica, _ := digestDBEngine(t)
		unsubscribe := NewUnsubscribeHandler(deliveryDBEngine{replica}, cfg, quietLogger(), gatePoolFor(t))
		attempted := make(chan struct{})
		gate := unsubscribe.gate
		unsubscribe.gate = func(ctx context.Context, recipient, account, digest string) (gateHold, error) {
			close(attempted)
			return gate(ctx, recipient, account, digest)
		}
		prefs := emailPageToken(t, LinkPreferences, LinkPayload{OwnerUserID: n.user, RecipientID: first, AccountID: n.org, EmailDigest: digest})
		optout, err := MintUnsubscribeToken(testSecret, UnsubscribePayload{OwnerUserID: n.user, RecipientID: second, AccountID: n.org, EmailDigest: digest})
		if err != nil {
			t.Fatal(err)
		}
		subscribed, unsubscribed := make(chan int, 1), make(chan int, 1)
		go func() { subscribed <- pageRequest(page, "POST", PreferencesPath, prefs, "subscribe").Code }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("preferences never reached grant")
		}
		go func() {
			rr := httptest.NewRecorder()
			unsubscribe.ServeHTTP(rr, httptest.NewRequest("POST", UnsubscribePath+"?token="+url.QueryEscape(optout), nil))
			unsubscribed <- rr.Code
		}()
		<-attempted
		select {
		case code := <-unsubscribed:
			t.Fatalf("opt-out overtook held mailbox decision: %d", code)
		case <-time.After(75 * time.Millisecond):
		}
		close(resume)
		if code := <-subscribed; code != 200 {
			t.Fatalf("subscribe=%d", code)
		}
		if code := <-unsubscribed; code != 200 {
			t.Fatalf("unsubscribe=%d", code)
		}
		if _, blocked, err := n.store.SuppressionForSend(n.system, n.org, digest); err != nil || !blocked {
			t.Fatalf("later one-click opt-out was lost: %v %v", blocked, err)
		}
	})

	t.Run("ViewReadsTheSentRevision", func(t *testing.T) {
		n := f.newEmptyNewsletter(t, "view-pages")
		rid := n.suffix + "-recipient"
		address := "view@example.test"
		if err := n.store.AddRecipient(n.owner, rid, n.audience, address, "Historical Reader", "manual", n.org, "subscribed", nil); err != nil {
			t.Fatal(err)
		}
		tid := n.suffix + "-tpl"
		before, found, err := n.store.TemplateByID(n.owner, tid)
		if err != nil || !found {
			t.Fatal(err)
		}
		revision := before.Revision
		at := memql.VersionTimeAfter(parseTime(revision), time.Now())
		if err := n.worker.newsletterWrite(n.owner, "updateTemplate", map[string]any{"templateId": tid, "name": "Changed", "subject": "Changed", "textBody": "Future content", "htmlBody": "<p>Future content</p>", "status": "ready", "versionTime": at.Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
		h := NewEmailLinkHandler(deliveryDBEngine{n.engine}, Config{UnsubscribeSecret: testSecret, UnsubscribeBaseURL: "https://example.test"}, quietLogger(), gatePoolFor(t))
		token := emailPageToken(t, LinkView, LinkPayload{OwnerUserID: n.user, RecipientID: rid, AccountID: n.org, EmailDigest: EmailDigest(address), TemplateID: tid, TemplateRevision: revision, SenderName: "Original Sender"})
		rr := pageRequest(h, "GET", ViewPath, token, "")
		if rr.Code != 200 || strings.Contains(rr.Body.String(), "Future content") || !strings.Contains(rr.Body.String(), "Welcome") || !strings.Contains(rr.Body.String(), "Original Sender") {
			t.Fatal(rr.Code, rr.Body.String())
		}
	})
}

type pausedEmailGrant struct {
	Engine
	entered chan struct{}
	resume  chan struct{}
}

func (e pausedEmailGrant) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "mutation recordConsentGrant") {
		close(e.entered)
		select {
		case <-e.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return e.Engine.Execute(ctx, q)
}
