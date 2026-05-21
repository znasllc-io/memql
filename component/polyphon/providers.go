package polyphon

import (
	"context"
	"io"
)

// ASRProvider abstracts speech-to-text transcription. The implementation
// is OpenAI Realtime in Stage 1; Stage 2 of the Deepgram migration adds
// Deepgram Nova-3 as the new default.
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

// ASRResult is a transcription result from the ASR provider.
type ASRResult struct {
	Text       string  `json:"text"`
	IsFinal    bool    `json:"isFinal"`
	Confidence float64 `json:"confidence"`
	SpeakerId  string  `json:"speakerId,omitempty"` // Only if diarization enabled
}

// TTSProvider abstracts text-to-speech synthesis. The implementation is
// OpenAI TTS in Stage 1; Stage 2 of the Deepgram migration adds Aura-2
// as the new default.
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
	CreateRoom(ctx context.Context, spaceId string, agents []AgentRoomConfig) (*RoomInfo, error)

	// DestroyRoom tears down a room and all its participants.
	DestroyRoom(ctx context.Context, spaceId string) error

	// GenerateToken creates an access token for a participant to join a room.
	GenerateToken(ctx context.Context, spaceId, participantId, displayName string) (*RoomToken, error)

	// GetRoomInfo returns the current state of a room.
	GetRoomInfo(ctx context.Context, spaceId string) (*RoomInfo, error)
}

// AgentRoomConfig describes an SI agent to be added to a room.
type AgentRoomConfig struct {
	AgentId    string `json:"agentId"`
	Name       string `json:"name"`
	VoiceModel string `json:"voiceModel"`
}

// RoomInfo describes the current state of an audio room.
type RoomInfo struct {
	RoomName string            `json:"roomName"`
	RoomSID  string            `json:"roomSID"`
	SpaceId  string            `json:"spaceId"`
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
