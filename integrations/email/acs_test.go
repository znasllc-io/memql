package email

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestACSDoesNotRetryAmbiguousAcceptanceOrEchoProviderSecrets(t *testing.T) {
	for _, code := range []int{400, 401, 403, 408, 429, 500, 503, 302} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			s := acsFixture(t)
			s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: code, Header: http.Header{"Retry-After": []string{"15"}}, Body: io.NopCloser(strings.NewReader("private-body-and-credential"))}, nil
			})
			err := s.Send(context.Background(), Message{To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}, SendAs{AccountID: "client"})
			if err == nil || strings.Contains(err.Error(), "private-body") {
				t.Fatal("unsafe provider failure", err)
			}
			if code == 429 {
				if delay, throttle := IsThrottled(err); !throttle || delay != 15*time.Second {
					t.Fatal("lost throttle", err)
				}
			} else if !IsPermanent(err) {
				t.Fatal("unsafe retry", err)
			}
		})
	}
	s := acsFixture(t)
	s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("network secret") })
	err := s.Send(context.Background(), Message{To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}, SendAs{AccountID: "client"})
	if !IsPermanent(err) || strings.Contains(err.Error(), "network secret") {
		t.Fatal("uncertain POST must not blindly retry", err)
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
