package memql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/visionarys-io/memql/component/auth"
	"github.com/visionarys-io/memql/component/events"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

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
// The Python voice-agent process (memql/voice-agent/, LiveKit Agents 1.5)
// authenticates as a service account and speaks the VoiceAgent* message
// surface. These handlers translate that surface into:
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

	// The voice-agent currently sends a placeholder ga_agent_id of
	// the form "<space_slug>-ga" (see voice-agent/voice_agent/main.py;
	// the real wiring through the LiveKit token is a Phase 11 follow-
	// up). Override rows and agent records are keyed by the GA's real
	// canonical agent id, so the placeholder can't address them. Look
	// the real id up from the space's group-GA participant; fall back
	// to the wire value when the lookup fails so we don't regress
	// existing callers that DO send a real id.
	if resolved := resolveGroupGAAgentId(s, spaceId); resolved != "" {
		gaAgentId = resolved
	}

	// Resolve the GA's initial audio + video modes from the persistent
	// override rows. Without this the voice-agent always saw
	// "mirror_user" (hardcoded), so toggling Sofia's video icon to
	// `always_on` never reached the avatar-gate at session start.
	audioMode := resolveInitialChannelMode(s, spaceId, gaAgentId, "audio")
	videoMode := resolveInitialChannelMode(s, spaceId, gaAgentId, "video")

	if s.logger != nil {
		s.logger.Info("voice-agent session start",
			"request_id", requestId,
			"space_id", spaceId,
			"ga_agent_id", gaAgentId,
			"room_name", msg.GetRoomName(),
			"avatar_vendor", msg.GetAvatarVendor(),
			"audio_mode", audioMode,
			"video_mode", videoMode)
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
	query := fmt.Sprintf(`queryGroupGAForSpace({spaceId: "%s"})`, canonicalSpace)
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
//   1. Active v1:cognition:{audio,video}override row for this
//      (space, agent) pair (the orb-corner toggle's output).
//   2. Agent record's audioControl / videoControl default.
//   3. "mirror_user" fallback.
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

	// 1) Active per-(space, agent) override.
	overrideQuery := fmt.Sprintf(`%s({spaceId: "%s"})`, queryName, spaceId)
	if result, err := s.service.engine.Execute(ctx, overrideQuery); err == nil {
		if mode, ok := extractAgentChannelMode(result.OutputPayload(), agentId); ok {
			return mode
		}
	}

	// 2) Agent record's default.
	agentQuery := fmt.Sprintf(`from(v1:agents:agent) ?.id=="%s" select id, payload.%s`, agentId, field)
	if result, err := s.service.engine.Execute(ctx, agentQuery); err == nil {
		if mode, ok := extractFirstAgentField(result.OutputPayload(), field); ok && isValidChannelMode(mode) {
			return mode
		}
	}

	return fallback
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

	pattern := "graph.node.created.*.v1:cognition:utterance"
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
		mode := resolveInitialChannelMode(s, spaceId, gaAgentId, "audio")
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
// the thread routed by the VoiceAgent's local migration state. The
// thread enum lives on VoiceAgentTurnRequest, NOT on the final
// transcript message itself -- the final's job is just "land the
// row." Routing happens at the row level via mutationSendTextUtterance
// (group thread) or mutationSendPrivateUtterance (team thread).
//
// Phase 6 still routes everything to the group thread here -- the
// VoiceAgentTurnRequest path is what differentiates Team vs Group
// based on its ThreadContext field. A future iteration can promote
// the thread enum onto VoiceAgentFinalTranscript too, but the
// dominant case in dev/staging is one-active-human-per-space which
// the group thread handles fine.
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
	if spaceId == "" || speakerId == "" || text == "" {
		return s.sendQueryError(requestId, correlate, codes.InvalidArgument, "spaceId, speakerUserId, finalText are required")
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

	go func() {
		textJSON, _ := json.Marshal(text)
		query := fmt.Sprintf(`mutationSendTextUtterance({utteranceId: "%s", spaceId: "%s", participantId: "%s", text: %s, source: {inputMethod: "stt", pipeline: "voice-agent"}})`,
			utteranceId, spaceId, speakerId, string(textJSON))

		ctx := contextWithVoiceAgentActor(context.Background())
		if _, err := s.service.engine.Execute(ctx, query); err != nil {
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
// handleVoiceAgentFinalTranscript (which fires graph.node.created.*.v1:cognition:utterance
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
	// utterance concept. The GA's reply lands as a row with
	// participantType="agent" + participantId=gaAgentId; filter for
	// that specific shape so other agents' replies (specialists,
	// chime-ins) don't trigger us.
	//
	// Both threads currently land as `v1:cognition:utterance`:
	// `handleVoiceAgentFinalTranscript` always writes via
	// mutationSendTextUtterance (group thread) regardless of the
	// VoiceAgentTurnRequest.ThreadContext enum -- the per-thread
	// routing is the unfinished half of the Phase 6 plan. Switching
	// the subscription concept on `thread` here while inserts stay
	// group-only created a silent mismatch: BFF waited on
	// `privateUtterance` rows that never get produced, hit the 30s
	// turn_timeout, and the GA appeared mute even though the reply
	// row landed in chat. Subscribe to the concept that matches
	// what we actually write; when team-thread inserts ship, flip
	// both sides in lockstep.
	pattern := "graph.node.created.*.v1:cognition:utterance"

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
		case reply := <-replyCh:
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
