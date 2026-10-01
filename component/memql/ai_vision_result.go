package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// VisionAIResult reports the routed model and journal source. Vision providers
// do not currently report token usage through VisionAIProvider; zero is unknown.
type VisionAIResult struct {
	Text       string
	Resolution airoute.Resolution
	Served     string
}

// CallAIVision uses the same caller-scoped router, work journal, step overrides,
// and provider guards as structured composition. The replay key includes actual
// image bytes and order, so a new reference cannot receive an old image's answer.
func (e *MemQLEngine) CallAIVision(ctx context.Context, req airoute.ResolveRequest, prompt string, images []common.VisionContent) (VisionAIResult, error) {
	var out VisionAIResult
	if len(images) == 0 || len(images) > 8 {
		return out, fmt.Errorf("vision: provide between one and eight images")
	}
	var total int
	for _, image := range images {
		if len(image.Data) == 0 || !strings.HasPrefix(image.MimeType, "image/") {
			return out, fmt.Errorf("vision: each input needs image bytes and an image MIME type")
		}
		total += len(image.Data)
		if total > 16<<20 {
			return out, fmt.Errorf("vision: image inputs exceed 16 MiB")
		}
	}
	req, err := ApplyStepOverride(ctx, req)
	if err != nil {
		return out, err
	}
	messages := withStepOverrideMessages(ctx, []common.ChatMessage{{Role: "user", Content: prompt}})
	var combined strings.Builder
	for _, message := range messages {
		combined.WriteString(message.Content)
		combined.WriteString("\n")
	}
	prompt = combined.String()
	req.Modality, req.Needs.Vision = airoute.ModalityVision, true
	req.Needs.MinContextTokens = max(req.Needs.MinContextTokens, airoute.EstimateMinContextTokens(prompt, 0)+len(images)*2048)
	resolved, err := e.resolveAI(ctx, req)
	if err != nil {
		return out, err
	}
	provider, ok := resolved.Client.(common.VisionAIProvider)
	if !ok || provider == nil {
		return out, fmt.Errorf("the router resolved %q without image understanding", resolved.Resolution.ProviderName)
	}
	out.Resolution = resolved.Resolution
	local := resolved.Resolution.Decision.Door == airoute.DoorLocal || strings.HasPrefix(resolved.Resolution.ProviderName, FleetReferencePrefix)
	out.Served = "live"
	if local {
		out.Served = "local"
	}
	request := common.ModelRequest{Provider: resolved.Resolution.ProviderName, Model: resolved.Resolution.Model, Messages: messages, Images: images}
	if resolved.Entry != nil {
		request.Settings = answerAffectingParams(resolved.Entry.Config.Params)
	}
	out.Text, err = e.modelSeam.serveText(ctx, request, req.PromptName, func(ctx context.Context) (modelCallOutcome, error) {
		text, err := provider.CallVision(ctx, prompt, images)
		return modelCallOutcome{Value: text, Local: local}, err
	}, func(call JournaledCall) {
		out.Served = call.Served
		if call.Served == "journal" {
			out.Resolution.ProviderName, out.Resolution.Model = call.Provider, call.Model
		}
	})
	return out, err
}
