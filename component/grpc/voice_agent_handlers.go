package memql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// voiceTimingKey is the discoverable structured-log key every voice-path
// timing record carries (#1426). The voice-agent process logs its spans
// under the same key (integrations/voice/agent/timing.go), so one grep
// (`voice_timing`) yields the full client+server breakdown of session setup,
// per-turn latency, and tool round-trips.
const voiceTimingKey = "voice_timing"

// logVoiceTiming emits one timing record for a completed phase that began at
// start: `voice_timing=<phase>` + `duration_ms` + caller context. Server-side
// twin of the voice-agent helper; measurement only, nil-logger safe.
func logVoiceTiming(logger *slog.Logger, phase string, start time.Time, attrs ...any) {
	if logger == nil {
		return
	}
	// append (not a len()+k make) so CodeQL's allocation-size-overflow rule
	// has no arithmetic to flag; the attr lists here are tiny constants.
	args := append([]any{voiceTimingKey, phase, "duration_ms", time.Since(start).Milliseconds()}, attrs...)
	logger.Info("voice timing", args...)
}

// randHex returns 2*n lowercase-hex characters of crypto/rand entropy.
// Used to disambiguate utterance ids without leaking the speaker's
// canonical participant id into the slug (which the engine's
// canonicalizer would mis-parse as a participant-typed row).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Cryptographic-rand failure is so unrecoverable we'd rather
		// crash than emit predictable ids. In practice this never fires.
		panic(fmt.Errorf("rand.Read failed: %w", err))
	}
	return hex.EncodeToString(b)
}

// -----------------------------------------------------------------------------
// Realtime Voice + Video gRPC handlers (Initiative C, Phase 6 wiring).
// -----------------------------------------------------------------------------
//
// The Go voice-agent process (integrations/voice/agent) authenticates as a
// service account and speaks the VoiceAgent* message surface. These handlers
// translate that surface into:
//
//   1. Custom voice events on the event bus (partial transcripts) -- so
//      the chat UI can ghost-text the in-progress utterance via the
//      existing subscription path.
//   2. Graph row inserts (finals) via mutationSend{Text,Private}Utterance,
//      thread-routed by the VoiceAgentTurnRequest.ThreadContext enum.
//   3. A bounded-time subscription wait for the GA's reply utterance
//      (memql cognition automation fires when the user's utterance row
//      lands; the GA's reply utterance row arrives back as a graph
//      event) and streams the reply text back as VoiceAgentTurnDelta
//      + VoiceAgentTurnComplete.
//
// Specialists are NEVER voice. If the conductor dispatches them they
// land in chat via the normal agent dispatch path inside memql; they
// never reach the voice-agent. The turn-request subscription filters
// for participantType=agent AND participantId=ga_agent_id so only the
// GA's reply triggers the TurnDelta stream.

// voiceTurnWaitTimeout is the maximum we'll block a VoiceAgentTurnRequest
// waiting for the GA's reply utterance to land. Aligns with the agent
// tool-loop's worst-case latency budget (long tool calls + retries can
// push close to 30s); past this we surface a structured error and the
// voice-agent's LLM plugin closes the LiveKit stream cleanly.
const voiceTurnWaitTimeout = 30 * time.Second

// voicePartialEventTopic is the event-bus subject for streaming
// partial transcripts. Subscribed by the chat UI via the existing
// SUBSCRIPTION_KIND_DOMAIN_EVENTS path so ghost-text rendering "just
// works" without needing a graph row.
const voicePartialEventTopic = "voice.partial.utterance"

// voiceGateDirectiveTopic is the event-bus subject the cognition gate
// (#477/#479) publishes its per-turn decision on: engage(mode, brevity) or
// defer, INSTEAD of authoring a reply. The voice turn relay subscribes to it
// and forwards the directive on VoiceAgentTurnComplete so the realtime model
// authors the words itself. Payload keys: spaceId, gaAgentId, engage (bool),
// mode, brevity, utteranceId. When no directive is published the relay falls
// back to the legacy authored-utterance path (extractGAReplyFromEvent).
const voiceGateDirectiveTopic = "voice.gate.directive"

// voiceGateDirective is the decoded gate decision carried on
// voiceGateDirectiveTopic.
type voiceGateDirective struct {
	engage      bool
	mode        string
	brevity     string
	utteranceId string
	grounding   string
}

// extractVoiceGateDirective decodes a voiceGateDirectiveTopic event for the
// target (space, agent), or returns ok=false when it is for a different
// space/agent or malformed.
func extractVoiceGateDirective(e events.Event, spaceId, gaAgentId string) (voiceGateDirective, bool) {
	if e.Payload == nil {
		return voiceGateDirective{}, false
	}
	if !spaceIdMatches(asString(e.Payload, "spaceId"), spaceId) {
		return voiceGateDirective{}, false
	}
	// gaAgentId match tolerant of canonical-vs-slug, same shape as the channel
	// override helper.
	if evAgent := asString(e.Payload, "gaAgentId"); evAgent != "" &&
		!channelOverrideTargetsAgent(evAgent, gaAgentId) {
		return voiceGateDirective{}, false
	}
	d := voiceGateDirective{
		mode:        strings.TrimSpace(asString(e.Payload, "mode")),
		brevity:     strings.TrimSpace(asString(e.Payload, "brevity")),
		utteranceId: asString(e.Payload, "utteranceId"),
		grounding:   asString(e.Payload, "grounding"),
	}
	if v, ok := e.Payload["engage"].(bool); ok {
		d.engage = v
	}
	return d, true
}

// handleVoiceAgentSessionStart binds a LiveKit room to a memql space.
// Phase 6: still returns the persona-resolver placeholder; Phase 9
// fills in avatar_persona_id, Phase 7 fills in the gate state.
func (s *streamSession) handleVoiceAgentSessionStart(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.VoiceAgentSessionStart) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	spaceId := strings.TrimSpace(msg.GetSpaceId())
	gaAgentId := strings.TrimSpace(msg.GetGaAgentId())
	if spaceId == "" || gaAgentId == "" {
		return s.sendQueryError(requestId, correlate, codes.InvalidArgument, "spaceId and gaAgentId are required")
	}

	// Session-setup timing (#1426): every engine call below runs SYNCHRONOUSLY
	// before the SessionAck the voice-agent is waiting on -- on a slow DB these
	// are the session-setup chokepoints. Each is timed individually plus the
	// handler total, all under the voice_timing key.
	setupStart := time.Now()

	// The voice-agent currently sends a placeholder ga_agent_id of
	// the form "<space_slug>-ga" (the real wiring through the LiveKit
	// token is a follow-up). Override rows and agent records are keyed by the GA's real
	// canonical agent id, so the placeholder can't address them. Look
	// the real id up from the space's group-GA participant; fall back
	// to the wire value when the lookup fails so we don't regress
	// existing callers that DO send a real id.
	gaResolveStart := time.Now()
	if resolved := resolveGroupGAAgentId(s, spaceId); resolved != "" {
		gaAgentId = resolved
	}
	logVoiceTiming(s.logger, "server.session_start.resolve_ga_id", gaResolveStart, "space_id", spaceId)

	// Resolve the GA's initial audio + video modes from the persistent
	// override rows. Without this the voice-agent always saw
	// "mirror_user" (hardcoded), so toggling Sofia's video icon to
	// `always_on` never reached the avatar-gate at session start.
	audioModeStart := time.Now()
	audioMode := resolveInitialChannelMode(s, spaceId, gaAgentId, "audio")
	logVoiceTiming(s.logger, "server.session_start.audio_mode", audioModeStart, "space_id", spaceId)
	videoModeStart := time.Now()
	videoMode := resolveInitialChannelMode(s, spaceId, gaAgentId, "video")
	logVoiceTiming(s.logger, "server.session_start.video_mode", videoModeStart, "space_id", spaceId)

	// Load the GA's persona identity (#478) from the agent record so the voice
	// session renders the real agent via the shared agentdef identity block,
	// not the neutral "Assistant, General Assistant" default. Best-effort: a
	// query failure leaves the fields empty and the voice builder degrades to
	// the neutral default, so the session still comes up.
	personaStart := time.Now()
	persona := resolveAgentPersona(s, gaAgentId)
	logVoiceTiming(s.logger, "server.session_start.persona", personaStart, "space_id", spaceId)
	logVoiceTiming(s.logger, "server.session_start.total", setupStart, "space_id", spaceId)

	if s.logger != nil {
		s.logger.Info("voice-agent session start",
			"request_id", requestId,
			"space_id", spaceId,
			"ga_agent_id", gaAgentId,
			"room_name", msg.GetRoomName(),
			"avatar_vendor", msg.GetAvatarVendor(),
			"audio_mode", audioMode,
			"video_mode", videoMode,
			"persona_name", persona.name,
			"persona_populated", persona.name != "" || persona.description != "" || persona.personality != "")
	}

	// Bind the stream to this (space, ga_agent_id) and kick off the
	// session-long SI-reply subscriber. The subscriber forwards SI
	// reply utterances as VoiceAgentSpeak so chat-typed user
	// messages produce audible replies via AgentSession.say(). The
	// existing TurnRequest path still handles STT-initiated turns;
	// the subscriber dedups against in-flight TurnRequests via the
	// voiceTurns map.
	s.startVoiceAgentSpeakSubscriber(spaceId, gaAgentId)

	return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSessionAck{
			VoiceAgentSessionAck: &memqlv1.VoiceAgentSessionAck{
				RequestId:         requestId,
				Success:           true,
				GaCanonicalVoice:  "alto",
				GaAvatarPersonaId: "",
				InitialAudioMode:  audioMode,
				InitialVideoMode:  videoMode,
				GaDisplayName:     persona.name,
				GaRole:            persona.role,
				GaDescription:     persona.description,
				GaPersonality:     persona.personality,
			},
		},
	})
}

// resolveGroupGAAgentId returns the canonical agent id of the group-
// GA participant in `spaceId`. Returns "" when no GA participant is
// active in the space, or on any query error -- callers fall back to
// their wire-supplied id.
//
// Read-only; the engine is the source of truth so we don't cache.
func resolveGroupGAAgentId(s *streamSession, spaceId string) string {
	if s == nil || s.service == nil || s.service.engine == nil {
		return ""
	}
	if strings.TrimSpace(spaceId) == "" {
		return ""
	}
	ctx := contextWithVoiceAgentActor(context.Background())
	// Canonicalize the spaceId on the Go side. Inlining canonicalId()
	// in the query predicate triggers "unsupported literal type
	// *ast.CanonicalIdExpr" once the WHERE chain includes a bool
	// comparison (see queryGroupGAForSpace.memql).
	canonicalSpace, err := s.service.engine.CanonicalizeIdValue(ctx, spaceId, "v1:cognition:space")
	if err != nil || canonicalSpace == "" {
		if s.logger != nil {
			s.logger.Debug("resolveGroupGAAgentId canonicalize failed",
				"space_id", spaceId, "error", err)
		}
		return ""
	}
	canonicalSpaceJSON, _ := json.Marshal(canonicalSpace)
	query := fmt.Sprintf(`queryGroupGAForSpace({spaceId: %s})`, string(canonicalSpaceJSON))
	result, err := s.service.engine.Execute(ctx, query)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("resolveGroupGAAgentId query failed",
				"space_id", spaceId, "error", err)
		}
		return ""
	}
	return extractFirstAgentIdFromParticipants(result.OutputPayload())
}

// extractFirstAgentIdFromParticipants pulls the first non-empty
// payload.agentId off a participant-shaped result. Probes the bare
// `agentId` key and the nested `payload.agentId` so it works on
// both shape() output and bundle-derived row maps. For `select
// payload.X` queries that surface only as a GraphBundle (no row
// map), drills into Bundle.Nodes as a final fallback.
func extractFirstAgentIdFromParticipants(payload any) string {
	for _, row := range normalizeResultRows(payload) {
		if v, ok := row["agentId"].(string); ok && v != "" {
			return v
		}
		if inner, ok := row["payload"].(map[string]any); ok {
			if v, ok := inner["agentId"].(string); ok && v != "" {
				return v
			}
		}
	}
	if bundle, ok := payload.(*memqlv1.GraphBundle); ok && bundle != nil {
		for _, n := range bundle.GetNodes() {
			p := n.GetPayload()
			if p == nil {
				continue
			}
			if f, ok := p.GetFields()["agentId"]; ok && f != nil {
				if v := f.GetStringValue(); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// resolveInitialChannelMode returns the effective audio or video
// publication mode for the GA at session-start time. Layers, top
// wins:
//  1. Active v1:cognition:{audio,video}override row for this
//     (space, agent) pair (the orb-corner toggle's output).
//  2. Agent record's audioControl / videoControl default.
//  3. "mirror_user" fallback.
//
// Read-only; failures fall through to "mirror_user" silently so a
// transient query error doesn't break session-start.
func resolveInitialChannelMode(s *streamSession, spaceId, agentId, channel string) string {
	const fallback = "mirror_user"
	if s == nil || s.service == nil || s.service.engine == nil {
		return fallback
	}
	concept := "v1:cognition:audioOverride"
	field := "audioControl"
	queryName := "queryAudioOverridesForSpace"
	if channel == "video" {
		concept = "v1:cognition:videoOverride"
		field = "videoControl"
		queryName = "queryVideoOverridesForSpace"
	}
	_ = concept // referenced for log context if we add one later

	ctx := contextWithVoiceAgentActor(context.Background())

	// 1) Active per-(space, agent) override. Marshal spaceId so an embedded
	// double quote cannot break out of the DSL string literal.
	spaceIdJSON, _ := json.Marshal(spaceId)
	overrideQuery := fmt.Sprintf(`%s({spaceId: %s})`, queryName, string(spaceIdJSON))
	if result, err := s.service.engine.Execute(ctx, overrideQuery); err == nil {
		if mode, ok := extractAgentChannelMode(result.OutputPayload(), agentId); ok {
			return mode
		}
	}

	// 2) Agent record's default. Marshal agentId (field is a fixed column name).
	agentIdJSON, _ := json.Marshal(agentId)
	agentQuery := fmt.Sprintf(`from(v1:agents:agent) ?.id==%s select id, payload.%s`, string(agentIdJSON), field)
	if result, err := s.service.engine.Execute(ctx, agentQuery); err == nil {
		if mode, ok := extractFirstAgentField(result.OutputPayload(), field); ok && isValidChannelMode(mode) {
			return mode
		}
	}

	return fallback
}

// agentPersonaFields is the GA persona identity loaded from the agent record
// for the session ack (#478): the human-facing name, role label, role
// description, and personality prose. Each is "" when the record omits it; the
// voice instruction builder degrades to the neutral default per field.
type agentPersonaFields struct {
	name        string
	role        string
	description string
	personality string
}

// resolveAgentPersona loads the GA's persona identity from its v1:agents:agent
// record in one query, mirroring resolveInitialChannelMode's agent-record read.
// Best-effort: any failure (nil engine, query error, missing row) returns the
// zero value so the caller stamps empty persona fields and the voice session
// falls back to the neutral default rather than failing bring-up.
func resolveAgentPersona(s *streamSession, agentId string) agentPersonaFields {
	var out agentPersonaFields
	if s == nil || s.service == nil || s.service.engine == nil {
		return out
	}
	ctx := contextWithVoiceAgentActor(context.Background())
	// JSON-marshal the interpolated id so an id containing a double quote cannot
	// break out of the DSL string literal (CodeQL "unsafe quoting"). marshal
	// yields a quoted, escaped literal, so the surrounding "%s" quotes are
	// dropped and %s carries the full `"<escaped>"` token.
	agentIdJSON, _ := json.Marshal(agentId)
	query := fmt.Sprintf(
		`from(v1:agents:agent) ?.id==%s select id, payload.name, payload.role, payload.description, payload.personality`,
		string(agentIdJSON))
	result, err := s.service.engine.Execute(ctx, query)
	if err != nil {
		return out
	}
	payload := result.OutputPayload()
	out.name, _ = extractFirstAgentField(payload, "name")
	out.role, _ = extractFirstAgentField(payload, "role")
	out.description, _ = extractFirstAgentField(payload, "description")
	out.personality, _ = extractFirstAgentField(payload, "personality")
	return out
}

// voiceAgentScopedToolNames returns the GA agent's expanded tool-name set when
// this stream is a voice-agent session bound to a resolved agent (#1419), or
// nil for every other caller (no scoping). The set is the SAME surface the
// text loop binds: the agent row's tools field expanded through
// ExpandCapabilitySlugs. Handing the model the unscoped global registry (517
// tools: internal logic sweeps, raw mutations) measurably inflated
// gpt-realtime's time-to-first-audio and surfaced tools no agent should call.
// Fail-open: a lookup error returns nil (unscoped) so a transient engine
// failure degrades to the old behaviour instead of stripping voice tools.
func (s *streamSession) voiceAgentScopedToolNames() map[string]struct{} {
	if s == nil {
		return nil
	}
	s.voiceAgentSpeakSubMu.Lock()
	agentId := s.voiceAgentGaAgentId
	s.voiceAgentSpeakSubMu.Unlock()
	if agentId == "" {
		return nil
	}
	raw, found := s.resolveAgentToolSlugs(agentId)
	if !found {
		return nil
	}
	expanded := memqlengine.ExpandCapabilitySlugs(raw)
	set := make(map[string]struct{}, len(expanded))
	for _, n := range expanded {
		set[n] = struct{}{}
	}
	return set
}

// resolveAgentToolSlugs loads the agent row's raw tools list (capability
// slugs + concrete names). found=false means the row could not be read
// (query error / no row) -- callers fail open. found=true with an empty
// slice is a real answer: the agent has no tools.
func (s *streamSession) resolveAgentToolSlugs(agentId string) ([]string, bool) {
	if s.service == nil || s.service.engine == nil {
		return nil, false
	}
	ctx := contextWithVoiceAgentActor(context.Background())
	agentIdJSON, _ := json.Marshal(agentId)
	query := fmt.Sprintf(
		`from(v1:agents:agent) ?.id==%s select id, payload.tools`,
		string(agentIdJSON))
	// #1426: synchronous engine read on the voice-session ListTools path
	// (part of the realtime media-bridge build / setup window).
	scopeStart := time.Now()
	result, err := s.service.engine.Execute(ctx, query)
	logVoiceTiming(s.logger, "server.tools.agent_scope_lookup", scopeStart,
		"agent_id", agentId, "ok", err == nil)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("voice-agent tool scope: agent query failed -- serving unscoped registry",
				"agent_id", agentId, "err", err)
		}
		return nil, false
	}
	return toolSlugsFromAgentRows(normalizeResultRows(result.OutputPayload()))
}

// toolSlugsFromAgentRows extracts the first row's tools list. Pure so the
// row-shape handling ([]string vs []any from JSON decoding) is unit-testable
// without an engine.
func toolSlugsFromAgentRows(rows []map[string]any) ([]string, bool) {
	for _, row := range rows {
		v, ok := row["tools"]
		if !ok || v == nil {
			// Row exists but carries no tools list -- a real (empty) answer.
			return nil, true
		}
		switch list := v.(type) {
		case []string:
			return append([]string(nil), list...), true
		case []any:
			out := make([]string, 0, len(list))
			for _, item := range list {
				if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
					out = append(out, name)
				}
			}
			return out, true
		default:
			return nil, true
		}
	}
	return nil, false
}

// normalizeResultRows accepts a shape() / from-select result payload
// in any of the three shapes the engine produces -- []map[string]any,
// []any holding maps, or a single map -- and returns a uniform
// []map[string]any. Mirrors integrations/agent/worker/integration.go's
// outputPayloadRows helper; copied to keep voice_agent_handlers self-
// contained instead of pulling a worker-build symbol into BFF.
func normalizeResultRows(payload any) []map[string]any {
	if payload == nil {
		return nil
	}
	switch v := payload.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}

func extractAgentChannelMode(result any, agentId string) (string, bool) {
	rows := normalizeResultRows(result)
	if len(rows) == 0 {
		return "", false
	}
	for _, rowMap := range rows {
		payload, _ := rowMap["payload"].(map[string]any)
		if payload == nil {
			payload = rowMap
		}
		rowAgent, _ := payload["agentId"].(string)
		active, _ := payload["active"].(bool)
		mode, _ := payload["mode"].(string)
		if !active || mode == "" {
			continue
		}
		if !channelOverrideTargetsAgent(rowAgent, agentId) {
			continue
		}
		if isValidChannelMode(mode) {
			return mode, true
		}
	}
	return "", false
}

func extractFirstAgentField(result any, field string) (string, bool) {
	rows := normalizeResultRows(result)
	if len(rows) == 0 {
		return "", false
	}
	row := rows[0]
	if value, ok := row[field].(string); ok && value != "" {
		return value, true
	}
	payload, _ := row["payload"].(map[string]any)
	if payload == nil {
		return "", false
	}
	if value, ok := payload[field].(string); ok && value != "" {
		return value, true
	}
	return "", false
}

func channelOverrideTargetsAgent(rowAgentId, target string) bool {
	a := strings.ToLower(strings.TrimSpace(rowAgentId))
	b := strings.ToLower(strings.TrimSpace(target))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	// Canonical id vs bare slug tolerance, same shape as the
	// cognition handler's sameAgent helper.
	return strings.HasSuffix(a, ":"+b) || strings.HasSuffix(b, ":"+a)
}

func isValidChannelMode(mode string) bool {
	switch mode {
	case "always_on", "always_off", "mirror_user":
		return true
	}
	return false
}

// handleVoiceAgentSessionEnd closes the session for audit + cleanup.
func (s *streamSession) handleVoiceAgentSessionEnd(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.VoiceAgentSessionEnd) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	if s.logger != nil {
		s.logger.Info("voice-agent session end",
			"request_id", requestId,
			"space_id", msg.GetSpaceId(),
			"room_name", msg.GetRoomName(),
			"reason", msg.GetReason())
	}

	s.stopVoiceAgentSpeakSubscriber()

	return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSessionAck{
			VoiceAgentSessionAck: &memqlv1.VoiceAgentSessionAck{
				RequestId: requestId,
				Success:   true,
			},
		},
	})
}

// startVoiceAgentSpeakSubscriber binds a session-long subscriber for
// the (space, ga_agent_id) pair. The subscriber forwards SI reply
// utterances as VoiceAgentSpeak messages so the voice-agent
// synthesizes them via AgentSession.say(), enabling chat-typed user
// messages to produce audible replies.
//
// Dedup against STT-initiated turns: when a VoiceAgentTurnRequest is
// in flight for the same (space, agent), the TurnDelta path already
// drives the TTS pipeline -- the subscriber skips the matching SI
// reply so we don't double-speak.
//
// Mode gating: only `always_on` triggers Speak. The `mirror_user`
// case is handled by the existing TurnRequest path (which fires
// only when the user actually speaks via STT, mirroring their mic
// state by construction). `always_off` is skipped silently.
//
// Idempotent: replacing an active subscriber on a re-issued
// SessionStart cancels the previous one first.
func (s *streamSession) startVoiceAgentSpeakSubscriber(spaceId, gaAgentId string) {
	if s == nil || s.service == nil || s.service.eventBus == nil {
		return
	}
	if spaceId == "" || gaAgentId == "" {
		return
	}

	s.voiceAgentSpeakSubMu.Lock()
	if s.voiceAgentSpeakStop != nil {
		s.voiceAgentSpeakStop()
		s.voiceAgentSpeakStop = nil
	}
	s.voiceAgentSpaceId = spaceId
	s.voiceAgentGaAgentId = gaAgentId
	s.voiceAgentSpeakSubMu.Unlock()

	// Per-subscriber dedup cache. Bounded simple set; we only ever
	// add the current session's forwarded utterance ids during its
	// lifetime. Voice-agent sessions are short-lived (per-room) so a
	// growing set never poses a memory problem in practice.
	seenIds := make(map[string]struct{})
	var seenMu sync.Mutex

	pattern := "graph.node.created.v1:cognition:utterance"
	subName := fmt.Sprintf("voice-agent-speak-%s-%s", spaceId, gaAgentId)
	unsubscribe := s.service.eventBus.Subscribe(pattern, func(e events.Event) {
		reply, ok := extractGAReplyFromEvent(e, spaceId, gaAgentId)
		if !ok {
			return
		}
		// Dedup: same utterance can fire multiple times during retries
		// or if the event bus replays.
		if reply.utteranceId != "" {
			seenMu.Lock()
			if _, already := seenIds[reply.utteranceId]; already {
				seenMu.Unlock()
				return
			}
			seenIds[reply.utteranceId] = struct{}{}
			seenMu.Unlock()
		}
		// Skip if a VoiceAgentTurnRequest is in flight -- that path
		// will deliver this same reply via TurnDelta.
		turnKey := spaceId + "|" + gaAgentId
		s.voiceTurnsMu.Lock()
		_, turnInFlight := s.voiceTurns[turnKey]
		s.voiceTurnsMu.Unlock()
		if turnInFlight {
			if s.logger != nil {
				s.logger.Debug("voice-agent speak skip: turn in flight",
					"space_id", spaceId,
					"ga_agent_id", gaAgentId,
					"utterance_id", reply.utteranceId)
			}
			return
		}
		// Resolve current audio mode. Only always_on triggers Speak.
		// #1426: synchronous engine query (up to two Execute calls) inside an
		// event-bus callback on EVERY utterance-created event for the space.
		modeStart := time.Now()
		mode := resolveInitialChannelMode(s, spaceId, gaAgentId, "audio")
		logVoiceTiming(s.logger, "server.speak.audio_mode_lookup", modeStart,
			"space_id", spaceId, "utterance_id", reply.utteranceId)
		if mode != "always_on" {
			if s.logger != nil {
				s.logger.Info("voice-agent speak skip: audio mode not always_on",
					"space_id", spaceId,
					"ga_agent_id", gaAgentId,
					"utterance_id", reply.utteranceId,
					"audio_mode", mode)
			}
			return
		}

		requestId := fmt.Sprintf("speak-%d-%s", time.Now().UnixNano(), randHex(4))
		if s.logger != nil {
			s.logger.Info("voice-agent speak emit",
				"request_id", requestId,
				"space_id", spaceId,
				"ga_agent_id", gaAgentId,
				"utterance_id", reply.utteranceId,
				"text_len", len(reply.text))
		}
		// Empty correlate_to -- this is a server-pushed message, not
		// a reply to a client request. Voice-agent's read loop has a
		// dedicated dispatch for unsolicited VoiceAgentSpeak.
		s.sendServerMessage("", &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentSpeak{
				VoiceAgentSpeak: &memqlv1.VoiceAgentSpeak{
					RequestId:   requestId,
					SpaceId:     spaceId,
					GaAgentId:   gaAgentId,
					UtteranceId: reply.utteranceId,
					Text:        reply.text,
				},
			},
		})
	}, events.WithSubscriberName(subName))

	s.voiceAgentSpeakSubMu.Lock()
	s.voiceAgentSpeakStop = unsubscribe
	s.voiceAgentSpeakSubMu.Unlock()
}

// stopVoiceAgentSpeakSubscriber tears down the session-long
// subscriber. Safe to call on a non-voice-agent stream (no-op).
func (s *streamSession) stopVoiceAgentSpeakSubscriber() {
	if s == nil {
		return
	}
	s.voiceAgentSpeakSubMu.Lock()
	stop := s.voiceAgentSpeakStop
	s.voiceAgentSpeakStop = nil
	s.voiceAgentSpaceId = ""
	s.voiceAgentGaAgentId = ""
	s.voiceAgentSpeakSubMu.Unlock()
	if stop != nil {
		stop()
	}
}

// handleVoiceAgentPartialTranscript emits a voice.partial.utterance
// event on the event bus. Subscribers (the chat UI via memql's
// existing graph-events subscription path) render the streaming text
// without a backing graph row.
func (s *streamSession) handleVoiceAgentPartialTranscript(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.VoiceAgentPartialTranscript) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	spaceId := strings.TrimSpace(msg.GetSpaceId())
	speakerId := strings.TrimSpace(msg.GetSpeakerUserId())
	if spaceId == "" || speakerId == "" {
		return s.sendQueryError(requestId, correlate, codes.InvalidArgument, "spaceId and speakerUserId are required")
	}

	if s.service.eventBus != nil {
		s.service.eventBus.Publish(events.NewEvent(
			voicePartialEventTopic,
			events.KindMessage,
			map[string]any{
				"spaceId":       spaceId,
				"speakerUserId": speakerId,
				"text":          msg.GetPartialText(),
				"sequence":      msg.GetSequence(),
			},
		))
	}

	return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentPartialAck{
			VoiceAgentPartialAck: &memqlv1.VoiceAgentPartialAck{
				RequestId: requestId,
				Success:   true,
			},
		},
	})
}

// handleVoiceAgentFinalTranscript inserts the user's utterance into
// the single space chat via mutationSendTextUtterance.
//
// speakerUserId is OPTIONAL on the wire (#1403): the voice-agent's
// CascadeConfig documents "empty is allowed (server resolves the active
// speaker)", so an empty speaker falls back to the space's SINGLE active
// human participant. The fallback is conservative -- zero or multiple
// active humans reject the insert (with a WARN; attribution is never
// guessed) via a failed VoiceAgentFinalAck the voice-agent now consumes.
func (s *streamSession) handleVoiceAgentFinalTranscript(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.VoiceAgentFinalTranscript) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	if s.service.engine == nil {
		return s.sendQueryError(requestId, correlate, codes.Unavailable, "engine not configured")
	}

	spaceId := strings.TrimSpace(msg.GetSpaceId())
	speakerId := strings.TrimSpace(msg.GetSpeakerUserId())
	text := strings.TrimSpace(msg.GetFinalText())
	if spaceId == "" || text == "" {
		// WARN on rejection: a rejected final transcript means the user's
		// voice utterance never reaches chat or cognition -- the silent
		// failure #1403 made invisible.
		if s.logger != nil {
			s.logger.Warn("voice-agent final transcript rejected: missing required fields",
				"request_id", requestId,
				"space_id", spaceId,
				"missing_space_id", spaceId == "",
				"missing_final_text", text == "")
		}
		return s.sendQueryError(requestId, correlate, codes.InvalidArgument, "spaceId and finalText are required")
	}

	// Mint a flat slug -- speakerId is the canonical participant id
	// (`<partition>:v1:cognition:participant:<slug>`) and embedding
	// it would inject `:v1:cognition:participant:` into the
	// utterance id, which memql's canonicalizer then parses out as
	// the type tag, rejecting the SI response's `replyToId` insert
	// with "is under concept v1:cognition:participant, expected
	// v1:cognition:utterance". The 8-hex suffix collapses 1e-15
	// nanosecond-collision risk further; nanos alone are unique in
	// practice but cheap belt-and-suspenders.
	utteranceId := fmt.Sprintf("utt-%d-%s", time.Now().UnixNano(), randHex(8))

	// In the #478 native 1-on-1 path the realtime model already authored AND
	// spoke the reply, so this user utterance is transcript-only: stamp
	// inputMethod="realtimeVoice" (isTranscriptOnlyRealtimeUtterance) so the
	// cognition automation skips authoring a second reply. The conductor-gated /
	// cascade path keeps inputMethod="stt" so cognition authors as today.
	inputMethod := "stt"
	if msg.GetNativeAuthored() {
		inputMethod = "realtimeVoice"
	}

	go func() {
		// Empty speaker: honor the documented contract by resolving the
		// space's single active human participant. Runs inside the goroutine
		// so the engine round-trip never blocks the stream read loop.
		if speakerId == "" {
			speakerResolveStart := time.Now()
			resolved, rerr := resolveSingleActiveHumanParticipantId(
				contextWithVoiceAgentActor(context.Background()), s.service.engine, spaceId)
			logVoiceTiming(s.logger, "server.final_transcript.resolve_speaker", speakerResolveStart,
				"space_id", spaceId, "ok", rerr == nil)
			if rerr != nil {
				if s.logger != nil {
					s.logger.Warn("voice-agent final transcript rejected: speakerUserId empty and active speaker unresolvable",
						"request_id", requestId,
						"space_id", spaceId,
						"error", rerr)
				}
				s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
					Payload: &memqlv1.MemqlServerMessage_VoiceAgentFinalAck{
						VoiceAgentFinalAck: &memqlv1.VoiceAgentFinalAck{
							RequestId:    requestId,
							Success:      false,
							ErrorCode:    "speaker_unresolvable",
							ErrorMessage: rerr.Error(),
						},
					},
				})
				return
			}
			speakerId = resolved
			if s.logger != nil {
				s.logger.Info("voice-agent final transcript: resolved fallback speaker",
					"request_id", requestId,
					"space_id", spaceId,
					"speaker_user_id", speakerId)
			}
		}

		// JSON-marshal every interpolated string so a value containing a double
		// quote cannot break out of its DSL string literal (CodeQL "unsafe
		// quoting"). text already used this; do the same for the id fields and
		// the source enum.
		textJSON, _ := json.Marshal(text)
		utteranceIdJSON, _ := json.Marshal(utteranceId)
		spaceIdJSON, _ := json.Marshal(spaceId)
		speakerIdJSON, _ := json.Marshal(speakerId)
		inputMethodJSON, _ := json.Marshal(inputMethod)
		query := fmt.Sprintf(`mutationSendTextUtterance({utteranceId: %s, spaceId: %s, participantId: %s, text: %s, source: {inputMethod: %s, pipeline: "voice-agent"}})`,
			string(utteranceIdJSON), string(spaceIdJSON), string(speakerIdJSON), string(textJSON), string(inputMethodJSON))

		ctx := contextWithVoiceAgentActor(context.Background())
		// #1426: the user-utterance insert (engine mutation -> DB) that also
		// triggers the cognition automation for the turn.
		insertStart := time.Now()
		_, err := s.service.engine.Execute(ctx, query)
		logVoiceTiming(s.logger, "server.final_transcript.insert", insertStart,
			"space_id", spaceId, "input_method", inputMethod, "ok", err == nil)
		if err != nil {
			if s.logger != nil {
				s.logger.Error("voice-agent final transcript insert failed",
					"request_id", requestId,
					"space_id", spaceId,
					"error", err)
			}
			s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_VoiceAgentFinalAck{
					VoiceAgentFinalAck: &memqlv1.VoiceAgentFinalAck{
						RequestId:    requestId,
						Success:      false,
						ErrorCode:    "insert_failed",
						ErrorMessage: err.Error(),
					},
				},
			})
			return
		}

		s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentFinalAck{
				VoiceAgentFinalAck: &memqlv1.VoiceAgentFinalAck{
					RequestId:   requestId,
					Success:     true,
					UtteranceId: utteranceId,
					Thread:      "group",
				},
			},
		})
	}()

	return nil
}

// handleVoiceAgentTurnRequest waits for the cognition pipeline's
// dispatch to land the GA's reply utterance in the space, then streams
// the reply text back as VoiceAgentTurnDelta + VoiceAgentTurnComplete.
//
// The user's final utterance was already inserted by
// handleVoiceAgentFinalTranscript (which fires graph.node.created.v1:cognition:utterance
// -- the existing cognition automation handler consumes that and
// dispatches the GA's reply). Phase 6 just subscribes to the matching
// reply event with a bounded timeout and translates to the
// VoiceAgent* surface.
//
// True per-token delta streaming (matching AgentGenerateTurnDelta on
// the agent node) is a Phase 11+ follow-up; the current path emits
// the whole reply as a single delta. Latency-wise this is the same as
// the Bridge Agent path's TTS handoff -- both wait for the full text
// before pushing to the TTS plugin. Improving on this is part of the
// post-cutover latency-tuning work.
func (s *streamSession) handleVoiceAgentTurnRequest(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.VoiceAgentTurnRequest) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	spaceId := strings.TrimSpace(msg.GetSpaceId())
	gaAgentId := strings.TrimSpace(msg.GetGaAgentId())
	utterance := strings.TrimSpace(msg.GetUtteranceText())
	if spaceId == "" || gaAgentId == "" || utterance == "" {
		return s.sendQueryError(requestId, correlate, codes.InvalidArgument, "spaceId, gaAgentId, utteranceText are required")
	}

	if s.service.eventBus == nil {
		return s.sendQueryError(requestId, correlate, codes.Unavailable, "event bus not configured")
	}

	// Prefer the canonical GA id already resolved at SessionStart so
	// this path's turnKey matches the session-long speak subscriber's
	// turnKey. Without this, voice-driven turns (using the placeholder
	// `<space>-ga` from the wire) and the speak-subscriber (using the
	// resolved canonical id) build mismatched keys -- the dedup check
	// misses and the speak path fires concurrently with the TurnDelta
	// path, so Sofia ends up synthesizing the same reply twice.
	s.voiceAgentSpeakSubMu.Lock()
	if resolved := s.voiceAgentGaAgentId; resolved != "" {
		gaAgentId = resolved
	}
	s.voiceAgentSpeakSubMu.Unlock()

	if s.logger != nil {
		s.logger.Info("voice-agent turn request",
			"request_id", requestId,
			"space_id", spaceId,
			"ga_agent_id", gaAgentId,
			"thread", msg.GetThread().String(),
			"utterance_len", len(utterance))
	}

	// Subscribe to graph node-created events for the space's
	// utterance concept. The assistant's reply lands as a row with
	// participantType="agent" + participantId=gaAgentId; filter for
	// that specific shape so other agents' replies (specialists,
	// chime-ins) don't trigger us.
	pattern := "graph.node.created.v1:cognition:utterance"

	replyCh := make(chan voiceAgentReply, 4)
	unsubscribe := s.service.eventBus.Subscribe(pattern, func(e events.Event) {
		reply, ok := extractGAReplyFromEvent(e, spaceId, gaAgentId)
		if !ok {
			return
		}
		// Non-blocking send so a slow consumer can't back-pressure
		// the event bus. We only need the first matching reply.
		select {
		case replyCh <- reply:
		default:
		}
	}, events.WithSubscriberName(fmt.Sprintf("voice-agent-turn-%s", requestId)))

	// Gate path (#477/#479): subscribe to the conductor gate's directive for
	// this turn. When the gate publishes a decision the model authors the words
	// itself -- the relay forwards engage(mode, brevity) or defers immediately,
	// instead of waiting for cognition to author. Whichever of the two paths
	// fires first wins; absent a directive the legacy authored-utterance path
	// above still drives the turn.
	directiveCh := make(chan voiceGateDirective, 4)
	unsubscribeDirective := s.service.eventBus.Subscribe(voiceGateDirectiveTopic, func(e events.Event) {
		dir, ok := extractVoiceGateDirective(e, spaceId, gaAgentId)
		if !ok {
			return
		}
		select {
		case directiveCh <- dir:
		default:
		}
	}, events.WithSubscriberName(fmt.Sprintf("voice-agent-gate-%s", requestId)))

	// One active turn-request per (space, gaAgent) at a time. If the
	// user fires a new utterance while the previous turn is still
	// parked (classifier suppressed it / it hit the 30s timeout / it
	// is genuinely mid-LLM), cancel the previous one so its goroutine
	// returns immediately instead of accumulating as an orphan
	// subscriber. Multiple orphans on the same v1:cognition:utterance
	// pattern caused the "agent reply lands in chat but no audio
	// played" symptom: every orphan matched the same SI reply row,
	// each sent its own TurnDelta back to voice-agent, and
	// LK Agents 1.5's AgentSession had multiple LLMStreams racing --
	// only one's TTS reached Aura-2.
	turnKey := spaceId + "|" + gaAgentId
	// turnStart anchors the server-side share of the per-turn window (#1426):
	// turn request received -> gate directive / authored reply / timeout.
	turnStart := time.Now()
	ctx, cancel := context.WithTimeout(s.stream.Context(), voiceTurnWaitTimeout)
	// Use a fresh sentinel so the "is this still my slot" check on
	// cleanup is by pointer-identity, not by comparing function
	// values (Go function comparison is not well-defined).
	mySentinel := new(struct{})
	s.voiceTurnsMu.Lock()
	if prevCancel, ok := s.voiceTurns[turnKey]; ok {
		prevCancel()
	}
	s.voiceTurns[turnKey] = cancel
	// Stash the sentinel alongside the cancel so cleanup can verify
	// it still owns the slot.
	if s.voiceTurnSentinels == nil {
		s.voiceTurnSentinels = make(map[string]*struct{})
	}
	s.voiceTurnSentinels[turnKey] = mySentinel
	s.voiceTurnsMu.Unlock()

	go func() {
		defer unsubscribe()
		defer unsubscribeDirective()
		defer cancel()
		defer func() {
			s.voiceTurnsMu.Lock()
			if s.voiceTurnSentinels[turnKey] == mySentinel {
				delete(s.voiceTurns, turnKey)
				delete(s.voiceTurnSentinels, turnKey)
			}
			s.voiceTurnsMu.Unlock()
		}()

		select {
		case dir := <-directiveCh:
			// Gate path (#479): the conductor decided WHEN + brevity; the model
			// authors WHAT. A defer (or an engage with no mode) suppresses with
			// an empty TurnComplete -- the executor emits no response.create. An
			// engage forwards the content-free directive; the executor renders it
			// and the model generates natively (no authored text relayed).
			complete := &memqlv1.VoiceAgentTurnComplete{
				RequestId:          requestId,
				UtteranceId:        dir.utteranceId,
				EffectiveAudioMode: "mirror_user",
				EffectiveVideoMode: "mirror_user",
			}
			if dir.engage && strings.TrimSpace(dir.mode) != "" && !strings.EqualFold(dir.mode, "defer") {
				complete.DirectiveMode = dir.mode
				complete.Brevity = dir.brevity
				complete.Grounding = dir.grounding
			}
			if s.logger != nil {
				s.logger.Info("voice-agent turn: gate directive",
					"request_id", requestId, "space_id", spaceId,
					"engage", complete.DirectiveMode != "", "mode", dir.mode, "brevity", dir.brevity)
			}
			logVoiceTiming(s.logger, "server.turn.gate_directive", turnStart,
				"space_id", spaceId, "request_id", requestId,
				"engage", complete.DirectiveMode != "", "mode", dir.mode)
			s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
					VoiceAgentTurnComplete: complete,
				},
			})
		case reply := <-replyCh:
			logVoiceTiming(s.logger, "server.turn.authored_reply", turnStart,
				"space_id", spaceId, "request_id", requestId, "chars", len(reply.text))
			s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnDelta{
					VoiceAgentTurnDelta: &memqlv1.VoiceAgentTurnDelta{
						RequestId: requestId,
						TextDelta: reply.text,
					},
				},
			})
			s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
					VoiceAgentTurnComplete: &memqlv1.VoiceAgentTurnComplete{
						RequestId:          requestId,
						FinalText:          reply.text,
						UtteranceId:        reply.utteranceId,
						EffectiveAudioMode: "mirror_user",
						EffectiveVideoMode: "mirror_user",
					},
				},
			})
		case <-ctx.Done():
			// ctx.Err() differentiates the two cases. DeadlineExceeded
			// means a real 30s silent wait -- usually classifier
			// suppression on a voice ack. Canceled means a newer turn
			// arrived and superseded us. Either way voice-agent gets
			// a TurnComplete so its MemqlLLMStream closes; the error
			// code is informational for the log.
			reason := "turn_timeout"
			msg := "no reply utterance within " + voiceTurnWaitTimeout.String()
			if errors.Is(ctx.Err(), context.Canceled) {
				reason = "turn_superseded"
				msg = "superseded by a newer turn request for this space"
			}
			if s.logger != nil {
				s.logger.Warn("voice-agent turn ended without reply",
					"request_id", requestId,
					"space_id", spaceId,
					"ga_agent_id", gaAgentId,
					"reason", reason)
			}
			logVoiceTiming(s.logger, "server.turn.no_reply", turnStart,
				"space_id", spaceId, "request_id", requestId, "reason", reason)
			s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
				Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
					VoiceAgentTurnComplete: &memqlv1.VoiceAgentTurnComplete{
						RequestId:          requestId,
						EffectiveAudioMode: "always_off",
						EffectiveVideoMode: "always_off",
						ErrorCode:          reason,
						ErrorMessage:       msg,
					},
				},
			})
		}
	}()

	return nil
}

// voiceAgentReply is a tiny helper struct -- the extracted text +
// utterance id from a graph.node.created event.
type voiceAgentReply struct {
	text        string
	utteranceId string
}

// extractGAReplyFromEvent pulls the (text, id) tuple out of a
// graph.node.created event payload and filters for "is this the
// GA's reply in the target space."
func extractGAReplyFromEvent(e events.Event, spaceId, gaAgentId string) (voiceAgentReply, bool) {
	if e.Payload == nil {
		return voiceAgentReply{}, false
	}
	payload := e.Payload

	// graph.node.created events stamp the node's payload under "payload"
	// or "node" depending on the producer. Probe both for robustness.
	nodeFields := pickMap(payload, "payload", "node")
	if nodeFields == nil {
		// Some producers flatten payload fields onto the event payload
		// directly (no nested "payload" key). Fall through and read
		// from the top-level map.
		nodeFields = payload
	}

	// memql auto-canonicalizes relationship fields on insert, so the
	// reply utterance's spaceId lands as
	// `<partition>:v1:cognition:space:<slug>` while the voice-agent
	// passes the bare slug. Compare on the trailing slug so either
	// form matches.
	nodeSpaceId := asString(nodeFields, "spaceId")
	if nodeSpaceId == "" {
		nodeSpaceId = asString(nodeFields, "spaceId")
	}
	if !spaceIdMatches(nodeSpaceId, spaceId) {
		return voiceAgentReply{}, false
	}
	// Match any SI-participant reply in the target space. Backend
	// convention stamps `participantType="si"` on every system-
	// intelligence utterance (the wire-format value memql actually
	// writes; the earlier "agent" check here was aspirational and
	// never matched a row). Cognition only dispatches one winner
	// per turn, so the first SI reply that lands in the space after
	// the turn-request subscription opened IS the reply to the
	// active voice turn -- there is no fan-out worth disambiguating
	// here, and the previous gaAgentId match was checking against a
	// voice-agent-side placeholder (`<space_id>-ga`) that has
	// nothing to do with the real `v1:cognition:participant` row id
	// the agent path inserts.
	pt := asString(nodeFields, "participantType")
	if pt != "si" {
		return voiceAgentReply{}, false
	}
	// Skip utterances the realtime model already spoke natively (#478). The
	// gpt-realtime output capture (handleVoiceAgentRealtimeOutput) inserts a
	// participantType="si" utterance stamped source.outputMethod="realtimeVoice";
	// without this guard the speak subscriber would match it and push a
	// VoiceAgentSpeak, making the model re-voice its own reply (double-speak).
	// The speak path exists to voice TEXT replies cognition authored, never
	// audio the model already produced.
	if source := pickMap(nodeFields, "source"); source != nil {
		if strings.TrimSpace(asString(source, "outputMethod")) == "realtimeVoice" {
			return voiceAgentReply{}, false
		}
	}
	_ = gaAgentId // retained on the signature for caller-side logging
	text := strings.TrimSpace(asString(nodeFields, "text"))
	if text == "" {
		return voiceAgentReply{}, false
	}
	utteranceId := asString(payload, "id")
	if utteranceId == "" {
		utteranceId = asString(nodeFields, "id")
	}
	return voiceAgentReply{text: text, utteranceId: utteranceId}, true
}

func pickMap(src map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		v, ok := src[k]
		if !ok {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return nil
}

func asString(src map[string]any, key string) string {
	v, ok := src[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// spaceIdMatches returns true when `nodeId` (read off a graph row) and
// `wantId` (passed in by the voice-agent on the wire) point at the
// same v1:cognition:space row, regardless of whether either side is
// canonical (`<partition>:v1:cognition:space:<slug>`) or bare slug.
// memql auto-canonicalizes relationship fields at insert time, but
// the voice-agent passes the bare slug it parsed off the LiveKit
// room name -- strict-string equality across the two forms misses.
func spaceIdMatches(nodeId, wantId string) bool {
	if nodeId == "" || wantId == "" {
		return false
	}
	if nodeId == wantId {
		return true
	}
	return trailingSlug(nodeId) == trailingSlug(wantId)
}

func trailingSlug(id string) string {
	if i := strings.LastIndex(id, ":"); i != -1 {
		return id[i+1:]
	}
	return id
}

// contextWithVoiceAgentActor stamps the service-actor identity for
// graph writes the voice-agent triggers. Mirrors contextWithSystemActor
// in polyphon_handlers.go; kept distinct so audit + telemetry can tell
// voice-agent inserts from the legacy bridge-agent inserts during the
// cutover window.
// handleVoiceAgentRealtimeOutput captures the assistant's spoken output
// for one realtime turn and inserts it as an SI utterance with full
// chat/canvas/audit parity (#437).
//
// The cascade routes assistant replies through VoiceAgentTurnRequest --
// cognition runs the agent loop and inserts the SI utterance itself
// (insertSIResponse in integrations/cognition/si_responder.go), stamping
// participantType="si" + citations off the respondToUser envelope. The
// realtime executor (gpt-realtime) speaks directly and never enters that
// path, so this handler is the sole writer of realtime SI utterances.
// It mirrors insertSIResponse's wire shape exactly:
//
//   - participantType="si"  (the value the frontend keys sender lookup on)
//   - participantId         = the GA's v1:cognition:participant row id,
//     NOT the agent template id (resolved here via
//     querySiParticipantForSpace)
//   - utteranceType="text"  (transcript is text; audio rode LiveKit)
//   - source                = {outputMethod:"voice", inputMethod:"realtime",
//     pipeline:"voice-agent-realtime", agentId}
//   - citations             = same {domainId, matchedPhrase} entries the
//     cascade persists (omitted entirely when empty)
//
// so chat (streaming reveal + role label + citation chips), canvas
// announcements, conductor history, and audit all read a realtime voice
// turn identically to a text/cascade turn. reply_id doubles as the
// utterance id so it matches any in-flight streaming replyId the
// frontend already keyed.
func (s *streamSession) handleVoiceAgentRealtimeOutput(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.VoiceAgentRealtimeOutput) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	if s.service.engine == nil {
		return s.sendQueryError(requestId, correlate, codes.Unavailable, "engine not configured")
	}

	spaceId := strings.TrimSpace(msg.GetSpaceId())
	gaAgentId := strings.TrimSpace(msg.GetGaAgentId())
	text := strings.TrimSpace(msg.GetText())
	if spaceId == "" || gaAgentId == "" || text == "" {
		return s.sendQueryError(requestId, correlate, codes.InvalidArgument, "spaceId, gaAgentId, text are required")
	}

	// The committed utterance id IS the reply_id when provided so it
	// matches the chat panel's streaming replyId contract. Fall back to
	// a freshly-minted slug (same flat `utt-<nanos>-<hex>` shape the
	// final-transcript path uses -- never embed a participant id, which
	// the canonicalizer would mis-parse as the type tag).
	utteranceId := strings.TrimSpace(msg.GetReplyId())
	if utteranceId == "" {
		utteranceId = fmt.Sprintf("utt-si-%d-%s", time.Now().UnixNano(), randHex(8))
	}
	replyToId := strings.TrimSpace(msg.GetReplyToId())

	ack := func(success bool, utterId, code, errMsg string) error {
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentRealtimeOutputAck{
				VoiceAgentRealtimeOutputAck: &memqlv1.VoiceAgentRealtimeOutputAck{
					RequestId:    requestId,
					Success:      success,
					UtteranceId:  utterId,
					ErrorCode:    code,
					ErrorMessage: errMsg,
				},
			},
		})
	}

	go func() {
		ctx := contextWithVoiceAgentActor(context.Background())

		// Resolve the GA's SI participant row in the space. The frontend
		// resolves a sender via participantMap.get(utterance.participantId),
		// which is the v1:cognition:participant id -- NOT the agent
		// template id. querySiParticipantForSpace returns the active SI
		// participant; voice rooms have a single GA, so the first active
		// SI participant is the speaker.
		participantResolveStart := time.Now()
		participantId := s.resolveSIParticipantId(ctx, spaceId)
		logVoiceTiming(s.logger, "server.realtime_output.resolve_participant", participantResolveStart,
			"space_id", spaceId, "ok", participantId != "")
		if participantId == "" {
			if s.logger != nil {
				s.logger.Error("voice-agent realtime output: no SI participant resolved",
					"request_id", requestId, "space_id", spaceId, "ga_agent_id", gaAgentId)
			}
			_ = ack(false, "", "participant_not_found", "no active SI participant in space")
			return
		}

		// Source attribution. outputMethod="realtimeVoice" is the
		// concept's enum value for the gpt-realtime speech-to-speech
		// path (v1:cognition:utterance.source.outputMethod). pipeline
		// tags the wire path so cognition's voice-vs-text routing keys
		// on it; sttProvider/agentId mirror what insertSIResponse stamps.
		source := map[string]string{
			"outputMethod": "realtimeVoice",
			"pipeline":     "voice-agent-realtime",
			"sttProvider":  "openai-realtime",
			"agentId":      gaAgentId,
		}
		sourceJSON, _ := json.Marshal(source)

		// Citations parity: same {domainId, matchedPhrase} entries the
		// cascade persists. Dropped entirely when empty so an
		// un-grounded realtime reply is byte-identical to an un-grounded
		// text reply (no `"citations": []`).
		citationsClause := buildRealtimeCitationsClause(msg.GetCitations())

		textJSON, _ := json.Marshal(text)
		replyToClause := ""
		if replyToId != "" {
			replyToJSON, _ := json.Marshal(replyToId)
			replyToClause = fmt.Sprintf(`, replyToId: %s`, string(replyToJSON))
		}

		// JSON-marshal the interpolated ids so an id containing a double quote
		// cannot break out of its DSL string literal (CodeQL unsafe-quoting,
		// alert #188). text / source / replyTo already use this escaping.
		utteranceIdJSON, _ := json.Marshal(utteranceId)
		spaceIdJSON, _ := json.Marshal(spaceId)
		participantIdJSON, _ := json.Marshal(participantId)
		query := fmt.Sprintf(
			`mutationSendTextUtterance({utteranceId: %s, spaceId: %s, participantId: %s, participantType: "si", text: %s%s, source: %s%s})`,
			string(utteranceIdJSON), string(spaceIdJSON), string(participantIdJSON),
			string(textJSON), replyToClause, string(sourceJSON), citationsClause,
		)

		// #1426: the assistant-utterance insert (engine mutation -> DB).
		insertStart := time.Now()
		_, err := s.service.engine.Execute(ctx, query)
		logVoiceTiming(s.logger, "server.realtime_output.insert", insertStart,
			"space_id", spaceId, "ok", err == nil)
		if err != nil {
			if s.logger != nil {
				s.logger.Error("voice-agent realtime output insert failed",
					"request_id", requestId, "space_id", spaceId,
					"utterance_id", utteranceId, "error", err)
			}
			_ = ack(false, "", "insert_failed", err.Error())
			return
		}

		if s.logger != nil {
			s.logger.Info("voice-agent realtime output inserted",
				"request_id", requestId, "space_id", spaceId,
				"utterance_id", utteranceId, "participant_id", participantId,
				"text_len", len(text), "citations", len(msg.GetCitations()))
		}
		_ = ack(true, utteranceId, "", "")
	}()

	return nil
}

// resolveSIParticipantId returns the active SI participant row id for a
// space, or "" if none. Read-only; mirrors the resolution insertSIResponse
// relies on (participantId must be the v1:cognition:participant id, not
// the agent template id) so realtime utterances attribute identically.
func (s *streamSession) resolveSIParticipantId(ctx context.Context, spaceId string) string {
	if s == nil || s.service == nil || s.service.engine == nil {
		return ""
	}
	spaceJSON, _ := json.Marshal(spaceId)
	query := fmt.Sprintf(`querySiParticipantForSpace({spaceId: %s})`, string(spaceJSON))
	result, err := s.service.engine.Execute(ctx, query)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("resolveSIParticipantId query failed",
				"space_id", spaceId, "error", err)
		}
		return ""
	}
	for _, row := range normalizeResultRows(result.OutputPayload()) {
		if v, ok := row["id"].(string); ok && v != "" {
			return v
		}
		if inner, ok := row["payload"].(map[string]any); ok {
			if v, ok := inner["id"].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// buildRealtimeCitationsClause renders the `, citations: [...]` mutation
// clause from the proto citations, or "" when there are none worth
// emitting. Each entry is validated (both fields non-empty) so a partial
// citation never lands a malformed chip. Mirrors the citations clause in
// integrations/cognition/si_responder.go insertSIResponse for byte-for-
// byte chat-render parity.
func buildRealtimeCitationsClause(citations []*memqlv1.AgentTurnCitation) string {
	if len(citations) == 0 {
		return ""
	}
	entries := make([]map[string]string, 0, len(citations))
	for _, c := range citations {
		if c == nil {
			continue
		}
		d := strings.TrimSpace(c.GetDomainId())
		p := strings.TrimSpace(c.GetMatchedPhrase())
		if d == "" || p == "" {
			continue
		}
		entries = append(entries, map[string]string{
			"domainId":      d,
			"matchedPhrase": p,
		})
	}
	if len(entries) == 0 {
		return ""
	}
	citationsJSON, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(`, citations: %s`, string(citationsJSON))
}

// voiceParticipantResolver is the narrow engine surface the FinalTranscript
// speaker fallback needs. *memqlengine.MemQLEngine satisfies it; an interface
// so the resolution rule is unit-testable without standing up an engine.
type voiceParticipantResolver interface {
	CanonicalizeIdValue(ctx context.Context, value, conceptType string) (string, error)
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
}

// resolveSingleActiveHumanParticipantId implements the documented
// VoiceAgentFinalTranscript fallback (#1403): when the voice-agent sends an
// empty speakerUserId, attribute the utterance to the space's SINGLE active
// human participant. Conservative rule: exactly one active human resolves;
// zero or multiple return an error so the caller rejects rather than
// guessing attribution.
func resolveSingleActiveHumanParticipantId(ctx context.Context, engine voiceParticipantResolver, spaceId string) (string, error) {
	if engine == nil {
		return "", fmt.Errorf("engine not configured")
	}
	// Participant rows carry the auto-canonicalized space id
	// (`<partition>:v1:cognition:space:<slug>`) while the voice-agent passes
	// the bare slug; canonicalize before filtering (mirrors
	// resolveGroupGAAgentId). On canonicalize failure fall back to the wire
	// value so an already-canonical id still resolves.
	canonicalSpace, err := engine.CanonicalizeIdValue(ctx, spaceId, "v1:cognition:space")
	if err != nil || canonicalSpace == "" {
		canonicalSpace = spaceId
	}
	// JSON-marshal the interpolated id so an embedded double quote cannot
	// break out of the DSL string literal.
	spaceJSON, _ := json.Marshal(canonicalSpace)
	query := fmt.Sprintf(`querySpaceParticipants({spaceId: %s, participantType: "human", status: "active"})`, string(spaceJSON))
	result, err := engine.Execute(ctx, query)
	if err != nil {
		return "", fmt.Errorf("query active human participants: %w", err)
	}
	if result == nil {
		return "", fmt.Errorf("no active human participant in space")
	}
	return singleActiveHumanParticipantId(result.OutputPayload())
}

// singleActiveHumanParticipantId applies the conservative fallback rule to a
// querySpaceParticipants result payload: exactly one active human participant
// row -> its canonical participant id; zero or multiple -> error. The
// defensive concept / participantType re-checks mirror
// integrations/cognition's findParticipantsByType (the shape runtime has been
// observed leaking non-matching nodes through a filtered query).
func singleActiveHumanParticipantId(payload any) (string, error) {
	seen := map[string]bool{}
	ids := make([]string, 0, 2)
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}

	for _, row := range normalizeResultRows(payload) {
		if concept, _ := row["concept"].(string); concept != "" && concept != "v1:cognition:participant" {
			continue
		}
		inner, _ := row["payload"].(map[string]any)
		pt, hasPT := "", false
		if inner != nil {
			if v, ok := inner["participantType"].(string); ok && v != "" {
				pt, hasPT = v, true
			}
		}
		if v, ok := row["participantType"].(string); ok && v != "" {
			pt, hasPT = v, true
		}
		if hasPT && pt != "human" {
			continue
		}
		id, _ := row["id"].(string)
		if id == "" && inner != nil {
			id, _ = inner["id"].(string)
		}
		add(id)
	}
	// `select payload.X` results can surface only as a GraphBundle (no row
	// maps); drill into Bundle.Nodes as the final fallback, mirroring
	// extractFirstAgentIdFromParticipants.
	if bundle, ok := payload.(*memqlv1.GraphBundle); ok && bundle != nil {
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			if c := n.GetConcept(); c != "" && c != "v1:cognition:participant" {
				continue
			}
			if p := n.GetPayload(); p != nil {
				if f, ok := p.GetFields()["participantType"]; ok && f != nil {
					if v := f.GetStringValue(); v != "" && v != "human" {
						continue
					}
				}
			}
			add(n.GetId())
		}
	}

	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", fmt.Errorf("no active human participant in space")
	default:
		return "", fmt.Errorf("%d active human participants in space (ambiguous speaker)", len(ids))
	}
}

func contextWithVoiceAgentActor(ctx context.Context) context.Context {
	claims := map[string]any{
		"sub":   "voice-agent",
		"email": "voice-agent@memql.internal",
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}
