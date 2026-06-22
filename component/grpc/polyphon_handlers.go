package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
)

// handlePolyphonRoomToken generates a LiveKit room token for a browser participant.
func (s *streamSession) handlePolyphonRoomToken(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.PolyphonRoomTokenMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	// Token generation only needs the LiveKit room provider; the
	// Polyphon score engine lives on the cognition node and is not
	// touched on this path. Don't refuse to mint tokens just because
	// the score engine isn't local -- the BFF (which is what
	// browsers connect to) doesn't host one.
	if s.service.roomProvider == nil {
		s.sendQueryError(requestId, correlate, codes.Unavailable, "polyphon room provider not configured (LiveKit env missing)")
		return nil
	}

	spaceId := strings.TrimSpace(msg.GetScopeId())
	participantId := strings.TrimSpace(msg.GetParticipantId())
	if spaceId == "" || participantId == "" {
		s.sendQueryError(requestId, correlate, codes.InvalidArgument, "spaceId and participantId are required")
		return nil
	}

	displayName := strings.TrimSpace(msg.GetDisplayName())
	if displayName == "" {
		displayName = "Participant"
	}

	go func() {
		token, err := s.service.roomProvider.GenerateToken(context.Background(), spaceId, participantId, displayName)
		if err != nil {
			s.sendPolyphonError(requestId, correlate, "failed to generate room token", err)
			return
		}

		s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_PolyphonRoomTokenResult{
				PolyphonRoomTokenResult: &memqlv1.PolyphonRoomTokenResult{
					RequestId:  requestId,
					Token:      token.Token,
					RoomName:   token.RoomName,
					LivekitUrl: token.LiveKitURL,
					ExpiresAt:  token.ExpiresAt,
				},
			},
		})

		// (Initiative C, Phase 11): the legacy bridge-agent HTTP
		// notify was deleted here. The Go voice-agent
		// (integrations/voice/agent) self-dispatches into rooms via its
		// own join config; memql no longer fan-outs on token mint.
	}()
	return nil
}

// handlePolyphonStatus returns Polyphon score engine health and session count.
func (s *streamSession) handlePolyphonStatus(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.PolyphonStatusMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	enabled := s.service.scoreEngine != nil && s.service.roomProvider != nil
	activeSessions := int32(0)
	if s.service.scoreEngine != nil {
		activeSessions = int32(s.service.scoreEngine.Sessions().ActiveSessions())
	}

	s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_PolyphonStatusResult{
			PolyphonStatusResult: &memqlv1.PolyphonStatusResult{
				RequestId:      requestId,
				Enabled:        enabled,
				ActiveSessions: activeSessions,
				Platform:       string(polyphon.PlatformOpenAI),
			},
		},
	})
	return nil
}

// handlePolyphonUtterance inserts a human utterance from the voice pipeline.
func (s *streamSession) handlePolyphonUtterance(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.PolyphonUtteranceMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	if s.service.engine == nil {
		s.sendQueryError(requestId, correlate, codes.Unavailable, "engine not configured")
		return nil
	}

	spaceId := strings.TrimSpace(msg.GetScopeId())
	participantId := strings.TrimSpace(msg.GetParticipantId())
	text := strings.TrimSpace(msg.GetText())

	if spaceId == "" || participantId == "" || text == "" {
		s.sendQueryError(requestId, correlate, codes.InvalidArgument, "spaceId, participantId, and text are required")
		return nil
	}

	go func() {
		// Use the sendTextUtterance mutation function instead of inline insert.
		utteranceId := fmt.Sprintf("utt-%s-%d", participantId, time.Now().UnixNano())
		textJSON, _ := json.Marshal(text)
		query := fmt.Sprintf(`mutationSendTextUtterance({utteranceId: "%s", spaceId: "%s", participantId: "%s", text: %s, source: {inputMethod: "stt", pipeline: "polyphon"}})`,
			utteranceId, spaceId, participantId, string(textJSON))

		ctx := contextWithSystemActor(context.Background())
		if _, err := s.service.engine.Execute(ctx, query); err != nil {
			s.sendPolyphonError(requestId, correlate, "failed to insert utterance", err)
			return
		}

		s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_PolyphonUtteranceResult{
				PolyphonUtteranceResult: &memqlv1.PolyphonUtteranceResult{
					RequestId:   requestId,
					UtteranceId: utteranceId,
				},
			},
		})
	}()
	return nil
}

// sendPolyphonError sends a typed error for polyphon operations.
func (s *streamSession) sendPolyphonError(requestId, correlate, message string, err error) {
	if s.logger != nil {
		s.logger.Error(message, "requestId", requestId, "error", err)
	}
	s.sendQueryError(requestId, correlate, codes.Internal, message)
}

// contextWithSystemActor injects a system actor identity into the context
// for MemQL queries executed on behalf of the Polyphon voice pipeline.
func contextWithSystemActor(ctx context.Context) context.Context {
	claims := map[string]any{
		"sub":   "polyphon-bridge-agent",
		"email": "polyphon-bridge-agent@memql.internal",
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}
