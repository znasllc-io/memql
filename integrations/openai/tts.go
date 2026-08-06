package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/core/audio"
)

const ttsEndpoint = "https://api.openai.com/v1/audio/speech"

// TTSClient implements polyphon.TTSProvider using the OpenAI /v1/audio/speech
// HTTP API. Audio is requested in raw PCM16 format at 24kHz and downsampled
// to 16kHz for the Polyphon pipeline.
type TTSClient struct {
	apiKey string
	model  string
	voice  string
	logger *slog.Logger
}

// NewTTSClient creates an OpenAI TTS client.
func NewTTSClient(cfg Config) (*TTSClient, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	logger := cfg.logger()
	logger.Info("openai tts: initialized", "model", cfg.TTSModel, "voice", cfg.TTSVoice)

	return &TTSClient{
		apiKey: cfg.APIKey,
		model:  cfg.TTSModel,
		voice:  cfg.TTSVoice,
		logger: logger,
	}, nil
}

// Model returns the configured TTS model name.
func (c *TTSClient) Model() string {
	return c.model
}

// Synthesize generates complete audio from text using OpenAI's TTS API.
// Returns a reader containing PCM16 16kHz mono audio.
func (c *TTSClient) Synthesize(ctx context.Context, config polyphon.TTSConfig) (io.ReadCloser, error) {
	voice := config.VoiceModel
	if voice == "" {
		voice = c.voice
	}
	speed := config.Speed
	if speed == 0 {
		speed = 1.0
	}

	pcm24k, err := c.requestPCM(ctx, config.Text, voice, speed)
	if err != nil {
		return nil, err
	}

	// Downsample 24kHz -> 16kHz.
	downsampler, err := audio.NewPCM16Resampler(audio.OpenAISampleRate, audio.PolyphonSampleRate)
	if err != nil {
		return nil, fmt.Errorf("openai tts: create downsampler: %w", err)
	}

	pcm16k := downsampler.Resample(pcm24k)
	return io.NopCloser(bytes.NewReader(pcm16k)), nil
}

// SynthesizeStream generates streaming audio from text using OpenAI's TTS API.
// Audio is fetched in full, then chunked into ~100ms segments for low-latency
// delivery through the Polyphon pipeline.
func (c *TTSClient) SynthesizeStream(ctx context.Context, config polyphon.TTSConfig) (polyphon.TTSStream, error) {
	voice := config.VoiceModel
	if voice == "" {
		voice = c.voice
	}
	speed := config.Speed
	if speed == 0 {
		speed = 1.0
	}

	pcm24k, err := c.requestPCM(ctx, config.Text, voice, speed)
	if err != nil {
		return nil, err
	}

	// Create downsampler: OpenAI 24kHz -> Polyphon 16kHz.
	downsampler, err := audio.NewPCM16Resampler(audio.OpenAISampleRate, audio.PolyphonSampleRate)
	if err != nil {
		return nil, fmt.Errorf("openai tts: create downsampler: %w", err)
	}

	s := &openaiTTSStream{
		chunks: make(chan polyphon.TTSChunk, 16),
		logger: c.logger,
	}

	// Start goroutine to chunk and downsample audio.
	go s.readLoop(pcm24k, downsampler)

	return s, nil
}

// AvailableVoices returns the list of OpenAI TTS voices.
func (c *TTSClient) AvailableVoices(_ context.Context) ([]polyphon.VoiceInfo, error) {
	return []polyphon.VoiceInfo{
		{ID: "alloy", Name: "Alloy", Language: "en-US", Gender: "neutral"},
		{ID: "nova", Name: "Nova", Language: "en-US", Gender: "female"},
		{ID: "coral", Name: "Coral", Language: "en-US", Gender: "female"},
		{ID: "sage", Name: "Sage", Language: "en-US", Gender: "neutral"},
		{ID: "echo", Name: "Echo", Language: "en-US", Gender: "male"},
		{ID: "shimmer", Name: "Shimmer", Language: "en-US", Gender: "female"},
	}, nil
}

// SynthesizeOGGOpus generates OGG/Opus encoded audio from text using the
// OpenAI TTS API. Buffered -- returns the full OGG response after the
// API call completes. Use SynthesizeOGGOpusStream when the caller wants
// to publish frames as they arrive.
func (c *TTSClient) SynthesizeOGGOpus(ctx context.Context, config polyphon.TTSConfig) ([]byte, error) {
	voice := config.VoiceModel
	if voice == "" {
		voice = c.voice
	}
	speed := config.Speed
	if speed == 0 {
		speed = 1.0
	}

	return c.requestFormat(ctx, config.Text, voice, speed, "opus")
}

// SynthesizeOGGOpusStream returns the OpenAI TTS response body as an
// io.ReadCloser without buffering. The caller reads bytes incrementally
// (typically through a streaming OGG parser) and publishes each Opus
// frame as soon as it lands. Time-to-first-audio drops from "wait for
// full response" (~1.3s) to "first chunk arrives" (~200ms).
//
// Caller MUST close the returned reader.
func (c *TTSClient) SynthesizeOGGOpusStream(ctx context.Context, config polyphon.TTSConfig) (io.ReadCloser, error) {
	voice := config.VoiceModel
	if voice == "" {
		voice = c.voice
	}
	speed := config.Speed
	if speed == 0 {
		speed = 1.0
	}
	if config.Text == "" {
		return nil, fmt.Errorf("openai tts: text is required")
	}

	reqBody := map[string]any{
		"model":           c.model,
		"input":           config.Text,
		"voice":           voice,
		"response_format": "opus",
		"speed":           speed,
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("openai tts stream: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ttsEndpoint, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("openai tts stream: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai tts stream: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("openai tts stream: status %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// Close is a no-op; each synthesis request uses a fresh HTTP connection.
func (c *TTSClient) Close() error {
	return nil
}

// requestPCM calls the OpenAI TTS API and returns raw PCM16 24kHz audio bytes.
func (c *TTSClient) requestPCM(ctx context.Context, text, voice string, speed float64) ([]byte, error) {
	return c.requestFormat(ctx, text, voice, speed, "pcm")
}

// requestFormat calls the OpenAI TTS API and returns audio in the specified format.
// Supported formats: "pcm" (raw PCM16 24kHz), "opus" (OGG/Opus), "mp3", "aac", "flac", "wav".
func (c *TTSClient) requestFormat(ctx context.Context, text, voice string, speed float64, format string) ([]byte, error) {
	if text == "" {
		return nil, fmt.Errorf("openai tts: text is required")
	}

	reqBody := map[string]any{
		"model":           c.model,
		"input":           text,
		"voice":           voice,
		"response_format": format,
		"speed":           speed,
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("openai tts: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ttsEndpoint, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("openai tts: create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai tts: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai tts: request failed with status %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai tts: read response: %w", err)
	}

	return data, nil
}

// openaiTTSStream wraps chunked TTS audio delivery.
type openaiTTSStream struct {
	chunks chan polyphon.TTSChunk
	logger *slog.Logger
	once   sync.Once
}

// Chunks returns the channel of audio chunks.
func (s *openaiTTSStream) Chunks() <-chan polyphon.TTSChunk {
	return s.chunks
}

// Close terminates the TTS stream.
func (s *openaiTTSStream) Close() error {
	s.once.Do(func() {
		// Let readLoop drain naturally.
	})
	return nil
}

// readLoop chunks 24kHz PCM audio into ~100ms segments, downsamples each to
// 16kHz, and emits them as TTSChunks.
//
// At 24kHz PCM16 mono: 100ms = 2400 samples * 2 bytes = 4800 bytes per chunk.
// After downsampling to 16kHz: ~3200 bytes per chunk (~100ms).
func (s *openaiTTSStream) readLoop(pcm24k []byte, downsampler *audio.PCM16Resampler) {
	defer close(s.chunks)

	const chunkSize = 4800 // 100ms at 24kHz PCM16 mono

	seq := 0
	for offset := 0; offset < len(pcm24k); offset += chunkSize {
		end := offset + chunkSize
		if end > len(pcm24k) {
			end = len(pcm24k)
		}

		chunk24k := pcm24k[offset:end]

		// Downsample this chunk to 16kHz.
		chunk16k := downsampler.Resample(chunk24k)

		isLast := end >= len(pcm24k)

		if len(chunk16k) > 0 {
			s.chunks <- polyphon.TTSChunk{
				Audio:    chunk16k,
				Sequence: seq,
				Done:     false,
			}
			seq++
		}

		if isLast {
			// Send final empty chunk to signal completion.
			s.chunks <- polyphon.TTSChunk{
				Sequence: seq,
				Done:     true,
			}
			return
		}
	}

	// Edge case: empty audio.
	s.chunks <- polyphon.TTSChunk{
		Sequence: seq,
		Done:     true,
	}
}
