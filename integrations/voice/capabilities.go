package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Integration exposes the canonical voice catalog to the DSL via two
// builtins (`voice.pickForGender`, `voice.resolve`). Stateless --
// every call goes through the catalog tables in voices.go.
type Integration struct {
	Logger *slog.Logger
}

// New returns a new voice integration.
func New(logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{Logger: logger}
}

// IntegrationName satisfies memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "voice" }

// Capabilities exposes the picker + resolver to the DSL.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "pickForGender",
			Description: "Pick a canonical voice name for an agent of the given gender, biased away from the exclude list.",
			Handler:     i.handlePick,
		},
		{
			Name:        "resolve",
			Description: "Resolve a canonical voice name + provider name to the provider-specific voice ID.",
			Handler:     i.handleResolve,
		},
	}
}

func (i *Integration) handlePick(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	gender, _ := args["gender"].(string)
	if gender == "" {
		return nil, fmt.Errorf("voice.pickForGender requires gender")
	}

	exclude := stringSlice(args["exclude"])
	canonical := PickVoiceForGender(gender, exclude)

	return wrap("voice:pick:"+canonical, map[string]any{
		"voiceId": canonical,
		"gender":  CanonicalGender(canonical),
	})
}

func (i *Integration) handleResolve(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	canonical, _ := args["voiceId"].(string)
	provider, _ := args["provider"].(string)

	resolved := ResolveVoice(canonical, provider)
	return wrap("voice:resolve:"+canonical+":"+provider, map[string]any{
		"voiceId":         canonical,
		"provider":        provider,
		"providerVoiceId": resolved,
	})
}

func stringSlice(v any) []string {
	switch raw := v.(type) {
	case []string:
		return raw
	case []any:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func wrap(id string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:        id,
		Concept:   "integration:voice:result",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: "system:integration:voice",
		Payload:   data,
	}}, nil
}
