package agent

// realtime_output.go captures the gpt-realtime model's final spoken output for
// one turn and forwards it to memQL as a v1:cognition:utterance with full
// chat/canvas/audit parity (#458, the Go analog of
// the Python voice-agent's realtime_output.py).
//
// Why this exists. In the cascade path cognition owns the assistant turn end
// to end: a VoiceAgentTurnRequest runs the agent loop and inserts the SI reply
// utterance itself (with citations off the respondToUser envelope). The
// realtime executor (#457) replaces that -- gpt-realtime both thinks and speaks
// and never routes through VoiceAgentTurnRequest -- so nothing inserts the SI
// utterance and every utterance-backed surface (chat, canvas, conductor
// history, audit) goes dark for voice turns. This forwarder closes that gap by
// capturing the model's final transcript, deriving citations from the injected
// grounding (CitationResolver), and sending VoiceAgentRealtimeOutput to the
// existing handler (handleVoiceAgentRealtimeOutput), which inserts an SI
// utterance whose wire shape is byte-for-byte identical to a text/cascade reply
// (copresent#135 already renders this shape).
//
// It is pure Go and CGO-free: it depends only on the gRPC Client seam and the
// pure CitationResolver, so the capture -> wire-shape mapping is unit-tested in
// the default lane with a mocked client.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// RealtimeOutputForwarder forwards the realtime model's spoken output to memQL
// as SI utterances. One per session. It holds the space + GA attribution and
// the citation resolver; Forward mints a reply id (the chat streaming-replyId
// contract), derives citations, and sends VoiceAgentRealtimeOutput. The server
// inserts the SI utterance and acks with the canonical row id. Mirrors
// realtime_output.py::RealtimeOutputForwarder.
type RealtimeOutputForwarder struct {
	client    realtimeOutputSender
	spaceID   string
	gaAgentID string
	resolver  CitationResolver

	// minted counts forwarded outputs (diagnostics only).
	forwarded atomic.Int64
}

// Compile-time assurance that the production gRPC Client satisfies the seams
// the voice-tagged room wiring (room_realtime_voice.go) passes it through. This
// lives in the default (CGO-free) lane so a signature drift is caught there
// rather than only in the voice (cgo) lane.
var (
	_ realtimeOutputSender = (*Client)(nil)
	_ toolBridgeLister     = (*Client)(nil)
)

// realtimeOutputSender is the minimal gRPC surface the forwarder needs: a
// request/reply send awaiting the VoiceAgentRealtimeOutputAck. Satisfied by
// *Client (via SendRequest) and stubbed in tests so the wire shape is exercised
// without a real stream.
type realtimeOutputSender interface {
	SendRequest(ctx context.Context, envelope *memqlv1.MemqlClientMessage) (*memqlv1.MemqlServerMessage, error)
}

// NewRealtimeOutputForwarder builds a forwarder for a space + GA with a
// citation resolver derived from the session's grounding context (#456). Pass
// the no-op resolver (NewCitationResolver(GroundingContext{})) for an
// ungrounded session so realtime replies land byte-identical to ungrounded
// text replies.
func NewRealtimeOutputForwarder(client realtimeOutputSender, spaceID, gaAgentID string, resolver CitationResolver) *RealtimeOutputForwarder {
	return &RealtimeOutputForwarder{
		client:    client,
		spaceID:   strings.TrimSpace(spaceID),
		gaAgentID: strings.TrimSpace(gaAgentID),
		resolver:  resolver,
	}
}

// Forward sends one completed assistant transcript as a VoiceAgentRealtimeOutput
// and returns the committed utterance id (or "" on a no-op / failed insert --
// the caller logs; the voice turn still played). replyToID threads the reply
// under the triggering human turn for ordering parity with the cascade; pass
// "" when unknown. A blank transcript is a no-op (the model emitted audio with
// no resolvable text yet). Mirrors realtime_output.py::RealtimeOutputForwarder.forward.
func (f *RealtimeOutputForwarder) Forward(ctx context.Context, text, replyToID string) (string, error) {
	body := strings.TrimSpace(text)
	if body == "" {
		return "", nil
	}
	if f.client == nil {
		return "", fmt.Errorf("voice-agent realtime output: nil client")
	}

	replyID := mintRealtimeReplyID()
	citations := f.resolver.Resolve(body)

	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentRealtimeOutput{
			VoiceAgentRealtimeOutput: &memqlv1.VoiceAgentRealtimeOutput{
				SpaceId:   f.spaceID,
				GaAgentId: f.gaAgentID,
				ReplyId:   replyID,
				Text:      body,
				ReplyToId: strings.TrimSpace(replyToID),
				Citations: citations,
			},
		},
	}

	reply, err := f.client.SendRequest(ctx, envelope)
	if err != nil {
		return "", err
	}
	ack := reply.GetVoiceAgentRealtimeOutputAck()
	if ack == nil {
		return "", fmt.Errorf("voice-agent realtime output: no ack in reply")
	}
	if !ack.GetSuccess() {
		return "", fmt.Errorf("voice-agent realtime output insert failed: %s %s",
			ack.GetErrorCode(), ack.GetErrorMessage())
	}
	f.forwarded.Add(1)
	return ack.GetUtteranceId(), nil
}

// mintRealtimeReplyID mints a stable reply/utterance id matching the server's
// fallback shape (utt-si-<nanos>-<hex>). Flat -- it never embeds a participant
// id, which the engine's canonicalizer would mis-parse as the type tag and
// reject. The server accepts this as the committed utterance id so it matches
// the chat panel's streaming replyId. Mirrors realtime_output.py::_mint_reply_id.
func mintRealtimeReplyID() string {
	return fmt.Sprintf("utt-si-%d-%s", time.Now().UnixNano(), uuid.NewString()[:8])
}
