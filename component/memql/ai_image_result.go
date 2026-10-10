package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// ImageAIProvider produces raster bytes, unlike VisionAIProvider which reads them.
type ImageAIProvider interface {
	GenerateImage(context.Context, FleetImageRequest) ([]FleetImage, error)
}

// StoredImageAIResult journals a durable storage receipt, never image bytes.
type StoredImageAIResult struct {
	Receipt    string
	Resolution airoute.Resolution
	Served     string
}

// CallAIImage stores one bounded image before journaling its receipt. The sink
// must write immutable bytes under storageKey and reconcile uncertain writes.
// Including that key in the request prevents replay into another destination.
// A journal hit returns the receipt without regenerating or uploading an image.
func (e *MemQLEngine) CallAIImage(ctx context.Context, req airoute.ResolveRequest, input FleetImageRequest, storageKey string, store func(context.Context, FleetImage, airoute.Resolution) (string, error)) (StoredImageAIResult, error) {
	var out StoredImageAIResult
	input.Prompt = withStepOverrideText(ctx, input.Prompt)
	if strings.TrimSpace(input.Prompt) == "" || len(input.Prompt) > 12000 || storageKey == "" || store == nil {
		return out, fmt.Errorf("image: a bounded prompt and durable destination are required")
	}
	if input.Count != 1 || input.Width < 256 || input.Width > 1536 || input.Height < 256 || input.Height > 1536 {
		return out, fmt.Errorf("image: request one image with dimensions between 256 and 1536 pixels")
	}
	req, err := ApplyStepOverride(ctx, req)
	if err != nil {
		return out, err
	}
	req.Modality, req.Needs.Image, req.Needs.MinContextTokens = airoute.ModalityImage, true, 0
	resolved, err := e.resolveAI(ctx, req)
	if err != nil {
		return out, err
	}
	provider, ok := resolved.Client.(ImageAIProvider)
	if !ok || provider == nil {
		return out, fmt.Errorf("the selected provider does not generate images")
	}
	out.Resolution = resolved.Resolution
	out.Served = servedLabel(servedLocally(out.Resolution.Decision.Door, out.Resolution.ProviderName))
	request := common.ModelRequest{Provider: out.Resolution.ProviderName, Model: out.Resolution.Model,
		Messages: []common.ChatMessage{{Role: "user", Content: input.Prompt}},
		Settings: map[string]any{"modality": "image", "width": input.Width, "height": input.Height, "count": input.Count, "format": input.Format, "storageKey": storageKey}}
	out.Receipt, err = e.modelSeam.serveText(ctx, request, req.PromptName, func(ctx context.Context) (modelCallOutcome, error) {
		images, callErr := provider.GenerateImage(ctx, input)
		outcome := modelCallOutcome{Local: out.Served == "local"}
		if served, ok := servedBy(provider); ok {
			outcome.Served = &served
			outcome.Local = servedLocally(served.Decision.Door, served.ProviderName)
			out.Resolution, out.Served = resolutionServed(served), servedLabel(outcome.Local)
		}
		if callErr != nil {
			return outcome, callErr
		}
		if len(images) != 1 || len(images[0].Data) == 0 || len(images[0].Data) > 8<<20 || !strings.HasPrefix(images[0].MediaType, "image/") {
			return outcome, fmt.Errorf("image: the provider must return one image no larger than 8 MiB")
		}
		receipt, err := store(ctx, images[0], out.Resolution)
		if err == nil && strings.TrimSpace(receipt) == "" {
			err = fmt.Errorf("image storage returned no receipt")
		}
		outcome.Value = receipt
		return outcome, err
	}, func(call JournaledCall) {
		out.Served = call.Served
		if call.Served == "journal" {
			out.Resolution.ProviderName, out.Resolution.Model = call.Provider, call.Model
		}
	})
	return out, err
}
