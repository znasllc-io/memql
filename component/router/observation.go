package router

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/id"
)

type attemptKey struct{}
type modalityKey struct{}

func startObservation(ctx context.Context, req ResolveRequest, resolved Resolved, start time.Time) context.Context {
	attempt := id.NewShortId()
	ctx = context.WithValue(ctx, attemptKey{}, attempt)
	ctx = context.WithValue(ctx, modalityKey{}, string(req.Modality))
	airoute.Observe(ctx, airoute.CallObservation{ID: attempt, Phase: "running", StartedAt: start,
		Provider: resolved.ProviderName, Model: resolved.Model, Vendor: resolved.Vendor,
		Policy: resolved.Decision.Policy, Rule: resolved.Decision.Rule, Door: resolved.Decision.Door, Modality: string(req.Modality)})
	return ctx
}
func (r *Router) recordObserved(ctx context.Context, rec CallRecord) {
	r.recordCall(rec)
	attempt, _ := ctx.Value(attemptKey{}).(string)
	if attempt == "" {
		attempt = id.NewShortId()
	}
	modality, _ := ctx.Value(modalityKey{}).(string)
	phase := "completed"
	if rec.Outcome == "error" || rec.Outcome == "cancelled" {
		phase = "failed"
	}
	if rec.Outcome == "fallback_used" {
		phase = "fallback"
	}
	airoute.Observe(ctx, airoute.CallObservation{ID: attempt, Phase: phase, StartedAt: rec.StartedAt,
		Provider: rec.ProviderName, Model: rec.Model, Vendor: rec.Vendor, Modality: modality, Policy: rec.Policy, Rule: rec.Rule,
		Door: rec.Door, ExecutionSurface: rec.ExecutionSurface, ServedModel: rec.ServedModel, CacheKind: rec.CacheKind,
		InputTokens: rec.InputTokens, OutputTokens: rec.OutputTokens, TokensEstimated: rec.TokensEstimated,
		TotalCost: rec.TotalCost, PricingConfigured: rec.PricingConfigured, Billing: rec.Billing,
		ElapsedMS: rec.TotalDurationMs, FirstTokenMS: rec.TimeToFirstTokenMs, Error: rec.ErrorMessage})
}
