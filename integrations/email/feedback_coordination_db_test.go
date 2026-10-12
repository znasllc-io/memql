package email

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every send can occupy a gate connection without needing another connection
// to commit its campaign ledger. Distinct campaigns can fill the deployed pool.
func TestConcurrentACSReservationsShareReceiptAndLedgerConnection(t *testing.T) {
	const capacity = 4
	first, _, _ := reserveFixture(t, capacity)
	pool := first.operations.(*gatedOperations).pool
	entered := make(chan struct{}, capacity)
	released := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(released) }) }
	defer unblock()
	done := make(chan error, capacity)
	for n := 0; n < capacity; n++ {
		sender := acsFixture(t)
		sender.operations = &gatedOperations{pool: pool}
		sender.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
			entered <- struct{}{}
			<-released
			return accepted(), nil
		})
		go func(n int, sender *ACSSender) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			key := fmt.Sprintf("campaign-provider-ledger:pool-%d", n)
			msg := Message{IntentID: fmt.Sprintf("pool-%d", n), To: "reader@example.test", Subject: "Synthetic concurrency", TextBody: "Fixture"}
			reserved, release, err := sender.Reserve(ctx, msg, asClient, key)
			if err != nil {
				done <- err
				return
			}
			defer release()
			if err = sender.Send(reserved, msg, asClient); err == nil {
				owner := CopyDeliveryGates(context.Background(), reserved)
				verify, held := BorrowDeliveryGate(owner, key)
				if !held {
					err = fmt.Errorf("reservation lost the ledger gate")
				} else {
					err = verify(owner)
				}
				if _, foreign := BorrowDeliveryGate(owner, "another-ledger"); foreign {
					err = fmt.Errorf("reservation admitted an unrelated ledger")
				}
			}
			done <- err
		}(n, sender)
	}
	for n := 0; n < capacity; n++ {
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("send ended before pool filled: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent sends exhausted the gate pool")
		}
	}
	unblock()
	for n := 0; n < capacity; n++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

type feedbackCommitStore struct {
	ConfigWriter
	before func(context.Context)
}

func (s feedbackCommitStore) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "mutation storeEmailSendOperation(") {
		s.before(ctx)
	}
	return s.ConfigWriter.Execute(ctx, query)
}

func TestACSStatusPollKeepsGateWhenCanceledAtCommit(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	sender, op := w.send(t, "poll-cancellation")
	w.now = w.now.Add(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := NewAzureFeedbackHandler(func() *Integration { return w.instances[1] }, func(ctx context.Context, report AzureDeliveryReport) error { return report.Verify(ctx) })
	response := make(chan int, 1)
	observed := false
	base := w.instances[0].azure
	guarded := *base.store
	guarded.engine = feedbackCommitStore{ConfigWriter: base.store.engine, before: func(commit context.Context) {
		observed = true
		cancel()
		time.Sleep(10 * time.Millisecond)
		if commit.Err() != nil {
			t.Error("receipt commit inherited request cancellation")
		}
		if !lockHeldBy(t, w.db.DB, "email-operation:"+sender.operationKey("poll-cancellation")) {
			t.Error("receipt gate ended before commit")
		}
		go func() { response <- w.request(handler, w.event(t, op, "Delivered"), nil).Code }()
		select {
		case code := <-response:
			t.Errorf("feedback passed the poller's uncommitted receipt: %d", code)
		case <-time.After(40 * time.Millisecond):
		}
	}}
	poller := *base
	poller.store = &guarded
	poller.operationClient = &http.Client{Transport: acsRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%q,"status":"Running"}`, op.ID)))}, nil
	})}
	if err := poller.pollOperation(ctx, &storedACSOperations{connection: &guarded}, sender.operationKey("poll-cancellation")); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("poll did not reach receipt commit")
	}
	select {
	case code := <-response:
		if code != 204 {
			t.Fatalf("feedback status %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("feedback did not resume after receipt commit")
	}
	current, _, _, err := sender.operations.read(context.Background(), sender.operationKey("poll-cancellation"))
	if err != nil || current.Status != "succeeded" || current.DeliveryStatus != "delivered" {
		t.Fatalf("poll erased provider proof: %+v %v", current, err)
	}
}

func TestAzureFeedbackReadinessTracksCurrentConnection(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	check := func(want string) {
		t.Helper()
		got, err := w.instances[1].azure.feedbackStatus(context.Background(), w.account)
		if err != nil || got != want {
			t.Fatalf("readiness %q want %q: %v", got, want, err)
		}
	}
	check("ready")
	var cluster azureClusterConnection
	if err := w.instances[0].azure.store.change(context.Background(), azureClusterKey, &cluster, func(bool) error { cluster.Status = "disconnected"; return nil }); err != nil {
		t.Fatal(err)
	}
	check("not-configured")
	if err := w.instances[0].azure.store.change(context.Background(), azureClusterKey, &cluster, func(bool) error { cluster.Status = "connected"; return nil }); err != nil {
		t.Fatal(err)
	}
	check("ready")
	var connection azureConnection
	if err := w.instances[0].azure.store.change(context.Background(), "organization:"+w.account, &connection, func(bool) error { connection.Plan.CommunicationService = "replacement"; return nil }); err != nil {
		t.Fatal(err)
	}
	check("not-configured")
}

func TestACSInitialReceiptKeepsGateWhenCanceledAtCommit(t *testing.T) {
	w := newAzureFeedbackWorld(t)
	sender, err := NewACSSender(w.connection.Config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guarded := *w.instances[0].azure.store
	writes := 0
	guarded.engine = feedbackCommitStore{ConfigWriter: guarded.engine, before: func(commit context.Context) {
		writes++
		if writes != 1 {
			return
		}
		cancel()
		time.Sleep(10 * time.Millisecond)
		if commit.Err() != nil {
			t.Error("initial receipt inherited cancellation")
		}
		if !lockHeldBy(t, w.db.DB, "email-operation:"+sender.operationKey("initial-cancel")) {
			t.Error("initial receipt lost its gate before commit")
		}
	}}
	sender.operations = &storedACSOperations{connection: &guarded}
	sender.client = &http.Client{Transport: acsRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Context().Err() == nil {
			t.Error("POST did not honor original cancellation")
		}
		return nil, request.Context().Err()
	})}
	msg := Message{IntentID: "initial-cancel", To: "receiver@example.test", Subject: "Synthetic cancellation", TextBody: "Fixture"}
	if err = sender.Send(ctx, msg, SendAs{AccountID: w.account}); err == nil {
		t.Fatal("canceled send succeeded")
	}
	saved, _, found, err := sender.operations.read(context.Background(), sender.operationKey(msg.IntentID))
	if err != nil || !found || saved.Status != "unknown" || writes != 2 {
		t.Fatalf("receipt did not settle: %+v found=%v writes=%d error=%v", saved, found, writes, err)
	}
}
