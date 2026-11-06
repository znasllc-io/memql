package email

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type acsRoundTrip func(*http.Request) (*http.Response, error)

func (f acsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func acsFixture(t *testing.T) *ACSSender {
	t.Helper()
	s, err := NewACSSender(ACSConfig{AccountID: "client", Endpoint: "https://client.communication.azure.com", AccessKey: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), Senders: map[string]string{"news@client.test": "Client"}, Default: "news@client.test"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	s.operations = &memoryACSOperations{}
	return s
}

func TestACSSendsTheClientsVerifiedIdentityAndSignedPayload(t *testing.T) {
	s := acsFixture(t)
	s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.URL.String() != "https://client.communication.azure.com/emails:send?api-version=2025-09-01" {
			t.Fatal(r.URL)
		}
		var payload map[string]any
		if err = json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["senderAddress"] != "news@client.test" || payload["userEngagementTrackingDisabled"] != true {
			t.Fatal("sender or tracking policy lost")
		}
		if len(payload["replyTo"].([]any)) != 1 || payload["headers"].(map[string]any)["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
			t.Fatal("reply or unsubscribe headers lost")
		}
		digest := sha256.Sum256(body)
		hash := base64.StdEncoding.EncodeToString(digest[:])
		date := "Thu, 01 Oct 2026 12:00:00 GMT"
		mac := hmac.New(sha256.New, []byte(strings.Repeat("k", 32)))
		_, _ = io.WriteString(mac, "POST\n/emails:send?api-version=2025-09-01\n"+date+";client.communication.azure.com;"+hash)
		want := "HMAC-SHA256 SignedHeaders=x-ms-date;host;x-ms-content-sha256&Signature=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if r.Header.Get("Authorization") != want || r.Header.Get("x-ms-date") != date || r.Header.Get("x-ms-content-sha256") != hash {
			t.Fatal("request signature differs from ACS wire contract")
		}
		return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":"Running"}`))}, nil
	})
	err := s.Send(context.Background(), Message{To: "Reader <reader@example.test>", Subject: "Welcome", TextBody: "Hello", HTMLBody: "<p>Hello</p>", Headers: map[string]string{"Reply-To": "Help <help@client.test>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"}}, SendAs{AccountID: "client", Address: "news@client.test", FromName: "Client"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestACSRefusesForeignOrganizationsAndUnverifiedSendersBeforeNetwork(t *testing.T) {
	s := acsFixture(t)
	s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("refused identity reached network")
		return nil, nil
	})
	for _, as := range []SendAs{{AccountID: "other", Address: "news@client.test"}, {AccountID: "client", Address: "owner@operator.test"}, {AccountID: "client", FromName: "Other brand"}, {AccountID: "client", Address: "news@client.test\r\nBcc:secret@example.test"}, {}} {
		if err := s.Send(context.Background(), Message{To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}, as); !IsPermanent(err) {
			t.Fatalf("identity was not refused: %v", err)
		}
	}
}

// A refusal Azure answered with is a definite non-acceptance: the message may be
// submitted again. Only an answer that says nothing about acceptance -- none came
// back, a redirect, or 409 (Azure already has an operation under this
// Operation-Id) -- is ambiguous, and an ambiguous POST is never retried blindly.
func TestACSDoesNotRetryAmbiguousAcceptanceOrEchoProviderSecrets(t *testing.T) {
	for _, tc := range []struct {
		code                           int
		notAccepted, permanent, paused bool
	}{
		{code: 400, notAccepted: true, permanent: true},
		{code: 401, notAccepted: true, permanent: true},
		{code: 403, notAccepted: true, permanent: true},
		{code: 408, notAccepted: true},
		{code: 429, notAccepted: true, paused: true},
		{code: 500, notAccepted: true},
		{code: 503, notAccepted: true, paused: true},
		{code: 302, permanent: true},
		{code: 409, permanent: true},
	} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			s := acsFixture(t)
			s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.code, Header: http.Header{"Retry-After": []string{"15"}}, Body: io.NopCloser(strings.NewReader("private-body-and-credential"))}, nil
			})
			err := s.Send(context.Background(), Message{To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}, SendAs{AccountID: "client"})
			if err == nil || strings.Contains(err.Error(), "private-body") {
				t.Fatal("unsafe provider failure", err)
			}
			if IsNotAccepted(err) != tc.notAccepted || IsPermanent(err) != tc.permanent {
				t.Fatalf("not accepted %v, permanent %v, want %v and %v: %v", IsNotAccepted(err), IsPermanent(err), tc.notAccepted, tc.permanent, err)
			}
			if delay, throttle := IsThrottled(err); throttle != tc.paused || tc.paused && delay != 15*time.Second {
				t.Fatal("lost throttle", err)
			}
		})
	}
	s := acsFixture(t)
	s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("network secret") })
	err := s.Send(context.Background(), Message{To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}, SendAs{AccountID: "client"})
	if !IsPermanent(err) || IsNotAccepted(err) || strings.Contains(err.Error(), "network secret") {
		t.Fatal("uncertain POST must not blindly retry", err)
	}
}

// TestASendWithoutACampaignHasNoCampaignHeader: a single send (a welcome, an
// automation step) belongs to no campaign. An empty header says nothing, and is
// not sent.
func TestASendWithoutACampaignHasNoCampaignHeader(t *testing.T) {
	s := acsFixture(t)
	var headers map[string]any
	s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		headers, _ = payload["headers"].(map[string]any)
		return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	msg := Message{To: "reader@example.test", Subject: "Hello", TextBody: "Hello", Headers: map[string]string{
		"X-Campaign-Id": "", "List-Unsubscribe": "<https://example.test/u>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}}
	if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); err != nil {
		t.Fatal(err)
	}
	if _, present := headers["X-Campaign-Id"]; present {
		t.Fatalf("an empty campaign header was sent: %v", headers)
	}
	if headers["List-Unsubscribe"] != "<https://example.test/u>" || headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("the headers that say something were lost: %v", headers)
	}
}

// TestARefusedSendIsSubmittedAgainUnderItsOperationId: after Azure refused a
// delivery, its next submission goes -- rendered afresh, as a retry is (its links
// are minted again) -- under the receipt's Operation-Id, so a request Azure did
// accept despite answering with an error cannot be accepted a second time.
func TestARefusedSendIsSubmittedAgainUnderItsOperationId(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		retryAfter string
		saved      string
	}{
		{"a refusal", 400, "", "rejected"},
		{"a throttle", 429, "15", "throttled"},
		{"a server error", 500, "", "rejected"},
		{"an unavailable service", 503, "", "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := acsFixture(t)
			ops := s.operations.(*memoryACSOperations)
			var posted []string
			s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				posted = append(posted, r.Header.Get("Operation-Id"))
				if len(posted) == 1 {
					return &http.Response{StatusCode: tc.code, Header: http.Header{"Retry-After": []string{tc.retryAfter}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				}
				return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			msg := Message{IntentID: "campaign-single:refused", To: "reader@example.test", Subject: "Hello", TextBody: "Hello, token one"}
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); !IsNotAccepted(err) {
				t.Fatalf("a refusal is not a definite non-acceptance: %v", err)
			}
			if got := ops.rows[s.operationKey(msg.IntentID)].Status; got != tc.saved {
				t.Fatalf("the receipt says %q, want %q", got, tc.saved)
			}
			later := s.now().Add(time.Minute)
			s.now = func() time.Time { return later }
			msg.TextBody = "Hello, token two"
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); err != nil {
				t.Fatalf("the second submission: %v", err)
			}
			if len(posted) != 2 || posted[0] == "" || posted[0] != posted[1] {
				t.Fatalf("submissions went under Operation-Ids %q, want two under one", posted)
			}
			if got := ops.rows[s.operationKey(msg.IntentID)].Status; got != "accepted" {
				t.Fatalf("the receipt says %q after Azure accepted it, want accepted", got)
			}
		})
	}
}

// TestARevisedMessageGoesUnderAFreshOperationId: after Azure did not accept a
// delivery, its caller changed what the delivery sends (another sender, another
// template) and says so (Message.Revised). Only a refusal that proves Azure took
// nothing under the earlier Operation-Id -- a client refusal (a 4xx other than
// 408, 409 and 429) or a throttle (429), as the receipt recorded it -- gives the
// revised message a fresh id. After a server error, a timeout, a refusal the
// receipt recorded no class for, or Azure saying it had NO RECORD of the earlier
// submission (notfound), the id is kept: a first POST Azure took after all is
// matched by it, and the person gets at most the earlier message, once. The same
// message sent again keeps its id (TestARefusedSendIsSubmittedAgainUnderItsOperationId);
// a revised message whose earlier submission Azure may have accepted is not
// submitted at all.
func TestARevisedMessageGoesUnderAFreshOperationId(t *testing.T) {
	// Each case: Azure answers the first submission with code (Retry-After as
	// given), the receipt records what kind of refusal it was, and the revised
	// message goes under a fresh id only if that proves Azure took nothing under the
	// old one: a client refusal or a throttle. A server error can follow a request
	// Azure took, and a timeout says nothing either way: the id is kept, so a
	// request Azure did take is matched by it and not taken twice.
	for _, tc := range []struct {
		name, retryAfter, class string
		code                    int
		fresh                   bool
	}{
		{"after a client refusal (400)", "", "client", 400, true},
		{"after a throttle (429)", "15", "throttle", 429, true},
		{"not after a server error (503)", "", "server", 503, false},
		{"not after a server error that names a wait (503)", "15", "server", 503, false},
		{"not after a server error (504)", "", "server", 504, false},
		{"not after a timeout (408)", "", "", 408, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := acsFixture(t)
			ops := s.operations.(*memoryACSOperations)
			var posted []string
			s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				posted = append(posted, r.Header.Get("Operation-Id"))
				if len(posted) == 1 {
					return &http.Response{StatusCode: tc.code, Header: http.Header{"Retry-After": []string{tc.retryAfter}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				}
				return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			msg := Message{IntentID: "campaign-single:revised", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); !IsNotAccepted(err) {
				t.Fatalf("a %d is not a definite non-acceptance: %v", tc.code, err)
			}
			if got := ops.rows[s.operationKey(msg.IntentID)].Refusal; got != tc.class {
				t.Fatalf("the receipt records a %d as a %q refusal, want %q", tc.code, got, tc.class)
			}
			later := s.now().Add(time.Hour) // past any wait Azure named
			s.now = func() time.Time { return later }
			msg.Subject, msg.TextBody, msg.Revised = "Welcome", "Welcome aboard", true
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); err != nil {
				t.Fatalf("the revised message: %v", err)
			}
			if len(posted) != 2 || (posted[0] != posted[1]) != tc.fresh || !azureUUID.MatchString(posted[1]) {
				t.Fatalf("after a %d the earlier and the revised message went under Operation-Ids %q, want a fresh id: %v", tc.code, posted, tc.fresh)
			}
			if op := ops.rows[s.operationKey(msg.IntentID)]; op.ID != posted[1] || op.Status != "accepted" || op.Refusal != "" {
				t.Fatalf("the receipt is %+v, want the revised message's operation, accepted, with no refusal", op)
			}
		})
	}
	// A receipt written before refusals had a class says nothing that proves Azure
	// took nothing: the id is kept.
	for _, status := range []string{"rejected", "throttled"} {
		t.Run("not after a refusal of no recorded class ("+status+")", func(t *testing.T) {
			s := acsFixture(t)
			ops := s.operations.(*memoryACSOperations)
			msg := Message{IntentID: "campaign-single:revised-legacy-" + status, To: "reader@example.test", Subject: "Welcome", TextBody: "Welcome aboard", Revised: true}
			op := "12345678-1234-1234-1234-123456789012"
			ops.rows = map[string]acsOperation{s.operationKey(msg.IntentID): {ID: op, AccountID: "client", Endpoint: s.cfg.Endpoint, Digest: "the earlier message", Intent: msg.IntentID, Status: status}}
			var posted []string
			s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				posted = append(posted, r.Header.Get("Operation-Id"))
				return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); err != nil || len(posted) != 1 || posted[0] != op {
				t.Fatalf("a revised message after a %s receipt with no class went under %q (%v), want the receipt's id %q", status, posted, err, op)
			}
		})
	}
	// Azure's poller found no operation under the receipt's id: the earlier POST
	// was lost, and may yet be taken.
	missing := func(t *testing.T) (*ACSSender, *memoryACSOperations, Message, acsOperation) {
		t.Helper()
		s := acsFixture(t)
		s.cfg.Senders["hello@client.test"] = "Client"
		ops := s.operations.(*memoryACSOperations)
		msg := Message{IntentID: "campaign-single:revised-missing", DeliveryID: "delivery-1", To: "reader@example.test", Subject: "Welcome", TextBody: "Welcome aboard", Revised: true}
		earlier := acsOperation{ID: "12345678-1234-1234-1234-123456789012", AccountID: "client", Endpoint: s.cfg.Endpoint, Digest: "the earlier message", Intent: msg.IntentID,
			DeliveryID: "delivery-1", RecipientDigest: azureMailboxDigest("reader@example.test"), SenderDigest: azureMailboxDigest("news@client.test"),
			Status: "notfound", SubmittedAt: s.now().Add(-time.Hour)}
		ops.rows = map[string]acsOperation{s.operationKey(msg.IntentID): earlier}
		return s, ops, msg, earlier
	}
	t.Run("not after Azure had no record of the earlier one: same id", func(t *testing.T) {
		s, ops, msg, earlier := missing(t)
		var posted []string
		s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
			posted = append(posted, r.Header.Get("Operation-Id"))
			return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})
		if err := s.Send(context.Background(), msg, SendAs{AccountID: "client", Address: "hello@client.test"}); err != nil {
			t.Fatalf("the revised message: %v", err)
		}
		if len(posted) != 1 || posted[0] != earlier.ID {
			t.Fatalf("after Azure had no record of the earlier submission the revised one went under %q, want the receipt's id %q", posted, earlier.ID)
		}
		if op := ops.rows[s.operationKey(msg.IntentID)]; op.ID != earlier.ID || op.Status != "accepted" || op.SenderDigest != azureMailboxDigest("hello@client.test") {
			t.Fatalf("the receipt is %+v, want the revised message accepted under the same id", op)
		}
	})
	t.Run("not after Azure had no record of the earlier one, which it then took late", func(t *testing.T) {
		s, ops, msg, earlier := missing(t)
		posts := 0
		s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
			posts++
			if got := r.Header.Get("Operation-Id"); got != earlier.ID {
				t.Errorf("the revised message went under %q, want the receipt's id %q", got, earlier.ID)
			}
			return &http.Response{StatusCode: 409, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})
		err := s.Send(context.Background(), msg, SendAs{AccountID: "client", Address: "hello@client.test"})
		if err == nil || IsNotAccepted(err) || !HeldEarlier(err) || posts != 1 {
			t.Fatalf("Azure holding the earlier submission under the id: %v after %d posts, want this message not taken and the earlier one being confirmed", err, posts)
		}
		// The receipt still describes the submission Azure holds, so its delivery
		// reports (matched on the id and the sender) and its acceptance time are
		// the earlier message's, and nothing is said about the revised one. The
		// poller's window starts at this POST.
		op := ops.rows[s.operationKey(msg.IntentID)]
		if op.ID != earlier.ID || op.Status != "unknown" || op.Digest != earlier.Digest || op.SenderDigest != earlier.SenderDigest || op.DeliveryID != earlier.DeliveryID || !op.HeldSubmittedAt.Equal(earlier.SubmittedAt) || !op.SubmittedAt.Equal(s.now()) {
			t.Fatalf("the receipt is %+v, want the earlier submission's, being confirmed (%+v)", op, earlier)
		}
		if outcome, _ := s.DeliveryOutcome(context.Background(), msg, SendAs{AccountID: "client"}); outcome != OutcomeUnknown {
			t.Fatalf("while Azure's outcome is being confirmed the delivery reads %v, want unknown", outcome)
		}
		// Once the poller finds the earlier operation succeeded, the delivery was
		// taken, once, when the earlier message was submitted.
		op.Status = "succeeded"
		ops.rows[s.operationKey(msg.IntentID)] = op
		if outcome, _ := s.DeliveryOutcome(context.Background(), msg, SendAs{AccountID: "client"}); outcome != OutcomeAccepted {
			t.Fatalf("after the poller's answer the delivery reads %v, want accepted", outcome)
		}
		if at, err := s.AcceptedAt(context.Background(), msg, SendAs{AccountID: "client"}); err != nil || !at.Equal(earlier.SubmittedAt) {
			t.Fatalf("the delivery was taken at %s (%v), want when the earlier message was submitted", at, err)
		}
	})
	for _, status := range []string{"submitting", "unknown", "accepted", "succeeded"} {
		t.Run("not while Azure may have the earlier one ("+status+")", func(t *testing.T) {
			s := acsFixture(t)
			ops := s.operations.(*memoryACSOperations)
			msg := Message{IntentID: "campaign-single:revised-" + status, To: "reader@example.test", Subject: "Welcome", TextBody: "Welcome aboard", Revised: true}
			op := "12345678-1234-1234-1234-123456789012"
			ops.rows = map[string]acsOperation{s.operationKey(msg.IntentID): {ID: op, AccountID: "client", Endpoint: s.cfg.Endpoint, Digest: "the earlier message", Intent: msg.IntentID, Status: status}}
			posts := 0
			s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
				posts++
				return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); err == nil || IsNotAccepted(err) || posts != 0 {
				t.Fatalf("a revised message whose earlier submission Azure may have: %v after %d posts, want it held back unsent", err, posts)
			}
			if got := ops.rows[s.operationKey(msg.IntentID)]; got.ID != op || got.Status != status {
				t.Fatalf("the receipt became %+v", got)
			}
		})
	}
}

// TestAnAcceptedSendSaysWhenAzureTookIt: the receipt of a send Azure accepted says
// when it was submitted, the time Azure's delivery reports carry as its acceptance.
// Nothing else does: a send not accepted, another organization's, or one with no
// receipt answers the zero time.
func TestAnAcceptedSendSaysWhenAzureTookIt(t *testing.T) {
	submitted := time.Date(2026, 10, 1, 11, 59, 0, 0, time.UTC)
	for _, tc := range []struct {
		status  string
		account string
		want    time.Time
	}{
		{"accepted", "client", submitted},
		{"succeeded", "client", submitted},
		{"running", "client", submitted},
		{"unknown", "client", time.Time{}},
		{"rejected", "client", time.Time{}},
		{"accepted", "another", time.Time{}},
	} {
		t.Run(tc.status+" for "+tc.account, func(t *testing.T) {
			s := acsFixture(t)
			msg := Message{IntentID: "campaign-single:taken"}
			s.operations.(*memoryACSOperations).rows = map[string]acsOperation{s.operationKey(msg.IntentID): {ID: "12345678-1234-1234-1234-123456789012", AccountID: "client", Status: tc.status, SubmittedAt: submitted}}
			if got, err := s.AcceptedAt(context.Background(), msg, SendAs{AccountID: tc.account}); err != nil || !got.Equal(tc.want) {
				t.Fatalf("AcceptedAt = %s (%v), want %s", got, err, tc.want)
			}
		})
	}
	if got, err := acsFixture(t).AcceptedAt(context.Background(), Message{IntentID: "campaign-single:none"}, SendAs{AccountID: "client"}); err != nil || !got.IsZero() {
		t.Fatalf("a send with no receipt says it was taken at %s (%v)", got, err)
	}
}

// TestAnOperationIdIsRenewedOnlyWhileEveryPOSTUnderItTookNothing: the receipt's
// latest refusal is not enough. A revised message gets a fresh Operation-Id only
// while EVERY POST made under the receipt's current id provably took nothing --
// each was a client refusal or a throttle. A server error, a lost answer, Azure
// having no record (notfound), or a receipt written before the proof existed
// leaves a POST under the id that Azure may have taken, and a later refusal under
// the same id does not undo that: the id is kept, so that submission is matched by
// it and the person gets at most one message. (The review's chains.)
func TestAnOperationIdIsRenewedOnlyWhileEveryPOSTUnderItTookNothing(t *testing.T) {
	answer := func(code int) *http.Response {
		header := http.Header{}
		if code == http.StatusTooManyRequests {
			header.Set("Retry-After", "15")
		}
		return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader("{}"))}
	}
	for _, tc := range []struct {
		name string
		// seed is the status of a receipt already there, written before refusals
		// had a class or a proof; "" for none, the chain's first POST making it.
		seed  string
		chain []int // Azure's answers to the unchanged message, in order
		fresh bool  // whether the revised message then gets a fresh id
	}{
		{"504 then 429", "", []int{504, 429}, false},
		{"504 then 400", "", []int{504, 400}, false},
		{"notfound then 429", "notfound", []int{429}, false},
		{"legacy rejected then 429", "rejected", []int{429}, false},
		{"legacy rejected then 400", "rejected", []int{400}, false},
		{"400", "", []int{400}, true},
		{"400 then 429", "", []int{400, 429}, true},
		{"429", "", []int{429}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := acsFixture(t)
			ops := s.operations.(*memoryACSOperations)
			msg := Message{IntentID: "campaign-single:chain", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
			first := ""
			if tc.seed != "" {
				first = "12345678-1234-1234-1234-123456789012"
				ops.rows = map[string]acsOperation{s.operationKey(msg.IntentID): {ID: first, AccountID: "client", Endpoint: s.cfg.Endpoint, Digest: "the first message", Intent: msg.IntentID, Status: tc.seed}}
			}
			answers := append(append([]int(nil), tc.chain...), http.StatusAccepted)
			var posted []string
			s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				posted = append(posted, r.Header.Get("Operation-Id"))
				code := answers[0]
				answers = answers[1:]
				return answer(code), nil
			})
			clock := s.now()
			s.now = func() time.Time { return clock }
			for range tc.chain {
				if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); !IsNotAccepted(err) {
					t.Fatalf("an answer of the chain is not a definite non-acceptance: %v", err)
				}
				clock = clock.Add(time.Hour) // past any wait Azure named
			}
			if first == "" {
				first = posted[0]
			}
			for _, id := range posted {
				if id != first {
					t.Fatalf("the unchanged message moved off its Operation-Id: %q", posted)
				}
			}
			msg.Subject, msg.TextBody, msg.Revised = "Welcome", "Welcome aboard", true
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); err != nil {
				t.Fatalf("the revised message: %v", err)
			}
			if revised := posted[len(posted)-1]; (revised != first) != tc.fresh {
				t.Fatalf("after %s the revised message went under %q (the earlier id %q), want a fresh id: %v", tc.name, revised, first, tc.fresh)
			}
		})
	}
}

// TestAnAmbiguousAnswerIsNeverSubmittedAgain: no answer, a redirect and a 409 say
// nothing about whether Azure accepted the message. A second Send does not POST;
// it waits for Azure's own record of the operation.
func TestAnAmbiguousAnswerIsNeverSubmittedAgain(t *testing.T) {
	for name, answer := range map[string]func() (*http.Response, error){
		"no answer": func() (*http.Response, error) { return nil, errors.New("connection reset") },
		"a redirect": func() (*http.Response, error) {
			return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
		"a conflict": func() (*http.Response, error) {
			return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(""))}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := acsFixture(t)
			posts := 0
			s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) { posts++; return answer() })
			msg := Message{IntentID: "campaign-single:ambiguous", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
			for range 2 {
				if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); err == nil || IsNotAccepted(err) {
					t.Fatalf("an ambiguous answer reads as %v", err)
				}
			}
			if posts != 1 {
				t.Fatalf("an ambiguous submission was POSTed %d times", posts)
			}
			if outcome, err := s.DeliveryOutcome(context.Background(), msg, SendAs{AccountID: "client"}); err != nil || outcome != OutcomeUnknown {
				t.Fatalf("the outcome of an ambiguous submission is %v (%v), want unknown", outcome, err)
			}
		})
	}
}

// TestAzureHavingNoRecordOfASendLetsItGoAgain: the poller asks Azure for the
// operation of a submission whose answer was lost. No record means Azure never
// took it, and the next submission goes under the same Operation-Id. An operation
// Azure took and could not finish goes again under a new one: its own id names
// that failed operation.
func TestAzureHavingNoRecordOfASendLetsItGoAgain(t *testing.T) {
	s := acsFixture(t)
	op := "12345678-1234-1234-1234-123456789012"
	s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"private"}}`))}, nil
	})
	if status, _, err := s.getOperation(context.Background(), op); err != nil || status != "notfound" {
		t.Fatalf("an operation Azure has no record of reads %q (%v), want notfound", status, err)
	}
	for _, tc := range []struct {
		status   string
		reuseID  bool
		outcome  Outcome
		resubmit bool
	}{
		{"notfound", true, OutcomeNotAccepted, true},
		{"failed", false, OutcomeNotAccepted, true},
		{"canceled", false, OutcomeNotAccepted, true},
		{"accepted", true, OutcomeAccepted, false},
		{"succeeded", true, OutcomeAccepted, false},
		{"unknown", true, OutcomeUnknown, false},
		{"unconfirmed", true, OutcomeUnknown, false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			s := acsFixture(t)
			ops := s.operations.(*memoryACSOperations)
			msg := Message{IntentID: "campaign-single:" + tc.status, To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
			body := []byte(`{"stored":"elsewhere"}`)
			key := s.operationKey(msg.IntentID)
			ops.rows = map[string]acsOperation{key: {ID: op, AccountID: "client", Endpoint: s.cfg.Endpoint, Digest: fmt.Sprintf("%x", sha256.Sum256(body)), Intent: msg.IntentID, Status: tc.status}}
			if outcome, err := s.DeliveryOutcome(context.Background(), msg, SendAs{AccountID: "client"}); err != nil || outcome != tc.outcome {
				t.Fatalf("the outcome of a %s operation is %v (%v), want %v", tc.status, outcome, err, tc.outcome)
			}
			if outcome, _ := s.DeliveryOutcome(context.Background(), msg, SendAs{AccountID: "another"}); outcome != OutcomeUnknown {
				t.Fatalf("another organization's connection read this delivery's receipt: %v", outcome)
			}
			var posted []string
			s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				posted = append(posted, r.Header.Get("Operation-Id"))
				return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			err := s.Send(context.Background(), msg, SendAs{AccountID: "client"})
			if !tc.resubmit {
				if len(posted) != 0 {
					t.Fatalf("a %s delivery was submitted again", tc.status)
				}
				return
			}
			if err != nil || len(posted) != 1 {
				t.Fatalf("a %s delivery was not submitted again: %v, %d posts", tc.status, err, len(posted))
			}
			if (posted[0] == op) != tc.reuseID || !azureUUID.MatchString(posted[0]) {
				t.Fatalf("a %s delivery went under Operation-Id %q (the receipt's is %q)", tc.status, posted[0], op)
			}
		})
	}
}

func TestACSRejectsCredentialsDirectedOutsideAzure(t *testing.T) {
	for _, endpoint := range []string{"http://client.communication.azure.com", "https://communication.azure.com.evil.test", "https://client.communication.azure.com@evil.test", "https://client.communication.azure.com/path", "https://client.communication.azure.com?target=other", "https://client.communication.azure.com:443"} {
		cfg := acsFixture(t).cfg
		cfg.Endpoint = endpoint
		if _, err := NewACSSender(cfg); err == nil {
			t.Fatal("invalid endpoint accepted", endpoint)
		}
	}
}

// TestADeliveryAzureAskedToWaitIsRefusedBeforeItIsAttempted: after a throttle,
// Azure's receipt keeps the delivery waiting until the time Azure named. A caller
// that reserves the delivery before recording an attempt is refused there, with
// the wait, so it records no attempt and spends none; once the wait is over the
// delivery is reserved and submitted again.
func TestADeliveryAzureAskedToWaitIsRefusedBeforeItIsAttempted(t *testing.T) {
	s := acsFixture(t)
	ops := s.operations.(*memoryACSOperations)
	posts := 0
	s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
		posts++
		if posts == 1 {
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"600"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}
		return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	msg := Message{IntentID: "campaign-single:throttled", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
	if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); !IsNotAccepted(err) {
		t.Fatalf("a throttle is not a definite non-acceptance: %v", err)
	}
	start := s.now()
	later := start.Add(2 * time.Minute)
	s.now = func() time.Time { return later }
	_, _, err := s.Reserve(context.Background(), msg, SendAs{AccountID: "client"})
	if wait, throttled := IsThrottled(err); !throttled || !IsNotAccepted(err) || wait != 8*time.Minute {
		t.Fatalf("a delivery inside Azure's wait was reserved: %v (throttled %v, wait %s)", err, throttled, wait)
	}
	if !ops.gate.TryLock() {
		t.Fatal("the refusal kept the delivery's lock")
	}
	ops.gate.Unlock()
	later = start.Add(11 * time.Minute)
	reserved, release, err := s.Reserve(context.Background(), msg, SendAs{AccountID: "client"})
	if err != nil {
		t.Fatalf("a delivery whose wait is over was refused: %v", err)
	}
	err = s.Send(reserved, msg, SendAs{AccountID: "client"})
	release()
	if err != nil || posts != 2 {
		t.Fatalf("the delivery after its wait: %v, %d submissions", err, posts)
	}
}

// TestASenderWithNoRecordOfItsSubmissionsSaysSo: a transport that keeps no record
// of what it submitted (Graph, SMTP, the log) can never say what became of a
// message, and says that, rather than "not yet".
func TestASenderWithNoRecordOfItsSubmissionsSaysSo(t *testing.T) {
	msg := Message{IntentID: "campaign-single:unrecorded", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
	lazy := NewLazySender(NewLogSender(nil), nil, nil, nil)
	if outcome, err := lazy.DeliveryOutcome(context.Background(), msg, SendAs{}); err != nil || outcome != OutcomeUnrecorded {
		t.Fatalf("a transport with no record answered %v (%v), want unrecorded", outcome, err)
	}
	s := acsFixture(t)
	s.operations = nil
	if outcome, err := s.DeliveryOutcome(context.Background(), msg, SendAs{AccountID: "client"}); err != nil || outcome != OutcomeUnrecorded {
		t.Fatalf("an Azure sender with no receipt storage answered %v (%v), want unrecorded", outcome, err)
	}
}

// TestAnOrganizationsContactIsItsOwnConnections: the pages behind email links name
// whom to write to from the organization's own email connection -- its reply
// address, else its default sender -- resolved as the organization's sender is.
// Another organization's connection names nobody for it, and neither does the
// node's own transport: an organization without a connection has no contact.
func TestAnOrganizationsContactIsItsOwnConnections(t *testing.T) {
	ctx := context.Background()
	s := acsFixture(t)
	if got, err := Contact(ctx, s, "client"); err != nil || got != "news@client.test" {
		t.Fatalf("a connection with no reply address names %q (%v), want its default sender", got, err)
	}
	s.cfg.ReplyTo = "help@client.test"
	if got, err := Contact(ctx, s, "v1:accounts:account:client"); err != nil || got != "help@client.test" {
		t.Fatalf("a connection with a reply address names %q (%v), want it", got, err)
	}
	if got, err := Contact(ctx, s, "another"); err != nil || got != "" {
		t.Fatalf("another organization's connection named %q (%v)", got, err)
	}
	// A connection on Azure's own domain sends from Azure's no-reply mailbox:
	// that is never a contact. With a reply address of its own it names that.
	managed := acsFixture(t)
	managed.azureOwnDomain = true
	if got, err := Contact(ctx, managed, "client"); err != nil || got != "" {
		t.Fatalf("a connection on Azure's own domain with no reply address named %q (%v), want nobody", got, err)
	}
	managed.cfg.ReplyTo = "help@client.test"
	if got, err := Contact(ctx, managed, "client"); err != nil || got != "help@client.test" {
		t.Fatalf("a connection on Azure's own domain with a reply address named %q (%v), want it", got, err)
	}
	lazy := NewLazySender(&ACSSender{cfg: ACSConfig{AccountID: "self", Default: "ops@install.test"}}, nil, nil, nil)
	if got, err := Contact(ctx, lazy, "client"); err != nil || got != "" {
		t.Fatalf("with no organization connections the node's transport named %q (%v)", got, err)
	}
	lazy.organization = func(_ context.Context, account string) (Sender, error) {
		switch account {
		case "client":
			return s, nil
		case "self":
			return nil, nil // no connection of its own: the node's transport is not a contact
		}
		return nil, permanentSendRefusal("connect this organization's email domain before sending")
	}
	if got, err := Contact(ctx, lazy, "client"); err != nil || got != "help@client.test" {
		t.Fatalf("the organization's connection, through the node's sender, named %q (%v)", got, err)
	}
	if got, err := Contact(ctx, lazy, "self"); err != nil || got != "" {
		t.Fatalf("an organization with no connection named %q (%v), want nobody", got, err)
	}
	if got, err := Contact(ctx, lazy, "unconnected"); err == nil || got != "" {
		t.Fatalf("an organization whose connection cannot send named %q (%v), want nobody and the reason", got, err)
	}
	if got, err := Contact(ctx, NewLogSender(nil), "client"); err != nil || got != "" {
		t.Fatalf("a transport that cannot say named %q (%v)", got, err)
	}
}

// The refusals a person fixes on the sender or its domain are named, so a page
// can say which; the address is compared without regard to case, as sends do.
func TestSenderRefusalNamesWhatToFix(t *testing.T) {
	s := acsFixture(t) // Azure has news@client.test as "Client"
	check := func(address, fromName string) string {
		t.Helper()
		err := s.CheckSender(context.Background(), SendAs{AccountID: "client", Address: address, FromName: fromName})
		if err != nil && !IsPermanent(err) {
			t.Fatalf("%s: a sender refusal must not be retried: %v", address, err)
		}
		if err == nil {
			return "ready"
		}
		return SenderRefusal(err)
	}
	if got := check("News@Client.TEST", "Client"); got != "ready" {
		t.Fatalf("an address differing only in case was refused: %s", got)
	}
	if got := check("hello@client.test", "Client"); got != "unregistered" {
		t.Fatalf("an address Azure does not have: %q", got)
	}
	if got := check("news@client.test", "Client News"); got != "renamed" {
		t.Fatalf("a display name other than Azure's: %q", got)
	}
	// Its sentence, shown on campaigns and failed runs, points at the fix: the
	// sender's own display name, not the Azure connection.
	renamed := s.CheckSender(context.Background(), SendAs{AccountID: "client", Address: "news@client.test", FromName: "Client News"}).Error()
	if !strings.Contains(renamed, "give the sender the display name Azure has") || strings.Contains(renamed, "connection") {
		t.Fatalf("the renamed sentence points elsewhere: %q", renamed)
	}
	if got := SenderRefusal(s.CheckSender(context.Background(), SendAs{AccountID: "another", Address: "news@client.test"})); got != "" {
		t.Fatalf("another organization's sender is not a sender to fix: %q", got)
	}
	// Azure's own domain registers no other sender: that is its own refusal.
	s.azureOwnDomain = true
	if got := check("hello@client.test", "Client"); got != "azureOwnDomain" {
		t.Fatalf("Azure's own domain: %q", got)
	}
	if got := check("news@client.test", "Client"); got != "ready" {
		t.Fatalf("Azure's own sender on its own domain: %q", got)
	}
}
