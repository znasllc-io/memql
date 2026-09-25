package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	engine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"
)

func (s *streamSession) handleAskStream(ctx context.Context, requestID, correlate string, request *memqlv1.AiChatMsg) {
	ctx = engine.WithProviderOverride(ctx, request.GetProvider())
	chunks := make(chan common.StreamChunk, 16)
	send := func(chunk common.StreamChunk) {
		select {
		case chunks <- chunk:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(chunks)
		_, err := s.service.engine.RunAsk(ctx, request.GetConversationId(), requestID, request.GetMessages()[0].GetContent(), request.GetPageContext(), func(text string) { send(common.StreamChunk{Content: text}) }, func(event engine.AskEvent) {
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
			_ = s.sendQueryError(requestID, correlate, codes.Internal, chunk.Error.Error())
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
