package memql

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server/sihttp"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/agent"
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
func (s *streamSession) handleAiChat(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SIChatMsg) error {
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
		Payload: &memqlv1.MemqlServerMessage_SiChatResult{
			SiChatResult: &memqlv1.SIChatResult{
				RequestId: requestId,
				Message: &memqlv1.SIChatMessage{
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
	// stream:<requestId> instead of pushing SIStreamChunk over the forwardedStream
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
				Payload: &memqlv1.MemqlServerMessage_SiChunk{
					SiChunk: &memqlv1.SIStreamChunk{
						StreamId:  requestId,
						RequestId: requestId,
						Index:     idx,
						Chunk:     &memqlv1.SIStreamChunk_TextDelta{TextDelta: chunk.Content},
						Done:      chunk.Done,
					},
				},
			})
			idx++
		}

		if chunk.Done {
			_ = s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_SiChatResult{
					SiChatResult: &memqlv1.SIChatResult{
						RequestId: requestId,
						Message: &memqlv1.SIChatMessage{
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
func (s *streamSession) handleAiSpeech(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SISpeechMsg) error {
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
			Payload: &memqlv1.MemqlServerMessage_SiSpeechResult{
				SiSpeechResult: &memqlv1.SISpeechResult{
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
func (s *streamSession) handleAiTranscribe(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SITranscribeMsg) error {
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
			Payload: &memqlv1.MemqlServerMessage_SiTranscribeResult{
				SiTranscribeResult: &memqlv1.SITranscribeResult{
					RequestId: requestId,
					Text:      text,
				},
			},
		})
	}()

	return nil
}

// handleAiSuggest handles AI suggestion requests for spaces, agents, and groups.
func (s *streamSession) handleAiSuggest(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SISuggestMsg) error {
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

		// Each domain pulls its own required fields out of the
		// payload -- `description` is a legacy legacy-core input for
		// the three suggest-from-description flows, but newer flows
		// (e.g. spaceTitle) take a different seed key. Moving the
		// required-check into the switch keeps "what's required" a
		// property of the domain and not the handler.
		description, _ := payload["description"].(string)

		var messages []common.ChatMessage
		var postProcess func(map[string]any)
		var schema json.RawMessage
		var schemaName string

		switch domain {
		case "spaces":
			// Title-only suggestion for the CreateSpaceModal AI-describe
			// path. Under the one-assistant space model (copresent #124)
			// a space has 1+ humans plus EXACTLY ONE assistant
			// (auto-joined by the backend autoJoinAI automation); there
			// is no agent picker and no architecture choice. The client
			// sends an empty `agents` array and consumes only `title`,
			// so we neither require nor read agents here. No postProcess
			// -- there are no agentIds / architecture fields to validate.
			if strings.TrimSpace(description) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "description is required in payload")
				return
			}
			messages = sihttp.BuildSpaceSuggestMessages(description)
			schema = sihttp.SpaceSuggestSchemaJSON
			schemaName = "spaceConfigSuggest"

		case "spaceTitle":
			// Lightweight title-from-purpose path used by the
			// CreateSpaceModal Configure-manually flow. The user
			// types a short "Purpose" sentence; we turn it into a
			// compact title they can override. No agents / no
			// architecture -- just `{title}`.
			purpose, _ := payload["purpose"].(string)
			if strings.TrimSpace(purpose) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "purpose is required in payload for spaceTitle suggestions")
				return
			}
			messages = sihttp.BuildSpaceTitleSuggestMessages(purpose)
			schema = sihttp.SpaceTitleSuggestSchemaJSON
			schemaName = "spaceTitleSuggest"
			postProcess = func(suggestion map[string]any) {
				sihttp.PostProcessSpaceTitleSuggestion(suggestion)
			}

		case "agents":
			if strings.TrimSpace(description) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "description is required in payload")
				return
			}
			existingAgents := extractExistingAgents(payload)
			messages = agent.BuildAgentSuggestMessages(description, existingAgents)
			schema = agent.AgentSuggestSchemaJSON
			schemaName = "agentConfigSuggest"
			postProcess = func(suggestion map[string]any) {
				agent.PostProcessAgentSuggestion(suggestion, existingAgents)
			}

		case "groups":
			if strings.TrimSpace(description) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "description is required in payload")
				return
			}
			users := extractGroupUsers(payload)
			messages = sihttp.BuildGroupSuggestMessages(description, users)
			schema = sihttp.GroupSuggestSchemaJSON
			schemaName = "groupConfigSuggest"
			postProcess = func(suggestion map[string]any) {
				sihttp.PostProcessGroupSuggestion(suggestion, users)
			}

		case "groupDescription":
			// Lightweight description-from-name path used by the
			// GroupModal Configure-manually flow. The user types a
			// Group name; we derive a one-line description they can
			// override. Mirrors the spaceTitle domain (purpose -> title)
			// but in the opposite direction (name -> description).
			// No member roster, no name suggestion -- just `{description}`.
			name, _ := payload["name"].(string)
			if strings.TrimSpace(name) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "name is required in payload for groupDescription suggestions")
				return
			}
			messages = sihttp.BuildGroupDescriptionSuggestMessages(name)
			schema = sihttp.GroupDescriptionSuggestSchemaJSON
			schemaName = "groupDescriptionSuggest"
			postProcess = func(suggestion map[string]any) {
				sihttp.PostProcessGroupDescriptionSuggestion(suggestion)
			}

		case "agentCardSummary":
			// Body for the agent.created canvas card. Distinct from
			// the existing `agents` domain (which proposes a NAME +
			// personality for a new agent from a free-text
			// description); here the agent already exists and we just
			// need a tagline / blurb / use-cases trio for the
			// presentation card. Frontend caches the result keyed by
			// agent id so a single agent only triggers this call once
			// per browser.
			name, _ := payload["name"].(string)
			if strings.TrimSpace(name) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "name is required in payload for agentCardSummary suggestions")
				return
			}
			input := sihttp.AgentCardSummaryInput{
				Name:        name,
				Role:        stringFromMap(payload, "role"),
				RoleSlug:    stringFromMap(payload, "roleSlug"),
				Description: stringFromMap(payload, "description"),
				Personality: stringFromMap(payload, "personality"),
				Domains:     stringSliceFromMap(payload, "domains"),
				Keywords:    stringSliceFromMap(payload, "keywords"),
				Tools:       stringSliceFromMap(payload, "tools"),
			}
			messages = sihttp.BuildAgentCardSummarySuggestMessages(input)
			schema = sihttp.AgentCardSummarySchemaJSON
			schemaName = "agentCardSummarySuggest"
			postProcess = func(suggestion map[string]any) {
				sihttp.PostProcessAgentCardSummarySuggestion(suggestion)
			}

		case "spaceCardSummary":
			// Body for the space.created canvas card. Mirrors the
			// agent.created and group.created card surfaces (tagline /
			// blurb / use cases) so the welcome moment for a new
			// space looks and feels the same as for new agents and
			// groups. Distinct from the existing `spaces` (proposes
			// a NEW space) and `spaceTitle` (purpose -> title)
			// domains; this one runs in the inverse direction --
			// space already exists and we just want a friendly blurb
			// for the canvas.
			name, _ := payload["name"].(string)
			if strings.TrimSpace(name) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "name is required in payload for spaceCardSummary suggestions")
				return
			}
			privateFlag := false
			if v, ok := payload["private"].(bool); ok {
				privateFlag = v
			}
			input := sihttp.SpaceCardSummaryInput{
				Name:        name,
				Description: stringFromMap(payload, "description"),
				Kind:        stringFromMap(payload, "kind"),
				Private:     privateFlag,
				AgentRoles:  stringSliceFromMap(payload, "agentRoles"),
			}
			messages = sihttp.BuildSpaceCardSummarySuggestMessages(input)
			schema = sihttp.SpaceCardSummarySchemaJSON
			schemaName = "spaceCardSummarySuggest"
			postProcess = func(suggestion map[string]any) {
				sihttp.PostProcessSpaceCardSummarySuggestion(suggestion)
			}

		case "groupCardSummary":
			// Body for the group.created canvas card. Mirrors the
			// agent.created surface (tagline / blurb / use cases) so
			// the welcome moment for a new group looks and feels the
			// same as the welcome moment for a new agent. Distinct
			// from the existing `groups` domain (which proposes a NEW
			// group from a free-text description); here the group
			// already exists with a stable name + description and we
			// just want a friendly blurb for the canvas.
			name, _ := payload["name"].(string)
			if strings.TrimSpace(name) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "name is required in payload for groupCardSummary suggestions")
				return
			}
			memberCount := 0
			if v, ok := payload["memberCount"].(float64); ok {
				memberCount = int(v)
			}
			agentCount := 0
			if v, ok := payload["agentCount"].(float64); ok {
				agentCount = int(v)
			}
			input := sihttp.GroupCardSummaryInput{
				Name:        name,
				Description: stringFromMap(payload, "description"),
				MemberCount: memberCount,
				AgentCount:  agentCount,
				AgentRoles:  stringSliceFromMap(payload, "agentRoles"),
			}
			messages = sihttp.BuildGroupCardSummarySuggestMessages(input)
			schema = sihttp.GroupCardSummarySchemaJSON
			schemaName = "groupCardSummarySuggest"
			postProcess = func(suggestion map[string]any) {
				sihttp.PostProcessGroupCardSummarySuggestion(suggestion)
			}

		case "knowledge":
			// Describe-phase + AI-suggest path on the CoPresent
			// KnowledgeModal. The user types (or speaks) a free-text
			// description of what content the domain should cover; we
			// turn that into {name, description, category}. The model
			// is shown the existing-domain list so its proposed name
			// doesn't collide and so the category lines up with how
			// related domains have been categorised. Scope
			// (workspace / private) is collected by the modal itself
			// -- it's an authorization decision, not a generative one.
			if strings.TrimSpace(description) == "" {
				s.sendQueryError(requestId, correlate, codes.InvalidArgument, "description is required in payload")
				return
			}
			existingDomains := extractKnowledgeExistingDomains(payload)
			messages = sihttp.BuildKnowledgeSuggestMessages(description, existingDomains)
			schema = sihttp.KnowledgeSuggestSchemaJSON
			schemaName = "knowledgeConfigSuggest"
			postProcess = func(suggestion map[string]any) {
				sihttp.PostProcessKnowledgeSuggestion(suggestion)
			}

		case "guide":
			// Author an immersive voice-driven Guide (ordered Scenes) from
			// the summary of a voice intake (copresent#194). The structured
			// result is returned to copresent, which persists it via the
			// v1:guide mutations (copresent#556) and runs it via the client
			// Guide runtime (copresent#190). `intake` is the gathered
			// conversation summary (industry / role / interests / goals);
			// falls back to `description` for callers that reuse that key.
			intake := stringFromMap(payload, "intake")
			if strings.TrimSpace(intake) == "" {
				intake = description
			}
			input := sihttp.GuideSuggestInput{
				Intake:   intake,
				Kind:     stringFromMap(payload, "kind"),
				UserName: stringFromMap(payload, "userName"),
			}
			messages = sihttp.BuildGuideSuggestMessages(input)
			schema = sihttp.GuideSuggestSchemaJSON
			schemaName = "guideSuggest"
			postProcess = func(suggestion map[string]any) {
				sihttp.PostProcessGuideSuggestion(suggestion)
			}

		default:
			s.sendQueryError(requestId, correlate, codes.InvalidArgument, fmt.Sprintf("unsupported suggest domain: %q", domain))
			return
		}

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
			Payload: &memqlv1.MemqlServerMessage_SiSuggestResult{
				SiSuggestResult: &memqlv1.SISuggestResult{
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
// here so the gRPC SISuggestMsg path doesn't need to import sihttp
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

// extractExistingAgents extracts existingAgent structs from a suggest payload map.
func extractExistingAgents(payload map[string]any) []agent.ExistingAgent {
	agentsRaw, ok := payload["existingAgents"].([]any)
	if !ok {
		return nil
	}
	var agents []agent.ExistingAgent
	for _, a := range agentsRaw {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		existing := agent.ExistingAgent{
			ID:          stringFromMap(m, "id"),
			Name:        stringFromMap(m, "name"),
			Description: stringFromMap(m, "description"),
		}
		if caps, ok := m["capabilities"].(map[string]any); ok {
			existing.Capabilities = &agent.AgentCapabilities{
				Domains: stringSliceFromMap(caps, "domains"),
				Tools:   stringSliceFromMap(caps, "tools"),
			}
		}
		agents = append(agents, existing)
	}
	return agents
}

// extractKnowledgeExistingDomains extracts existing knowledge domain
// descriptors from a suggest payload map. The CoPresent
// KnowledgeModal passes the workspace's existing domains under
// "existingDomains" so the model can avoid name collisions and pick
// a category consistent with how related domains are categorised.
func extractKnowledgeExistingDomains(payload map[string]any) []sihttp.KnowledgeExistingDomain {
	domainsRaw, ok := payload["existingDomains"].([]any)
	if !ok {
		return nil
	}
	var domains []sihttp.KnowledgeExistingDomain
	for _, d := range domainsRaw {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		domains = append(domains, sihttp.KnowledgeExistingDomain{
			Name:        stringFromMap(m, "name"),
			Description: stringFromMap(m, "description"),
			Category:    stringFromMap(m, "category"),
		})
	}
	return domains
}

// extractGroupUsers extracts groupUser structs from a suggest payload map.
func extractGroupUsers(payload map[string]any) []sihttp.GroupUser {
	usersRaw, ok := payload["users"].([]any)
	if !ok {
		return nil
	}
	var users []sihttp.GroupUser
	for _, u := range usersRaw {
		m, ok := u.(map[string]any)
		if !ok {
			continue
		}
		users = append(users, sihttp.GroupUser{
			ID:          stringFromMap(m, "id"),
			DisplayName: stringFromMap(m, "displayName"),
			Email:       stringFromMap(m, "email"),
			Role:        stringFromMap(m, "role"),
		})
	}
	return users
}

func stringFromMap(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func stringSliceFromMap(m map[string]any, key string) []string {
	arr, ok := m[key].([]any)
	if !ok {
		return nil
	}
	var result []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			result = append(result, s)
		}
	}
	return result
}
