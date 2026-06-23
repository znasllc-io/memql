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
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
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
	args := make([]any, 0, len(attrs)+4)
	args = append(args, voiceTimingKey, phase, "duration_ms", time.Since(start).Milliseconds())
	args = append(args, attrs...)
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
// The ack carries the resolved channel modes, the GA persona identity (#478)
// and the vendor-issued avatar persona id (#1428); Phase 7 fills in the gate
// state.
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

	// Session-setup timing (#1426): every engine call below runs before the
	// SessionAck the voice-agent is waiting on -- on a slow DB these are the
	// session-setup chokepoints. #1429 restructured the shape: the GA-id
	// resolve stays first (every other leg keys off the resolved id), then the
	// four independent loads (audio override, video override, agent record,
	// avatar persona) run CONCURRENTLY, and the two per-channel agent-record
	// fallback reads + the persona read collapse into one agent-record query.
	// Net: many sequential engine round-trips -> 2 sequential stages, the
	// second 4-way parallel. Each leg is still timed individually plus the
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

	// Concurrent stage. The override resolves carry the orb-corner toggle
	// state (without them the voice-agent always saw "mirror_user", so
	// toggling Sofia's video icon to `always_on` never reached the avatar-gate
	// at session start); the agent-record read carries the persona identity
	// (#478) plus the audioControl/videoControl defaults the override layering
	// falls back to. Every leg is best-effort -- a query failure degrades that
	// leg to its fallback so the session still comes up.
	state := loadVoiceSessionStartState(voiceSessionStartLoads{
		audioOverride: func() (string, bool) {
			legStart := time.Now()
			mode, ok := resolveChannelOverrideMode(s, spaceId, gaAgentId, "audio")
			logVoiceTiming(s.logger, "server.session_start.audio_mode", legStart, "space_id", spaceId)
			return mode, ok
		},
		videoOverride: func() (string, bool) {
			legStart := time.Now()
			mode, ok := resolveChannelOverrideMode(s, spaceId, gaAgentId, "video")
			logVoiceTiming(s.logger, "server.session_start.video_mode", legStart, "space_id", spaceId)
			return mode, ok
		},
		agentRecord: func() agentRecordFields {
			legStart := time.Now()
			record := resolveAgentSessionRecord(s, gaAgentId)
			logVoiceTiming(s.logger, "server.session_start.persona", legStart, "space_id", spaceId)
			return record
		},
		// The vendor-issued avatar persona id (Simli faceId / Anam persona id)
		// for the vendor the voice-agent runs (#1428): stamped catalog row id
		// -> hydrate to the entry's vendor-issued id; stamped raw vendor id ->
		// verbatim; unstamped -> catalog default (gender-matched, else first).
		// Vendor-gated: the ack has no vendor field, so an id is returned only
		// when the resolved entry's vendor matches the vendor the agent asked
		// for. Best-effort: any failure yields "" and the session rides
		// audio-only. Independent of the other legs (its own agent-record +
		// catalog reads), so it rides the parallel stage.
		avatarPersona: func() string {
			legStart := time.Now()
			id := resolveAgentAvatarPersona(s, gaAgentId, msg.GetAvatarVendor())
			logVoiceTiming(s.logger, "server.session_start.avatar_persona", legStart, "space_id", spaceId)
			return id
		},
		// #1470 Option C: the bound space's name + goal statement and the human
		// participants present. Injected into the realtime session instructions
		// so Sofia knows WHERE she is and WHO she's talking to -- the native
		// realtime path bypasses cognition (which is where the chat path gets
		// this). Independent of the other legs (its own space + participant
		// reads), so it rides the parallel stage. Best-effort: any failure
		// yields the zero value and the ack fields stay empty.
		spaceContext: func() spaceContextFields {
			legStart := time.Now()
			ctxFields := resolveVoiceSpaceContext(s, spaceId)
			logVoiceTiming(s.logger, "server.session_start.space_context", legStart, "space_id", spaceId)
			return ctxFields
		},
	})
	audioMode := state.audioMode
	videoMode := state.videoMode
	persona := state.persona
	avatarPersonaId := state.avatarPersonaId
	spaceCtx := state.spaceContext
	logVoiceTiming(s.logger, "server.session_start.total", setupStart, "space_id", spaceId)

	if s.logger != nil {
		s.logger.Info("voice-agent session start",
			"request_id", requestId,
			"space_id", spaceId,
			"ga_agent_id", gaAgentId,
			"room_name", msg.GetRoomName(),
			"avatar_vendor", msg.GetAvatarVendor(),
			"avatar_persona_resolved", avatarPersonaId != "",
			"audio_mode", audioMode,
			"video_mode", videoMode,
			"persona_name", persona.name,
			"persona_populated", persona.name != "" || persona.description != "" || persona.personality != "",
			"space_name", spaceCtx.name,
			"space_purpose_set", spaceCtx.purpose != "",
			"participant_count", len(spaceCtx.participantNames))
	}

	// Bind the stream to this (space, ga_agent_id) and kick off the
	// session-long AI-reply subscriber. The subscriber forwards AI
	// reply utterances as VoiceAgentSpeak so chat-typed user
	// messages produce audible replies via AgentSession.say(). The
	// existing TurnRequest path still handles STT-initiated turns;
	// the subscriber dedups against in-flight TurnRequests via the
	// voiceTurns map.
	s.startVoiceAgentSpeakSubscriber(spaceId, gaAgentId, audioMode)

	// Stash the GA's role on the session so handleCallTool can gate tool
	// EXECUTION against the GA's role (assistant), not the voice-agent caller's
	// empty role. Without this, @allowedRoles("assistant") GA-only tools (the
	// operator/uiClick Takeover surface, produceArtifact) pass the ListTools
	// scope gate (#1467) but are rejected at execution ("not allowed for caller
	// role"), so Sofia acknowledges the action but can't actually perform it.
	s.voiceAgentSpeakSubMu.Lock()
	s.voiceAgentGaRole = persona.role
	s.voiceAgentSpeakSubMu.Unlock()

	return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSessionAck{
			VoiceAgentSessionAck: &memqlv1.VoiceAgentSessionAck{
				RequestId:         requestId,
				Success:           true,
				GaCanonicalVoice:  "alto",
				GaAvatarPersonaId: avatarPersonaId,
				InitialAudioMode:  audioMode,
				InitialVideoMode:  videoMode,
				GaDisplayName:     persona.name,
				GaRole:            persona.role,
				GaDescription:     persona.description,
				GaPersonality:     persona.personality,
				SpaceName:         spaceCtx.name,
				SpacePurpose:      spaceCtx.purpose,
				ParticipantNames:  spaceCtx.participantNames,
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
	var logger *slog.Logger
	if s != nil {
		logger = s.logger
	}
	ctx := contextWithVoiceAgentActor(context.Background())
	return resolveGroupGAAgentIdVia(ctx, s.service.engine, spaceId, logger)
}

// resolveGroupGAAgentIdVia is the seam-based core of resolveGroupGAAgentId:
// it resolves the canonical GA agent id of the group-GA participant in
// `spaceId` through the narrow voiceParticipantResolver interface so the
// resolution (canonicalize -> queryGroupGAForSpace -> first participant
// agentId) is unit-testable without a live engine. Returns "" when no GA
// participant is active or on any query/canonicalize error -- callers fall
// back to their wire-supplied id.
func resolveGroupGAAgentIdVia(ctx context.Context, engine voiceParticipantResolver, spaceId string, logger *slog.Logger) string {
	if engine == nil || strings.TrimSpace(spaceId) == "" {
		return ""
	}
	// Canonicalize the spaceId on the Go side. Inlining canonicalId()
	// in the query predicate triggers "unsupported literal type
	// *ast.CanonicalIdExpr" once the WHERE chain includes a bool
	// comparison (see queryGroupGAForSpace.memql).
	canonicalSpace, err := engine.CanonicalizeIdValue(ctx, spaceId, "v1:cognition:space")
	if err != nil || canonicalSpace == "" {
		if logger != nil {
			logger.Debug("resolveGroupGAAgentId canonicalize failed",
				"space_id", spaceId, "error", err)
		}
		return ""
	}
	canonicalSpaceJSON, _ := json.Marshal(canonicalSpace)
	query := fmt.Sprintf(`queryGroupGAForSpace({spaceId: %s})`, string(canonicalSpaceJSON))
	result, err := engine.Execute(ctx, query)
	if err != nil || result == nil {
		if logger != nil {
			logger.Debug("resolveGroupGAAgentId query failed",
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

	// 1) Active per-(space, agent) override.
	if mode, ok := resolveChannelOverrideMode(s, spaceId, agentId, channel); ok {
		return mode
	}

	// 2) Agent record's default, read off the agent row via the tested
	// queryAgentById named query (agentFull carries audioControl/videoControl).
	// The retired `?.id==` raw query parsed-failed here, so this leg always fell
	// through to "mirror_user" (#1454).
	field := "audioControl"
	if channel == "video" {
		field = "videoControl"
	}
	ctx := contextWithVoiceAgentActor(context.Background())
	if result, err := s.service.engine.Execute(ctx, queryAgentByIdQuery(agentId)); err == nil && result != nil {
		if mode, ok := extractFirstAgentField(result.OutputPayload(), field); ok && isValidChannelMode(mode) {
			return mode
		}
	}

	return fallback
}

// resolveChannelOverrideMode runs ONLY the override-row leg of the channel-mode
// layering: the active v1:cognition:{audio,video}override row for this (space,
// agent) pair (the orb-corner toggle's output). ok=false means no applicable
// override (or a query error) -- callers fall through to the agent-record
// default / "mirror_user". Split out of resolveInitialChannelMode (#1429) so
// session-start can run it concurrently with the single combined agent-record
// read instead of paying two sequential queries per channel.
func resolveChannelOverrideMode(s *streamSession, spaceId, agentId, channel string) (string, bool) {
	if s == nil || s.service == nil || s.service.engine == nil {
		return "", false
	}
	queryName := "queryAudioOverridesForSpace"
	if channel == "video" {
		queryName = "queryVideoOverridesForSpace"
	}
	ctx := contextWithVoiceAgentActor(context.Background())
	// Marshal spaceId so an embedded double quote cannot break out of the DSL
	// string literal.
	spaceIdJSON, _ := json.Marshal(spaceId)
	overrideQuery := fmt.Sprintf(`%s({spaceId: %s})`, queryName, string(spaceIdJSON))
	result, err := s.service.engine.Execute(ctx, overrideQuery)
	if err != nil {
		return "", false
	}
	return extractAgentChannelMode(result.OutputPayload(), agentId)
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

// agentRecordFields is everything session-start needs off the GA's
// v1:agents:agent row, loaded in ONE query (#1429): the persona identity
// (#478) plus the audioControl/videoControl defaults the channel-mode
// layering falls back to when no override row applies. Previously the
// persona and each channel's control default were three separate reads of
// the same row.
type agentRecordFields struct {
	persona      agentPersonaFields
	audioControl string
	videoControl string
}

// queryAgentByIdQuery builds the canonical agent-row read every voice-session
// leg (persona, channel-mode default, avatar persona, tool scope) issues: the
// tested `queryAgentById({agentId: <id>})` named query, which filters
// `id==args.agentId` and projects the agentFull shape (name, role, description,
// personality, audioControl, videoControl, avatarVendor, avatarPersonaId,
// gender, capabilities). It REPLACES the four hand-written
// `from(v1:agents:agent) ?.id==%s select …` raw queries that used the retired
// `?.` optional-chain syntax the parser rejects -- so every voice-session read
// off the agent row parse-failed and fell back (#1454).
//
// agentId is JSON-marshalled so an id carrying a double quote cannot break out
// of the DSL string literal (CodeQL "unsafe quoting"): marshal yields a quoted,
// escaped literal token, dropped straight into the named-query arg.
func queryAgentByIdQuery(agentId string) string {
	agentIdJSON, _ := json.Marshal(agentId)
	return fmt.Sprintf(`queryAgentById({agentId: %s})`, string(agentIdJSON))
}

// resolveAgentSessionRecord loads the GA's persona identity + channel-control
// defaults from its v1:agents:agent record in one query. Best-effort: any
// failure (nil engine, query error, missing row) returns the zero value so the
// caller stamps empty persona fields / falls back to "mirror_user" and the
// voice session comes up rather than failing bring-up.
func resolveAgentSessionRecord(s *streamSession, agentId string) agentRecordFields {
	if s == nil || s.service == nil || s.service.engine == nil {
		return agentRecordFields{}
	}
	ctx := contextWithVoiceAgentActor(context.Background())
	return resolveAgentSessionRecordVia(ctx, s.service.engine, agentId)
}

// resolveAgentSessionRecordVia is the seam-based core of
// resolveAgentSessionRecord: it reads the GA's persona identity (#478) +
// channel-control defaults off its v1:agents:agent row through the narrow
// voiceParticipantResolver, so the persona resolution is unit-testable and
// provably keyed on the REAL resolved agent id (#1442/#1387) rather than the
// placeholder. Best-effort: any failure returns the zero value.
func resolveAgentSessionRecordVia(ctx context.Context, engine voiceParticipantResolver, agentId string) agentRecordFields {
	var out agentRecordFields
	if engine == nil {
		return out
	}
	// Read the persona + channel-control defaults off the agent row via the
	// tested queryAgentById named query (returns agentFull, which carries name /
	// role / description / personality / audioControl / videoControl). The prior
	// hand-written `from(...) ?.id==` raw query used the RETIRED optional-chain
	// syntax, which the parser rejects ("unexpected token after expression:
	// \"?.\""), so this leg ALWAYS errored -> empty persona -> voice fell back to
	// the neutral default voice (#1454).
	result, err := engine.Execute(ctx, queryAgentByIdQuery(agentId))
	if err != nil || result == nil {
		return out
	}
	payload := result.OutputPayload()
	out.persona.name, _ = extractFirstAgentField(payload, "name")
	out.persona.role, _ = extractFirstAgentField(payload, "role")
	out.persona.description, _ = extractFirstAgentField(payload, "description")
	out.persona.personality, _ = extractFirstAgentField(payload, "personality")
	out.audioControl, _ = extractFirstAgentField(payload, "audioControl")
	out.videoControl, _ = extractFirstAgentField(payload, "videoControl")
	return out
}

// spaceContextFields is the bound-space context the SessionAck carries for the
// realtime instructions (#1470 Option C): the space display name, its goal
// statement (purpose), and the human participants present. Each is best-effort
// -- a resolution failure leaves it empty and the voice instruction builder
// simply omits the corresponding line.
type spaceContextFields struct {
	name             string
	purpose          string
	participantNames []string
}

// resolveVoiceSpaceContext loads the bound space's name + goal statement and the
// human participants present, for injection into the realtime session
// instructions (#1470). Best-effort throughout: a nil engine, a query error, or
// a missing row degrades that piece to empty rather than failing session
// bring-up (the voice session still comes up, just without that context line).
func resolveVoiceSpaceContext(s *streamSession, spaceId string) spaceContextFields {
	if s == nil || s.service == nil || s.service.engine == nil {
		return spaceContextFields{}
	}
	ctx := contextWithVoiceAgentActor(context.Background())
	return resolveVoiceSpaceContextVia(ctx, s.service.engine, spaceId)
}

// resolveVoiceSpaceContextVia is the seam-based core of resolveVoiceSpaceContext
// (mirrors resolveAgentSessionRecordVia / resolveGroupGAAgentIdVia): it resolves
// the space name + goal statement + human-participant display names through the
// narrow voiceParticipantResolver so the resolution is unit-testable without a
// live engine. Each leg is independent and best-effort.
func resolveVoiceSpaceContextVia(ctx context.Context, engine voiceParticipantResolver, spaceId string) spaceContextFields {
	var out spaceContextFields
	if engine == nil || strings.TrimSpace(spaceId) == "" {
		return out
	}

	// Canonicalize the bare space slug the voice-agent passes to the stored
	// `<partition>:v1:cognition:space:<slug>` form the rows are keyed by, mirroring
	// resolveSingleActiveHumanParticipantId. On failure fall back to the wire
	// value so an already-canonical id still resolves.
	canonicalSpace, err := engine.CanonicalizeIdValue(ctx, spaceId, "v1:cognition:space")
	if err != nil || canonicalSpace == "" {
		canonicalSpace = spaceId
	}
	spaceJSON, _ := json.Marshal(canonicalSpace)

	// Space name + goal statement off the space row (spaceFull projects name +
	// goal).
	if result, err := engine.Execute(ctx, fmt.Sprintf(`querySpaceMeta({spaceId: %s})`, string(spaceJSON))); err == nil && result != nil {
		payload := result.OutputPayload()
		out.name, _ = extractFirstAgentField(payload, "name")
		out.purpose = extractSpaceGoalStatement(payload)
	}

	// Human participant display names. Reuse the existing querySpaceParticipants
	// path the speaker-attribution fallback uses.
	if result, err := engine.Execute(ctx, fmt.Sprintf(`querySpaceParticipants({spaceId: %s, participantType: "human", status: "active"})`, string(spaceJSON))); err == nil && result != nil {
		out.participantNames = extractParticipantNames(result.OutputPayload())
	}

	return out
}

// resolveVoiceSpaceOwnerVia resolves a space's ownerUserId through the narrow
// voiceParticipantResolver so it is unit-testable without a live engine
// (mirrors resolveVoiceSpaceContextVia). This is the source of the auto-injected
// `ownerUserId` tool default on the realtime voice CallTool proxy hop (#1503):
// the voice-agent authenticates as a service identity (class="voice_agent"),
// so actor.userId is NOT the human and the ownerUserId default would otherwise
// be empty -- the space owner is the correct artifact attribution (a plan in a
// space belongs to the space owner; the daily space owner IS the user). Returns
// "" on any lookup miss so the caller leaves the default unset rather than
// stamping a wrong owner.
func resolveVoiceSpaceOwnerVia(ctx context.Context, engine voiceParticipantResolver, spaceId string) string {
	if engine == nil || strings.TrimSpace(spaceId) == "" {
		return ""
	}
	// Canonicalize the bare space slug to the stored
	// `<partition>:v1:cognition:space:<slug>` form (mirrors
	// resolveVoiceSpaceContextVia); fall back to the wire value so an
	// already-canonical id still resolves.
	canonicalSpace, err := engine.CanonicalizeIdValue(ctx, spaceId, "v1:cognition:space")
	if err != nil || canonicalSpace == "" {
		canonicalSpace = spaceId
	}
	spaceJSON, _ := json.Marshal(canonicalSpace)
	result, err := engine.Execute(ctx, fmt.Sprintf(`querySpaceMeta({spaceId: %s})`, string(spaceJSON)))
	if err != nil || result == nil {
		return ""
	}
	owner, _ := extractFirstAgentField(result.OutputPayload(), "ownerUserId")
	return strings.TrimSpace(owner)
}

// extractSpaceGoalStatement pulls payload.goal.statement off a spaceFull-shaped
// result row. Empty when the space carries no goal (the AI-describe create path
// produces a name without a goal). Probes both the bare `goal` key and the
// nested `payload.goal` map so it works on shape() output and bundle-derived
// row maps.
func extractSpaceGoalStatement(payload any) string {
	for _, row := range normalizeResultRows(payload) {
		goal, ok := row["goal"].(map[string]any)
		if !ok {
			if inner, ok := row["payload"].(map[string]any); ok {
				goal, _ = inner["goal"].(map[string]any)
			}
		}
		if goal == nil {
			continue
		}
		if v, ok := goal["statement"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// extractParticipantNames pulls the human participants' display names off a
// querySpaceParticipants result. Falls back to the userId when a participant
// carries no displayName so a present-but-unnamed human still surfaces. Order
// follows the result rows; duplicates are de-duped. Returns nil when there are
// no rows.
func extractParticipantNames(payload any) []string {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, row := range normalizeResultRows(payload) {
		inner, _ := row["payload"].(map[string]any)
		pickField := func(field string) string {
			if v, ok := row[field].(string); ok && v != "" {
				return v
			}
			if inner != nil {
				if v, ok := inner[field].(string); ok && v != "" {
					return v
				}
			}
			return ""
		}
		if name := pickField("displayName"); name != "" {
			add(name)
			continue
		}
		add(pickField("userId"))
	}
	return names
}

// voiceAvatarPersonaCatalogPrefix is the canonical id prefix of the operator
// avatar-persona catalog rows (v1:agents:avatarPersona). The CoPresent
// PersonaPicker stamps agents with the CATALOG ROW ID, not the vendor-issued
// face/persona id, so a stamped value carrying this prefix must be hydrated
// through the catalog before it reaches the avatar vendor client (#1336).
// Mirrors integrations/avatardirect's avatarPersonaCatalogPrefix.
const voiceAvatarPersonaCatalogPrefix = "v1:agents:avatarPersona:"

// resolveAgentAvatarPersona resolves the vendor-issued avatar persona id
// (Simli faceId / Anam persona id) the voice session ack carries (#1428),
// applying the avatardirect #1336/#1341 rules to the voice path:
//
//  1. Agent stamped with a catalog row id -> hydrate via queryAvatarPersonaById
//     to the entry's vendor-issued personaId.
//  2. Agent stamped with a raw vendor id (legacy direct stamping) -> verbatim,
//     when the stamped vendor matches the requested one.
//  3. Agent unstamped (every auto-provisioned assistant) -> the catalog
//     default: the entry matching the agent's gender, else the first entry.
//
// requestedVendor is the vendor the voice-agent runs (VoiceAgentSessionStart.
// avatar_vendor, from MEMQL_AVATAR_VENDOR). The ack proto carries no vendor
// field, so a persona id is returned ONLY when the resolved entry's vendor
// matches -- handing a Simli client an Anam id would just fail the vendor REST
// call. Best-effort throughout: nil engine, query errors, or no usable entry
// all return "" (the voice session rides audio-only, never fails bring-up).
func resolveAgentAvatarPersona(s *streamSession, agentId, requestedVendor string) string {
	vendor := strings.ToLower(strings.TrimSpace(requestedVendor))
	if vendor == "" || vendor == "none" {
		return ""
	}
	if s == nil || s.service == nil || s.service.engine == nil {
		return ""
	}
	ctx := contextWithVoiceAgentActor(context.Background())

	// Agent record: stamped vendor/persona id + gender (for the catalog
	// default), read via the tested queryAgentById named query (agentFull
	// carries avatarVendor / avatarPersonaId / gender). The prior `?.id==` raw
	// query used the retired optional-chain syntax the parser rejects, so this
	// leg always errored -> audio-only (#1454).
	result, err := s.service.engine.Execute(ctx, queryAgentByIdQuery(agentId))
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("voice-agent avatar persona: agent query failed -- audio-only",
				"agent_id", agentId, "err", err)
		}
		return ""
	}
	payload := result.OutputPayload()
	stampedVendor, _ := extractFirstAgentField(payload, "avatarVendor")
	stampedId, _ := extractFirstAgentField(payload, "avatarPersonaId")
	gender, _ := extractFirstAgentField(payload, "gender")
	stampedId = strings.TrimSpace(stampedId)

	// 1. Catalog row id -> hydrate to the entry's vendor-issued persona id.
	if strings.HasPrefix(stampedId, voiceAvatarPersonaCatalogPrefix) {
		catalogIdJSON, _ := json.Marshal(stampedId)
		raw, err := s.service.engine.Execute(ctx,
			fmt.Sprintf(`queryAvatarPersonaById({avatarPersonaId: %s})`, string(catalogIdJSON)))
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("voice-agent avatar persona: catalog hydrate failed -- audio-only",
					"agent_id", agentId, "catalog_id", stampedId, "err", err)
			}
			return ""
		}
		return pickAvatarPersonaId(rowsFromEnginePayload(raw.OutputPayload()), vendor, "")
	}

	// 2. Raw stamped vendor id: verbatim when the stamped vendor matches.
	if stampedId != "" && strings.EqualFold(strings.TrimSpace(stampedVendor), vendor) {
		return stampedId
	}

	// 3. Catalog default for unstamped (or vendor-mismatched) agents.
	raw, err := s.service.engine.Execute(ctx, `queryAvatarPersonas({})`)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("voice-agent avatar persona: catalog default lookup failed -- audio-only",
				"agent_id", agentId, "err", err)
		}
		return ""
	}
	resolved := pickAvatarPersonaId(rowsFromEnginePayload(raw.OutputPayload()), vendor, gender)
	if resolved != "" && s.logger != nil {
		s.logger.Info("voice-agent avatar persona: using catalog default",
			"agent_id", agentId, "vendor", vendor, "gender", gender)
	}
	return resolved
}

// pickAvatarPersonaId picks the vendor-issued persona id from avatar-persona
// catalog rows: only rows whose vendor matches requestedVendor (lowercased)
// and that carry a non-empty personaId qualify; a row matching the agent's
// gender (when given) is preferred, else the first qualifying row wins. Pure
// so the selection rules are unit-tested without an engine.
func pickAvatarPersonaId(rows []map[string]any, requestedVendor, gender string) string {
	gender = strings.TrimSpace(gender)
	var pick string
	for _, row := range rows {
		rowVendor := strings.ToLower(strings.TrimSpace(rowStringField(row, "vendor")))
		if rowVendor != requestedVendor {
			continue
		}
		personaId := strings.TrimSpace(rowStringField(row, "personaId"))
		if personaId == "" {
			continue
		}
		if pick == "" {
			pick = personaId
		}
		if gender != "" && strings.EqualFold(strings.TrimSpace(rowStringField(row, "gender")), gender) {
			return personaId
		}
	}
	return pick
}

// rowStringField reads a string field off a row map, probing the bare key and
// the nested payload map (shape() output nests under payload). Mirrors
// avatardirect's rowString helper.
func rowStringField(row map[string]any, field string) string {
	if v, ok := row[field].(string); ok {
		return v
	}
	if payload, ok := row["payload"].(map[string]any); ok {
		if v, ok := payload[field].(string); ok {
			return v
		}
	}
	return ""
}

// rowsFromEnginePayload normalizes an engine result payload into row maps,
// additionally unwrapping the GraphBundle shape named queries can surface
// (normalizeResultRows handles only the map/list shapes). Mirrors
// avatardirect's extractRows.
func rowsFromEnginePayload(payload any) []map[string]any {
	if bundle, ok := payload.(*memqlv1.GraphBundle); ok && bundle != nil {
		out := make([]map[string]any, 0, len(bundle.GetNodes()))
		for _, n := range bundle.GetNodes() {
			if n == nil {
				continue
			}
			row := map[string]any{"id": n.GetId(), "concept": n.GetConcept()}
			if p := n.GetPayload(); p != nil {
				for k, v := range p.AsMap() {
					row[k] = v
				}
			}
			out = append(out, row)
		}
		return out
	}
	return normalizeResultRows(payload)
}

// voiceSessionStartLoads is the set of independent session-start loads
// loadVoiceSessionStartState fans out (#1429). Each leg is best-effort and
// must be safe to run concurrently with the others; the override legs return
// ok=false (and the record leg the zero value) on failure so the combine
// degrades per-leg instead of failing the ack.
type voiceSessionStartLoads struct {
	audioOverride func() (string, bool)
	videoOverride func() (string, bool)
	agentRecord   func() agentRecordFields
	avatarPersona func() string
	// spaceContext resolves the bound space's name + goal statement and the
	// human participants present (#1470 Option C), injected into the realtime
	// instructions so Sofia knows WHERE she is and WHO she's talking to.
	// Best-effort: any failure yields the zero value and the corresponding ack
	// fields stay empty.
	spaceContext func() spaceContextFields
}

// voiceSessionStartState is the combined result the SessionAck is built from.
type voiceSessionStartState struct {
	audioMode       string
	videoMode       string
	persona         agentPersonaFields
	avatarPersonaId string
	spaceContext    spaceContextFields
}

// loadVoiceSessionStartState runs the four independent session-start legs
// concurrently, waits for ALL of them (the ack needs every field), and
// combines them with the same layering resolveInitialChannelMode applies:
// active override row > valid agent-record control default > "mirror_user".
func loadVoiceSessionStartState(loads voiceSessionStartLoads) voiceSessionStartState {
	var (
		audioOverride, videoOverride     string
		audioOverrideOK, videoOverrideOK bool
		record                           agentRecordFields
		avatarPersonaId                  string
	)
	var spaceCtx spaceContextFields
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		if loads.audioOverride != nil {
			audioOverride, audioOverrideOK = loads.audioOverride()
		}
	}()
	go func() {
		defer wg.Done()
		if loads.videoOverride != nil {
			videoOverride, videoOverrideOK = loads.videoOverride()
		}
	}()
	go func() {
		defer wg.Done()
		if loads.agentRecord != nil {
			record = loads.agentRecord()
		}
	}()
	go func() {
		defer wg.Done()
		if loads.avatarPersona != nil {
			avatarPersonaId = loads.avatarPersona()
		}
	}()
	go func() {
		defer wg.Done()
		if loads.spaceContext != nil {
			spaceCtx = loads.spaceContext()
		}
	}()
	wg.Wait()
	return voiceSessionStartState{
		audioMode:       effectiveChannelMode(audioOverride, audioOverrideOK, record.audioControl),
		videoMode:       effectiveChannelMode(videoOverride, videoOverrideOK, record.videoControl),
		persona:         record.persona,
		avatarPersonaId: avatarPersonaId,
		spaceContext:    spaceCtx,
	}
}

// effectiveChannelMode applies the channel-mode layering to pre-fetched
// inputs: an applicable override row wins, then a VALID agent-record control
// default, then "mirror_user". Mirrors resolveInitialChannelMode exactly --
// keep the two in sync.
func effectiveChannelMode(override string, overrideOK bool, control string) string {
	if overrideOK {
		return override
	}
	if isValidChannelMode(control) {
		return control
	}
	return "mirror_user"
}

// voiceAgentScopedToolNames returns the GA agent's expanded tool-name set when
// this stream is a voice-agent session (#1419). The second return value reports
// whether scoping APPLIES:
//
//   - (set, true)  -- this is a voice session bound to a resolved GA whose
//     agent row was read authoritatively. The handler MUST restrict the
//     exposed surface to `set` exactly, EVEN IF `set` is empty (the GA has no
//     tools is a real, authoritative answer -- it is NOT a reason to dump the
//     global registry).
//   - (nil, false) -- scoping does not apply: either this is not a voice
//     session (no bound space), or the GA-id / agent-row lookup hit a genuine
//     engine error. The handler fails OPEN to the unscoped registry in this
//     case, with a loud WARN at the lookup site, so a transient engine failure
//     degrades to the old behaviour instead of stripping every voice tool.
//
// The set is the SAME surface the text loop binds: the agent row's tools field
// expanded through ExpandCapabilitySlugs. Handing the model the unscoped global
// registry (517 tools: internal logic sweeps, raw mutations) measurably
// inflated gpt-realtime's time-to-first-audio and surfaced tools no agent
// should call (#1442).
//
// CRITICAL (#1442): this resolves the GA's REAL canonical id itself, from the
// session's bound space, via the same resolveGroupGAAgentId helper SessionStart
// uses. It does NOT trust s.voiceAgentGaAgentId -- that field is only populated
// at the END of SessionStart, so ListTools (a voice setup-window read) can race
// ahead of it (field empty), and even after SessionStart it may hold the
// voice-agent's placeholder `<space>-ga` when the participant resolve came up
// empty. Either way the placeholder/empty id matched no agent row, found=false,
// and the surface failed open to 517 tools -- the regression this fixes.
func (s *streamSession) voiceAgentScopedToolNames() (map[string]struct{}, bool) {
	set, _, scoped := s.voiceAgentScopedToolNamesForRequest("", "")
	return set, scoped
}

// stampVoiceAgentScopeOnListTools copies the bound voice-session scope
// (spaceId + GA agent id) onto a ListToolsMsg before it is proxied to the agent
// node (#1448). The agent-node receiver has no local voiceAgentScopeId of its
// own, so without this the scope context is lost across the hop and the surface
// fails open to the full registry. No-op when this session isn't a voice-agent
// session (both fields empty), so non-voice callers proxy unchanged.
func (s *streamSession) stampVoiceAgentScopeOnListTools(msg *memqlv1.ListToolsMsg) {
	if s == nil || msg == nil {
		return
	}
	s.voiceAgentSpeakSubMu.Lock()
	spaceId := s.voiceAgentScopeId
	agentId := s.voiceAgentGaAgentId
	s.voiceAgentSpeakSubMu.Unlock()
	if spaceId != "" {
		msg.VoiceAgentScopeId = spaceId
	}
	if agentId != "" {
		msg.VoiceAgentGaAgentId = agentId
	}
}

// stampVoiceAgentScopeOnCallTool copies the bound voice-session execution
// context (spaceId + the resolved GA role) onto a CallToolMsg before it is
// proxied to the agent node. The agent-node receiver has no local
// voiceAgentScopeId / voiceAgentGaRole (those bind only on the bff session that
// received VoiceAgentSessionStart), so without this the agent-node role gate
// rejects GA-only tools against the empty caller role and the client-tool relay
// has no voice space to scope to. No-op for non-voice callers. Mirror of
// stampVoiceAgentScopeOnListTools (#1448).
func (s *streamSession) stampVoiceAgentScopeOnCallTool(msg *memqlv1.CallToolMsg) {
	if s == nil || msg == nil {
		return
	}
	s.voiceAgentSpeakSubMu.Lock()
	spaceId := s.voiceAgentScopeId
	gaRole := s.voiceAgentGaRole
	s.voiceAgentSpeakSubMu.Unlock()
	if spaceId != "" {
		msg.VoiceAgentScopeId = spaceId
	}
	if gaRole != "" {
		msg.VoiceAgentGaRole = gaRole
	}
}

// contextWithVoiceCallToolDefaults attaches the realtime-voice CallTool
// auto-injection defaults to ctx so the engine's central applyToolDefaults
// (in ExecuteTool) can stamp the `@autoInjected` spaceId / agentId /
// ownerUserId fields the voice model is forbidden from supplying (#1503).
//
// This is the agent-node (proxied receiver) counterpart to the chat path's
// turn-context stamping (integrations/agent/streaming.go). The voice CallTool
// arrives over the bff->agent proxy hop carrying the bound voice scope on the
// CallToolMsg (VoiceAgentScopeId / VoiceAgentGaAgentId, stamped by
// stampVoiceAgentScopeOnCallTool); the local agent-node session has none of it.
//
//   - spaceId  <- the threaded VoiceAgentScopeId.
//   - agentId  <- the space's group-GA agent (resolveGroupGAAgentId), the same
//     resolution the ListTools scope gate uses. Becomes the produced plan's
//     ownerAgentId so the production turn dispatches straight to the assistant.
//   - ownerUserId <- the bound space's owner (resolveVoiceSpaceOwner). The
//     voice-agent authenticates as a service identity, so actor.userId is NOT
//     the human; the space owner is the correct artifact attribution.
//
// agentId is resolved server-side (not threaded on the CallToolMsg) so no proto
// change is needed; ownerUserId/spaceId are the load-bearing defaults for
// produceArtifact (only those two are @required on the builtin).
//
// No-op (returns ctx unchanged) for non-voice callers -- a plain browser
// CallTool carries no VoiceAgentScopeId, and the chat path already stamps its
// own defaults upstream. Setting it for those callers would be inert anyway
// (applyToolDefaults only touches a tool's declared @autoInjected fields), but
// gating on the voice marker keeps the extra space lookups off the hot browser
// path.
func (s *streamSession) contextWithVoiceCallToolDefaults(ctx context.Context, msg *memqlv1.CallToolMsg) context.Context {
	if s == nil || s.service == nil || s.service.engine == nil || msg == nil {
		return ctx
	}
	return voiceCallToolDefaultsVia(ctx, s.service.engine, msg, s.logger)
}

// voiceCallToolDefaultsVia is the seam-based core of
// contextWithVoiceCallToolDefaults (mirrors voiceAgentScopedToolNamesVia): it
// reads the threaded voice scope off the CallToolMsg and resolves the
// auto-injection defaults through the narrow voiceParticipantResolver, so the
// cross-node proxy hop (read CallToolMsg.VoiceAgentScopeId -> resolve space
// owner -> stamp ToolDefaults) is unit-testable without a live engine + DB.
func voiceCallToolDefaultsVia(ctx context.Context, engine voiceParticipantResolver, msg *memqlv1.CallToolMsg, logger *slog.Logger) context.Context {
	if engine == nil || msg == nil {
		return ctx
	}
	spaceId := strings.TrimSpace(msg.GetVoiceAgentScopeId())
	if spaceId == "" {
		// Not a proxied voice CallTool (or pre-#1503 bff that doesn't thread
		// the scope) -- leave the context untouched.
		return ctx
	}
	// Run the resolution reads as the voice-agent service actor (the same actor
	// the other voice-path resolvers use), independent of the incoming stream
	// ctx's provenance. querySpaceMeta is @public; queryGroupGAForSpace reads
	// the space's GA participant.
	resolveCtx := contextWithVoiceAgentActor(ctx)
	defaults := map[string]any{"spaceId": spaceId}
	if agentId := resolveGroupGAAgentIdVia(resolveCtx, engine, spaceId, logger); agentId != "" {
		defaults["agentId"] = agentId
	}
	if owner := resolveVoiceSpaceOwnerVia(resolveCtx, engine, spaceId); owner != "" {
		defaults["ownerUserId"] = owner
	}
	return common.ContextWithToolDefaults(ctx, defaults)
}

// voiceAgentScopedToolNamesForRequest resolves the GA tool-scope using the
// scope threaded through the ListTools request first (reqSpaceId / reqAgentId,
// stamped by the bff before proxying -- #1448), falling back to this session's
// locally bound scope for non-proxied callers. Everything else is identical to
// the single-node #1442 resolution: resolve the GA's real id, read its tool
// surface, fail open ONLY on a genuine lookup error.
func (s *streamSession) voiceAgentScopedToolNamesForRequest(reqSpaceId, reqAgentId string) (map[string]struct{}, string, bool) {
	if s == nil {
		return nil, "", false
	}
	s.voiceAgentSpeakSubMu.Lock()
	localSpaceId := s.voiceAgentScopeId
	localAgentId := s.voiceAgentGaAgentId
	s.voiceAgentSpeakSubMu.Unlock()

	// The threaded request scope wins (the proxied agent-node receiver path,
	// where the local session has no bound scope); local session state is the
	// fallback for in-process / non-proxied callers.
	spaceId := strings.TrimSpace(reqSpaceId)
	if spaceId == "" {
		spaceId = localSpaceId
	}
	boundAgentId := strings.TrimSpace(reqAgentId)
	if boundAgentId == "" {
		boundAgentId = localAgentId
	}
	if s.service == nil {
		return nil, "", false
	}
	return voiceAgentScopedToolNamesVia(
		contextWithVoiceAgentActor(context.Background()),
		s.service.engine, spaceId, boundAgentId, s.logger)
}

// voiceAgentScopedToolNamesVia is the seam-based core of the GA tool-scope
// resolution (#1442/#1448): given the effective (spaceId, boundAgentId) -- after
// the threaded-request-vs-local-session selection -- it resolves the GA's real
// id and reads its tool surface through the narrow voiceParticipantResolver, so
// the cross-node threaded path is unit-testable without a live engine. Returns
// (nil,false) -- fail open to the unscoped registry -- ONLY on a genuine lookup
// error or when nothing is bound; an authoritative empty surface scopes to the
// empty set (the #1442 policy).
func voiceAgentScopedToolNamesVia(
	ctx context.Context,
	engine voiceParticipantResolver,
	spaceId, boundAgentId string,
	logger *slog.Logger,
) (scope map[string]struct{}, role string, scoped bool) {
	// Not a voice-agent session: no space bound and no agent bound -> no
	// scoping applies, every other caller keeps the unscoped registry.
	if spaceId == "" && boundAgentId == "" {
		return nil, "", false
	}
	if engine == nil {
		return nil, "", false
	}

	// Resolve the GA's real canonical id now, independent of SessionStart
	// ordering. Prefer a fresh space-based resolve; fall back to the bound id
	// only when we have no space to resolve from (defensive -- in practice a
	// voice session always binds a space).
	agentId := ""
	if spaceId != "" {
		agentId = resolveGroupGAAgentIdVia(ctx, engine, spaceId, logger)
	}
	if agentId == "" {
		// The space resolve failed (no active AI participant, or query error)
		// -- fall back to the bound id. This may be the placeholder, in which
		// case resolveAgentToolSlugs reports found=false and we fail open.
		agentId = boundAgentId
	}
	if agentId == "" {
		// Nothing to scope against and no row to read -> fail open with a WARN
		// so this is visible if it ever happens on a real voice session.
		if logger != nil {
			logger.Warn("voice-agent tool scope: no GA id resolvable -- serving unscoped registry",
				"space_id", spaceId)
		}
		return nil, "", false
	}

	raw, agentRole, found := resolveAgentToolSlugsVia(ctx, engine, agentId, logger)
	set, ok := scopeSetFromSlugs(raw, found)
	if !ok {
		// Genuine lookup failure (query error, or no agent row for this id) --
		// fail open. resolveAgentToolSlugsVia already logged the WARN for a query
		// error; log here for the row-not-found case so both are visible.
		if logger != nil {
			logger.Warn("voice-agent tool scope: agent row not resolved -- serving unscoped registry",
				"space_id", spaceId, "agent_id", agentId)
		}
		return nil, "", false
	}
	if logger != nil {
		logger.Debug("voice-agent tool scope resolved",
			"space_id", spaceId, "agent_id", agentId, "scoped_count", len(set), "agent_role", agentRole)
	}
	return set, agentRole, true
}

// scopeSetFromSlugs turns the (raw slugs, found) answer from resolveAgentToolSlugs
// into the (scope set, scoped) the ListTools handler enforces. Pure so the
// fail-open policy is unit-testable without an engine (#1442):
//
//   - found=false (query error / no agent row) -> (nil, false): fail open to
//     the unscoped registry. This is the ONLY fail-open path.
//   - found=true -> (set, true): authoritative. The set is the slug-expanded
//     surface, EVEN WHEN EMPTY. A GA that legitimately exposes no tools scopes
//     to nothing -- it must NOT fall open to the 517-tool global registry,
//     which is the privilege/latency footgun that hid the #1442 regression.
func scopeSetFromSlugs(raw []string, found bool) (map[string]struct{}, bool) {
	if !found {
		return nil, false
	}
	expanded := memqlengine.ExpandCapabilitySlugs(raw)
	set := make(map[string]struct{}, len(expanded))
	for _, n := range expanded {
		set[n] = struct{}{}
	}
	return set, true
}

// resolveAgentToolSlugsVia reads the agent's effective tool surface through the
// narrow voiceParticipantResolver so the row-shape + fail-open distinction is
// unit-testable. It runs the tested queryAgentById named query, reads the
// agent's capabilities.skillIds[], and resolves those skill ids into concrete
// tool slugs via engine.ResolveSkills -- the SAME path the cognition local
// generation uses (integrations/cognition/ai_responder.go's getAgent, #158).
//
// This REPLACES the retired `from(v1:agents:agent) ?.id==%s select id,
// payload.tools` raw query, which (a) used the `?.` optional-chain syntax the
// parser rejects (so it ALWAYS errored -> fail-open to the 517-tool registry),
// and (b) read the flat payload.tools list, which is EMPTY on every assistant
// materialized after the #158 skills migration (capabilities now live under
// capabilities.skillIds). Both bugs independently produced the unscoped 517
// (#1454).
//
// found=false means the agent row could not be read (query error / no row) --
// callers fail OPEN to the unscoped registry, the ONLY fail-open path (#1442).
// found=true (even with an empty slice) is AUTHORITATIVE: the agent's resolved
// tool surface, which the caller scopes to exactly. A skill-resolution error
// (ResolveSkills failing) is treated like a query error -> fail open with a
// WARN, since serving zero tools off a botched resolve would silently break the
// session. Keeps the #1432 server.tools.agent_scope_lookup span.
func resolveAgentToolSlugsVia(ctx context.Context, engine voiceParticipantResolver, agentId string, logger *slog.Logger) (slugs []string, role string, found bool) {
	if engine == nil {
		return nil, "", false
	}
	// #1426: synchronous engine read on the voice-session ListTools path
	// (part of the realtime media-bridge build / setup window).
	scopeStart := time.Now()
	result, err := engine.Execute(ctx, queryAgentByIdQuery(agentId))
	logVoiceTiming(logger, "server.tools.agent_scope_lookup", scopeStart,
		"agent_id", agentId, "ok", err == nil)
	if err != nil || result == nil {
		if logger != nil {
			logger.Warn("voice-agent tool scope: agent query failed -- serving unscoped registry",
				"agent_id", agentId, "err", err)
		}
		return nil, "", false
	}
	rows := normalizeResultRows(result.OutputPayload())
	// The GA agent's role gates which tools it may expose. Read it from the same
	// row so the ListTools role check runs against the SCOPED agent's role, not
	// the (empty) voice-agent caller role -- without this, @allowedRoles("assistant")
	// GA-only tools (produceArtifact, the operator/uiClick surface) are scoped in
	// but then role-filtered out, since an empty caller role defaults to "specialist".
	role = roleFromAgentRows(rows)
	skillIds, found := skillIdsFromAgentRows(rows)
	if !found {
		// No agent row -> lookup failure -> fail open.
		return nil, "", false
	}
	if len(skillIds) == 0 {
		// Row found but no skills declared -- an authoritative empty surface.
		return nil, role, true
	}
	bundle, err := engine.ResolveSkills(ctx, skillIds)
	if err != nil {
		if logger != nil {
			logger.Warn("voice-agent tool scope: skill resolution failed -- serving unscoped registry",
				"agent_id", agentId, "skill_ids", skillIds, "err", err)
		}
		return nil, "", false
	}
	// bundle.ToolSlugs is the resolved tool surface; scopeSetFromSlugs applies
	// ExpandCapabilitySlugs for any operator fan-out (copresent-takeover ->
	// uiClick/uiType/...).
	return bundle.ToolSlugs, role, true
}

// roleFromAgentRows extracts the agent's role (payload.role) from a queryAgentById
// result row. Empty when absent -- callers fall back to the caller role.
func roleFromAgentRows(rows []map[string]any) string {
	for _, row := range rows {
		if v := strings.TrimSpace(rowStringField(row, "role")); v != "" {
			return v
		}
	}
	return ""
}

// skillIdsFromAgentRows extracts the first row's capabilities.skillIds list.
// Pure so the row-shape handling (nested capabilities map; []string vs []any
// from JSON decoding) is unit-testable without an engine. found=false means no
// row at all (lookup failure -> fail open); found=true with an empty slice
// means the agent exists but declares no skills (authoritative empty surface).
func skillIdsFromAgentRows(rows []map[string]any) ([]string, bool) {
	for _, row := range rows {
		caps := capabilitiesMapFromRow(row)
		if caps == nil {
			// Row exists but carries no capabilities block -- a real (empty)
			// answer. (A pre-#158 row with a flat tools[] list is not a valid
			// post-migration assistant; treat the missing skillIds as empty.)
			return nil, true
		}
		return stringSliceFromRowValue(caps["skillIds"]), true
	}
	return nil, false
}

// capabilitiesMapFromRow returns the agent row's capabilities object, probing
// the bare key and the nested payload map (shaped results nest under payload).
func capabilitiesMapFromRow(row map[string]any) map[string]any {
	if caps, ok := row["capabilities"].(map[string]any); ok {
		return caps
	}
	if payload, ok := row["payload"].(map[string]any); ok {
		if caps, ok := payload["capabilities"].(map[string]any); ok {
			return caps
		}
	}
	return nil
}

// stringSliceFromRowValue normalizes a JSON-decoded list value ([]string or
// []any) into a trimmed, non-empty []string. Mirrors the row-shape handling the
// old payload.tools reader carried.
func stringSliceFromRowValue(v any) []string {
	switch list := v.(type) {
	case []string:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
				out = append(out, strings.TrimSpace(name))
			}
		}
		return out
	default:
		return nil
	}
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
	case *memqlv1.GraphBundle:
		// A plain `select payload.X` query (no `shape` template) surfaces only
		// as a GraphBundle: ExecuteResult.OutputPayload() returns r.Bundle and
		// the projected fields live in each node's Payload (applyPlanProjection
		// writes them there). Without this case every voice-agent read off such
		// a query parsed to zero rows -> blank persona / channel controls and a
		// tool-scope fail-open to the 517-tool registry (#1387/#1448). Flatten
		// each node into the row shape the row-map callers expect: payload
		// fields hoisted to the top level, a nested `payload` map, and the id.
		return graphBundleRows(v)
	}
	return nil
}

// graphBundleRows flattens a GraphBundle's nodes into the row-map shape the
// voice-agent extractors consume. Each node yields one row carrying the node's
// payload fields at the top level (so `row[field]` hits) AND under a nested
// `payload` key (so the `row["payload"][field]` fallback hits), plus the node
// id. Pure so the shape handling is unit-testable without an engine.
func graphBundleRows(bundle *memqlv1.GraphBundle) []map[string]any {
	if bundle == nil {
		return nil
	}
	nodes := bundle.GetNodes()
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		row := map[string]any{}
		if p := n.GetPayload(); p != nil {
			fields := p.AsMap()
			for k, val := range fields {
				row[k] = val
			}
			row["payload"] = fields
		}
		if nid := n.GetId(); nid != "" {
			row["id"] = nid
		}
		out = append(out, row)
	}
	return out
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

// voiceSpeakQueueDepth bounds the speak-forward queue between the event-bus
// callback (producer) and the per-session worker (consumer). Speak-eligible
// replies are rare (chat-typed AI replies under always_on); 32 is generous.
// On overflow the callback DROPS the reply with a warn instead of blocking
// the bus dispatch.
const voiceSpeakQueueDepth = 32

// voiceSpeakAudioModeTTL bounds the staleness of the per-session cached audio
// mode the speak worker gates on. Within the TTL no engine query runs at all
// for a speak-eligible reply; past it the worker re-resolves (off the bus
// thread). Consequence: an orb-corner audio toggle takes up to this long to
// affect the speak path -- acceptable for a human-driven toggle, and the
// trade that removes up-to-two engine queries per chat message (#1429).
const voiceSpeakAudioModeTTL = 3 * time.Second

// channelModeCache is a tiny single-value TTL cache for a session's resolved
// channel mode. now is injectable for tests; nil means time.Now.
type channelModeCache struct {
	mu      sync.Mutex
	mode    string
	fetched time.Time
	ttl     time.Duration
	now     func() time.Time
}

// get returns the cached mode while fresh, otherwise calls resolve and caches
// its answer. resolve runs under the cache lock -- the worker is the only
// caller, so this just makes the cache safe if that ever changes.
func (c *channelModeCache) get(resolve func() string) string {
	nowFn := c.now
	if nowFn == nil {
		nowFn = time.Now
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != "" && nowFn().Sub(c.fetched) < c.ttl {
		return c.mode
	}
	c.mode = resolve()
	c.fetched = nowFn()
	return c.mode
}

// runVoiceSpeakWorker drains queued GA replies in FIFO order (per-session
// ordering: single worker, FIFO channel), resolving the CURRENT audio mode per
// reply via resolveMode (cached/TTL'd by the caller) and forwarding always_on
// replies via forward. Exits when done closes. Extracted from the subscriber
// (#1429) so the event-bus callback never does an engine round-trip -- the
// callback only filters and enqueues; this worker owns the slow legs.
func runVoiceSpeakWorker(queue <-chan voiceAgentReply, done <-chan struct{}, resolveMode func(reply voiceAgentReply) string, forward func(reply voiceAgentReply)) {
	for {
		select {
		case <-done:
			return
		case reply := <-queue:
			// Only always_on triggers Speak; everything else is skipped (the
			// caller logs the skip inside resolveMode/forward as it sees fit).
			if resolveMode(reply) != "always_on" {
				continue
			}
			forward(reply)
		}
	}
}

// startVoiceAgentSpeakSubscriber binds a session-long subscriber for
// the (space, ga_agent_id) pair. The subscriber forwards AI reply
// utterances as VoiceAgentSpeak messages so the voice-agent
// synthesizes them via AgentSession.say(), enabling chat-typed user
// messages to produce audible replies.
//
// Hot-path shape (#1429): the event-bus callback does ONLY in-memory work
// (match, dedup, turn-in-flight check) and enqueues; a single per-session
// worker goroutine resolves the audio mode (TTL-cached, seeded with the
// session-start resolve) and sends the Speak. The bus dispatch never waits
// on an engine round-trip, and the single worker preserves per-session
// reply ordering.
//
// Dedup against STT-initiated turns: when a VoiceAgentTurnRequest is
// in flight for the same (space, agent), the TurnDelta path already
// drives the TTS pipeline -- the subscriber skips the matching AI
// reply so we don't double-speak.
//
// Mode gating: only `always_on` triggers Speak. The `mirror_user`
// case is handled by the existing TurnRequest path (which fires
// only when the user actually speaks via STT, mirroring their mic
// state by construction). `always_off` is skipped silently.
//
// Idempotent: replacing an active subscriber on a re-issued
// SessionStart cancels the previous one (subscription AND worker) first.
func (s *streamSession) startVoiceAgentSpeakSubscriber(spaceId, gaAgentId, initialAudioMode string) {
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
	s.voiceAgentScopeId = spaceId
	s.voiceAgentGaAgentId = gaAgentId
	s.voiceAgentSpeakSubMu.Unlock()

	// Per-subscriber dedup cache. Bounded simple set; we only ever
	// add the current session's forwarded utterance ids during its
	// lifetime. Voice-agent sessions are short-lived (per-room) so a
	// growing set never poses a memory problem in practice.
	seenIds := make(map[string]struct{})
	var seenMu sync.Mutex

	// Speak-forward queue + worker lifetime. The callback enqueues
	// (non-blocking, drop+warn on overflow); the worker drains FIFO.
	speakQueue := make(chan voiceAgentReply, voiceSpeakQueueDepth)
	done := make(chan struct{})

	// Audio-mode cache, seeded with the session-start resolve so the first
	// speak-eligible reply usually pays zero queries.
	modeCache := &channelModeCache{ttl: voiceSpeakAudioModeTTL}
	if initialAudioMode != "" {
		modeCache.mode = initialAudioMode
		modeCache.fetched = time.Now()
	}

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
		// Hand off to the worker. Never block the event-bus dispatch: a full
		// queue drops the reply (surfaced as a warn, not as bus latency).
		select {
		case speakQueue <- reply:
		default:
			if s.logger != nil {
				s.logger.Warn("voice-agent speak queue full: dropping reply",
					"space_id", spaceId,
					"ga_agent_id", gaAgentId,
					"utterance_id", reply.utteranceId)
			}
		}
	}, events.WithSubscriberName(subName))

	go runVoiceSpeakWorker(speakQueue, done,
		func(reply voiceAgentReply) string {
			// Resolve current audio mode, TTL-cached. Only a cache miss pays
			// the engine round-trip (and is the only thing the timing record
			// measures -- a hit costs nothing and logs nothing).
			mode := modeCache.get(func() string {
				modeStart := time.Now()
				resolved := resolveInitialChannelMode(s, spaceId, gaAgentId, "audio")
				logVoiceTiming(s.logger, "server.speak.audio_mode_lookup", modeStart,
					"space_id", spaceId, "utterance_id", reply.utteranceId)
				return resolved
			})
			if mode != "always_on" && s.logger != nil {
				s.logger.Info("voice-agent speak skip: audio mode not always_on",
					"space_id", spaceId,
					"ga_agent_id", gaAgentId,
					"utterance_id", reply.utteranceId,
					"audio_mode", mode)
			}
			return mode
		},
		func(reply voiceAgentReply) {
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
		})

	s.voiceAgentSpeakSubMu.Lock()
	s.voiceAgentSpeakStop = func() {
		unsubscribe()
		close(done)
	}
	s.voiceAgentSpeakSubMu.Unlock()
}

// stopVoiceAgentSpeakSubscriber tears down the session-long
// subscriber (subscription AND speak worker). Safe to call on a
// non-voice-agent stream (no-op).
func (s *streamSession) stopVoiceAgentSpeakSubscriber() {
	if s == nil {
		return
	}
	s.voiceAgentSpeakSubMu.Lock()
	stop := s.voiceAgentSpeakStop
	s.voiceAgentSpeakStop = nil
	s.voiceAgentScopeId = ""
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
	// the type tag, rejecting the AI response's `replyToId` insert
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
	// played" symptom: every orphan matched the same AI reply row,
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
	// Match any AI-participant reply in the target space. Backend
	// convention stamps `participantType="si"` on every system-
	// intelligence utterance (the wire-format value memql actually
	// writes; the earlier "agent" check here was aspirational and
	// never matched a row). Cognition only dispatches one winner
	// per turn, so the first AI reply that lands in the space after
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
// for one realtime turn and inserts it as an AI utterance with full
// chat/canvas/audit parity (#437).
//
// The cascade routes assistant replies through VoiceAgentTurnRequest --
// cognition runs the agent loop and inserts the AI utterance itself
// (insertAIResponse in integrations/cognition/ai_responder.go), stamping
// participantType="si" + citations off the respondToUser envelope. The
// realtime executor (gpt-realtime) speaks directly and never enters that
// path, so this handler is the sole writer of realtime AI utterances.
// It mirrors insertAIResponse's wire shape exactly:
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

		// Resolve the GA's AI participant row in the space. The frontend
		// resolves a sender via participantMap.get(utterance.participantId),
		// which is the v1:cognition:participant id -- NOT the agent
		// template id. querySiParticipantForSpace returns the active AI
		// participant; voice rooms have a single GA, so the first active
		// AI participant is the speaker.
		participantResolveStart := time.Now()
		participantId := s.resolveAIParticipantId(ctx, spaceId)
		logVoiceTiming(s.logger, "server.realtime_output.resolve_participant", participantResolveStart,
			"space_id", spaceId, "ok", participantId != "")
		if participantId == "" {
			if s.logger != nil {
				s.logger.Error("voice-agent realtime output: no AI participant resolved",
					"request_id", requestId, "space_id", spaceId, "ga_agent_id", gaAgentId)
			}
			_ = ack(false, "", "participant_not_found", "no active AI participant in space")
			return
		}

		// Source attribution. outputMethod="realtimeVoice" is the
		// concept's enum value for the gpt-realtime speech-to-speech
		// path (v1:cognition:utterance.source.outputMethod). pipeline
		// tags the wire path so cognition's voice-vs-text routing keys
		// on it; sttProvider/agentId mirror what insertAIResponse stamps.
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

// resolveAIParticipantId returns the active AI participant row id for a
// space, or "" if none. Read-only; mirrors the resolution insertAIResponse
// relies on (participantId must be the v1:cognition:participant id, not
// the agent template id) so realtime utterances attribute identically.
func (s *streamSession) resolveAIParticipantId(ctx context.Context, spaceId string) string {
	if s == nil || s.service == nil || s.service.engine == nil {
		return ""
	}
	spaceJSON, _ := json.Marshal(spaceId)
	query := fmt.Sprintf(`querySiParticipantForSpace({spaceId: %s})`, string(spaceJSON))
	result, err := s.service.engine.Execute(ctx, query)
	if err != nil {
		if s.logger != nil {
			s.logger.Debug("resolveAIParticipantId query failed",
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

// handleVoiceAgentRealtimeSpeaking writes the GA participant's speaking-state
// presence for the native realtime voice path (#1421) so the CoPresent orb
// animates while the assistant speaks.
//
// On turnModeNative the realtime model authors + speaks the reply directly:
// cognition resets the GA's presence to idle at gate-publish (it no longer
// authors the reply, integrations/cognition/cognition_handler.go), and the only
// output capture (handleVoiceAgentRealtimeOutput) fires once at response.done.
// So nothing ever writes presence state=responding during the spoken turn. The
// voice-agent observes the realtime output stream and emits this signal --
// speaking=true on the first output audio frame, speaking=false on
// response.done -- and the WRITE lands here so it goes through the SAME engine
// mutation and the SAME graph.node.created.v1:cognition:* mesh routing rule
// every other presence write uses. That makes it multi-node correct: in a
// 2-replica cluster the presence row reaches the frontend presence stream
// cross-node, not just on the writer node.
//
// state=responding mirrors the cascade's "Speaking…" write
// (integrations/cognition presenceStateResponding); state=idle mirrors the
// gate-publish idle reset. Fire-and-forget: no ack (mirrors VoiceAgentSpeak).
// Ordering: the first-audio responding lands AFTER the gate-publish idle reset
// (the model only produces audio once the directive is published and it
// engages), so it is not stomped.
func (s *streamSession) handleVoiceAgentRealtimeSpeaking(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.VoiceAgentRealtimeSpeaking) error {
	if msg == nil {
		return nil
	}
	if s.service == nil || s.service.engine == nil {
		if s.logger != nil {
			s.logger.Warn("voice-agent realtime speaking: engine not configured")
		}
		return nil
	}

	spaceId := strings.TrimSpace(msg.GetSpaceId())
	gaAgentId := strings.TrimSpace(msg.GetGaAgentId())
	if spaceId == "" || gaAgentId == "" {
		if s.logger != nil {
			s.logger.Warn("voice-agent realtime speaking: spaceId and gaAgentId required",
				"space_id", spaceId, "ga_agent_id", gaAgentId)
		}
		return nil
	}

	speaking := msg.GetSpeaking()

	// Fire-and-forget: do the presence resolution + write off the read loop so
	// a slow engine call never stalls the stream (mirrors the realtime output
	// path). No ack message is sent.
	go func() {
		ctx := contextWithVoiceAgentActor(context.Background())

		// Resolve the GA's v1:cognition:participant row id (NOT the agent
		// template id) -- the presence row the frontend keys the orb on is
		// keyed by participantId, the same id handleVoiceAgentRealtimeOutput
		// attributes the AI utterance to.
		participantId := s.resolveAIParticipantId(ctx, spaceId)
		if participantId == "" {
			if s.logger != nil {
				s.logger.Debug("voice-agent realtime speaking: no AI participant resolved",
					"space_id", spaceId, "ga_agent_id", gaAgentId)
			}
			return
		}

		state := "idle"
		label := "Idle"
		if speaking {
			state = "responding"
			label = "Speaking…"
		}

		if err := s.writeRealtimeSpeakingPresence(ctx, spaceId, participantId, state, label); err != nil {
			if s.logger != nil {
				s.logger.Warn("voice-agent realtime speaking: presence write failed",
					"space_id", spaceId, "participant_id", participantId,
					"state", state, "error", err)
			}
			return
		}
		if s.logger != nil {
			s.logger.Debug("voice-agent realtime speaking: presence written",
				"space_id", spaceId, "participant_id", participantId, "state", state)
		}
	}()

	return nil
}

// writeRealtimeSpeakingPresence upserts the GA participant's presence row with
// the given state/label, byte-identical to cognition's upsertParticipantPresence
// (integrations/cognition/participant_presence.go) so the realtime speaking
// signal lands the SAME row shape the cascade/text path writes. Deterministic
// id (one presence record per participant) and the same field set keep the
// frontend's useParticipantPresence read unchanged.
func (s *streamSession) writeRealtimeSpeakingPresence(ctx context.Context, spaceId, participantId, state, label string) error {
	id := realtimePresenceRecordId(participantId)
	if id == "" {
		return fmt.Errorf("invalid presence record id")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{
		"spaceId":       spaceId,
		"participantId": participantId,
		"state":         state,
		"label":         label,
		"reason":        "",
		"sinceAt":       now,
		"lastUpdatedAt": now,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	idJSON, _ := json.Marshal(id)
	query := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		mustJSONString(memoryNodes.ConceptCognitionParticipantPresence),
		string(idJSON), string(payloadJSON))
	_, err = s.service.engine.Execute(ctx, query)
	return err
}

// realtimePresenceRecordId derives the deterministic presence record id from a
// participant id, mirroring cognition's presenceRecordId: the engine's
// validateShortId requires a bare shortId, so we take the last `:`-delimited
// segment of v1:cognition:participant:<short> and let the concept's storageId
// prepend the type tag.
func realtimePresenceRecordId(participantId string) string {
	pid := strings.TrimSpace(participantId)
	if pid == "" {
		return ""
	}
	if idx := strings.LastIndex(pid, ":"); idx >= 0 {
		return pid[idx+1:]
	}
	return pid
}

// mustJSONString JSON-encodes a string so an interpolated concept id / literal
// cannot break out of its DSL string literal (the unsafe-quoting escaping the
// realtime output path also uses).
func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// buildRealtimeCitationsClause renders the `, citations: [...]` mutation
// clause from the proto citations, or "" when there are none worth
// emitting. Each entry is validated (both fields non-empty) so a partial
// citation never lands a malformed chip. Mirrors the citations clause in
// integrations/cognition/ai_responder.go insertAIResponse for byte-for-
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
// speaker fallback + the persona / tool-scope reads need. *memqlengine.MemQLEngine
// satisfies it; an interface so the resolution rules are unit-testable without
// standing up an engine.
//
// ResolveSkills is part of the surface because the post-skills-migration (#158)
// tool-scope path resolves the agent's capabilities.skillIds[] into concrete
// tool slugs through the engine's skill catalog (mirrors
// integrations/cognition/ai_responder.go's getAgent). The flat tools[] list is
// empty on every assistant materialized after #158, so reading it directly --
// as the retired `?.`-syntax query did -- always scoped to nothing and fell
// open to the 517-tool registry (#1454).
type voiceParticipantResolver interface {
	CanonicalizeIdValue(ctx context.Context, value, conceptType string) (string, error)
	Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error)
	ResolveSkills(ctx context.Context, skillIds []string) (memqlengine.SkillBundle, error)
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
