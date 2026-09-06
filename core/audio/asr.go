package audio

import "context"

// The streaming speech-to-text contract.
//
// It lives here, in the leaf audio package, because that is the only place
// both sides of the seam can see. The provider implementation is in
// integrations/openai; the session that adapts it onto the engine's wire is in
// integrations/stt, and integrations/stt imports integrations/openai -- so the
// shared types cannot live in either without making the edge circular. They
// were in component/polyphon until that package was deleted with the voice
// node (memql#4990); nothing about them was ever specific to it.
//
// The one caller that matters is the MemQL OS Ask surface's hold-to-talk: a
// microphone, a transcript, and no room, no participant and no conversation.

// ASRProvider abstracts speech-to-text transcription.
type ASRProvider interface {
	// StartStream begins a new streaming transcription session. Audio chunks
	// are sent via SendAudio and results arrive on the Results channel.
	StartStream(ctx context.Context, config ASRConfig) (ASRStream, error)
}

// ASRStream is an active streaming transcription session.
type ASRStream interface {
	// SendAudio sends a chunk of audio for transcription.
	// Audio must be PCM16, 16kHz, mono.
	SendAudio(audio []byte) error

	// Results returns a channel of transcription results. Interim
	// (non-final) results may arrive before the final transcript.
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
//
// The zero value (ASRKindTranscript) is the transcript update whose finality
// is carried by IsFinal, which is the only thing a push-to-talk caller reads.
// ASRKindSpeechStarted is the provider's voice-activity onset; it is kept
// because the wire carries it, and a consumer that ignores it is correct.
type ASRResultKind int

const (
	// ASRKindTranscript is the default kind: a transcript update whose
	// finality is carried by IsFinal (interim when false, committed
	// end-of-utterance when true). It is the zero value, so a provider
	// that never sets Kind behaves exactly as it always did.
	ASRKindTranscript ASRResultKind = iota
	// ASRKindSpeechStarted marks a voice-activity onset (the provider's
	// server-VAD speech-started event). It carries no transcript text.
	ASRKindSpeechStarted
)

// ASRResult is a transcription result from the ASR provider.
type ASRResult struct {
	Text       string  `json:"text"`
	IsFinal    bool    `json:"isFinal"`
	Confidence float64 `json:"confidence"`
	SpeakerId  string  `json:"speakerId,omitempty"` // Only if diarization enabled
	// Kind discriminates the turn-structure role of this result. The zero
	// value (ASRKindTranscript) carries the Text/IsFinal behavior;
	// ASRKindSpeechStarted carries an onset signal with no text.
	Kind ASRResultKind `json:"kind,omitempty"`
}
