package memql

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/znasllc-io/memql/component/events"
)

// TestExtractGAReplyFromEvent_SkipsRealtimeVoiceOutput pins the #478 double-speak
// guard: an AI utterance the realtime model already spoke natively
// (source.outputMethod="realtimeVoice") must NOT be matched by the speak
// subscriber, otherwise the model would re-voice its own reply.
func TestExtractGAReplyFromEvent_SkipsRealtimeVoiceOutput(t *testing.T) {
	e := events.Event{Payload: map[string]any{
		"spaceId":         "space-1",
		"participantType": "si",
		"text":            "Here is the answer.",
		"id":              "utt-1",
		"source":          map[string]any{"outputMethod": "realtimeVoice", "pipeline": "voice-agent-realtime"},
	}}
	_, ok := extractGAReplyFromEvent(e, "space-1", "ga-1")
	assert.False(t, ok, "a realtimeVoice output must be skipped (already spoken natively)")
}

// TestExtractGAReplyFromEvent_AcceptsTextReply confirms a cognition-authored
// text reply (no realtimeVoice stamp) is still spoken via the subscriber -- the
// guard is scoped to the native-spoken case only.
func TestExtractGAReplyFromEvent_AcceptsTextReply(t *testing.T) {
	e := events.Event{Payload: map[string]any{
		"spaceId":         "space-1",
		"participantType": "si",
		"text":            "Let me help with that.",
		"id":              "utt-2",
		"source":          map[string]any{"outputMethod": "text", "pipeline": "cognition"},
	}}
	reply, ok := extractGAReplyFromEvent(e, "space-1", "ga-1")
	assert.True(t, ok)
	assert.Equal(t, "Let me help with that.", reply.text)
	assert.Equal(t, "utt-2", reply.utteranceId)
}

// TestExtractGAReplyFromEvent_AcceptsReplyWithoutSource guards against a
// regression where the new source probe rejects utterances that carry no
// source map at all (the cascade still produces these).
func TestExtractGAReplyFromEvent_AcceptsReplyWithoutSource(t *testing.T) {
	e := events.Event{Payload: map[string]any{
		"spaceId":         "space-1",
		"participantType": "si",
		"text":            "No source here.",
		"id":              "utt-3",
	}}
	reply, ok := extractGAReplyFromEvent(e, "space-1", "ga-1")
	assert.True(t, ok)
	assert.Equal(t, "No source here.", reply.text)
}

// TestExtractVoiceGateDirective_EngageAndDefer pins the #479 gate-directive
// decode: an engage directive for the space decodes its mode/brevity; a defer
// decodes engage=false; a different space is rejected.
func TestExtractVoiceGateDirective_EngageAndDefer(t *testing.T) {
	engage := events.Event{Payload: map[string]any{
		"spaceId": "space-1", "engage": true, "mode": "primary", "brevity": "short", "utteranceId": "u1",
	}}
	d, ok := extractVoiceGateDirective(engage, "space-1", "ga-1")
	assert.True(t, ok)
	assert.True(t, d.engage)
	assert.Equal(t, "primary", d.mode)
	assert.Equal(t, "short", d.brevity)
	assert.Equal(t, "u1", d.utteranceId)

	deferred := events.Event{Payload: map[string]any{"spaceId": "space-1", "engage": false}}
	d, ok = extractVoiceGateDirective(deferred, "space-1", "ga-1")
	assert.True(t, ok)
	assert.False(t, d.engage)

	// A directive for a different space is not matched.
	_, ok = extractVoiceGateDirective(engage, "space-2", "ga-1")
	assert.False(t, ok)
}
