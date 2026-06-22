package memql

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/stt"
)

// generateErrorId creates a short unique error ID for tracing across logs.
func generateErrorId() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("ERR-%x", b)
}

// sendAiError sends a QueryErrorMsg for AI operation failures, including an error ID in metadata.
func (s *streamSession) sendAiError(requestId, correlate string, message string, err error) error {
	eid := generateErrorId()
	if s.logger != nil {
		s.logger.Error(message, "error", err, "errorId", eid, "requestId", requestId)
	}
	return s.sendQueryErrorWithMetadata(requestId, correlate, codes.Internal, message, map[string]string{
		"errorId": eid,
	})
}

// handleAiChat handles non-streaming and streaming chat requests.
func (s *streamSession) handleAiChat(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.AiChatMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "si_chat request missing")
	}

	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	// BFF proxy: on BFF binaries, forward every aiChat envelope to a
	// worker peer of the configured target type (Agent by default).
	// The wire protocol back to the client is unchanged -- responses
	// arrive as standard MemqlServerMessage payloads on the same
	// bidirectional stream. shouldProxyAI short-circuits to false on
	// worker binaries so they execute locally.
	if s.shouldProxyAI(nodeTargetForChat()) {
		// Streaming chat over the substrate (memql#1266): forward the trigger to
		// the worker as usual, but consume the streamed token deltas from the
		// durable substrate (stream:<requestId>) rather than relaying the
		// forwardedStream responses -- so a streamed turn survives this bff replica
		// dying mid-stream (the next owner replays from the cursor). The worker's
		// handleAiChatStream produces to the substrate (not the forward stream)
		// under the same gate, so there is no double delivery. Non-streaming chat
		// and the non-substrate path keep the plain relay.
		if msg.GetStream() && s.service.streamingOverSubstrate() {
			return s.proxyAIStream(envelope, requestId, nodeTargetForChat(), s.consumeTokenStream)
		}
		return s.proxyAI(envelope, requestId, nodeTargetForChat())
	}

	if s.service.engine == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "MemQL engine unavailable")
	}

	if len(msg.GetMessages()) == 0 {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument, "messages array is required and must not be empty")
	}

	// Convert proto messages to common.ChatMessage
	messages := make([]common.ChatMessage, 0, len(msg.GetMessages()))
	for _, m := range msg.GetMessages() {
		messages = append(messages, common.ChatMessage{
			Role:    m.GetRole(),
			Content: m.GetContent(),
			Name:    m.GetName(),
		})
	}

	correlate := envelope.GetMessageId()

	if msg.GetStream() {
		go s.handleAiChatStream(requestId, correlate, messages, msg.GetProvider())
		return nil
	}

	go s.handleAiChatNonStream(requestId, correlate, messages, msg.GetProvider())
	return nil
}

func (s *streamSession) handleAiChatNonStream(requestId, correlate string, messages []common.ChatMessage, providerName string) {
	var chatProvider common.ChatAIProvider

	if providerName != "" {
		entry, ok := s.service.engine.ProviderEntry(providerName)
		if !ok || !entry.Available {
			s.sendQueryError(requestId, correlate, codes.InvalidArgument, fmt.Sprintf("provider %q not found", providerName))
			return
		}
		cp, ok := entry.Client.(common.ChatAIProvider)
		if !ok {
			s.sendQueryError(requestId, correlate, codes.InvalidArgument, fmt.Sprintf("provider %q does not support chat", providerName))
			return
		}
		chatProvider = cp
	} else {
		chatProvider = s.service.engine.DefaultChatProvider()
		if chatProvider == nil {
			s.sendQueryError(requestId, correlate, codes.Internal, "no non-streaming chat provider available")
			return
		}
	}

	ctx := s.stream.Context()
	result, err := chatProvider.CallChat(ctx, messages)
	if err != nil {
		s.sendAiError(requestId, correlate, "chat completion failed", err)
		return
	}

	_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AiChatResult{
			AiChatResult: &memqlv1.AiChatResult{
				RequestId: requestId,
				Message: &memqlv1.AiChatMessage{
					Role:    "assistant",
					Content: result,
				},
			},
		},
	})
}

func (s *streamSession) handleAiChatStream(requestId, correlate string, messages []common.ChatMessage, providerName string) {
	var streamProvider common.ChatStreamProvider

	if providerName != "" {
		streamProvider = s.service.engine.ChatStreamProviderByName(providerName)
		if streamProvider == nil {
			s.sendQueryError(requestId, correlate, codes.InvalidArgument, fmt.Sprintf("provider %q does not support streaming", providerName))
			return
		}
	} else {
		streamProvider = s.service.engine.ChatStreamProvider()
		if streamProvider == nil {
			s.sendQueryError(requestId, correlate, codes.Internal, "no streaming provider available")
			return
		}
	}

	ctx := s.stream.Context()
	chunks, err := streamProvider.CallChatStream(ctx, messages)
	if err != nil {
		s.sendAiError(requestId, correlate, "chat stream failed", err)
		return
	}

	// Substrate cutover (memql#1266): on a mesh worker the durable substrate is
	// the streamed-response delivery path -- produce ordered token frames to
	// stream:<requestId> instead of pushing AiStreamChunk over the forwardedStream
	// (the star-mesh drop/dup path). The WS-owning bff consumes them via
	// consumeTokenStream, surviving a mid-stream replica switch. Single-node /
	// non-mesh binaries (substrate nil) keep the direct push below.
	if s.streamProducerOverSubstrate() {
		s.produceTokenStreamToSubstrate(ctx, requestId, chunks)
		return
	}

	var idx int64
	var fullContent strings.Builder

	for chunk := range chunks {
		if chunk.Error != nil {
			s.sendAiError(requestId, correlate, "chat stream error", chunk.Error)
			return
		}

		if chunk.Content != "" {
			fullContent.WriteString(chunk.Content)
			_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_AiChunk{
					AiChunk: &memqlv1.AiStreamChunk{
						StreamId:  requestId,
						RequestId: requestId,
						Index:     idx,
						Chunk:     &memqlv1.AiStreamChunk_TextDelta{TextDelta: chunk.Content},
						Done:      chunk.Done,
					},
				},
			})
			idx++
		}

		if chunk.Done {
			_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_AiChatResult{
					AiChatResult: &memqlv1.AiChatResult{
						RequestId: requestId,
						Message: &memqlv1.AiChatMessage{
							Role:    "assistant",
							Content: fullContent.String(),
						},
					},
				},
			})
		}
	}
}

// handleAiSpeech handles text-to-speech requests.
func (s *streamSession) handleAiSpeech(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.AiSpeechMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "si_speech request missing")
	}

	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if s.shouldProxyAI(nodeTargetForSpeech()) {
		return s.proxyAI(envelope, requestId, nodeTargetForSpeech())
	}

	if s.service.engine == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "MemQL engine unavailable")
	}

	if strings.TrimSpace(msg.GetInput()) == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument, "input field is required")
	}

	correlate := envelope.GetMessageId()

	go func() {
		var ttsProvider memqlengine.TTSAIProvider
		if prov := msg.GetProvider(); prov != "" {
			var ok bool
			ttsProvider, ok = s.service.engine.TTSProviderByName(prov)
			if !ok {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "TTS provider not found: "+prov)
				return
			}
		} else {
			ttsProvider = s.service.engine.TTSProvider()
			if ttsProvider == nil {
				s.sendQueryError(requestId, correlate, codes.Internal, "no TTS provider available")
				return
			}
		}

		ctx := s.stream.Context()
		audioBytes, err := ttsProvider.Synthesize(ctx, msg.GetInput(), msg.GetVoice())
		if err != nil {
			s.sendAiError(requestId, correlate, "speech synthesis failed", err)
			return
		}

		format := msg.GetFormat()
		if format == "" {
			format = "wav"
		}

		_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_AiSpeechResult{
				AiSpeechResult: &memqlv1.AiSpeechResult{
					RequestId: requestId,
					Audio:     audioBytes,
					Format:    format,
				},
			},
		})
	}()

	return nil
}

// handleAiTranscribe handles speech-to-text requests.
func (s *streamSession) handleAiTranscribe(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.AiTranscribeMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "si_transcribe request missing")
	}

	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if s.shouldProxyAI(nodeTargetForTranscribe()) {
		return s.proxyAI(envelope, requestId, nodeTargetForTranscribe())
	}

	if s.service.sttProvider == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "transcription is not configured")
	}

	if strings.TrimSpace(msg.GetAudio()) == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument, "audio field is required")
	}

	correlate := envelope.GetMessageId()

	go func() {
		audioBytes, err := base64.StdEncoding.DecodeString(msg.GetAudio())
		if err != nil {
			s.sendQueryError(requestId, correlate, codes.InvalidArgument, "invalid base64 audio data")
			return
		}

		mimeType := msg.GetMimeType()
		if mimeType == "" {
			mimeType = "audio/webm"
		}

		ctx := s.stream.Context()
		session, err := s.service.sttProvider.StartStream(ctx, stt.StreamConfig{
			Format:     sttFormatFromMIME(mimeType),
			SampleRate: 16000,
			Channels:   1,
		})
		if err != nil {
			s.sendAiError(requestId, correlate, "failed to start transcription", err)
			return
		}

		if err := session.SendAudio(audioBytes); err != nil {
			session.Close()
			s.sendAiError(requestId, correlate, "failed to process audio", err)
			return
		}

		result, err := session.Finalize(ctx)
		if err != nil {
			s.sendAiError(requestId, correlate, "transcription failed", err)
			return
		}

		text := ""
		if result != nil {
			text = result.Text
		}

		_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_AiTranscribeResult{
				AiTranscribeResult: &memqlv1.AiTranscribeResult{
					RequestId: requestId,
					Text:      text,
				},
			},
		})
	}()

	return nil
}

// handleAiSuggest handles AI suggestion requests for spaces, agents, and groups.
func (s *streamSession) handleAiSuggest(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.AiSuggestMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "si_suggest request missing")
	}

	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	domain := strings.TrimSpace(msg.GetDomain())

	if domain == "" {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument, "domain is required")
	}

	if s.shouldProxyAI(nodeTargetForSuggest()) {
		return s.proxyAI(envelope, requestId, nodeTargetForSuggest())
	}

	if s.service.engine == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unavailable, "MemQL engine unavailable")
	}

	correlate := envelope.GetMessageId()

	// Look up the registered handler for this domain BEFORE spawning the
	// goroutine so an unsupported domain returns the same typed
	// InvalidArgument error synchronously. The suggest-domain surface is
	// extension-point driven (memql#1959): the 9 CoPresent product domains
	// register from the pack under the `copresent` build tag, `knowledge`
	// registers from core. Engine-only core builds carry only the core
	// domains -- which is exactly the zero-CoPresent-refs G3 goal.
	handler := memqlengine.LookupSuggestDomain(domain)
	if handler == nil {
		return s.sendQueryError(requestId, correlate, codes.InvalidArgument,
			fmt.Sprintf("unsupported suggest domain: %q (registered: %v)", domain, memqlengine.RegisteredSuggestDomains()))
	}

	go func() {
		suggestStart := time.Now()

		// Deserialize the Struct payload into a map
		var payload map[string]any
		if msg.GetPayload() != nil {
			payload = msg.GetPayload().AsMap()
		}
		if payload == nil {
			payload = make(map[string]any)
		}

		// The domain handler pulls its own required fields out of the
		// payload, builds the rendered prompt + schema, and returns an
		// optional post-process pass. A *SuggestValidationError maps to the
		// same codes.InvalidArgument "X is required in payload" error the old
		// per-domain checks emitted; any other error is an internal failure.
		plan, err := handler(memqlengine.SuggestContext{Payload: payload})
		if err != nil {
			var ve *memqlengine.SuggestValidationError
			if errors.As(err, &ve) {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, ve.Message)
				return
			}
			s.sendAiError(requestId, correlate, fmt.Sprintf("%s suggestion failed", domain), err)
			return
		}
		messages := plan.Messages
		schema := plan.Schema
		schemaName := plan.SchemaName
		postProcess := plan.PostProcess

		ctx := s.stream.Context()
		// Prefer structured output: the provider enforces the JSON
		// schema so parse / field-shape failures are eliminated
		// before they reach the client. Falls back to regular chat
		// when no structured-capable provider is registered.
		result, err := callSuggestWithSchema(ctx, s.service.engine, messages, schemaName, schema)
		if err != nil {
			s.sendAiError(requestId, correlate, fmt.Sprintf("%s suggestion failed", domain), err)
			return
		}

		var suggestion map[string]any
		if err := json.Unmarshal([]byte(result), &suggestion); err != nil {
			s.sendAiError(requestId, correlate, fmt.Sprintf("%s suggestion returned invalid JSON", domain), err)
			return
		}

		if s.logger != nil {
			s.logger.Info("AI suggest completed", "domain", domain, "durationMs", time.Since(suggestStart).Milliseconds())
		}

		if postProcess != nil {
			postProcess(suggestion)
		}

		resultStruct, err := structpb.NewStruct(suggestion)
		if err != nil {
			s.sendAiError(requestId, correlate, "failed to serialize suggestion result", err)
			return
		}

		_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_AiSuggestResult{
				AiSuggestResult: &memqlv1.AiSuggestResult{
					RequestId: requestId,
					Domain:    domain,
					Result:    resultStruct,
				},
			},
		})
	}()

	return nil
}

// sttFormatFromMIME converts a MIME type to an STT format string.
func sttFormatFromMIME(mimeType string) string {
	switch mimeType {
	case "audio/webm":
		return "webm"
	case "audio/wav", "audio/wave":
		return "pcm16"
	case "audio/ogg":
		return "opus"
	default:
		return "webm"
	}
}

// callSuggestWithSchema routes a suggest call through the structured
// chat provider when available, falling back to the regular suggest
// chat provider. The HTTP handlers in component/server/sihttp/ go
// through a sibling helper with the same contract; this one lives
// here so the gRPC AiSuggestMsg path doesn't need to import sihttp
// just for the plumbing.
func callSuggestWithSchema(
	ctx context.Context,
	engine *memqlengine.MemQLEngine,
	messages []common.ChatMessage,
	schemaName string,
	schema json.RawMessage,
) (string, error) {
	if engine == nil {
		return "", fmt.Errorf("engine unavailable")
	}
	if structured := engine.StructuredChatProvider(); structured != nil {
		return structured.CallChatStructured(ctx, messages, common.StructuredSchema{
			Name:        schemaName,
			Description: schemaName + " output",
			Schema:      schema,
			Strict:      true,
		})
	}
	provider := engine.SuggestChatProvider()
	if provider == nil {
		return "", fmt.Errorf("no non-streaming chat provider available")
	}
	return provider.CallChat(ctx, messages)
}
