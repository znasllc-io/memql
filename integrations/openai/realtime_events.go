package openai

import (
	"encoding/base64"
	"encoding/json"

	"github.com/znasllc-io/memql/integrations/audio"
)

// realtime_events.go is the pure-Go event vocabulary for the OpenAI
// gpt-realtime speech-to-speech WebSocket protocol (docs/internal/design/voice-453-gpt-realtime-go.md
// section 3). It is deliberately CGO-free and free of any websocket / media
// dependency so the encode/decode logic -- the load-bearing wire contract for
// the conductor gate and multi-party routing -- is exhaustively unit-testable
// in the default CI lane.
//
// Two halves:
//   - client events (what we send): session.update, input_audio_buffer.append/
//     commit, conversation.item.create, response.create / response.cancel,
//     output_audio_buffer.clear, function_call_output. Built by the encode*
//     helpers below.
//   - server events (what we receive): discriminated on the "type" string and
//     decoded by parseServerEvent into a flat RealtimeServerEvent the
//     receiveLoop switches on.
//
// All audio on the wire is base64 PCM16 at OpenAISampleRate (24 kHz), matching
// the asr.go transcription client.

// Client event type strings (client -> server).
const (
	clientEventSessionUpdate       = "session.update"
	clientEventInputAudioAppend    = "input_audio_buffer.append"
	clientEventInputAudioCommit    = "input_audio_buffer.commit"
	clientEventConversationItemNew = "conversation.item.create"
	clientEventResponseCreate      = "response.create"
	clientEventResponseCancel      = "response.cancel"
	clientEventOutputAudioClear    = "output_audio_buffer.clear"
)

// Server event type strings (server -> client). GA event names per the spike
// doc section 3.5. The preview-spelling aliases are folded in parseServerEvent.
const (
	serverEventSessionCreated        = "session.created"
	serverEventSessionUpdated        = "session.updated"
	serverEventResponseCreated       = "response.created"
	serverEventResponseDone          = "response.done"
	serverEventOutputAudioDelta      = "response.output_audio.delta"
	serverEventOutputAudioDone       = "response.output_audio.done"
	serverEventOutputTranscriptDelta = "response.output_audio_transcript.delta"
	serverEventOutputTranscriptDone  = "response.output_audio_transcript.done"
	serverEventFunctionArgsDelta     = "response.function_call_arguments.delta"
	serverEventFunctionArgsDone      = "response.function_call_arguments.done"
	serverEventInputSpeechStarted    = "input_audio_buffer.speech_started"
	serverEventInputSpeechStopped    = "input_audio_buffer.speech_stopped"
	serverEventError                 = "error"

	// Native input-transcription events (#478). When the session configures
	// input transcription, the model emits the user's transcript on these --
	// the channel that feeds the human's chat transcript without a separate
	// separate ASR pass on the critical path. Same wire names asr.go consumes.
	serverEventInputTranscriptDelta = "conversation.item.input_audio_transcription.delta"
	serverEventInputTranscriptDone  = "conversation.item.input_audio_transcription.completed"

	// Preview-spelling aliases (2024 preview model). Mapped onto the GA names
	// in parseServerEvent so a pinned preview model still decodes.
	previewEventAudioDelta      = "response.audio.delta"
	previewEventAudioTranscript = "response.audio_transcript.delta"
)

// RealtimeAudioFormat is the GA nested audio.{input,output}.format shape:
// {"type":"audio/pcm","rate":24000}.
type RealtimeAudioFormat struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

// RealtimeTool is one function tool declaration in session.update.tools. The
// MCP tool bridge (#458) populates the allowlist; #457 ships the empty set.
type RealtimeTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// TurnDetectionConfig configures the model's server-side turn detection. When a
// SessionConfig carries a non-nil TurnDetection, encodeSessionUpdate emits it in
// place of the explicit null -- letting the model detect end-of-turn and (when
// CreateResponse is set) author its own reply natively (#478, the 1-on-1 native
// mode). A nil TurnDetection preserves the conductor-gated posture
// (turn_detection:null), which the multi-party path (#433) depends on.
type TurnDetectionConfig struct {
	// Type is the detector, e.g. "semantic_vad" (meaning-based end-of-turn) or
	// "server_vad" (silence-timer).
	Type string `json:"type"`
	// CreateResponse lets the model auto-create a response when it detects
	// end-of-turn (native authorship). False keeps detection without auto-reply.
	CreateResponse bool `json:"create_response"`
	// InterruptResponse lets a new user onset interrupt the in-flight response
	// natively (model-driven barge-in), so the executor need not drive
	// response.cancel from a separate VAD on the native path.
	InterruptResponse bool `json:"interrupt_response"`
	// Eagerness tunes semantic_vad's turn-end sensitivity ("low" | "medium" |
	// "high" | "auto"). Omitted when empty.
	Eagerness string `json:"eagerness,omitempty"`
	// server_vad knobs (omitted when zero, so they don't leak onto a semantic_vad
	// config): Threshold is the speech-energy gate (0..1); PrefixPaddingMs is how
	// much audio before onset to keep; SilenceDurationMs is how long the input
	// must stay below threshold before the turn is committed -- the snappiness
	// dial (lower = faster end-of-turn). server_vad is deterministic where
	// semantic_vad's "is the turn semantically done?" can stall for seconds.
	Threshold         float64 `json:"threshold,omitempty"`
	PrefixPaddingMs   int     `json:"prefix_padding_ms,omitempty"`
	SilenceDurationMs int     `json:"silence_duration_ms,omitempty"`
}

// InputTranscriptionConfig enables the model's native transcription of the
// user's input audio (the conversation.item.input_audio_transcription.* events),
// so the human's chat transcript comes from the realtime model rather than a
// separate ASR pass on the critical path (#478). Nil leaves input
// transcription off -- the current default.
type InputTranscriptionConfig struct {
	// Model is the transcription model id (e.g. "gpt-4o-mini-transcribe").
	Model string `json:"model"`
	// Language is an optional ISO-639-1 hint; omitted when empty.
	Language string `json:"language,omitempty"`
}

// SessionConfig is the session.update payload (spike section 3.1). By default
// turn_detection is null (the conductor gate -- the model never self-triggers)
// and input transcription is off; audio in/out are PCM at OpenAISampleRate, and
// the static persona instructions + resolved realtime voice are carried
// verbatim. Setting TurnDetection / InputTranscription switches the session to
// the #478 native 1-on-1 posture (the model owns the turn).
type SessionConfig struct {
	// Instructions is the static persona instruction block
	// (agent.BuildSessionPersona().Instructions).
	Instructions string
	// Voice is the resolved gpt-realtime voice id
	// (agent.BuildSessionPersona().Voice).
	Voice string
	// Tools is the function-tool allowlist (empty under #457; #458 fills it).
	Tools []RealtimeTool
	// TurnDetection, when non-nil, enables model-driven turn detection (the
	// #478 native path). Nil keeps turn_detection:null (the conductor gate).
	TurnDetection *TurnDetectionConfig
	// InputTranscription, when non-nil, enables native input-audio
	// transcription (#478). Nil leaves it off.
	InputTranscription *InputTranscriptionConfig
}

// encodeSessionUpdate renders the session.update client event. turn_detection is
// emitted as the explicit null of the conductor gate when cfg.TurnDetection is
// nil (the null is load-bearing: it disables the model's input VAD, auto-commit,
// and auto-response so memQL's conductor is the sole driver of response.create,
// spike section 3.1), or as the configured detector object when set (the #478
// native path). Native input-audio transcription is added under audio.input only
// when cfg.InputTranscription is set.
func encodeSessionUpdate(cfg SessionConfig) ([]byte, error) {
	audioFmt := RealtimeAudioFormat{Type: "audio/pcm", Rate: audio.OpenAISampleRate}
	inputAudio := map[string]any{"format": audioFmt}
	if cfg.InputTranscription != nil {
		inputAudio["transcription"] = cfg.InputTranscription
	}
	// GA places turn_detection UNDER audio.input (the beta API had it at the
	// session top level). Encoded explicitly -- key present with a null value
	// rather than omitted -- so the server does not fall back to its server_vad
	// default on the conductor-gated path (turn_detection:null is load-bearing:
	// it disables the model's input VAD, auto-commit, and auto-response). A
	// non-nil detector switches the session to the #478 native posture.
	if cfg.TurnDetection != nil {
		inputAudio["turn_detection"] = cfg.TurnDetection
	} else {
		inputAudio["turn_detection"] = nil
	}
	session := map[string]any{
		"type":         "realtime",
		"instructions": cfg.Instructions,
		"audio": map[string]any{
			"input":  inputAudio,
			"output": map[string]any{"format": audioFmt, "voice": cfg.Voice},
		},
	}
	if len(cfg.Tools) > 0 {
		session["tools"] = cfg.Tools
		session["tool_choice"] = "auto"
	}
	return json.Marshal(map[string]any{
		"type":    clientEventSessionUpdate,
		"session": session,
	})
}

// encodeInputAudioAppend renders input_audio_buffer.append for one chunk of
// base64-encoded PCM16 (24 kHz). Under turn_detection:null the buffer is never
// auto-committed -- encodeInputAudioCommit does that explicitly.
func encodeInputAudioAppend(pcm24k []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":  clientEventInputAudioAppend,
		"audio": base64.StdEncoding.EncodeToString(pcm24k),
	})
}

// encodeInputAudioCommit renders input_audio_buffer.commit. Appends the
// buffered audio to the conversation as a user item; does NOT create a
// response (response creation stays conductor-gated). Driven from a human
// final transcript (spike section 3.2).
func encodeInputAudioCommit() ([]byte, error) {
	return json.Marshal(map[string]any{"type": clientEventInputAudioCommit})
}

// ConversationItem is a labeled message item injected into the session
// (multi-party labeled transcript per #433, or a grounding system message per
// #456). Role is "user" for an attributed human turn or "system" for grounding.
type ConversationItem struct {
	Role string
	Text string
}

// encodeConversationItem renders conversation.item.create for a message item.
// The content uses input_text (the read-side text channel, #433 section 2a) so
// the model can attribute it even when it never heard the audio.
func encodeConversationItem(item ConversationItem) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type": clientEventConversationItemNew,
		"item": map[string]any{
			"type": "message",
			"role": item.Role,
			"content": []map[string]any{
				{"type": "input_text", "text": item.Text},
			},
		},
	})
}

// encodeFunctionCallOutput renders the conversation.item.create that returns a
// tool result by call_id (spike section 5 step 4). The MCP tool bridge (#458)
// is the caller; #457 only defines the encoder.
func encodeFunctionCallOutput(callID, output string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type": clientEventConversationItemNew,
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  output,
		},
	})
}

// encodeResponseCreate renders response.create with a per-response instructions
// override (the conductor's rendered directive, spike section 3.3). An empty
// instructions string falls back to the session-level persona instructions.
// output_modalities is ["audio"] -- audio plus its transcript.
func encodeResponseCreate(instructions string) ([]byte, error) {
	response := map[string]any{
		"output_modalities": []string{"audio"},
	}
	if instructions != "" {
		response["instructions"] = instructions
	}
	return json.Marshal(map[string]any{
		"type":     clientEventResponseCreate,
		"response": response,
	})
}

// encodeResponseCancel renders response.cancel (barge-in, spike section 3.4).
// The caller pairs it with encodeOutputAudioClear so playback stops at once.
func encodeResponseCancel() ([]byte, error) {
	return json.Marshal(map[string]any{"type": clientEventResponseCancel})
}

// encodeOutputAudioClear renders output_audio_buffer.clear so the already-
// buffered (unplayed) assistant audio tail is dropped on barge-in.
func encodeOutputAudioClear() ([]byte, error) {
	return json.Marshal(map[string]any{"type": clientEventOutputAudioClear})
}

// ServerEventKind is the decoded category of a server event.
type ServerEventKind int

const (
	// EventUnknown is any event the executor does not act on (logged only).
	EventUnknown ServerEventKind = iota
	// EventSessionLifecycle is session.created / session.updated.
	EventSessionLifecycle
	// EventResponseCreated carries a started response id.
	EventResponseCreated
	// EventResponseDone marks a response finished.
	EventResponseDone
	// EventAudioDelta carries a decoded PCM16 audio frame (24 kHz).
	EventAudioDelta
	// EventAudioDone marks one item's audio finished.
	EventAudioDone
	// EventTranscriptDelta carries a streamed transcript token.
	EventTranscriptDelta
	// EventTranscriptDone carries the final transcript for an item.
	EventTranscriptDone
	// EventFunctionArgsDelta carries streamed tool-call argument tokens.
	EventFunctionArgsDelta
	// EventFunctionArgsDone carries a complete tool call.
	EventFunctionArgsDone
	// EventInputSpeechStarted is the input-VAD interruption hint (only
	// emitted when option-B input VAD is configured; under option A the
	// barge-in trigger comes from the turn machine, not this event).
	EventInputSpeechStarted
	// EventInputSpeechStopped fires when server-side VAD (semantic_vad /
	// server_vad) detects the human stopped speaking -- the turn-boundary marker
	// used for latency observability (end-of-speech -> first assistant audio).
	EventInputSpeechStopped
	// EventInputTranscriptDelta carries a streamed token of the USER's input
	// transcript (native input transcription, #478). Text holds the token.
	EventInputTranscriptDelta
	// EventInputTranscriptDone carries the final USER input transcript for an
	// item (#478). Text holds the full transcript.
	EventInputTranscriptDone
	// EventError carries an API error message.
	EventError
)

// RealtimeServerEvent is the flat, decoded shape of one server event. Only the
// fields relevant to the event's Kind are populated.
type RealtimeServerEvent struct {
	Kind ServerEventKind
	// Type is the raw "type" string (for logging unknowns).
	Type string
	// ResponseID is set on EventResponseCreated / EventResponseDone.
	ResponseID string
	// Audio is the decoded PCM16 (24 kHz) frame on EventAudioDelta.
	Audio []byte
	// Text is the transcript token (delta) or final transcript (done).
	Text string
	// CallID / FuncName / Arguments are set on the function-call events.
	CallID    string
	FuncName  string
	Arguments string
	// ErrorMessage is set on EventError.
	ErrorMessage string
	// AudioTokens is the input+output audio-token count reported on
	// EventResponseDone (response.usage), fed to the per-session token-budget
	// guardrail (#459). Zero when the server omits usage. Non-audio token
	// details (text/cached) are intentionally not summed here: the realtime
	// cost guardrail bounds the audio path specifically.
	AudioTokens int
}

// parseServerEvent decodes one raw server frame into a RealtimeServerEvent.
// It never errors on a malformed frame -- an undecodable or unknown event maps
// to EventUnknown so the receiveLoop can log and continue (matching the asr.go
// posture). Preview-spelling aliases are folded onto the GA kinds.
func parseServerEvent(data []byte) RealtimeServerEvent {
	var raw struct {
		Type       string `json:"type"`
		Delta      string `json:"delta"`
		Audio      string `json:"audio"`
		Transcript string `json:"transcript"`
		CallID     string `json:"call_id"`
		Name       string `json:"name"`
		Arguments  string `json:"arguments"`
		Response   struct {
			ID    string `json:"id"`
			Usage struct {
				InputTokenDetails struct {
					AudioTokens int `json:"audio_tokens"`
				} `json:"input_token_details"`
				OutputTokenDetails struct {
					AudioTokens int `json:"audio_tokens"`
				} `json:"output_token_details"`
			} `json:"usage"`
		} `json:"response"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return RealtimeServerEvent{Kind: EventUnknown}
	}

	ev := RealtimeServerEvent{Type: raw.Type}
	switch raw.Type {
	case serverEventSessionCreated, serverEventSessionUpdated:
		ev.Kind = EventSessionLifecycle
	case serverEventResponseCreated:
		ev.Kind = EventResponseCreated
		ev.ResponseID = raw.Response.ID
	case serverEventResponseDone:
		ev.Kind = EventResponseDone
		ev.ResponseID = raw.Response.ID
		ev.AudioTokens = raw.Response.Usage.InputTokenDetails.AudioTokens +
			raw.Response.Usage.OutputTokenDetails.AudioTokens
	case serverEventOutputAudioDelta, previewEventAudioDelta:
		ev.Kind = EventAudioDelta
		// delta is base64 PCM16; decode here so the executor handles raw PCM.
		if decoded, err := base64.StdEncoding.DecodeString(raw.Delta); err == nil {
			ev.Audio = decoded
		}
	case serverEventOutputAudioDone:
		ev.Kind = EventAudioDone
	case serverEventOutputTranscriptDelta, previewEventAudioTranscript:
		ev.Kind = EventTranscriptDelta
		ev.Text = raw.Delta
	case serverEventOutputTranscriptDone:
		ev.Kind = EventTranscriptDone
		ev.Text = raw.Transcript
	case serverEventFunctionArgsDelta:
		ev.Kind = EventFunctionArgsDelta
		ev.CallID = raw.CallID
		ev.Arguments = raw.Delta
	case serverEventFunctionArgsDone:
		ev.Kind = EventFunctionArgsDone
		ev.CallID = raw.CallID
		ev.FuncName = raw.Name
		ev.Arguments = raw.Arguments
	case serverEventInputSpeechStarted:
		ev.Kind = EventInputSpeechStarted
	case serverEventInputSpeechStopped:
		ev.Kind = EventInputSpeechStopped
	case serverEventInputTranscriptDelta:
		ev.Kind = EventInputTranscriptDelta
		ev.Text = raw.Delta
	case serverEventInputTranscriptDone:
		ev.Kind = EventInputTranscriptDone
		ev.Text = raw.Transcript
	case serverEventError:
		ev.Kind = EventError
		ev.ErrorMessage = raw.Error.Message
	default:
		ev.Kind = EventUnknown
	}
	return ev
}
