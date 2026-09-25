package memql

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// The AI handlers reach a model through the ENGINE'S RESOLVER and nowhere else
// (epic memql#5127, design D2). Each one declares the LEVEL the call needs and
// the MODALITY it derives; the router picks the provider, records the
// decision, and is the only thing in the tree that walks the registry.
//
// A caller-named provider is a PIN, not a lookup. It rides
// ResolveRequest.ExplicitProvider, so it wins over every rule and still passes
// through the doors, the ceiling check and the ledger. The three arms this
// replaced -- "provider %q not found", "no non-streaming chat provider
// available", "no streaming provider available" -- each answered a different
// question with the same shrug, and none of them could say which of the four
// doors was shut or what to do about it.

// chatResolveRequest is the non-streaming chat turn: an Ask, answered whole.
//
// STRONG rather than fast, because this is a person's question and the answer
// is the product. The context floor is estimated from the messages the call
// actually carries -- a floor of zero admits every entry and reads on the
// decision record exactly like a floor that was measured and cleared.
func chatResolveRequest(messages []common.ChatMessage, providerName string) airoute.ResolveRequest {
	return airoute.ResolveRequest{
		Level:            airoute.LevelStrong,
		Modality:         airoute.ModalityChat,
		PromptName:       askPromptName,
		Needs:            airoute.Needs{MinContextTokens: minContextForMessages(messages)},
		ExplicitProvider: providerName,
	}
}

// chatStreamResolveRequest is the same turn with the tokens arriving as they
// are produced. Same level and same prompt; only the modality differs, because
// only the interface the provider must satisfy differs.
func chatStreamResolveRequest(messages []common.ChatMessage, providerName string) airoute.ResolveRequest {
	return airoute.ResolveRequest{
		Level:            airoute.LevelStrong,
		Modality:         airoute.ModalityStreamingChat,
		PromptName:       askPromptName,
		Needs:            airoute.Needs{MinContextTokens: minContextForMessages(messages)},
		ExplicitProvider: providerName,
	}
}

// suggestResolveRequest is a suggestion: short, schema-shaped, and nobody is
// reading prose. FAST, and structured -- the schema is the point of the call,
// so Needs.Structured is asserted rather than left to the modality.
//
// It carries no PromptName: the rendered messages come from whichever suggest
// domain registered the handler, and naming one here would be a guess about
// somebody else's prompt.
func suggestResolveRequest(messages []common.ChatMessage) airoute.ResolveRequest {
	return airoute.ResolveRequest{
		Level:    airoute.LevelFast,
		Modality: airoute.ModalityStructured,
		Needs: airoute.Needs{
			Structured:       true,
			MinContextTokens: minContextForMessages(messages),
		},
	}
}

// askPromptName is what a rule branches on to recognise the interactive Ask
// turn. It is the name the two chat handlers declare and nothing else does.
const askPromptName = "ask"

// minContextForMessages is the context-window floor a chat turn needs: every
// message's text, plus the default completion budget.
func minContextForMessages(messages []common.ChatMessage) int {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		parts = append(parts, m.Content)
	}
	return airoute.EstimateMinContextTokensFor(0, parts...)
}

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

// sendAiModelError answers one failed AI call, and its whole job is to keep
// three conditions that need three different fixes from wearing one code.
//
//   - NO DOOR IS OPEN is FailedPrecondition. The cluster is working and the
//     call is well-formed; what is missing is a model somebody can reach --
//     a laptop is asleep, an app is signed out, a ceiling is reached. The
//     message NAMES THE REFUSAL CODE so a client can match on it rather than
//     on wording, and carries the door report, which is the only part with an
//     action in it: "no provider available" sends a person nowhere, while
//     "your laptop is offline; the federation hop is over its ceiling" names
//     the two places to look.
//   - THE RESOLVER IS UNWIRED is Internal, deliberately NOT FailedPrecondition.
//     It means nobody installed the router on this node -- a boot-wiring
//     fault, fixed in app/ by whoever deploys, not by the person holding the
//     laptop. Reporting it as an unavailable model sends them to their fleet
//     for something no fleet can fix.
//   - Anything else is Internal with an error id, as before.
func (s *streamSession) sendAiModelError(requestId, correlate, message string, err error) {
	text, metadata, isRefusal := aiRefusalStatus(err)
	if !isRefusal {
		s.sendAiError(requestId, correlate, message, err)
		return
	}
	if s.logger != nil {
		s.logger.Warn(message, "error", err, "refusalCode", metadata["refusalCode"], "requestId", requestId)
	}
	_ = s.sendQueryErrorWithMetadata(requestId, correlate, codes.FailedPrecondition, text, metadata)
}

// aiRefusalStatus reports whether err is a router refusal and, if so, the
// message and metadata the FailedPrecondition carries.
//
// It is a PURE function over the error so the classification can be tested
// without a stream, a session or a server: the thing worth checking is which
// of the three conditions an error lands in and whether the code survives into
// the message, and none of that is about gRPC plumbing.
func aiRefusalStatus(err error) (message string, metadata map[string]string, ok bool) {
	// Checked FIRST and by identity, not by message. An unwired resolver is a
	// boot-wiring fault whose sentence deliberately says "not an unavailable
	// provider" -- and if its wording ever drifted into naming a refusal code,
	// a message match below would silently start reporting it as a shut door
	// and send an operator to look at their fleet.
	if errors.Is(err, memqlengine.ErrAIResolverUnwired) {
		return "", nil, false
	}
	code, doors, isRefusal := work.DoorsFrom(err)
	if !isRefusal {
		return "", nil, false
	}
	// The code leads unless the rendered error already carries it. The router
	// builds its message that way on purpose (work.InferenceRefusalCode reads
	// it back out of a recorded string), so this prefix only ever fires for a
	// refusal that travelled here some other way.
	message = err.Error()
	if !strings.Contains(message, code) {
		message = code + ": " + message
	}
	metadata = map[string]string{"refusalCode": code}
	if shut := doorsShutIn(doors); shut != "" {
		metadata["doorsShut"] = shut
	}
	return message, metadata, true
}

// doorsShutIn is the SET of doors that did not open, deduplicated in the order
// the chain tried them. The full report is in the message; this is the summary
// a client can branch on without parsing prose.
func doorsShutIn(doors []work.DoorReport) string {
	seen := make(map[string]bool, len(doors))
	names := make([]string, 0, len(doors))
	for _, d := range doors {
		if d.Door == "" || seen[d.Door] {
			continue
		}
		seen[d.Door] = true
		names = append(names, d.Door)
	}
	return strings.Join(names, ",")
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
		if msg.GetStream() && msg.GetConversationId() == "" && s.service.streamingOverSubstrate() {
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
	ctx, err := chatCallContext(s.stream.Context(), msg.GetProvider(), msg.GetFleetRegistrationId())
	if err != nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument, err.Error())
	}

	if msg.GetConversationId() != "" {
		if !msg.GetStream() || len(msg.GetMessages()) != 1 || msg.GetMessages()[0].GetRole() != "user" {
			return s.sendQueryError(requestId, envelope.GetMessageId(), codes.InvalidArgument, "Ask accepts one user message and a server-owned conversation")
		}
		ctx, release, err := s.beginAskRequest(ctx, requestId)
		if err != nil {
			return s.sendQueryError(requestId, envelope.GetMessageId(), codes.AlreadyExists, err.Error())
		}
		go func() {
			defer release()
			s.handleAskStream(ctx, requestId, envelope.GetMessageId(), msg)
		}()
		return nil
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
		go s.handleAiChatStream(ctx, requestId, correlate, messages, msg.GetProvider())
		return nil
	}

	go s.handleAiChatNonStream(ctx, requestId, correlate, messages, msg.GetProvider())
	return nil
}

// chatCallContext binds the pin on the RECEIVING replica, after the complete
// AiChat envelope has crossed the BFF hop. The stream context stays unchanged.
func chatCallContext(ctx context.Context, provider, registrationId string) (context.Context, error) {
	registrationId = strings.TrimSpace(registrationId)
	if registrationId != "" {
		_, fleet := memqlengine.IsFleetReference(provider)
		_, selector := memqlengine.IsFleetSelector(provider)
		if !fleet || selector || memqlengine.IsFleetWildcard(provider) {
			return nil, errors.New("a selected machine requires an explicit fleet:<modelId> provider")
		}
		const concept = "v1:worker:registration"
		if err := id.ValidateShortId(concept, registrationId); err != nil {
			return nil, errors.New("invalid fleet registration id")
		}
		_, shortId, err := id.ParseNodeId(registrationId)
		if err != nil || shortId == "" {
			return nil, errors.New("invalid fleet registration id")
		}
		registrationId = id.BuildNodeId(concept, shortId)
	}
	return common.ContextWithFleetRegistration(ctx, registrationId), nil
}

func (s *streamSession) handleAiChatNonStream(ctx context.Context, requestId, correlate string, messages []common.ChatMessage, providerName string) {
	chatProvider, _, err := memqlengine.ResolveAITyped[common.ChatAIProvider](
		ctx, s.service.engine, chatResolveRequest(messages, providerName))
	if err != nil {
		s.sendAiModelError(requestId, correlate, "no model is reachable for this chat turn", err)
		return
	}

	result, err := chatProvider.CallChat(ctx, messages)
	if err != nil {
		s.sendAiModelError(requestId, correlate, "chat completion failed", err)
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

func (s *streamSession) handleAiChatStream(ctx context.Context, requestId, correlate string, messages []common.ChatMessage, providerName string) {
	// Provider work belongs to this turn, not the lifetime of the peer stream.
	// A failed response publication must release the model and its capacity.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	streamProvider, _, err := memqlengine.ResolveAITyped[common.ChatStreamProvider](
		ctx, s.service.engine, chatStreamResolveRequest(messages, providerName))
	if err != nil {
		s.sendAiModelError(requestId, correlate, "no model is reachable for this streaming chat turn", err)
		return
	}

	chunks, err := streamProvider.CallChatStream(ctx, messages)
	if err != nil {
		s.sendAiModelError(requestId, correlate, "chat stream failed", err)
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
			s.sendAiModelError(requestId, correlate, "chat stream error", chunk.Error)
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
	// extension-point driven (memql#1959): the 9 product suggest domains
	// register from the pack under its build tag, `knowledge`
	// registers from core. Engine-only core builds carry only the core
	// domains -- which is exactly the zero-product-refs G3 goal.
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
		plan, err := handler(memqlengine.SuggestContext{
			Payload: payload,
			// So a domain can keep its instruction in .memql rather than in a
			// Go string literal (memql#4654). The handler gets the render
			// function and nothing else -- not the engine, not the session.
			RenderPrompt: s.service.engine.RenderPrompt,
		})
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
			// sendAiModelError rather than sendAiError: this one call can fail
			// because no door is open, which is a condition the person asking
			// can act on, and it must not arrive wearing the same Internal
			// code as a provider that returned malformed output.
			s.sendAiModelError(requestId, correlate, fmt.Sprintf("%s suggestion failed", domain), err)
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

// callSuggestWithSchema runs a suggest call against a structured-output
// provider the router picked.
//
// THE PLAIN-CHAT FALLBACK IS GONE, and its absence is the point. It existed
// because "is a structured-capable provider registered" was a question this
// call site had to answer for itself, and its answer when the registry had
// none was to ask for prose and hope it parsed as JSON -- which produced an
// invalid-JSON error one layer up, naming the model rather than the missing
// capability. The request now DECLARES Needs.Structured, so the chain refuses
// with the door report instead of degrading into a shape the caller cannot
// use.
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
	structured, _, err := memqlengine.ResolveAITyped[common.ChatStructuredProvider](
		ctx, engine, suggestResolveRequest(messages))
	if err != nil {
		return "", err
	}
	return structured.CallChatStructured(ctx, messages, common.StructuredSchema{
		Name:        schemaName,
		Description: schemaName + " output",
		Schema:      schema,
		Strict:      true,
	})
}
