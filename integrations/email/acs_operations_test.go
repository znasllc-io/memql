package email

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/secret"
)

// Memory is a protocol-test fixture only. Production always uses PostgreSQL.
type memoryACSOperations struct {
	gate      sync.Mutex
	rows      map[string]acsOperation
	failWrite int
	writes    int
}

type memoryOperationGate struct{ release func() }

func (h memoryOperationGate) Release()                         { h.release() }
func (h memoryOperationGate) Verify(ctx context.Context) error { return ctx.Err() }
func (s *memoryACSOperations) lock(context.Context, string, ...string) (acsOperationGate, error) {
	s.gate.Lock()
	return memoryOperationGate{release: s.gate.Unlock}, nil
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
		// The submission context expires and the persisted operation turns out to be
		// reconciled before the original network call returns. The delivery holds the
		// receipt's lock across the POST, so no other worker can have done it while
		// the message was out; this is the receipt a lock lost under the sender
		// would show, and the outcome must not regress it.
		cancel()
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

// pollStore is the connection state the poller resolves an organization's sender
// from, sealed as the connection store keeps it: state id to sealed value.
type pollStore map[string]string

func (s pollStore) Execute(_ context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "query clientAccountById") {
		return []map[string]any{{"status": "active"}}, nil
	}
	for id, sealed := range s {
		if strings.Contains(query, id) {
			return []map[string]any{{"createdAt": time.Now().UTC(), "encryptedValue": sealed}}, nil
		}
	}
	return []map[string]any{}, nil
}

// TestAnAcceptedOperationAzureNoLongerReportsIsNeverSubmittedAgain: a missing
// operation means Azure never took the request only while nothing confirmed that
// it did. An operation Azure accepted and no longer reports (its record expired)
// stays accepted: it is never made resubmittable, and Send does not POST it again.
func TestAnAcceptedOperationAzureNoLongerReportsIsNeverSubmittedAgain(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ac", 32))
	seal := func(value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sealed, _, err := secret.Encrypt(string(data))
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cfg := acsFixture(t).cfg
	store := pollStore{
		emailStateID("organization:client"): seal(azureConnection{Status: "ready", AccountID: "client", Config: cfg, Plan: azurePlan{ClusterID: "cluster"}}),
		emailStateID(azureClusterKey):       seal(azureClusterConnection{ID: "cluster", Status: "connected"}),
	}
	op := "12345678-1234-1234-1234-123456789012"
	body := []byte(`{"stored":"elsewhere"}`)
	for _, tc := range []struct {
		before, after string
		outcome       Outcome
	}{
		{"accepted", "accepted", OutcomeAccepted},
		{"running", "running", OutcomeAccepted},
		{"unknown", "notfound", OutcomeNotAccepted},
		{"submitting", "notfound", OutcomeNotAccepted},
	} {
		t.Run(tc.before, func(t *testing.T) {
			gets := 0
			a := &azureSetup{store: &connectionStore{engine: store}, now: func() time.Time { return now }, operationClient: &http.Client{Transport: acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatalf("the poller sent a %s", r.Method)
				}
				gets++
				return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"NotFound"}}`))}, nil
			})}}
			s := acsFixture(t)
			ops := s.operations.(*memoryACSOperations)
			msg := Message{IntentID: "campaign-single:polled", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
			key := s.operationKey(msg.IntentID)
			ops.rows = map[string]acsOperation{key: {ID: op, AccountID: "client", Endpoint: cfg.Endpoint, Digest: fmt.Sprintf("%x", sha256.Sum256(body)), Intent: msg.IntentID, Status: tc.before, SubmittedAt: now.Add(-time.Minute), NextPollAt: now.Add(-time.Second)}}
			if err := a.pollOperation(context.Background(), ops, key); err != nil {
				t.Fatal(err)
			}
			if gets != 1 {
				t.Fatalf("the poller asked Azure %d times, want once", gets)
			}
			if got := ops.rows[key].Status; got != tc.after {
				t.Fatalf("a %s operation Azure has no record of is %q, want %q", tc.before, got, tc.after)
			}
			if outcome, err := s.DeliveryOutcome(context.Background(), msg, SendAs{AccountID: "client"}); err != nil || outcome != tc.outcome {
				t.Fatalf("its outcome is %v (%v), want %v", outcome, err, tc.outcome)
			}
			if tc.outcome != OutcomeAccepted {
				return
			}
			posts := 0
			s.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
				posts++
				return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			_ = s.Send(context.Background(), msg, SendAs{AccountID: "client"})
			if posts != 0 {
				t.Fatal("an operation Azure accepted was submitted again")
			}
		})
	}
}
