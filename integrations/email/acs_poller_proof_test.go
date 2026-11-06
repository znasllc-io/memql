package email

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/secret"
)

// A first POST whose outcome write is lost leaves a "submitting" receipt that
// still carries the proof that nothing was taken under its id (it was minted
// with the id). The poller then hears notfound, which proves nothing for a POST
// Azure may still register; the unchanged retry is throttled under the same id;
// and a revised message follows. The poller's clearing of the proof is the only
// thing that keeps the revised message on the same Operation-Id here, so a POST
// Azure took late is deduplicated instead of delivered twice.
func TestThePollerClearsTheProofThatNothingWasTaken(t *testing.T) {
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
	for _, firstAnswer := range []string{"202", "lost"} {
		t.Run(firstAnswer, func(t *testing.T) {
			s := acsFixture(t)
			ops := s.operations.(*memoryACSOperations)
			ops.failWrite = 2 // the first POST's outcome write is lost
			store := pollStore{
				emailStateID("organization:client"): seal(azureConnection{Status: "ready", AccountID: "client", Config: s.cfg, Plan: azurePlan{ClusterID: "cluster"}}),
				emailStateID(azureClusterKey):       seal(azureClusterConnection{ID: "cluster", Status: "connected"}),
			}
			var posted []string
			step := 0
			s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				posted = append(posted, r.Header.Get("Operation-Id"))
				step++
				switch step {
				case 1:
					if firstAnswer == "lost" {
						return nil, errors.New("connection reset")
					}
					return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				case 2:
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"15"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				}
				return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			clock := s.now()
			s.now = func() time.Time { return clock }
			msg := Message{IntentID: "campaign-single:poller-proof", To: "reader@example.test", Subject: "Hello", TextBody: "Hello"}
			_ = s.Send(context.Background(), msg, SendAs{AccountID: "client"})
			key := s.operationKey(msg.IntentID)
			if got := ops.rows[key]; got.Status != "submitting" || !got.Untaken {
				t.Fatalf("setup: the receipt is %+v, want submitting with the proof", got)
			}
			pollAt := clock.Add(time.Minute)
			a := &azureSetup{store: &connectionStore{engine: store}, now: func() time.Time { return pollAt }, operationClient: &http.Client{Transport: acsRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}}
			if err := a.pollOperation(context.Background(), ops, key); err != nil {
				t.Fatal(err)
			}
			if got := ops.rows[key]; got.Status != "notfound" || got.Untaken {
				t.Fatalf("after the poller: %+v, want notfound without the proof", got)
			}
			clock = clock.Add(time.Hour)
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); !IsNotAccepted(err) {
				t.Fatalf("the unchanged retry: %v", err)
			}
			clock = clock.Add(time.Hour)
			msg.Subject, msg.TextBody, msg.Revised = "Welcome", "Welcome aboard", true
			if err := s.Send(context.Background(), msg, SendAs{AccountID: "client"}); err != nil {
				t.Fatalf("the revised message: %v", err)
			}
			if posted[2] != posted[0] {
				t.Errorf("Operation-Ids %q: the revised message went under a fresh id after notfound then 429", posted)
			}
		})
	}
}
