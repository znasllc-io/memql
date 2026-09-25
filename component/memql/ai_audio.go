package memql

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/znasllc-io/memql/core/airoute"
)

// The same typed contracts serve fleet machines and federated audio providers.
type TranscriptionAIProvider interface {
	Transcribe(context.Context, FleetAudio) (FleetTranscript, error)
}
type SpeechAIProvider interface {
	Speak(context.Context, FleetSpeechRequest) (FleetAudio, error)
}

func AudioResolveRequest(modality airoute.Modality, provider string) airoute.ResolveRequest {
	return airoute.ResolveRequest{Level: airoute.LevelFast, Modality: modality, ExplicitProvider: provider,
		PromptName: "askAudio", Needs: airoute.Needs{AudioIn: modality == airoute.ModalityTranscribe, AudioOut: modality == airoute.ModalitySpeech, MinContextTokens: 1}}
}
func (e *MemQLEngine) TranscribeAudio(ctx context.Context, audio FleetAudio, provider string) (FleetTranscript, error) {
	if len(audio.Data) == 0 || len(audio.Data) > 16<<20 {
		return FleetTranscript{}, fmt.Errorf("audio must contain at most 16 MB")
	}
	p, _, err := ResolveAITyped[TranscriptionAIProvider](ctx, e, AudioResolveRequest(airoute.ModalityTranscribe, provider))
	if err != nil {
		return FleetTranscript{}, err
	}
	return p.Transcribe(ctx, audio)
}
func (e *MemQLEngine) SpeakAudio(ctx context.Context, text, voice, provider string) (FleetAudio, error) {
	if len(text) == 0 || len(text) > 12000 {
		return FleetAudio{}, fmt.Errorf("speech text must contain at most 12000 bytes")
	}
	p, _, err := ResolveAITyped[SpeechAIProvider](ctx, e, AudioResolveRequest(airoute.ModalitySpeech, provider))
	if err != nil {
		return FleetAudio{}, err
	}
	return p.Speak(ctx, FleetSpeechRequest{Text: text, Voice: voice, Format: "wav"})
}

// OpenAI speech uses the same registry credential and circuit guard as chat.
func (p *openAITTSProvider) Speak(ctx context.Context, req FleetSpeechRequest) (FleetAudio, error) {
	voice := req.Voice
	switch voice {
	case "female":
		voice = "marin"
	case "male":
		voice = "cedar"
	case "":
		voice = stringParam(p.params["voice"], "marin")
	}
	data, err := p.speak(ctx, req.Text, voice, "wav")
	return FleetAudio{Data: data, MediaType: "audio/wav", SampleRateHz: 24000}, err
}

type openAISTTProvider struct {
	client *openai.Client
	model  string
}

func newOpenAISTTProvider(cfg ProviderConfig) (AIProvider, error) {
	client, err := newOpenAIClient(cfg, guardedHTTPClient(nil))
	if err != nil {
		return nil, err
	}
	return &openAISTTProvider{client: client, model: cfg.Model}, nil
}
func (p *openAISTTProvider) Call(context.Context, string) (any, error) {
	return nil, fmt.Errorf("transcription requires audio")
}
func (p *openAISTTProvider) Transcribe(ctx context.Context, audio FleetAudio) (FleetTranscript, error) {
	ext := "wav"
	switch strings.ToLower(audio.MediaType) {
	case "audio/webm":
		ext = "webm"
	case "audio/mpeg":
		ext = "mp3"
	case "audio/ogg":
		ext = "ogg"
	case "audio/wav", "audio/x-wav":
	default:
		return FleetTranscript{}, fmt.Errorf("unsupported transcription audio format %q", audio.MediaType)
	}
	result, err := p.client.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		Model: openai.AudioModel(p.model), File: openai.File(bytes.NewReader(audio.Data), "speech."+ext, audio.MediaType), ResponseFormat: openai.AudioResponseFormatJSON,
	})
	if err != nil {
		return FleetTranscript{}, err
	}
	return FleetTranscript{Text: result.Text}, nil
}
