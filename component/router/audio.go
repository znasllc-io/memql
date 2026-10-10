package router

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/core/airoute"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

type mediaChain struct {
	servedMu sync.Mutex
	served   airoute.Served
	router   *Router
	chain    []string
	req      ResolveRequest
	resolved Resolved
	modality providerModality
}

func (r *Router) resolveMedia(ctx context.Context, req ResolveRequest, modality providerModality) (any, Resolved, error) {
	// Carry the same media eligibility rule through provider binding and the
	// worker hop. Clearing only the catalog filter leaves a false token floor
	// on the actual call and rejects every runtime with no chat window.
	req.Needs.MinContextTokens = 0
	chain, resolved, err := r.resolveChain(ctx, req, modality)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &mediaChain{router: r, chain: chain, req: req, resolved: resolved, modality: modality}, resolved, nil
}
func (a *mediaChain) call(ctx context.Context, inputChars int, invoke func(context.Context, any) (any, int, error)) (any, error) {
	a.servedMu.Lock()
	a.served = airoute.Served{}
	a.servedMu.Unlock()
	var last error
	var previous Resolved
	var previousCtx context.Context
	selection := a.resolved
	for _, name := range a.chain {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		client, resolved, ok := a.router.providerLookup(ctx, a.req, name, a.modality)
		if !ok {
			continue
		}
		resolved = resolved.withDecisionFrom(selection)
		if last != nil {
			a.router.recordObserved(previousCtx, fallbackRecord(a.req, previous, last))
		}
		start := time.Now()
		attemptCtx := startObservation(ctx, a.req, resolved, start)
		result, outputChars, err := invoke(attemptCtx, client)
		record := buildRecord(a.req, resolved, client, EstimateTokensFromChars(inputChars), EstimateTokensFromChars(outputChars), 0, start, time.Time{}, time.Now(), false, err, ctx.Err())
		callID := a.router.recordAttempt(attemptCtx, record)
		a.servedMu.Lock()
		a.served = airoute.Served{RouterCallId: callID, ProviderName: resolved.ProviderName, Vendor: resolved.Vendor, Model: resolved.Model, Decision: resolved.Decision}
		a.servedMu.Unlock()
		if err == nil {
			return result, nil
		}
		last = err
		previous = resolved
		previousCtx = attemptCtx
		selection = selection.withAttemptFailed(resolved, err)
		// Media generation is a pure inference request; a failed request has no workspace
		// mutation to replay. Each attempt remains separately visible in Activity.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if last == nil {
		last = fmt.Errorf("no media provider is reachable")
	}
	return nil, last
}
func (a *mediaChain) Transcribe(ctx context.Context, audio memql.FleetAudio) (memql.FleetTranscript, error) {
	result, err := a.call(ctx, 0, func(ctx context.Context, client any) (any, int, error) {
		output, err := client.(memql.TranscriptionAIProvider).Transcribe(ctx, audio)
		return output, len(output.Text), err
	})
	if err != nil {
		return memql.FleetTranscript{}, err
	}
	return result.(memql.FleetTranscript), nil
}
func (a *mediaChain) Speak(ctx context.Context, speech memql.FleetSpeechRequest) (memql.FleetAudio, error) {
	result, err := a.call(ctx, len(speech.Text), func(ctx context.Context, client any) (any, int, error) {
		output, err := client.(memql.SpeechAIProvider).Speak(ctx, speech)
		return output, 0, err
	})
	if err != nil {
		return memql.FleetAudio{}, err
	}
	return result.(memql.FleetAudio), nil
}

func (a *mediaChain) GenerateImage(ctx context.Context, request memql.FleetImageRequest) ([]memql.FleetImage, error) {
	result, err := a.call(ctx, len(request.Prompt), func(ctx context.Context, client any) (any, int, error) {
		output, err := client.(memql.ImageAIProvider).GenerateImage(ctx, request)
		return output, 0, err
	})
	if err != nil {
		return nil, err
	}
	return result.([]memql.FleetImage), nil
}

func (a *mediaChain) LastServed() (airoute.Served, bool) {
	a.servedMu.Lock()
	defer a.servedMu.Unlock()
	return a.served, a.served.ProviderName != ""
}
