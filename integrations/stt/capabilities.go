package stt

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql"
)

// STTIntegration wraps a StreamingProvider as an IntegrationProvider for
// batch (non-streaming) transcription from the MemQL DSL.
type STTIntegration struct {
	provider StreamingProvider
}

// NewSTTIntegration creates an STT integration wrapping the given provider.
func NewSTTIntegration(provider StreamingProvider) *STTIntegration {
	return &STTIntegration{provider: provider}
}

// IntegrationName returns the stable identifier.
func (s *STTIntegration) IntegrationName() string {
	return "stt"
}

// Capabilities returns DSL-callable transcription operations.
func (s *STTIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "transcribe",
			Description: "Transcribe audio data to text using the configured STT provider. Non-streaming batch transcription.",
			Handler:     s.handleTranscribe,
			ArgsSchema: map[string]string{
				"audio":      "string",
				"format":     "string",
				"sampleRate": "number",
			},
		},
	}
}

func (s *STTIntegration) handleTranscribe(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	audioStr, _ := args["audio"].(string)
	format, _ := args["format"].(string)
	if format == "" {
		format = "pcm16"
	}

	sampleRate := 16000
	if sr, ok := args["sampleRate"].(float64); ok && sr > 0 {
		sampleRate = int(sr)
	}

	if audioStr == "" {
		return nil, fmt.Errorf("stt.transcribe requires audio data")
	}

	session, err := s.provider.StartStream(ctx, StreamConfig{
		Format:     format,
		SampleRate: sampleRate,
		Channels:   1,
	})
	if err != nil {
		return nil, fmt.Errorf("start stt stream: %w", err)
	}
	defer session.Close()

	if err := session.SendAudio([]byte(audioStr)); err != nil {
		return nil, fmt.Errorf("send audio: %w", err)
	}

	final, err := session.Finalize(ctx)
	if err != nil {
		return nil, fmt.Errorf("finalize transcription: %w", err)
	}

	text := ""
	if final != nil {
		text = final.Text
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"text":         text,
		"provider":     s.provider.Name(),
		"format":       format,
		"sampleRate":   sampleRate,
		"transcribedAt": time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("stt-transcription:%d", time.Now().UnixNano()),
		Concept:   "integration:stt:transcription",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}
