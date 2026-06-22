package polyphon

import (
	"context"
	"io"
)

// ASRProvider abstracts speech-to-text transcription. The implementation
// is OpenAI Realtime (transcription-only mode).
type ASRProvider interface {
	// StartStream begins a new streaming transcription session.
	// Audio chunks are sent via SendAudio, and transcription results
	// arrive on the Results channel.
	StartStream(ctx context.Context, config ASRConfig) (ASRStream, error)
}

// ASRStream represents an active streaming transcription session.
type ASRStream interface {
	// SendAudio sends a chunk of audio data for transcription.
	// Audio must be PCM16, 16kHz, mono.
	SendAudio(audio []byte) error

	// Results returns a channel of transcription results.
	// Intermediate (non-final) results may arrive before the final transcript.
	Results() <-chan ASRResult

	// Close terminates the stream and releases resources.
	Close() error
}

// ASRConfig configures a streaming ASR session.
type ASRConfig struct {
	// SampleRate in Hz (default: 16000).
	SampleRate int `json:"sampleRate"`

	// Language is a BCP-47 language code (e.g., "en-US", "es-MX").
	Language string `json:"language"`

	// EnableDiarization enables speaker identification in the transcript.
	EnableDiarization bool `json:"enableDiarization"`

	// MaxSpeakers is the maximum number of speakers for diarization.
	MaxSpeakers int `json:"maxSpeakers"`
}

// DefaultASRConfig returns sensible defaults for ASR configuration.
func DefaultASRConfig() ASRConfig {
	return ASRConfig{
		SampleRate:        16000,
		Language:          "en-US",
		EnableDiarization: false,
		MaxSpeakers:       4,
	}
}

// ASRResultKind discriminates the turn-structure role of an ASRResult.
// It is an additive, backward-compatible enrichment of the stream
// contract (see docs/internal/design/voice-452-turntaking-orchestration-go.md, step 1):
// the zero value (ASRKindTranscript) preserves the historical
// interim/final-via-IsFinal behavior, so existing consumers that only
// read Text/IsFinal are unaffected. The Go turn-taking machine (#455)
// additionally reads Kind to drive barge-in onset off ASRKindSpeechStarted.
type ASRResultKind int

const (
	// ASRKindTranscript is the default kind: a transcript update whose
	// finality is carried by IsFinal (interim when false, committed
	// end-of-utterance when true). This is the only kind pre-#455
	// providers emitted, so it is the zero value for backward
	// compatibility.
	ASRKindTranscript ASRResultKind = iota
	// ASRKindSpeechStarted marks a voice-activity onset (the provider's
	// server-VAD speech-started event). It carries no transcript text; it is the
	// signal the turn-taking machine uses to enter human-turn and to
	// raise a barge-in candidate while the assistant has the floor.
	ASRKindSpeechStarted
)

// ASRResult is a transcription result from the ASR provider.
type ASRResult struct {
	Text       string  `json:"text"`
	IsFinal    bool    `json:"isFinal"`
	Confidence float64 `json:"confidence"`
	SpeakerId  string  `json:"speakerId,omitempty"` // Only if diarization enabled
	// Kind discriminates the turn-structure role of this result.
	// The zero value (ASRKindTranscript) preserves the historical
	// Text/IsFinal behavior; ASRKindSpeechStarted carries an onset
	// signal with no text. Additive for the turn-taking machine (#455);
	// transcript-only consumers ignore it.
	Kind ASRResultKind `json:"kind,omitempty"`
}

// TTSProvider abstracts text-to-speech synthesis. The implementation is
// OpenAI TTS (/v1/audio/speech).
type TTSProvider interface {
	// Synthesize converts text to speech audio, returning a reader of audio data.
	// The audio format depends on the provider (typically PCM16 or WAV).
	Synthesize(ctx context.Context, config TTSConfig) (io.ReadCloser, error)

	// SynthesizeStream converts text to speech with streaming output.
	// Audio chunks arrive on the returned channel as they're generated.
	SynthesizeStream(ctx context.Context, config TTSConfig) (TTSStream, error)

	// AvailableVoices returns the list of voice models this provider supports.
	AvailableVoices(ctx context.Context) ([]VoiceInfo, error)
}

// TTSStream represents a streaming TTS session.
type TTSStream interface {
	// Chunks returns a channel of audio chunks as they're generated.
	Chunks() <-chan TTSChunk

	// Close terminates the stream.
	Close() error
}

// TTSConfig configures a TTS synthesis request.
type TTSConfig struct {
	// Text to synthesize.
	Text string `json:"text"`

	// VoiceModel is the voice identity to use (provider-specific).
	// For OpenAI: "alloy", "nova", "coral", etc.
	VoiceModel string `json:"voiceModel"`

	// SampleRate in Hz for output audio (default: 16000).
	SampleRate int `json:"sampleRate"`

	// Speed controls speaking rate (1.0 = normal, 0.5 = slow, 2.0 = fast).
	Speed float64 `json:"speed"`

	// Language is a BCP-47 language code (e.g., "en-US").
	Language string `json:"language"`
}

// DefaultTTSConfig returns sensible defaults for TTS configuration.
func DefaultTTSConfig(text, voiceModel string) TTSConfig {
	return TTSConfig{
		Text:       text,
		VoiceModel: voiceModel,
		SampleRate: 16000,
		Speed:      1.0,
		Language:   "en-US",
	}
}

// TTSChunk is a chunk of synthesized audio from the TTS provider.
type TTSChunk struct {
	Audio    []byte `json:"audio"`    // PCM16 or WAV audio data
	Sequence int    `json:"sequence"` // Chunk sequence number
	Done     bool   `json:"done"`     // Is this the last chunk?
}

// VoiceInfo describes an available TTS voice.
type VoiceInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Language string `json:"language"`
	Gender   string `json:"gender"` // "male", "female", "neutral"
}

// RoomProvider abstracts the audio room management (LiveKit or equivalent).
// The Bridge Agent implements this interface to manage multi-party audio rooms.
type RoomProvider interface {
	// CreateRoom creates a new audio room for a space.
	CreateRoom(ctx context.Context, scopeId string, agents []AgentRoomConfig) (*RoomInfo, error)

	// DestroyRoom tears down a room and all its participants.
	DestroyRoom(ctx context.Context, scopeId string) error

	// GenerateToken creates an access token for a participant to join a room.
	GenerateToken(ctx context.Context, scopeId, participantId, displayName string) (*RoomToken, error)

	// GetRoomInfo returns the current state of a room.
	GetRoomInfo(ctx context.Context, scopeId string) (*RoomInfo, error)
}

// AgentRoomConfig describes an AI agent to be added to a room.
type AgentRoomConfig struct {
	AgentId    string `json:"agentId"`
	Name       string `json:"name"`
	VoiceModel string `json:"voiceModel"`
}

// RoomInfo describes the current state of an audio room.
type RoomInfo struct {
	RoomName string            `json:"roomName"`
	RoomSID  string            `json:"roomSID"`
	ScopeId  string            `json:"scopeId"`
	Active   bool              `json:"active"`
	Humans   []RoomParticipant `json:"humans"`
	Agents   []RoomParticipant `json:"agents"`
}

// RoomParticipant describes a participant in a room.
type RoomParticipant struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"` // "human" or "agent"
	IsSpeaking bool   `json:"isSpeaking"`
}

// RoomToken is an access token for joining a room.
type RoomToken struct {
	Token      string `json:"token"`
	RoomName   string `json:"roomName"`
	LiveKitURL string `json:"livekitUrl"`
	ExpiresAt  int64  `json:"expiresAt"` // Unix timestamp
}
