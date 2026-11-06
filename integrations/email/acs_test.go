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
