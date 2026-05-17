package memql

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	memqlengine "github.com/visionarys-io/memql/component/memql"
	"github.com/visionarys-io/memql/core/common"
)

// AgentTurnHandler is the narrow interface the gRPC server uses to drive
// one AgentGenerateTurnMsg. Implemented by integrations/agent.Replier on
// agent binaries. Nil on non-agent binaries -- the dispatcher surfaces a
// structured error when an AgentGenerateTurnMsg lands on the wrong node.
type AgentTurnHandler interface {
	Handle(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg, sink AgentTurnSink) (*AgentTurnResult, error)
}

// AgentTurnSink receives deltas emitted by the handler as a turn runs.
// The gRPC handler implements this by writing AgentGenerateTurnDelta
// messages on the stream back to cognition.
type AgentTurnSink interface {
	TextDelta(text string)
	ToolCall(id, name, argumentsJSON string)
	ToolResult(id, resultJSON, errMsg string)
}

// AgentTurnResult summarises a completed turn.
type AgentTurnResult struct {
	FinalText  string
	TextChunks int
	ToolCalls  []common.ToolCall
	Iterations int
	// Citations is the structured source-attribution list extracted
	// from the respondToUser envelope. Each entry pairs a knowledge
	// domain id with a verbatim substring of FinalText. The handler
	// stamps these onto AgentGenerateTurnComplete.citations so
	// cognition can persist them on the committed utterance.
	Citations []*memqlv1.AgentTurnCitation
	// Retrieved is the full RAG retrieval pool the replier surfaced
	// to the LLM BEFORE the model decided what to cite. The handler
	// stamps these onto AgentGenerateTurnComplete.retrieved; cognition
	// persists them on the utterance row, and the frontend renders
	// them in the "Show details" expander beneath each reply -- so
	// the user can see passages the model considered but didn't cite.
	Retrieved []*memqlv1.AgentRetrievedChunk
}

// SetAgentTurnHandler installs the per-binary handler. Called from app
// bootstrap on agent builds. Must be called before Start() for the
// handler to be visible to the constructed service; late installs update
// the existing service reference too.
func (s *Server) SetAgentTurnHandler(h AgentTurnHandler) {
	if s == nil {
		return
	}
	s.agentReplier = h
	if s.serviceRef != nil && s.serviceRef.svc != nil {
		s.serviceRef.svc.agentReplier = h
	}
}

// handleAgentGenerateTurn drives one turn through the installed handler
// and streams AgentGenerateTurnDelta replies back on the open bidi stream,
// terminating with one AgentGenerateTurnComplete (or AgentTurnError).
//
// The replier is run on a background goroutine so the gRPC/NodeService
// read loop that dispatched us stays free. That matters for two reasons:
//
//  1. A turn can last up to 30 s when the tool loop is waiting on a
//     client-executed tool. Blocking the read loop that long would
//     silence every other message on the same connection (heartbeats,
//     other turns, and crucially the ClientToolResult continuation
//     the cross-node client-tool relay delivers via AiForwardRequest
//     to unblock that same waiter -- self-deadlock by design).
//  2. AgentGenerateTurnDelta / Complete writes are already goroutine-
//     safe on sendServerMessage, so relocating the Handle() call off
//     the read loop has no ordering implications.
func (s *streamSession) handleAgentGenerateTurn(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.AgentGenerateTurnMsg) error {
	correlate := envelope.GetMessageId()
	requestId := ""
	if msg != nil {
		requestId = msg.GetRequestId()
	}
	if requestId == "" {
		requestId = correlate
	}

	if msg == nil {
		return s.sendAgentTurnError(correlate, requestId, "invalid_request", "agent_generate_turn message missing")
	}

	if s.service == nil || s.service.agentReplier == nil {
		if s.service != nil && s.service.logger != nil {
			s.service.logger.Warn("agent_generate_turn arrived on non-agent binary", "request_id", requestId)
		}
		return s.sendAgentTurnError(correlate, requestId, "unavailable",
			"AgentGenerateTurn is only handled on agent nodes")
	}

	// Sink writes each delta onto the stream. sendServerMessage is
	// goroutine-safe (streamSession mutex) so running Handle() in a
	// detached goroutine is still ordered.
	sink := &grpcAgentTurnSink{session: s, correlate: correlate, requestId: requestId}

	// Use the stream's context so an aborted peer connection cancels the
	// replier too.
	ctx := s.stream.Context()
	// Re-hydrate cross-node provenance: AgentGenerateTurn typically
	// arrives via AiForward from cognition. Any row the agent tool
	// loop writes (utterances, tool side-effects) stamps the caller's
	// provenance instead of a fresh agent-side default.
	ctx = contextWithEnvelopeProvenance(ctx, envelope)
	// Attach this session as the ClientToolInvoker so the agent loop's
	// engine.ExecuteTool can round-trip client_execution tools back to
	// the originating browser. Also attach the acting agent's
	// guardrail role so engine.ExecuteTool can enforce
	// Tool.AllowedRoles without re-reading envelope metadata.
	ctx = memqlengine.WithClientToolInvoker(ctx, s)
	if acting := msg.GetActingAgent(); acting != nil {
		if role := strings.TrimSpace(acting.GetRole()); role != "" {
			ctx = memqlengine.WithActingAgentRole(ctx, role)
		}
		if id := strings.TrimSpace(acting.GetId()); id != "" {
			// Stamp the acting agent's id on ctx so any
			// ClientToolCall envelope emitted by this turn (e.g.
			// uiRequestControl, uiClick) carries it. The frontend
			// reads ClientToolCall.AgentId to attribute the
			// CoPresent Control Widget transcript to the actual
			// driving agent (e.g. "Briar") instead of a generic label.
			ctx = memqlengine.WithActingAgentId(ctx, id)
		}
	}

	go s.runAgentTurn(ctx, correlate, requestId, msg, sink)
	return nil
}

// runAgentTurn invokes the replier and emits the terminal
// AgentGenerateTurnComplete (or AgentTurnError) envelope. Called from a
// goroutine so the stream read loop that dispatched the
// AgentGenerateTurn envelope is free to accept continuation messages
// (ClientToolResult from the cluster relay, AiForwardCancel, ...) while
// the turn is still running.
func (s *streamSession) runAgentTurn(
	ctx context.Context,
	correlate, requestId string,
	msg *memqlv1.AgentGenerateTurnMsg,
	sink AgentTurnSink,
) {
	result, err := s.service.agentReplier.Handle(ctx, msg, sink)
	if err != nil {
		if s.service != nil && s.service.logger != nil {
			s.service.logger.Warn("agent turn handler failed", "request_id", requestId, "error", err)
		}
		_ = s.sendAgentTurnError(correlate, requestId, "handler_failure", err.Error())
		return
	}
	if result == nil {
		result = &AgentTurnResult{}
	}

	complete := &memqlv1.AgentGenerateTurnComplete{
		RequestId:  requestId,
		FinalText:  result.FinalText,
		TextChunks: int32(result.TextChunks),
		Iterations: int32(result.Iterations),
		Citations:  result.Citations,
		Retrieved:  result.Retrieved,
	}
	if len(result.ToolCalls) > 0 {
		complete.ToolCalls = make([]*memqlv1.AgentTurnToolCall, 0, len(result.ToolCalls))
		for _, tc := range result.ToolCalls {
			complete.ToolCalls = append(complete.ToolCalls, &memqlv1.AgentTurnToolCall{
				ToolCallId:    tc.ID,
				ToolName:      tc.Name,
				ArgumentsJson: tc.Arguments,
			})
		}
	}

	_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AgentGenerateTurnComplete{
			AgentGenerateTurnComplete: complete,
		},
	})
}

func (s *streamSession) sendAgentTurnError(correlate, requestId, code, message string) error {
	complete := &memqlv1.AgentGenerateTurnComplete{
		RequestId: requestId,
		Error: &memqlv1.AgentTurnError{
			ErrorId: newAgentTurnErrorId(),
			Code:    code,
			Message: message,
		},
	}
	_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AgentGenerateTurnComplete{
			AgentGenerateTurnComplete: complete,
		},
	})
	// Also surface as a QueryError so non-agent-aware observers see something.
	return s.sendQueryError(requestId, correlate, codes.FailedPrecondition, fmt.Sprintf("%s: %s", code, message))
}

// grpcAgentTurnSink adapts the streamSession send path to AgentTurnSink.
type grpcAgentTurnSink struct {
	session   *streamSession
	correlate string
	requestId string
}

func (s *grpcAgentTurnSink) TextDelta(text string) {
	if text == "" {
		return
	}
	_ = s.session.sendServerMessage(s.correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AgentGenerateTurnDelta{
			AgentGenerateTurnDelta: &memqlv1.AgentGenerateTurnDelta{
				RequestId: s.requestId,
				Chunk: &memqlv1.AgentGenerateTurnDelta_Text{
					Text: &memqlv1.AgentTurnTextDelta{Text: text},
				},
			},
		},
	})
}

func (s *grpcAgentTurnSink) ToolCall(id, name, argumentsJSON string) {
	_ = s.session.sendServerMessage(s.correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AgentGenerateTurnDelta{
			AgentGenerateTurnDelta: &memqlv1.AgentGenerateTurnDelta{
				RequestId: s.requestId,
				Chunk: &memqlv1.AgentGenerateTurnDelta_ToolCall{
					ToolCall: &memqlv1.AgentTurnToolCall{
						ToolCallId:    id,
						ToolName:      name,
						ArgumentsJson: argumentsJSON,
					},
				},
			},
		},
	})
}

func (s *grpcAgentTurnSink) ToolResult(id, resultJSON, errMsg string) {
	_ = s.session.sendServerMessage(s.correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AgentGenerateTurnDelta{
			AgentGenerateTurnDelta: &memqlv1.AgentGenerateTurnDelta{
				RequestId: s.requestId,
				Chunk: &memqlv1.AgentGenerateTurnDelta_ToolResult{
					ToolResult: &memqlv1.AgentTurnToolResult{
						ToolCallId: id,
						ResultJson: resultJSON,
						Error:      errMsg,
					},
				},
			},
		},
	})
}

// newAgentTurnErrorId returns an ERR-XXXXXX identifier consistent with
// the SI HTTP error ID format (component/server/sihttp/errorid.go).
func newAgentTurnErrorId() string {
	return generateErrorId()
}
