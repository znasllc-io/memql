package openai

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/integrations/audio"
)

// decodeJSON unmarshals a client event into a generic map for assertions.
func decodeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

// TestEncodeSessionUpdate_TurnDetectionNull verifies the load-bearing
// conductor-gate posture: turn_detection is present and explicitly null so the
// model never self-triggers (spike section 3.1).
func TestEncodeSessionUpdate_TurnDetectionNull(t *testing.T) {
	data, err := encodeSessionUpdate(SessionConfig{
		Instructions: "You are Sofia.",
		Voice:        "marin",
	})
	require.NoError(t, err)

	// The key must be present with a JSON null (not omitted) so the server
	// does not apply its server_vad default.
	assert.Contains(t, string(data), `"turn_detection":null`)

	m := decodeJSON(t, data)
	assert.Equal(t, clientEventSessionUpdate, m["type"])
	session := m["session"].(map[string]any)
	assert.Equal(t, "realtime", session["type"])
	assert.Equal(t, "You are Sofia.", session["instructions"])
	// turn_detection decodes to a literal nil.
	val, present := session["turn_detection"]
	assert.True(t, present, "turn_detection key must be present")
	assert.Nil(t, val, "turn_detection must be null")

	audioCfg := session["audio"].(map[string]any)
	output := audioCfg["output"].(map[string]any)
	assert.Equal(t, "marin", output["voice"])
	format := output["format"].(map[string]any)
	assert.Equal(t, "audio/pcm", format["type"])
	assert.EqualValues(t, audio.OpenAISampleRate, format["rate"])

	// No tools declared under #457 -> tools/tool_choice absent.
	_, hasTools := session["tools"]
	assert.False(t, hasTools, "no tools should be declared under #457")
}

func TestEncodeSessionUpdate_WithTools(t *testing.T) {
	data, err := encodeSessionUpdate(SessionConfig{
		Instructions: "x",
		Voice:        "alloy",
		Tools: []RealtimeTool{
			{Type: "function", Name: "search", Description: "search the graph"},
		},
	})
	require.NoError(t, err)
	session := decodeJSON(t, data)["session"].(map[string]any)
	tools := session["tools"].([]any)
	require.Len(t, tools, 1)
	assert.Equal(t, "search", tools[0].(map[string]any)["name"])
	assert.Equal(t, "auto", session["tool_choice"])
}

func TestEncodeInputAudioAppend_Base64(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	data, err := encodeInputAudioAppend(pcm)
	require.NoError(t, err)
	m := decodeJSON(t, data)
	assert.Equal(t, clientEventInputAudioAppend, m["type"])
	decoded, err := base64.StdEncoding.DecodeString(m["audio"].(string))
	require.NoError(t, err)
	assert.Equal(t, pcm, decoded)
}

func TestEncodeInputAudioCommit(t *testing.T) {
	data, err := encodeInputAudioCommit()
	require.NoError(t, err)
	assert.Equal(t, clientEventInputAudioCommit, decodeJSON(t, data)["type"])
}

func TestEncodeConversationItem_LabeledUser(t *testing.T) {
	data, err := encodeConversationItem(ConversationItem{
		Role: "user",
		Text: "[Maria · Finance] cut cloud spend",
	})
	require.NoError(t, err)
	m := decodeJSON(t, data)
	assert.Equal(t, clientEventConversationItemNew, m["type"])
	item := m["item"].(map[string]any)
	assert.Equal(t, "message", item["type"])
	assert.Equal(t, "user", item["role"])
	content := item["content"].([]any)
	first := content[0].(map[string]any)
	assert.Equal(t, "input_text", first["type"])
	assert.Equal(t, "[Maria · Finance] cut cloud spend", first["text"])
}

func TestEncodeResponseCreate_WithInstructions(t *testing.T) {
	data, err := encodeResponseCreate("Be brief.")
	require.NoError(t, err)
	m := decodeJSON(t, data)
	assert.Equal(t, clientEventResponseCreate, m["type"])
	resp := m["response"].(map[string]any)
	assert.Equal(t, "Be brief.", resp["instructions"])
	mods := resp["output_modalities"].([]any)
	assert.Equal(t, "audio", mods[0])
}

func TestEncodeResponseCreate_EmptyInstructionsOmitted(t *testing.T) {
	data, err := encodeResponseCreate("")
	require.NoError(t, err)
	resp := decodeJSON(t, data)["response"].(map[string]any)
	_, has := resp["instructions"]
	assert.False(t, has, "empty instructions must be omitted to fall back to session default")
}

func TestEncodeResponseCancel(t *testing.T) {
	data, err := encodeResponseCancel()
	require.NoError(t, err)
	assert.Equal(t, clientEventResponseCancel, decodeJSON(t, data)["type"])
}

func TestEncodeOutputAudioClear(t *testing.T) {
	data, err := encodeOutputAudioClear()
	require.NoError(t, err)
	assert.Equal(t, clientEventOutputAudioClear, decodeJSON(t, data)["type"])
}

func TestEncodeFunctionCallOutput(t *testing.T) {
	data, err := encodeFunctionCallOutput("call_42", `{"ok":true}`)
	require.NoError(t, err)
	item := decodeJSON(t, data)["item"].(map[string]any)
	assert.Equal(t, "function_call_output", item["type"])
	assert.Equal(t, "call_42", item["call_id"])
	assert.Equal(t, `{"ok":true}`, item["output"])
}

func TestParseServerEvent_AudioDelta_DecodesPCM(t *testing.T) {
	pcm := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	frame := `{"type":"response.output_audio.delta","delta":"` +
		base64.StdEncoding.EncodeToString(pcm) + `"}`
	ev := parseServerEvent([]byte(frame))
	assert.Equal(t, EventAudioDelta, ev.Kind)
	assert.Equal(t, pcm, ev.Audio)
}

func TestParseServerEvent_PreviewAudioAlias(t *testing.T) {
	pcm := []byte{0x10, 0x20}
	frame := `{"type":"response.audio.delta","delta":"` +
		base64.StdEncoding.EncodeToString(pcm) + `"}`
	ev := parseServerEvent([]byte(frame))
	assert.Equal(t, EventAudioDelta, ev.Kind, "preview spelling must fold onto the GA audio-delta kind")
	assert.Equal(t, pcm, ev.Audio)
}

func TestParseServerEvent_TranscriptDeltaAndDone(t *testing.T) {
	d := parseServerEvent([]byte(`{"type":"response.output_audio_transcript.delta","delta":"Hel"}`))
	assert.Equal(t, EventTranscriptDelta, d.Kind)
	assert.Equal(t, "Hel", d.Text)

	done := parseServerEvent([]byte(`{"type":"response.output_audio_transcript.done","transcript":"Hello there"}`))
	assert.Equal(t, EventTranscriptDone, done.Kind)
	assert.Equal(t, "Hello there", done.Text)
}

func TestParseServerEvent_ResponseLifecycle(t *testing.T) {
	created := parseServerEvent([]byte(`{"type":"response.created","response":{"id":"resp_1"}}`))
	assert.Equal(t, EventResponseCreated, created.Kind)
	assert.Equal(t, "resp_1", created.ResponseID)

	done := parseServerEvent([]byte(`{"type":"response.done","response":{"id":"resp_1"}}`))
	assert.Equal(t, EventResponseDone, done.Kind)
	assert.Equal(t, "resp_1", done.ResponseID)
	assert.Equal(t, 0, done.AudioTokens, "no usage block -> zero audio tokens")
}

// TestParseServerEvent_ResponseDoneAudioTokens verifies the per-session
// token-budget guardrail (#459) gets its input+output audio-token total off the
// response.done usage block.
func TestParseServerEvent_ResponseDoneAudioTokens(t *testing.T) {
	done := parseServerEvent([]byte(`{"type":"response.done","response":{"id":"resp_2",` +
		`"usage":{"input_token_details":{"audio_tokens":120},"output_token_details":{"audio_tokens":340}}}}`))
	assert.Equal(t, EventResponseDone, done.Kind)
	assert.Equal(t, "resp_2", done.ResponseID)
	assert.Equal(t, 460, done.AudioTokens, "input+output audio tokens summed")
}

func TestParseServerEvent_FunctionCall(t *testing.T) {
	ev := parseServerEvent([]byte(
		`{"type":"response.function_call_arguments.done","call_id":"c1","name":"search","arguments":"{\"q\":\"x\"}"}`))
	assert.Equal(t, EventFunctionArgsDone, ev.Kind)
	assert.Equal(t, "c1", ev.CallID)
	assert.Equal(t, "search", ev.FuncName)
	assert.Equal(t, `{"q":"x"}`, ev.Arguments)
}

func TestParseServerEvent_Error(t *testing.T) {
	ev := parseServerEvent([]byte(`{"type":"error","error":{"message":"rate limited"}}`))
	assert.Equal(t, EventError, ev.Kind)
	assert.Equal(t, "rate limited", ev.ErrorMessage)
}

func TestParseServerEvent_Lifecycle(t *testing.T) {
	assert.Equal(t, EventSessionLifecycle, parseServerEvent([]byte(`{"type":"session.created"}`)).Kind)
	assert.Equal(t, EventSessionLifecycle, parseServerEvent([]byte(`{"type":"session.updated"}`)).Kind)
	assert.Equal(t, EventInputSpeechStarted, parseServerEvent([]byte(`{"type":"input_audio_buffer.speech_started"}`)).Kind)
}

func TestParseServerEvent_UnknownAndMalformed(t *testing.T) {
	assert.Equal(t, EventUnknown, parseServerEvent([]byte(`{"type":"something.new"}`)).Kind)
	// Malformed JSON must not panic; maps to unknown.
	assert.Equal(t, EventUnknown, parseServerEvent([]byte(`not json`)).Kind)
}
