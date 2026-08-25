package client

// SDK-owned auto-reconnect with resubscribe, Go half (memql#4537).
//
// The Go dispatcher was stream-bound: a Recv error closed stopCh, unexpectedCh
// AND eventCh, so a drop ended the dispatcher as an object and every
// `range Events()` consumer with it. Under supervision a drop ends one
// GENERATION: pending requests fail so nobody hangs, the transport-down signal
// fires so a supervisor can redial, and the dispatcher's identity survives --
// which is what keeps a caller's typed clients and channels valid.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestSupervisedDispatcherSurvivesATransportDrop(t *testing.T) {
	first := newMockStream()
	d := NewSupervisedDispatcher(first, nil)
	go d.Run()
	t.Cleanup(d.Stop)

	events := d.Events()
	down := d.TransportDown()

	// Kill the transport the way the network does.
	close(first.recvCh)

	select {
	case <-down:
	case <-time.After(2 * time.Second):
		t.Fatal("transport-down never fired")
	}

	// The identity survives: stopCh open, and the event channel a caller is
	// ranging is NOT closed. Closing it here -- correct for an unsupervised
	// dispatcher -- would end every consumer on a drop the SDK recovers from.
	select {
	case <-d.Done():
		t.Fatal("a supervised transport drop must not finish the dispatcher")
	default:
	}
	select {
	case _, ok := <-events:
		require.True(t, ok, "the event channel must not close on a supervised drop")
	default:
	}

	// Rebind, run again, and the SAME channel a caller has been holding since
	// before the drop delivers.
	second := newMockStream()
	require.NoError(t, d.Rebind(second))
	go d.Run()
	second.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_Event{
			Event: &memqlv1.EventNotification{SubscriptionId: "sub-1", Seq: 1},
		},
	}
	select {
	case msg := <-events:
		assert.Equal(t, "sub-1", msg.GetEvent().GetSubscriptionId())
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery on the rebound stream")
	}
}

func TestSupervisedDispatcherFailsPendingSoNoCallerHangs(t *testing.T) {
	stream := newMockStream()
	d := NewSupervisedDispatcher(stream, nil)
	go d.Run()
	t.Cleanup(d.Stop)

	errCh := make(chan error, 1)
	go func() {
		_, err := d.SendAndWait(t.Context(), &memqlv1.MemqlClientMessage{
			MessageId: "m-1",
			Payload:   &memqlv1.MemqlClientMessage_ClientHello{ClientHello: &memqlv1.ClientHello{}},
		})
		errCh <- err
	}()
	// Let the send land before the drop.
	select {
	case <-stream.sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the request never went out")
	}
	close(stream.recvCh)

	select {
	case err := <-errCh:
		require.Error(t, err, "a request in flight when the transport died must fail, not park")
	case <-time.After(2 * time.Second):
		t.Fatal("SendAndWait hung across a supervised drop")
	}
}

func TestSubscriptionManagerReplaysEverySpecWithItsOriginalId(t *testing.T) {
	stream := newMockStream()
	d := NewSupervisedDispatcher(stream, nil)
	sm := NewSubscriptionManager(d)
	t.Cleanup(sm.Stop)

	idA, _, err := sm.SubscribeGraph(t.Context(), GraphSubscribeOptions{
		Concept: "v1:worker:registration",
		Actions: []GraphAction{GraphActionCreated},
	})
	require.NoError(t, err)
	idB, _, err := sm.SubscribeGraph(t.Context(), GraphSubscribeOptions{Concept: "v1:workbench:workspace"})
	require.NoError(t, err)
	drain(t, stream, 2)

	require.Equal(t, 2, sm.Replay())
	replayed := map[string]string{}
	for _, msg := range drain(t, stream, 2) {
		sub := msg.GetSubscribe()
		require.NotNil(t, sub)
		replayed[sub.GetSubscriptionId()] = sub.GetConcept()
	}
	// Original ids: they are client-minted and scoped to a stream, so the new
	// server session has never seen them -- and reusing them is what keeps the
	// caller's delivery channels valid. Fresh ids would leave the caller
	// reading a channel nothing writes to.
	assert.Equal(t, map[string]string{
		idA: "v1:worker:registration",
		idB: "v1:workbench:workspace",
	}, replayed)

	// An unsubscribed spec is not replayed.
	require.NoError(t, sm.Unsubscribe(idA))
	drain(t, stream, 1)
	assert.Equal(t, 1, sm.Replay())
}

func TestBackoffIsExponentialCappedAndJittered(t *testing.T) {
	initial, max := 100*time.Millisecond, time.Second
	// Full jitter: a uniform draw from [0, capped]. What is pinned is the
	// UPPER bound doubling per attempt and then holding at the ceiling; the
	// lower bound staying 0 is what decorrelates a fleet dropped all at once.
	for attempt, want := range map[int]time.Duration{
		0: 100 * time.Millisecond,
		1: 200 * time.Millisecond,
		3: 800 * time.Millisecond,
		4: time.Second,
		9: time.Second,
	} {
		for i := 0; i < 50; i++ {
			got := backoffDelay(attempt, initial, max)
			assert.GreaterOrEqual(t, got, time.Duration(0), "attempt %d", attempt)
			assert.LessOrEqual(t, got, want, "attempt %d must not exceed its cap", attempt)
		}
	}
	// A huge attempt count must clamp, not overflow into a negative shift.
	assert.LessOrEqual(t, backoffDelay(1000, initial, max), max)
}

func TestNormalizeReconnectRefusesACeilingUnderTheFloor(t *testing.T) {
	// A ceiling below the floor would make the backoff run BACKWARDS,
	// retrying faster the longer an outage lasts.
	got := normalizeReconnect(&ReconnectConfig{InitialDelay: 5 * time.Second, MaxDelay: time.Second})
	require.NotNil(t, got)
	assert.GreaterOrEqual(t, got.MaxDelay, got.InitialDelay)

	assert.Nil(t, normalizeReconnect(nil), "nil stays nil -- reconnect is opt-in")

	defaults := normalizeReconnect(&ReconnectConfig{})
	assert.Equal(t, time.Second, defaults.InitialDelay)
	assert.Equal(t, 30*time.Second, defaults.MaxDelay)
	assert.Equal(t, 10*time.Second, defaults.StableAfter)
}

func drain(t *testing.T, stream *mockStream, n int) []*memqlv1.MemqlClientMessage {
	t.Helper()
	out := make([]*memqlv1.MemqlClientMessage, 0, n)
	for i := 0; i < n; i++ {
		select {
		case msg := <-stream.sendCh:
			out = append(out, msg)
		case <-time.After(2 * time.Second):
			t.Fatalf("expected %d outbound frames, saw %d", n, len(out))
		}
	}
	return out
}
