package email

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Memory is a protocol-test fixture only. Production always uses PostgreSQL.
type memoryACSOperations struct {
	gate      sync.Mutex
	rows      map[string]acsOperation
	failWrite int
	writes    int
}

func (s *memoryACSOperations) lock(context.Context, string) (func(), error) {
	s.gate.Lock()
	return s.gate.Unlock, nil
}
func (s *memoryACSOperations) read(_ context.Context, key string) (acsOperation, time.Time, bool, error) {
	value, found := s.rows[key]
	return value, time.Time{}, found, nil
}
func (s *memoryACSOperations) write(_ context.Context, key string, op acsOperation, _ time.Time) (time.Time, error) {
	s.writes++
	if s.writes == s.failWrite {
		return time.Time{}, fmt.Errorf("lost write")
	}
	if s.rows == nil {
		s.rows = map[string]acsOperation{}
	}
	s.rows[key] = op
	return time.Now(), nil
}

func TestACSOperationReceiptPreventsDuplicateSubmissionAfterLostResponse(t *testing.T) {
	for _, failure := range []string{"response", "receipt"} {
		t.Run(failure, func(t *testing.T) {
			store := &memoryACSOperations{}
			if failure == "receipt" {
				store.failWrite = 2
			}
			var posts atomic.Int32
			s := acsFixture(t)
			s.operations = store
			s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				posts.Add(1)
				if !azureUUID.MatchString(r.Header.Get("Operation-Id")) {
					t.Error("missing client operation UUID")
				}
				if failure == "response" {
					return nil, fmt.Errorf("lost response")
				}
				return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			msg := Message{IntentID: "campaign:one:recipient:one", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); !IsPermanent(err) {
				t.Fatal(err)
			}
			// A fresh sender has no local memory of the original request.
			fresh := acsFixture(t)
			fresh.operations = store
			fresh.client = s.client
			if err := fresh.Send(context.Background(), msg, SendAs{AccountID: "client"}); !IsPermanent(err) {
				t.Fatal(err)
			}
			if posts.Load() != 1 {
				t.Fatal("ambiguous operation submitted twice")
			}
		})
	}
}

func TestACSOperationProcessingUsesSignedGETAndNeverFollowsProviderURLs(t *testing.T) {
	s := acsFixture(t)
	op := "12345678-1234-1234-1234-123456789012"
	for _, state := range []string{"NotStarted", "Running", "Succeeded", "Failed", "Canceled"} {
		s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.String() != s.cfg.Endpoint+"/emails/operations/"+op+"?api-version="+acsEmailAPIVersion || !strings.HasPrefix(r.Header.Get("Authorization"), "HMAC-SHA256 ") {
				t.Fatal("invalid status request")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Operation-Location": []string{"https://evil.test/credential"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%q,"status":%q,"error":{"message":"private content"}}`, op, state)))}, nil
		})
		status, _, err := s.getOperation(context.Background(), op)
		if err != nil || status == "delivered" || status == "" {
			t.Fatalf("status=%q err=%v", status, err)
		}
	}
	s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"another-operation","status":"Succeeded"}`))}, nil
	})
	if _, _, err := s.getOperation(context.Background(), op); err == nil {
		t.Fatal("accepted another operation's result")
	}
}

func TestACSDoesNotPostWithoutDurableReceipt(t *testing.T) {
	s := acsFixture(t)
	s.operations = &memoryACSOperations{failWrite: 1}
	s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("network preceded receipt"); return nil, nil })
	if err := s.Send(context.Background(), Message{To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}, SendAs{AccountID: "client"}); err == nil {
		t.Fatal("missing write accepted")
	}
}

func TestACSDelayedPOSTDoesNotOverwriteAReconciledOperation(t *testing.T) {
	s := acsFixture(t)
	store := &memoryACSOperations{}
	s.operations = store
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
		// The submission context expires and another worker reconciles the
		// persisted operation before the original network call returns.
		cancel()
		release, _ := store.lock(context.Background(), "")
		defer release()
		for key, op := range store.rows {
			op.Status = "succeeded"
			store.rows[key] = op
		}
		return nil, fmt.Errorf("late lost POST response")
	})
	if err := s.Send(ctx, Message{IntentID: "late-post", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}, SendAs{AccountID: "client"}); err != nil {
		t.Fatal(err)
	}
	for _, op := range store.rows {
		if op.Status != "succeeded" {
			t.Fatal("late POST regressed reconciled status")
		}
	}
}
