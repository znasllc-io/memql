package router

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

type imageContextFleet struct{ contextFleet }

func (f *imageContextFleet) Call(_ context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	f.requests = append(f.requests, req)
	if req.Kind != memql.FleetKindImage || req.ContextTokens != 0 {
		return memql.FleetCallResult{}, &memql.FleetUnavailable{ModelId: req.ModelId}
	}
	return memql.FleetCallResult{Images: []memql.FleetImage{{Data: []byte("png"), MediaType: "image/png"}}}, nil
}

func TestImageRoutingRequiresGenerationAndNoChatWindow(t *testing.T) {
	// Document asset generation is background work, with the same live guards.
	ctx := memql.ContextWithBackgroundLane(context.Background())
	text := sizedModel("text-only", 1000000, 32768)
	image := sizedModel("image-model", 4000000000, 0)
	image.ImageGen = true
	fleet := &imageContextFleet{contextFleet{models: []memql.FleetModel{text, image}}}
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(fleet)
	r := New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"p": {memql.FleetFastest}}), testRules(t, defaultRule("p")), nil, nil)
	resolved, err := r.ResolveFor(ctx, ResolveRequest{UserId: "alice", Level: airoute.LevelFast, Modality: airoute.ModalityImage, Needs: airoute.Needs{MinContextTokens: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	provider := resolved.Client.(memql.ImageAIProvider)
	images, err := provider.GenerateImage(ctx, memql.FleetImageRequest{Prompt: "Portrait", Width: 512, Height: 512, Count: 1})
	if err != nil || len(images) != 1 || len(fleet.requests) != 1 || fleet.requests[0].ModelId != "image-model" {
		t.Fatalf("images=%v err=%v requests=%+v", images, err, fleet.requests)
	}
	served, ok := resolved.Client.(interface{ LastServed() (airoute.Served, bool) }).LastServed()
	if !ok || served.Model != "image-model" || served.RouterCallId == "" {
		t.Fatalf("actual image source missing: %+v", served)
	}
}
