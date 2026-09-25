package router

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

type audioChain struct {
	router   *Router
	chain    []string
	req      ResolveRequest
	resolved Resolved
	modality providerModality
}

func (r *Router) resolveAudio(ctx context.Context, req ResolveRequest, modality providerModality) (any, Resolved, error) {
	chain, resolved, err := r.resolveChain(ctx, req, modality)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &audioChain{r, chain, req, resolved, modality}, resolved, nil
}
func (a *audioChain) call(ctx context.Context, inputChars int, invoke func(context.Context, any) (any, int, error)) (any, error) {
	var last error
	var previous Resolved
	var previousCtx context.Context
	for _, name := range a.chain {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		client, resolved, ok := a.router.providerLookup(ctx, a.req, name, a.modality)
		if !ok {
			continue
		}
		resolved = resolved.withDecisionFrom(a.resolved)
		if last != nil {
			a.router.recordObserved(previousCtx, fallbackRecord(a.req, previous, last))
		}
		start := time.Now()
		attemptCtx := startObservation(ctx, a.req, resolved, start)
		result, outputChars, err := invoke(attemptCtx, client)
		record := buildRecord(a.req, resolved, client, EstimateTokensFromChars(inputChars), EstimateTokensFromChars(outputChars), 0, start, time.Time{}, time.Now(), false, err, ctx.Err())
		a.router.recordObserved(attemptCtx, record)
		if err == nil {
			return result, nil
		}
		last = err
		previous = resolved
		previousCtx = attemptCtx
		// Audio is a pure inference request; a failed request has no workspace
		// mutation to replay. Each attempt remains separately visible in Activity.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if last == nil {
		last = fmt.Errorf("no audio provider is reachable")
	}
	return nil, last
}
func (a *audioChain) Transcribe(ctx context.Context, audio memql.FleetAudio) (memql.FleetTranscript, error) {
	result, err := a.call(ctx, 0, func(ctx context.Context, client any) (any, int, error) {
		output, err := client.(memql.TranscriptionAIProvider).Transcribe(ctx, audio)
		return output, len(output.Text), err
	})
	if err != nil {
		return memql.FleetTranscript{}, err
	}
	return result.(memql.FleetTranscript), nil
}
func (a *audioChain) Speak(ctx context.Context, speech memql.FleetSpeechRequest) (memql.FleetAudio, error) {
	result, err := a.call(ctx, len(speech.Text), func(ctx context.Context, client any) (any, int, error) {
		output, err := client.(memql.SpeechAIProvider).Speak(ctx, speech)
		return output, 0, err
	})
	if err != nil {
		return memql.FleetAudio{}, err
	}
	return result.(memql.FleetAudio), nil
}
