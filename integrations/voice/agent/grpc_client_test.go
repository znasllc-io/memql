package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// fakeStream is an in-memory streamConn for driving the Client without a real
// gRPC connection. Sent envelopes are captured; queued server messages are
// handed to Recv in order. recvErr (default io.EOF) ends the read loop once
// the queue drains.
type fakeStream struct {
	mu      sync.Mutex
	sent    []*memqlv1.MemqlClientMessage
	recvCh  chan *memqlv1.MemqlServerMessage
	recvErr error
	closed  bool
	onSend  func(*memqlv1.MemqlClientMessage)
}

func newFakeStream() *fakeStream {
	return &fakeStream{recvCh: make(chan *memqlv1.MemqlServerMessage, 16), recvErr: io.EOF}
}

func (f *fakeStream) Send(m *memqlv1.MemqlClientMessage) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	cb := f.onSend
	f.mu.Unlock()
	if cb != nil {
		cb(m)
	}
	return nil
}

func (f *fakeStream) Recv() (*memqlv1.MemqlServerMessage, error) {
	msg, ok := <-f.recvCh
	if !ok {
		f.mu.Lock()
		err := f.recvErr
		f.mu.Unlock()
		return nil, err
	}
	return msg, nil
}

func (f *fakeStream) CloseSend() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.recvCh)
	}
	return nil
}

func (f *fakeStream) push(m *memqlv1.MemqlServerMessage) { f.recvCh <- m }

func (f *fakeStream) sentEnvelopes() []*memqlv1.MemqlClientMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*memqlv1.MemqlClientMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

// fakeDialer returns a Dialer that always yields the given fakeStream.
func fakeDialer(fs *fakeStream) Dialer {
	return func(_ context.Context, _, _ string) (streamConn, func(), error) {
		return fs, func() {}, nil
	}
}

func newTestClient(t *testing.T, fs *fakeStream) *Client {
	t.Helper()
	c := NewClient("test:0", "test-token", fakeDialer(fs), nil)
	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(c.Close)
	return c
}

func TestClient_SendRequest_CorrelatesReply(t *testing.T) {
	fs := newFakeStream()
	// Echo back a SessionAck correlated to the request's message id.
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentSessionAck{
				VoiceAgentSessionAck: &memqlv1.VoiceAgentSessionAck{
					RequestId:        "r1",
					Success:          true,
					GaCanonicalVoice: "alto",
				},
			},
		})
	}
	c := newTestClient(t, fs)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := c.SendRequest(ctx, &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentSessionStart{
			VoiceAgentSessionStart: &memqlv1.VoiceAgentSessionStart{PartitionId: "s1", GaAgentId: "s1-ga"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, reply.GetVoiceAgentSessionAck())
	assert.True(t, reply.GetVoiceAgentSessionAck().GetSuccess())
	assert.Equal(t, "alto", reply.GetVoiceAgentSessionAck().GetGaCanonicalVoice())

	// The request envelope carried a stamped (non-empty) message id.
	sent := fs.sentEnvelopes()
	require.Len(t, sent, 1)
	assert.NotEmpty(t, sent[0].GetMessageId())
	assert.NotNil(t, sent[0].GetVoiceAgentSessionStart())
}

func TestClient_SendRequest_ContextCancel(t *testing.T) {
	fs := newFakeStream() // never replies
	c := newTestClient(t, fs)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.SendRequest(ctx, &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentSessionEnd{
			VoiceAgentSessionEnd: &memqlv1.VoiceAgentSessionEnd{PartitionId: "s1"},
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestClient_StreamRequest_CollectsCorrelatedReplies(t *testing.T) {
	fs := newFakeStream()
	// On a TurnRequest, push two deltas + a complete correlated to the
	// request's message id (the streaming reply shape).
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentTurnRequest() == nil {
			return
		}
		corr := env.GetMessageId()
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: corr,
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnDelta{
				VoiceAgentTurnDelta: &memqlv1.VoiceAgentTurnDelta{TextDelta: "Hello "},
			},
		})
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: corr,
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnDelta{
				VoiceAgentTurnDelta: &memqlv1.VoiceAgentTurnDelta{TextDelta: "world"},
			},
		})
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: corr,
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
				VoiceAgentTurnComplete: &memqlv1.VoiceAgentTurnComplete{FinalText: "Hello world"},
			},
		})
	}
	c := newTestClient(t, fs)

	ch, release, err := c.StreamRequest(context.Background(), &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentTurnRequest{
			VoiceAgentTurnRequest: &memqlv1.VoiceAgentTurnRequest{PartitionId: "s1", GaAgentId: "s1-ga", UtteranceText: "hi"},
		},
	})
	require.NoError(t, err)
	defer release()

	var deltas []string
	var final string
	timeout := time.After(2 * time.Second)
	for {
		select {
		case msg := <-ch:
			if d := msg.GetVoiceAgentTurnDelta(); d != nil {
				deltas = append(deltas, d.GetTextDelta())
			}
			if done := msg.GetVoiceAgentTurnComplete(); done != nil {
				final = done.GetFinalText()
			}
		case <-timeout:
			t.Fatal("timed out waiting for streamed replies")
		}
		if final != "" {
			break
		}
	}
	assert.Equal(t, []string{"Hello ", "world"}, deltas)
	assert.Equal(t, "Hello world", final)
}

func TestClient_PushHandler_Dispatch(t *testing.T) {
	fs := newFakeStream()
	c := NewClient("test:0", "tok", fakeDialer(fs), nil)

	got := make(chan *memqlv1.VoiceAgentSpeak, 1)
	c.SetPushHandler("VoiceAgentSpeak", func(env *memqlv1.MemqlServerMessage) error {
		got <- env.GetVoiceAgentSpeak()
		return nil
	})
	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(c.Close)

	// Unsolicited push: no correlate_to set -> routed to the push handler.
	fs.push(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSpeak{
			VoiceAgentSpeak: &memqlv1.VoiceAgentSpeak{RequestId: "req-1", Text: "hello"},
		},
	})

	select {
	case speak := <-got:
		require.NotNil(t, speak)
		assert.Equal(t, "req-1", speak.GetRequestId())
		assert.Equal(t, "hello", speak.GetText())
	case <-time.After(2 * time.Second):
		t.Fatal("push handler was not invoked")
	}
}

func TestClient_UnmatchedPush_DoesNotPanic(t *testing.T) {
	fs := newFakeStream()
	c := newTestClient(t, fs)
	// No handler registered for VoiceAgentSpeak; an unsolicited message with
	// no correlate_to must be dropped (debug-logged) without crashing.
	fs.push(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSpeak{
			VoiceAgentSpeak: &memqlv1.VoiceAgentSpeak{Text: "ignored"},
		},
	})
	// Give the read loop a beat; then a correlated request must still work,
	// proving the loop survived the unmatched push.
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentSessionAck{
				VoiceAgentSessionAck: &memqlv1.VoiceAgentSessionAck{Success: true},
			},
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := c.SendRequest(ctx, &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentSessionStart{
			VoiceAgentSessionStart: &memqlv1.VoiceAgentSessionStart{PartitionId: "s1"},
		},
	})
	require.NoError(t, err)
	assert.True(t, reply.GetVoiceAgentSessionAck().GetSuccess())
}

func TestClient_SendRequest_AfterClose(t *testing.T) {
	fs := newFakeStream()
	c := newTestClient(t, fs)
	c.Close()
	_, err := c.SendRequest(context.Background(), &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentSessionStart{
			VoiceAgentSessionStart: &memqlv1.VoiceAgentSessionStart{PartitionId: "s1"},
		},
	})
	assert.ErrorIs(t, err, ErrClientClosed)
}

func TestClient_ConnectError(t *testing.T) {
	dialErr := errors.New("boom")
	c := NewClient("test:0", "tok", func(_ context.Context, _, _ string) (streamConn, func(), error) {
		return nil, nil, dialErr
	}, nil)
	err := c.Connect(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, dialErr)
}

func TestServerPayloadName(t *testing.T) {
	assert.Equal(t, "<nil>", ServerPayloadName(nil))
	assert.Equal(t, "<empty>", ServerPayloadName(&memqlv1.MemqlServerMessage{}))
	assert.Equal(t, "VoiceAgentSpeak", ServerPayloadName(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSpeak{VoiceAgentSpeak: &memqlv1.VoiceAgentSpeak{}},
	}))
	assert.Equal(t, "VoiceAgentSessionAck", ServerPayloadName(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSessionAck{VoiceAgentSessionAck: &memqlv1.VoiceAgentSessionAck{}},
	}))
}
