package models

import (
	"context"
	"fmt"
	"strings"
)

// Declared OpenAI-compatible runtimes (spec D1).
//
// The attributes come from policy.yaml because the protocol cannot supply
// them: `/v1/models` returns ids and nothing else. What IS probed is
// liveness -- and that probe is not a formality. Advertising a model whose
// server is down means the router picks this machine, sends somebody's
// prompt to it, and the call fails at the last hop; the machine is the
// only party that can know, so it is the one that has to check.

// openAIModelsResponse is the shape of GET {base}/models.
type openAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// probeDeclared returns the servable models of one declared runtime.
func (d *Discoverer) probeDeclared(ctx context.Context, rt DeclaredRuntime) ([]Info, string) {
	name := strings.TrimSpace(rt.Name)
	base := strings.TrimRight(strings.TrimSpace(rt.BaseURL), "/")
	if name == "" || base == "" {
		return nil, "a declared runtime is missing its name or base_url and was skipped"
	}
	if len(rt.Models) == 0 {
		return nil, fmt.Sprintf("declared runtime %s lists no models, so it offers none", name)
	}

	var listing openAIModelsResponse
	var err error
	switch rt.Transcription {
	case "", "chat", "openai":
		err = d.getJSON(ctx, base+"/models", d.getenv(rt.APIKeyEnv), &listing)
	case "whisper-cpp":
		var health struct {
			Status string `json:"status"`
		}
		err = d.getJSON(ctx, base+"/health", d.getenv(rt.APIKeyEnv), &health)
		if err == nil && health.Status != "ok" {
			err = fmt.Errorf("transcription model is not ready")
		}
	default:
		return nil, fmt.Sprintf("declared runtime %s has an unknown transcription protocol", name)
	}
	if err != nil {
		return nil, fmt.Sprintf("declared runtime %s at %s did not answer (%v); its models are not offered", name, base, err)
	}

	// An endpoint that answers but lists nothing is taken at its word.
	// Some servers implement /v1/models and some do not; one that returns
	// an EMPTY list is saying it has no model loaded, which is different
	// from not implementing the route -- and the second case is what the
	// error branch above already caught.
	served := make(map[string]bool, len(listing.Data))
	for _, m := range listing.Data {
		served[strings.TrimSpace(m.ID)] = true
	}

	var out []Info
	var missing []string
	for _, m := range rt.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if rt.Transcription != "whisper-cpp" && !served[id] {
			missing = append(missing, id)
			continue
		}
		// A declared max_concurrent of zero would reach the engine as
		// "unlimited", which is the one attribute whose absence is
		// permissive rather than safe. An operator who wants more than
		// one concurrent generation on this endpoint says so; silence
		// gets the number that cannot overcommit somebody's hardware.
		maxConcurrent, ceilingFrom := m.MaxConcurrent, "max_concurrent under models.runtimes in policy.yaml"
		if maxConcurrent <= 0 {
			maxConcurrent, ceilingFrom = 1, "the default (models.runtimes declares no max_concurrent for it)"
		}
		out = append(out, Info{
			ID:            id,
			Transcription: rt.Transcription, Voices: rt.Voices,
			Kind:          KindOpenAICompatible,
			Runtime:       name,
			BaseURL:       base,
			APIKeyEnv:     strings.TrimSpace(rt.APIKeyEnv),
			CeilingSource: ceilingFrom,
			Attributes: Attributes{
				ContextWindow:    m.ContextWindow,
				StructuredOutput: m.StructuredOutput,
				Embeddings:       m.Embeddings,
				MaxConcurrent:    maxConcurrent,
				// Size, quantization and tool support come across too,
				// and leaving any of them out here is not a missing
				// feature but a silent one: yaml accepts the key, the
				// operator sees no error, and the attribute never
				// reaches a label -- which looks exactly like a typo
				// they cannot find.
				//
				// It matters most for `tools`. resolveDuplicates lets a
				// declared entry win WHOLESALE over the Ollama probe, so
				// the documented escape hatch -- declare a model against
				// Ollama's own /v1 endpoint to overrule the capability
				// heuristic -- would otherwise DELETE tools=1 from that
				// model rather than restate it.
				Params:       m.Params,
				ActiveParams: m.ActiveParams,
				Quant:        strings.TrimSpace(m.Quant),
				Tools:        m.Tools,
				// The four modalities, same wholesale-win reasoning:
				// three of them have no probe anywhere, so a declared
				// entry dropped here is a modality that can never be
				// advertised by any route at all.
				Vision:   m.Vision,
				AudioIn:  m.AudioIn,
				AudioOut: m.AudioOut,
				ImageGen: m.ImageGen,
			},
		})
	}

	var note string
	if len(missing) > 0 {
		note = fmt.Sprintf("declared runtime %s does not currently serve %s; declared but not offered",
			name, strings.Join(missing, ", "))
	}
	return out, note
}
