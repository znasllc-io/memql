package deepgram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/voice"
)

// TTSClient implements polyphon.TTSProvider using Deepgram's `/v1/speak`
// REST API. The bridge agent already calls TTS per-sentence (the
// sentence-streaming pipeline that shipped before this migration), so
// sentence-granularity is the natural unit here -- one HTTP request
// per sentence, audio bytes stream back as the response body.
//
// Output is OGG/Opus to match the bridge's LiveKit publish path; the
// existing publishOpusFrames consumer reads OGG containers and writes
// Opus frames into the LiveKit audio track unchanged. The same shape
// the OpenAI TTS client (integrations/openai/tts.go) returns.
//
// Token-by-token input streaming (the Aura-2 WebSocket Speak/Flush
// protocol) is a future optimization. The current bridge interfaces
// land each TTS call with a full sentence already in hand; switching
// to token-by-token would require an end-to-end LLM->TTS streaming
// surface change that's out of scope for the migration. The Aura-2
// REST path still wins the >70% TTS-TTFB delta vs OpenAI in practice
// because Aura-2 itself is faster, even before the protocol upgrade.
type TTSClient struct {
	cfg           Config
	voiceOverride string // empty when POLYPHON_DEEPGRAM_TTS_VOICE_OVERRIDE is unset
	logger        *slog.Logger
	http          *http.Client
}

// EnvVoiceOverride forces every TTS synthesis to use the named Aura-2
// voice id, ignoring whatever the cognition handler resolved from the
// canonical catalog. Set to an Aura-2 voice id like
// "aura-2-asteria-en" to A/B different voices without touching the
// canonical catalog or restarting cognition.
const EnvVoiceOverride = "POLYPHON_DEEPGRAM_TTS_VOICE_OVERRIDE"

// NewTTSClient constructs a Deepgram TTS client. Config validation
// applies the package defaults (TTSModel="aura-2-thalia-en", BaseURL=
// "wss://api.deepgram.com" -- the speak endpoint reuses the host with
// the https scheme).
//
// Reads POLYPHON_DEEPGRAM_TTS_VOICE_OVERRIDE: when set, every synthesis
// uses that Aura-2 voice id regardless of what the caller passes in
// the per-call TTSConfig. Useful for A/B testing voices.
func NewTTSClient(cfg Config) (*TTSClient, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	logger := cfg.logger()
	override := strings.TrimSpace(os.Getenv(EnvVoiceOverride))
	if override != "" {
		logger.Info("deepgram tts: voice override active",
			"override", override,
			"env", EnvVoiceOverride,
		)
	}
	logger.Info("deepgram tts: initialized", "model", cfg.TTSModel, "override", override)
	return &TTSClient{
		cfg:           cfg,
		voiceOverride: override,
		logger:        logger,
		http:          http.DefaultClient,
	}, nil
}

// Model returns the configured Aura-2 model id (e.g. "aura-2" or
// the per-voice form "aura-2-thalia-en"). Phase 6 wires the canonical
// voice catalog to the Aura-2 voice ids; until then the bridge passes
// the model through unchanged via TTSConfig.VoiceModel.
func (c *TTSClient) Model() string { return c.cfg.TTSModel }

// Close is a no-op -- each synthesis is its own HTTP request.
func (c *TTSClient) Close() error { return nil }

// Synthesize returns PCM16 audio at 16kHz mono so the polyphon.TTSProvider
// contract is honored. Most callers prefer SynthesizeOGGOpus (the
// bridge's audio-publish path) -- this method exists for the small
// number of consumers that need raw PCM.
func (c *TTSClient) Synthesize(ctx context.Context, config polyphon.TTSConfig) (io.ReadCloser, error) {
	data, err := c.request(ctx, config, "linear16", "")
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// SynthesizeStream returns audio in fixed-size chunks for clients that
// prefer the channel-based delivery surface. The implementation buffers
// the full response and then chunks it -- matches integrations/openai's
// SynthesizeStream behavior. The bridge uses SynthesizeOGGOpusStream
// instead for true low-TTFB streaming.
func (c *TTSClient) SynthesizeStream(ctx context.Context, config polyphon.TTSConfig) (polyphon.TTSStream, error) {
	data, err := c.request(ctx, config, "linear16", "")
	if err != nil {
		return nil, err
	}
	s := &deepgramTTSStream{
		chunks: make(chan polyphon.TTSChunk, 16),
		logger: c.logger,
	}
	go s.readLoop(data)
	return s, nil
}

// AvailableVoices returns a representative list of Aura-2 voices. The
// canonical voice catalog (Phase 6) is the authoritative
// canonical-name -> Aura-2-voice-id map; this surface is here for
// completeness against polyphon.TTSProvider.
func (c *TTSClient) AvailableVoices(_ context.Context) ([]polyphon.VoiceInfo, error) {
	return []polyphon.VoiceInfo{
		{ID: "aura-2-thalia-en", Name: "Thalia", Language: "en-US", Gender: "female"},
		{ID: "aura-2-asteria-en", Name: "Asteria", Language: "en-US", Gender: "female"},
		{ID: "aura-2-luna-en", Name: "Luna", Language: "en-US", Gender: "female"},
		{ID: "aura-2-stella-en", Name: "Stella", Language: "en-US", Gender: "female"},
		{ID: "aura-2-athena-en", Name: "Athena", Language: "en-US", Gender: "female"},
		{ID: "aura-2-hera-en", Name: "Hera", Language: "en-US", Gender: "female"},
		{ID: "aura-2-orion-en", Name: "Orion", Language: "en-US", Gender: "male"},
		{ID: "aura-2-arcas-en", Name: "Arcas", Language: "en-US", Gender: "male"},
		{ID: "aura-2-perseus-en", Name: "Perseus", Language: "en-US", Gender: "male"},
		{ID: "aura-2-orpheus-en", Name: "Orpheus", Language: "en-US", Gender: "male"},
		{ID: "aura-2-angus-en", Name: "Angus", Language: "en-US", Gender: "male"},
		{ID: "aura-2-helios-en", Name: "Helios", Language: "en-US", Gender: "male"},
	}, nil
}

// SynthesizeOGGOpus returns the full OGG/Opus payload after the
// synthesis request completes. Matches integrations/openai's
// SynthesizeOGGOpus shape so the bridge's duck-typed dispatch path
// (handleSentenceJob's opusSynthesizer interface) picks it up
// unchanged.
func (c *TTSClient) SynthesizeOGGOpus(ctx context.Context, config polyphon.TTSConfig) ([]byte, error) {
	return c.request(ctx, config, "opus", "ogg")
}

// SynthesizeOGGOpusStream returns the response body as a streaming
// io.ReadCloser, so callers can publish Opus frames as bytes arrive
// rather than waiting on the full synthesis. The bridge's streaming
// publish path (handleSentenceJob's opusStreamSynthesizer dispatch)
// uses this; TTFB drops from "full synthesis" to "first chunk on
// the wire".
//
// Caller MUST close the returned reader.
func (c *TTSClient) SynthesizeOGGOpusStream(ctx context.Context, config polyphon.TTSConfig) (io.ReadCloser, error) {
	resp, err := c.dispatch(ctx, config, "opus", "ogg")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// request is the buffered helper: dispatch + ReadAll + close. Returns
// the full body bytes.
func (c *TTSClient) request(ctx context.Context, config polyphon.TTSConfig, encoding, container string) ([]byte, error) {
	resp, err := c.dispatch(ctx, config, encoding, container)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("deepgram tts: read response: %w", readErr)
	}
	return data, nil
}

// dispatch fires the POST /v1/speak request. The caller decides
// whether to buffer (request) or stream (SynthesizeOGGOpusStream) the
// response body.
func (c *TTSClient) dispatch(ctx context.Context, config polyphon.TTSConfig, encoding, container string) (*http.Response, error) {
	text := strings.TrimSpace(config.Text)
	if text == "" {
		return nil, fmt.Errorf("deepgram tts: text is required")
	}

	model := c.resolveModel(config.VoiceModel)

	speakURL, err := c.speakURL(model, encoding, container)
	if err != nil {
		return nil, err
	}

	body := map[string]string{"text": text}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("deepgram tts: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, speakURL,
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("deepgram tts: build request: %w", err)
	}
	req.Header.Set("Authorization", c.cfg.authHeaderValue())
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deepgram tts: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("deepgram tts: status %d: %s",
			resp.StatusCode, string(errBody))
	}
	return resp, nil
}

// resolveModel turns whatever VoiceModel the caller passed (canonical
// voice name, OpenAI voice id, Aura-2 voice id, empty string) into a
// valid Aura-2 model id.
//
// Resolution order:
//
//  1. POLYPHON_DEEPGRAM_TTS_VOICE_OVERRIDE set (any non-empty value) ->
//     use that. Skips the catalog entirely so testing a single voice
//     across every agent in the cluster is a one-env-var change.
//  2. Aura-2 voice id ("aura-2-...") -> pass through.
//  3. Canonical voice name or other provider's id -> resolve via the
//     catalog, which falls back to a gender-appropriate Aura-2 default
//     for unknowns so the audio path never goes silent.
//  4. Anything still non-Aura-2 -> configured default.
//
// Defense in depth: when a node in the cluster disagrees on the
// active provider and sends an OpenAI-style voice id, the catalog
// rescues it instead of 400ing the synthesis. The voice.ActiveProvider()
// rule is the primary fix; resolveModel is the seatbelt.
func (c *TTSClient) resolveModel(input string) string {
	if c.voiceOverride != "" {
		return c.voiceOverride
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return c.cfg.TTSModel
	}
	if strings.HasPrefix(strings.ToLower(input), "aura-") {
		return input
	}
	resolved := voice.ResolveVoice(input, "deepgram")
	if strings.HasPrefix(strings.ToLower(resolved), "aura-") {
		return resolved
	}
	return c.cfg.TTSModel
}

// speakURL builds the POST URL for the speak endpoint. The TTS REST
// API is on https://api.deepgram.com/v1/speak (not wss://), so we
// rewrite the scheme from the Config's BaseURL (which defaults to
// wss://api.deepgram.com for the ASR streaming side).
func (c *TTSClient) speakURL(model, encoding, container string) (string, error) {
	base, err := url.Parse(c.cfg.BaseURL)
	if err != nil {
		return "", fmt.Errorf("deepgram tts: parse base url %q: %w", c.cfg.BaseURL, err)
	}
	// REST is https; promote wss -> https.
	if base.Scheme == "wss" {
		base.Scheme = "https"
	} else if base.Scheme == "ws" {
		base.Scheme = "http"
	}
	base.Path = "/v1/speak"

	q := base.Query()
	q.Set("model", model)
	q.Set("encoding", encoding)
	if container != "" {
		q.Set("container", container)
	}
	// Sample rate is only meaningful when encoding is one of the raw
	// PCM forms (linear16, mulaw, alaw). For opus + container=ogg
	// the codec defines its own rate, and passing sample_rate
	// triggers a 400 from Deepgram.
	switch encoding {
	case "linear16", "mulaw", "alaw":
		q.Set("sample_rate", strconv.Itoa(16000))
	}
	base.RawQuery = q.Encode()

	return base.String(), nil
}

// ---------------------------------------------------------------------------
// Chunk-based stream (SynthesizeStream)
// ---------------------------------------------------------------------------

type deepgramTTSStream struct {
	chunks chan polyphon.TTSChunk
	logger *slog.Logger
	once   sync.Once
}

func (s *deepgramTTSStream) Chunks() <-chan polyphon.TTSChunk {
	return s.chunks
}

func (s *deepgramTTSStream) Close() error {
	s.once.Do(func() {
		// Let readLoop drain naturally; it closes chunks when done.
	})
	return nil
}

// readLoop chunks a fully-buffered PCM16 payload into TTSChunks of
// ~100ms each (matches openai's behavior).
func (s *deepgramTTSStream) readLoop(pcm []byte) {
	defer close(s.chunks)
	// 16kHz mono PCM16 = 32 bytes/ms = 3200 bytes/100ms.
	const chunkBytes = 3200
	seq := 0
	for off := 0; off < len(pcm); off += chunkBytes {
		end := off + chunkBytes
		done := false
		if end >= len(pcm) {
			end = len(pcm)
			done = true
		}
		s.chunks <- polyphon.TTSChunk{
			Audio:    pcm[off:end],
			Sequence: seq,
			Done:     done,
		}
		seq++
	}
}
