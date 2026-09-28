package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	engine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
)

// handleAskStream runs one Ask turn.
//
// THE TURN'S SOURCE AND LEVEL (AiChatMsg.provider and .level) ARE A ROUTING
// CHOICE, NOT A CONTEXT VALUE. The turn is a goal: compiled on a planner, its
// steps run on an agent, neither sharing memory with this replica -- so a
// provider override on this handler's context reached none of the calls that
// mattered. The engine writes the choice onto the goal's run row instead, and
// every node that executes the run applies it from there (RunAsk).
//
// The grammar is checked HERE, before the engine is reached, so a word outside
// the picker's vocabulary never opens a goal, a run or a transcript turn; a
// route the cluster does not have is the engine's to refuse, and arrives with
// the same code.
func (s *streamSession) handleAskStream(ctx context.Context, requestID, correlate string, request *memqlv1.AiChatMsg) {
	route := engine.AskRoute{Source: request.GetProvider(), Level: request.GetLevel()}
	if _, err := engine.ParseRouteChoice(route.Source, route.Level); err != nil {
		code, message, metadata := askFailureStatus(err)
		_ = s.sendQueryErrorWithMetadata(requestID, correlate, code, message, metadata)
		return
	}
	chunks := make(chan common.StreamChunk, 16)
	send := func(chunk common.StreamChunk) {
		select {
		case chunks <- chunk:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(chunks)
		_, err := s.service.engine.RunAsk(ctx, request.GetConversationId(), requestID, request.GetMessages()[0].GetContent(), request.GetPageContext(), route, func(text string) { send(common.StreamChunk{Content: text}) }, func(event engine.WorkEvent) {
			encoded, _ := json.Marshal(event)
			var metadata map[string]any
			_ = json.Unmarshal(encoded, &metadata)
			send(common.StreamChunk{Metadata: map[string]any{"ask": metadata}})
		})
		if err != nil {
			send(common.StreamChunk{Error: err})
			return
		}
		send(common.StreamChunk{Done: true})
	}()
	var text strings.Builder
	var index int64
	for chunk := range chunks {
		if chunk.Error != nil {
			code, message, metadata := askFailureStatus(chunk.Error)
			_ = s.sendQueryErrorWithMetadata(requestID, correlate, code, message, metadata)
			return
		}
		out := &memqlv1.AiStreamChunk{StreamId: requestID, RequestId: requestID, Index: index}
		if chunk.Content != "" {
			text.WriteString(chunk.Content)
			out.Chunk = &memqlv1.AiStreamChunk_TextDelta{TextDelta: chunk.Content}
		}
		if len(chunk.Metadata) != 0 {
			metadata, err := structpb.NewStruct(chunk.Metadata)
			if err != nil {
				return
			}
			out.Chunk = &memqlv1.AiStreamChunk_Metadata{Metadata: metadata}
		}
		if out.Chunk != nil {
			if err := s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_AiChunk{AiChunk: out}}); err != nil {
				return
			}
			index++
		}
		if chunk.Done {
			_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{Payload: &memqlv1.MemqlServerMessage_AiChatResult{AiChatResult: &memqlv1.AiChatResult{RequestId: requestID, Message: &memqlv1.AiChatMessage{Role: "assistant", Content: text.String()}}}})
		}
	}
}

// askFailureStatus is how one failed Ask turn is reported. A refused ROUTE
// CHOICE is the caller's to fix, so it is InvalidArgument and its metadata
// carries the stable code and the half of the choice refused ("source" or
// "level") for a client to branch on without reading prose. Anything else is
// Internal, as before.
func askFailureStatus(err error) (codes.Code, string, map[string]string) {
	var choice *engine.RouteChoiceError
	if errors.As(err, &choice) {
		return codes.InvalidArgument, choice.Error(), map[string]string{"code": choice.Code, "field": choice.Field}
	}
	return codes.Internal, err.Error(), nil
}

// Register before launching work: CancelRequest may be the very next envelope.
// Only server-generated mesh IDs enter the replica-wide cancellation registry;
// client-chosen IDs are scoped to their authenticated browser session.
func (s *streamSession) beginAskRequest(ctx context.Context, requestID string) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	if _, exists := s.activeRequests.LoadOrStore(requestID, cancel); exists {
		cancel()
		return nil, nil, fmt.Errorf("request is already running")
	}
	meshID := ""
	if forwarded, ok := s.stream.(*forwardedStream); ok {
		meshID = forwarded.requestId
		if _, exists := s.service.askCancels.LoadOrStore(meshID, cancel); exists {
			s.activeRequests.Delete(requestID)
			cancel()
			return nil, nil, fmt.Errorf("forwarded request is already running")
		}
	}
	return ctx, func() {
		cancel()
		s.activeRequests.Delete(requestID)
		if meshID != "" {
			s.service.askCancels.Delete(meshID)
		}
	}, nil
}

func isAskEnvelope(envelope *memqlv1.MemqlClientMessage) bool {
	return envelope.GetAiChat().GetConversationId() != "" || envelope.GetAskVoiceStart() != nil
}
