package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// fakeOutputSender records the VoiceAgentRealtimeOutput envelopes the forwarder
// sends and replies with a configurable ack.
type fakeOutputSender struct {
	mu   sync.Mutex
	sent []*memqlv1.VoiceAgentRealtimeOutput
	ack  *memqlv1.VoiceAgentRealtimeOutputAck
	err  error
}

func (f *fakeOutputSender) SendRequest(_ context.Context, env *memqlv1.MemqlClientMessage) (*memqlv1.MemqlServerMessage, error) {
	f.mu.Lock()
	f.sent = append(f.sent, env.GetVoiceAgentRealtimeOutput())
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentRealtimeOutputAck{
			VoiceAgentRealtimeOutputAck: f.ack,
		},
	}, nil
}

func (f *fakeOutputSender) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeOutputSender) lastSent() *memqlv1.VoiceAgentRealtimeOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[len(f.sent)-1]
}

// TestForward_WireShape verifies the forwarder sends a VoiceAgentRealtimeOutput
// with the expected fields and returns the committed utterance id.
func TestForward_WireShape(t *testing.T) {
	sender := &fakeOutputSender{ack: &memqlv1.VoiceAgentRealtimeOutputAck{Success: true, UtteranceId: "utt-1"}}
	f := NewRealtimeOutputForwarder(sender, "s1", "ga1", NewCitationResolver(GroundingContext{Facts: []GroundingFact{
		{Text: "annual recurring revenue", DomainName: "finance"},
	}}))

	id, err := f.Forward(context.Background(), "Our annual recurring revenue is up.", "reply-to-99")
	require.NoError(t, err)
	assert.Equal(t, "utt-1", id)

	sent := sender.lastSent()
	require.NotNil(t, sent)
	assert.Equal(t, "s1", sent.GetSpaceId())
	assert.Equal(t, "ga1", sent.GetGaAgentId())
	assert.Equal(t, "Our annual recurring revenue is up.", sent.GetText())
	assert.Equal(t, "reply-to-99", sent.GetReplyToId())
	assert.NotEmpty(t, sent.GetReplyId(), "a reply id is minted")
	require.Len(t, sent.GetCitations(), 1)
	assert.Equal(t, "finance", sent.GetCitations()[0].GetDomainId())
	assert.Equal(t, "annual recurring revenue", sent.GetCitations()[0].GetMatchedPhrase())
}

// TestForward_UngroundedNoCitations verifies an ungrounded reply carries no
// citations (byte-identical to an ungrounded text reply).
func TestForward_UngroundedNoCitations(t *testing.T) {
	sender := &fakeOutputSender{ack: &memqlv1.VoiceAgentRealtimeOutputAck{Success: true, UtteranceId: "utt-2"}}
	f := NewRealtimeOutputForwarder(sender, "s1", "ga1", NewCitationResolver(GroundingContext{}))

	_, err := f.Forward(context.Background(), "Just a plain answer.", "")
	require.NoError(t, err)
	assert.Empty(t, sender.lastSent().GetCitations())
}

// TestForward_BlankIsNoOp verifies a blank transcript does not hit the wire.
func TestForward_BlankIsNoOp(t *testing.T) {
	sender := &fakeOutputSender{ack: &memqlv1.VoiceAgentRealtimeOutputAck{Success: true}}
	f := NewRealtimeOutputForwarder(sender, "s1", "ga1", NewCitationResolver(GroundingContext{}))
	id, err := f.Forward(context.Background(), "   ", "")
	require.NoError(t, err)
	assert.Empty(t, id)
	assert.Nil(t, sender.lastSent(), "blank transcript is a no-op (no wire send)")
}

// TestForward_FailedAck verifies an unsuccessful ack surfaces as an error and
// an empty utterance id.
func TestForward_FailedAck(t *testing.T) {
	sender := &fakeOutputSender{ack: &memqlv1.VoiceAgentRealtimeOutputAck{
		Success: false, ErrorCode: "insert_failed", ErrorMessage: "boom",
	}}
	f := NewRealtimeOutputForwarder(sender, "s1", "ga1", NewCitationResolver(GroundingContext{}))
	id, err := f.Forward(context.Background(), "answer", "")
	require.Error(t, err)
	assert.Empty(t, id)
}

// TestForward_TransportError verifies a transport error propagates.
func TestForward_TransportError(t *testing.T) {
	sender := &fakeOutputSender{err: errors.New("stream closed")}
	f := NewRealtimeOutputForwarder(sender, "s1", "ga1", NewCitationResolver(GroundingContext{}))
	_, err := f.Forward(context.Background(), "answer", "")
	require.Error(t, err)
}

// TestMintRealtimeReplyID verifies the minted id matches the flat utt-si-
// shape (never embeds a participant id).
func TestMintRealtimeReplyID(t *testing.T) {
	id := mintRealtimeReplyID()
	assert.Contains(t, id, "utt-si-")
	assert.NotContains(t, id, ":v1:cognition:participant:")
}
