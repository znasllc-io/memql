package audiows

import "github.com/visionarys-io/memql/integrations/stt"

// AudioMessage represents an incoming message from the client.
type AudioMessage struct {
	// Type is the message type: "start", "chunk", or "end"
	Type string `json:"type"`

	// StreamId is the client-generated unique identifier for this audio stream
	StreamId string `json:"streamId"`

	// SpaceId is the space ID (required for "start")
	SpaceId string `json:"spaceId,omitempty"`

	// ParticipantId is the participant ID (required for "start")
	ParticipantId string `json:"participantId,omitempty"`

	// Format is the audio format: "pcm16", "opus", "webm" (for "start")
	Format string `json:"format,omitempty"`

	// SampleRate is the audio sample rate in Hz (for "start")
	SampleRate int `json:"sampleRate,omitempty"`

	// Channels is the number of audio channels (for "start")
	Channels int `json:"channels,omitempty"`

	// LanguageHint is an optional language code hint (for "start")
	LanguageHint string `json:"languageHint,omitempty"`

	// Audio is the base64-encoded audio data (for "chunk")
	Audio string `json:"audio,omitempty"`

	// Sequence is the chunk sequence number for ordering (for "chunk")
	Sequence int `json:"sequence,omitempty"`

	// Cancelled indicates the user cancelled the recording (for "end")
	Cancelled bool `json:"cancelled,omitempty"`
}

// AudioResponse represents an outgoing message to the client.
type AudioResponse struct {
	// Type is the response type: "started", "transcription", "ended", or "error"
	Type string `json:"type"`

	// StreamId is the stream this response relates to
	StreamId string `json:"streamId,omitempty"`

	// Text is the transcribed text (for "transcription")
	Text string `json:"text,omitempty"`

	// IsFinal indicates if this is the final transcription (for "transcription")
	IsFinal bool `json:"isFinal,omitempty"`

	// Confidence is the transcription confidence score 0.0-1.0 (for "transcription")
	Confidence float32 `json:"confidence,omitempty"`

	// Words contains word-level timestamps (for final "transcription")
	Words []stt.WordTimestamp `json:"words,omitempty"`

	// UtteranceId is the ID of the created utterance (for final "transcription")
	UtteranceId string `json:"utteranceId,omitempty"`

	// DurationMS is the audio duration in milliseconds (for final "transcription")
	DurationMS int64 `json:"durationMs,omitempty"`

	// Cancelled indicates the stream was cancelled by user (for "ended")
	Cancelled bool `json:"cancelled,omitempty"`

	// Error contains error details (for "error")
	Error *AudioError `json:"error,omitempty"`
}

// AudioError contains error information.
type AudioError struct {
	// Code is a machine-readable error code
	Code string `json:"code"`

	// Message is a human-readable error message
	Message string `json:"message"`
}

// SynthesizeMessage represents a TTS request from the client.
type SynthesizeMessage struct {
	// Type is always "synthesize"
	Type string `json:"type"`

	// RequestId is a client-generated unique identifier for this TTS request
	RequestId string `json:"requestId"`

	// Text is the text to synthesize
	Text string `json:"text"`

	// Voice is the optional voice ID (defaults to agent's configured voice)
	Voice string `json:"voice,omitempty"`

	// Format is the audio format: "pcm16" (default), "mp3", "opus"
	Format string `json:"format,omitempty"`

	// SampleRate is the audio sample rate in Hz (default: 24000)
	SampleRate int `json:"sampleRate,omitempty"`

	// SpaceId is the space ID (for context/agent lookup)
	SpaceId string `json:"spaceId,omitempty"`

	// ParticipantId is the AI participant ID to use for TTS
	ParticipantId string `json:"participantId,omitempty"`
}

// TTSStartedMessage is sent when TTS synthesis begins.
type TTSStartedMessage struct {
	// Type is always "tts_started"
	Type string `json:"type"`

	// RequestId matches the synthesize request (or "auto-<uuid>" for server-initiated)
	RequestId string `json:"requestId"`

	// Format is the audio format being used (e.g., "opus", "mp3", "pcm")
	Format string `json:"format"`

	// SampleRate is the audio sample rate in Hz
	SampleRate int `json:"sampleRate"`

	// SpaceId is the space this TTS belongs to
	SpaceId string `json:"spaceId,omitempty"`

	// ParticipantId is the AI participant generating the audio
	ParticipantId string `json:"participantId,omitempty"`

	// Text is the text being synthesized
	Text string `json:"text"`
}

// TTSChunkMessage represents a TTS audio chunk sent to the client.
type TTSChunkMessage struct {
	// Type is always "tts_chunk"
	Type string `json:"type"`

	// RequestId matches the synthesize request (or "auto-<uuid>" for server-initiated)
	RequestId string `json:"requestId"`

	// Audio is the base64-encoded audio chunk
	Audio string `json:"audio"`

	// Format is the audio format (e.g., "opus", "mp3", "pcm")
	Format string `json:"format"`

	// SampleRate is the audio sample rate in Hz
	SampleRate int `json:"sampleRate"`

	// Sequence is the chunk sequence number (starts at 0)
	Sequence int `json:"sequence"`

	// Done indicates whether this is the final chunk
	Done bool `json:"done"`

	// SpaceId is the space this TTS belongs to
	SpaceId string `json:"spaceId"`

	// ParticipantId is the AI participant generating the audio
	ParticipantId string `json:"participantId"`

	// Text is the text being synthesized (for display/debugging)
	Text string `json:"text"`
}

// TTSEndedMessage is sent when TTS synthesis completes or is cancelled.
type TTSEndedMessage struct {
	// Type is always "tts_ended"
	Type string `json:"type"`

	// RequestId matches the synthesize request
	RequestId string `json:"requestId"`

	// SpaceId is the space this TTS belongs to
	SpaceId string `json:"spaceId,omitempty"`

	// ParticipantId is the AI participant that generated the audio
	ParticipantId string `json:"participantId,omitempty"`

	// Cancelled indicates if the TTS was cancelled before completion
	Cancelled bool `json:"cancelled,omitempty"`

	// Error contains error details if TTS failed
	Error string `json:"error,omitempty"`
}
