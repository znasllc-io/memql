package cognition

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestVoiceAgentToolLoopEnabled verifies the A2 flag (#1198) is OFF by default
// (today's #479 gate path) and opt-in via MEMQL_VOICE_AGENT_TOOL_LOOP=true.
func TestVoiceAgentToolLoopEnabled(t *testing.T) {
	t.Setenv("MEMQL_VOICE_AGENT_TOOL_LOOP", "")
	assert.False(t, voiceAgentToolLoopEnabled(), "off by default")

	t.Setenv("MEMQL_VOICE_AGENT_TOOL_LOOP", "true")
	assert.True(t, voiceAgentToolLoopEnabled(), "opt-in via env")

	t.Setenv("MEMQL_VOICE_AGENT_TOOL_LOOP", "TRUE")
	assert.True(t, voiceAgentToolLoopEnabled(), "case-insensitive")

	t.Setenv("MEMQL_VOICE_AGENT_TOOL_LOOP", "false")
	assert.False(t, voiceAgentToolLoopEnabled(), "explicit off")
}

func TestDecideVoiceGate_PresenceAndCompletenessDefer(t *testing.T) {
	// A human mid-message -> defer even with an otherwise-engaging signal.
	d := DecideVoiceGate(VoiceGateSignals{HumanIsTyping: true, DirectAddressScore: 1.0})
	assert.False(t, d.Engage)
	assert.Equal(t, "defer", d.Mode)
	assert.Equal(t, "human_typing", d.Reason)

	// A thinking-pause fragment -> defer (checked before direct address too).
	d = DecideVoiceGate(VoiceGateSignals{IncompleteUtterance: true, DirectAddressScore: 1.0})
	assert.False(t, d.Engage)
	assert.Equal(t, "incomplete_utterance", d.Reason)
}

func TestDecideVoiceGate_DirectAddressEngages(t *testing.T) {
	d := DecideVoiceGate(VoiceGateSignals{DirectAddressScore: 1.0, CandidateCount: 3})
	assert.True(t, d.Engage)
	assert.Equal(t, "primary", d.Mode)
	assert.Equal(t, "normal", d.Brevity)
	assert.Equal(t, "direct_address", d.Reason)
}

func TestDecideVoiceGate_SingleAgentEngages(t *testing.T) {
	// 1-on-1: the gate is off -- a complete turn engages the only agent, even
	// with no address and an unknown intent.
	d := DecideVoiceGate(VoiceGateSignals{CandidateCount: 1, Intent: "unknown"})
	assert.True(t, d.Engage)
	assert.Equal(t, "primary", d.Mode)
	assert.Equal(t, "single_agent", d.Reason)

	// Zero candidates (only the GA) also engages.
	assert.True(t, DecideVoiceGate(VoiceGateSignals{CandidateCount: 0}).Engage)
}

func TestDecideVoiceGate_ThreadContinuityEngages(t *testing.T) {
	d := DecideVoiceGate(VoiceGateSignals{CandidateCount: 3, ActiveThreadWithAgent: true})
	assert.True(t, d.Engage)
	assert.Equal(t, "thread_continuity", d.Reason)
}

func TestDecideVoiceGate_ImplicitAskEngagesBriefly(t *testing.T) {
	// Multi-agent, no address, no thread, but a question intent -> "implicitly
	// asked me" -> engage short.
	d := DecideVoiceGate(VoiceGateSignals{CandidateCount: 3, Intent: "question"})
	assert.True(t, d.Engage)
	assert.Equal(t, "primary", d.Mode)
	assert.Equal(t, "short", d.Brevity)
	assert.Equal(t, "intent_ask", d.Reason)

	assert.True(t, DecideVoiceGate(VoiceGateSignals{CandidateCount: 3, Intent: "request_action"}).Engage)
}

func TestDecideVoiceGate_AnswerToPriorPromptBriefAck(t *testing.T) {
	d := DecideVoiceGate(VoiceGateSignals{CandidateCount: 3, Intent: "answer", AnswersPriorAgentPrompt: true})
	assert.True(t, d.Engage)
	assert.Equal(t, "brief_ack", d.Mode)
	assert.Equal(t, "short", d.Brevity)
}

func TestDecideVoiceGate_ConversationalDefers(t *testing.T) {
	for _, intent := range []string{"affirmation", "follow_up", "farewell", "smalltalk", "greeting"} {
		d := DecideVoiceGate(VoiceGateSignals{CandidateCount: 3, Intent: intent})
		assert.False(t, d.Engage, "intent %q must defer", intent)
		assert.Equal(t, "conversational_intent", d.Reason)
	}
}

func TestDecideVoiceGate_RoomActionChimesIn(t *testing.T) {
	d := DecideVoiceGate(VoiceGateSignals{CandidateCount: 3, CarriesAction: true, AddressedToRoom: true, Intent: "unknown"})
	assert.True(t, d.Engage)
	assert.Equal(t, "chimein", d.Mode)
	assert.Equal(t, "short", d.Brevity)
}

func TestDecideVoiceGate_NoSignalDefers(t *testing.T) {
	// Multi-agent, no address, no thread, unknown intent, no action -> defer
	// (the traffic-cop default; no chorus on unaddressed side chatter).
	d := DecideVoiceGate(VoiceGateSignals{CandidateCount: 3, Intent: "unknown"})
	assert.False(t, d.Engage)
	assert.Equal(t, "no_signal", d.Reason)
}

// TestDecideVoiceGate_OutputsAreValidDirectiveVocabulary asserts every engage
// decision uses the wire vocabulary the executor's RealtimeInstructionsForDirective
// understands, so the gate and the renderer cannot drift.
func TestDecideVoiceGate_OutputsAreValidDirectiveVocabulary(t *testing.T) {
	validModes := map[string]bool{"primary": true, "brief_ack": true, "chimein": true, "defer": true}
	validBrevity := map[string]bool{"short": true, "normal": true, "detailed": true, "": true}
	cases := []VoiceGateSignals{
		{DirectAddressScore: 1.0, CandidateCount: 3},
		{CandidateCount: 1},
		{CandidateCount: 3, ActiveThreadWithAgent: true},
		{CandidateCount: 3, Intent: "question"},
		{CandidateCount: 3, Intent: "answer", CarriesAction: true},
		{CandidateCount: 3, Intent: "affirmation"},
		{CandidateCount: 3, CarriesAction: true, AddressedToRoom: true},
		{CandidateCount: 3},
		{HumanIsTyping: true},
	}
	for _, c := range cases {
		d := DecideVoiceGate(c)
		assert.True(t, validModes[d.Mode], "mode %q not in directive vocabulary", d.Mode)
		assert.True(t, validBrevity[d.Brevity], "brevity %q not in directive vocabulary", d.Brevity)
	}
}
