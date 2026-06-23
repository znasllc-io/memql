package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// voiceGroundingEnabled reports whether per-turn knowledge grounding is enabled
// for voice replies (#490). Opt-in via MEMQL_VOICE_GROUNDING=true; off by
// default so the proven voice path is unchanged until grounding is validated
// live in a credentialed room.
func voiceGroundingEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MEMQL_VOICE_GROUNDING")), "true")
}

// voiceAgentToolLoopEnabled reports whether voice turns run cognition's full
// agent tool loop instead of the realtime model authoring natively (#1198, epic
// #1197 A2). Opt-in via MEMQL_VOICE_AGENT_TOOL_LOOP=true; off by default so the
// proven #479 gate path (model authors, cognition only gates WHEN/brevity) is
// unchanged until A2 is verified live. MUST match the voice-agent-side flag
// (Config.RealtimeAgentToolLoop): with this on, cognition authors the reply via
// the tool loop and the realtime model re-voices it; the executor must be in the
// gated path (create_response:false) so the model does not also auto-author.
func voiceAgentToolLoopEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MEMQL_VOICE_AGENT_TOOL_LOOP")), "true")
}

// retrieveVoiceGroundingBlock retrieves the top knowledge chunks for the user
// turn over the agent's domains and renders a numbered, domain-attributed
// grounding block the realtime model conditions its native generation on
// (#490). Runs cognition-side (where retrieval lives) so the gate path can ride
// the block to the executor on the directive. Best-effort and fail-safe: a nil
// engine, no domains, a query error, or no chunks returns "" (no grounding for
// this turn -- the model still answers from persona + conversation).
func (c *CognitionIntegration) retrieveVoiceGroundingBlock(ctx context.Context, query string, domains []string) string {
	if c == nil || c.engine == nil {
		return ""
	}
	query = strings.TrimSpace(query)
	clean := make([]string, 0, len(domains))
	for _, d := range domains {
		if d = strings.TrimSpace(d); d != "" {
			clean = append(clean, d)
		}
	}
	if query == "" || len(clean) == 0 {
		return ""
	}

	args, err := json.Marshal(map[string]any{
		"text":    query,
		"concept": "v1:knowledge:documentChunk",
		"domains": clean,
		"limit":   5,
	})
	if err != nil {
		return ""
	}
	result, err := c.engine.Execute(ctx, fmt.Sprintf("similarTo(%s)", string(args)))
	if err != nil {
		return ""
	}
	data, err := extractDataFromResult(result)
	if err != nil || data == nil {
		return ""
	}

	rows := normalizeGroundingRows(data)
	if len(rows) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Relevant context for the current turn (from the space's knowledge). " +
		"Use it to ground your answer; cite the domain it came from when you rely on it:")
	idx := 1
	for _, r := range rows {
		text := strings.TrimSpace(groundingRowString(r, "text"))
		if text == "" {
			continue
		}
		domain := strings.TrimSpace(groundingRowString(r, "domainId"))
		if domain != "" {
			fmt.Fprintf(&b, "\n[%d] (%s) %s", idx, domain, text)
		} else {
			fmt.Fprintf(&b, "\n[%d] %s", idx, text)
		}
		idx++
	}
	if idx == 1 {
		return ""
	}
	return b.String()
}

// normalizeGroundingRows coerces a similarTo result payload (any of the shapes
// the engine emits) into a uniform []map[string]any.
func normalizeGroundingRows(payload any) []map[string]any {
	switch v := payload.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, it := range v {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}

// groundingRowString reads a string field from a chunk row, checking the row
// directly and a nested "payload" map (the two shapes chunks arrive in).
func groundingRowString(row map[string]any, key string) string {
	if s, ok := row[key].(string); ok && s != "" {
		return s
	}
	if payload, ok := row["payload"].(map[string]any); ok {
		if s, ok := payload[key].(string); ok {
			return s
		}
	}
	return ""
}

// VoiceGateDirectiveTopic is the event-bus subject the voice gate publishes its
// per-turn decision on (#477/#479). It MUST match the relay's constant in
// component/grpc/voice_agent_handlers.go (voiceGateDirectiveTopic): the voice
// turn relay subscribes to it and forwards the directive on
// VoiceAgentTurnComplete so the realtime model authors the words itself.
const VoiceGateDirectiveTopic = "voice.gate.directive"

// publishVoiceGateDirective publishes a gate decision for a voice turn so the
// relay forwards it (engage -> the model authors with the mode+brevity
// directive; defer -> suppress). Best-effort: a nil eventBus is a no-op. The
// payload keys mirror what extractVoiceGateDirective decodes (space-scoped --
// voice is GA-only, so no per-agent key is needed).
func (c *CognitionIntegration) publishVoiceGateDirective(ctx context.Context, partitionId, utteranceId string, d VoiceGateDecision, grounding string) {
	if c == nil || c.eventBus == nil {
		return
	}
	partition := ""
	if p, ok := ctx.Value(partitionCtxKey{}).(string); ok {
		partition = p
	}
	c.eventBus.Publish(events.Event{
		Topic:     VoiceGateDirectiveTopic,
		Kind:      events.KindAIEvent,
		Timestamp: time.Now().UTC(),
		Payload: map[string]any{
			"partitionId":     partitionId,
			"engage":      d.Engage,
			"mode":        d.Mode,
			"brevity":     d.Brevity,
			"utteranceId": utteranceId,
			"grounding":   grounding,
		},
		Metadata:  map[string]string{"source": "cognition.voice_gate", "reason": d.Reason},
		Partition: partition,
	})
}

// voice_gate.go is the cheap, heuristic-first conductor GATE for voice turns
// (#477 design, #479 build). It answers WHEN (and how briefly) the assistant
// should take a freshly-committed user turn -- NOT what to say. The model
// authors the words; this decides engage/defer + mode/brevity.
//
// It is pure and deterministic: every signal it reads is pre-computed by the
// caller (the Polyphon scorer, the single-agent fast path, the voice-completeness
// heuristic, and the already-running message classifier -- see #477 section 3),
// so the gate is a reordering of work the handler already does, evaluated as a
// short-circuiting ladder. No network call, no LLM, no DB round-trip lives here;
// the bounded semantic escalation is the classifier signals the caller passes
// in, consulted only for the ambiguous residue. Unit-tested without a session.
//
// The output vocabulary (mode + brevity) is the wire-string form of
// DirectiveMode / Brevity, which the realtime executor renders via
// RealtimeInstructionsForDirective (#479 PR1). One gate, two configurations:
// a single-agent room short-circuits to engage (the gate is "off" for 1-on-1),
// a multi-agent room runs the full traffic-cop ladder.

// VoiceGateSignals is the set of cheap, pre-computed signals the gate reads.
// The caller populates each from the existing computation (cited per field);
// the gate never extracts them itself.
type VoiceGateSignals struct {
	// DirectAddressScore is scoreDirectAddress for the candidate agent
	// (>= 1.0 means a structured @mention or a strong start-of-utterance name
	// match -- decisive engage). #477 section 3.1.
	DirectAddressScore float64
	// CandidateCount is the number of AI candidates eligible this turn. 1 means
	// a single-agent room (1-on-1 / one GA) -- the gate is off, the WHO is
	// settled. #477 section 3.2.
	CandidateCount int
	// ActiveThreadWithAgent is true when the user is mid-thread with the
	// candidate agent inside the thread-timeout window. #477 section 3.3.
	ActiveThreadWithAgent bool
	// HumanIsTyping is the presence guard: a human is mid-message, so the
	// assistant should not talk over them. #477 section 3.4.
	HumanIsTyping bool
	// IncompleteUtterance is looksIncompleteVoiceUtterance: a thinking-pause
	// fragment committed as a final, not a real turn. #477 section 3.5.
	IncompleteUtterance bool

	// The bounded semantic escalation -- the already-running, cached, fail-open
	// message classifier's facts about THIS utterance (#477 section 4). Consulted
	// only for the residue the heuristics above could not settle.
	Intent                  string // question | request_action | answer | affirmation | follow_up | farewell | ...
	CarriesAction           bool
	AnswersPriorAgentPrompt bool
	AddressedToRoom         bool
}

// VoiceGateDecision is the gate's output: whether to engage, and (on engage)
// the content-free mode + brevity directive the model conditions on. Reason is
// the short label of the signal that fired, for the voice trace.
type VoiceGateDecision struct {
	Engage  bool
	Mode    string // primary | brief_ack | chimein | defer (wire DirectiveMode)
	Brevity string // short | normal | detailed (wire Brevity)
	Reason  string
}

func gateEngage(mode, brevity, reason string) VoiceGateDecision {
	return VoiceGateDecision{Engage: true, Mode: mode, Brevity: brevity, Reason: reason}
}

func gateDefer(reason string) VoiceGateDecision {
	return VoiceGateDecision{Engage: false, Mode: string(DirectiveDefer), Brevity: "", Reason: reason}
}

// DecideVoiceGate evaluates the heuristic-first ladder and returns the gate
// decision. Signals are checked cheapest-first and the gate short-circuits the
// moment a decisive one fires (#477 section 3). The ambiguous residue consults
// the classifier intent as the tiebreak; a genuinely no-signal multi-agent turn
// defers (no chorus on unaddressed side-chatter), while a single-agent room
// always engages a complete turn (the gate is off for 1-on-1).
func DecideVoiceGate(sig VoiceGateSignals) VoiceGateDecision {
	// Presence guard: a human is mid-message -> defer, don't talk over them.
	if sig.HumanIsTyping {
		return gateDefer("human_typing")
	}
	// Turn-completeness shape: a thinking-pause fragment is not a real turn.
	if sig.IncompleteUtterance {
		return gateDefer("incomplete_utterance")
	}
	// Direct address (structured @mention / strong name match): decisive engage.
	if sig.DirectAddressScore >= 1.0 {
		return gateEngage(string(DirectivePrimary), string(BrevityNormal), "direct_address")
	}
	// Single-agent room: the WHO is settled and there is no traffic to cop, so a
	// complete user turn engages the only agent. This is the structural reason
	// the gate is OFF for 1-on-1 (#477 section 6.1).
	if sig.CandidateCount <= 1 {
		return gateEngage(string(DirectivePrimary), string(BrevityNormal), "single_agent")
	}
	// Conversational continuity: the user is mid-thread with this agent.
	if sig.ActiveThreadWithAgent {
		return gateEngage(string(DirectivePrimary), string(BrevityNormal), "thread_continuity")
	}

	// Ambiguous residue (multi-agent, no address, no thread): consult the
	// classifier facts -- the bounded escalation that resolves "implicitly asked
	// me" without re-adding the conductor LLM.
	switch strings.ToLower(strings.TrimSpace(sig.Intent)) {
	case "question", "request_action", "correction":
		// An explicit ask / imperative even without address -> engage briefly.
		return gateEngage(string(DirectivePrimary), string(BrevityShort), "intent_ask")
	case "answer":
		// Answers a prior agent prompt / carries an action -> brief acknowledgment.
		if sig.AnswersPriorAgentPrompt || sig.CarriesAction {
			return gateEngage(string(DirectiveBriefAck), string(BrevityShort), "answers_prior")
		}
	case "affirmation", "follow_up", "farewell", "smalltalk", "greeting":
		// Purely conversational, no action -> stay quiet (no chorus).
		return gateDefer("conversational_intent")
	}

	// A room-addressed actionable utterance warrants a brief chime-in.
	if sig.CarriesAction && sig.AddressedToRoom {
		return gateEngage(string(DirectiveChimeIn), string(BrevityShort), "room_action")
	}

	// No signal in a multi-agent room -> defer (the traffic-cop default).
	return gateDefer("no_signal")
}
