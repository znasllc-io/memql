package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/polyphon"
)

// VoiceIntegration wraps OpenAI ASR/TTS clients as an IntegrationProvider.
type VoiceIntegration struct {
	tts polyphon.TTSProvider
}

// NewVoiceIntegration creates an OpenAI voice integration.
func NewVoiceIntegration(tts polyphon.TTSProvider) *VoiceIntegration {
	return &VoiceIntegration{tts: tts}
}

// IntegrationName returns the stable identifier.
func (v *VoiceIntegration) IntegrationName() string {
	return "openaiVoice"
}

// Capabilities returns DSL-callable voice operations.
func (v *VoiceIntegration) Capabilities() []memql.IntegrationCapability {
	caps := []memql.IntegrationCapability{}

	if v.tts != nil {
		caps = append(caps, memql.IntegrationCapability{
			Name:        "synthesize",
			Description: "Synthesize text to speech audio using OpenAI TTS API. Returns base64-encoded PCM16 audio.",
			Handler:     v.handleSynthesize,
			ArgsSchema: map[string]string{
				"text": "string",
			},
		})
	}

	return caps
}

func (v *VoiceIntegration) handleSynthesize(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("openaiVoice.synthesize requires text")
	}

	reader, err := v.tts.Synthesize(ctx, polyphon.TTSConfig{Text: text})
	if err != nil {
		return nil, fmt.Errorf("openai tts synthesize: %w", err)
	}
	defer reader.Close()

	audioData, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read tts audio: %w", err)
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"audio":        base64.StdEncoding.EncodeToString(audioData),
		"format":       "pcm16",
		"provider":     "openai",
		"textLength":   len(text),
		"audioBytes":   len(audioData),
		"synthesizedAt": time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("openai-tts:%d", time.Now().UnixNano()),
		Concept:   "integration:openaiVoice:synthesis",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}
