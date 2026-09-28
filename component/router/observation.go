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

// attemptOf is the attempt id startObservation opened on ctx, or "".
func attemptOf(ctx context.Context) string {
	attempt, _ := ctx.Value(attemptKey{}).(string)
	return attempt
}

// recordAttempt writes the row for the attempt startObservation opened on ctx,
// UNDER THAT ATTEMPT'S ID, and returns the row's full id.
//
// Naming the row before it is written is what lets the caller journal which
// decision record stands behind a call (airoute.Served.RouterCallId). It is
// the OBSERVER that calls this, with the context it opened -- never a fallback
// or cache row, whose context may carry an enclosing call's attempt and would
// then collide with that call's own row.
func (r *Router) recordAttempt(ctx context.Context, rec CallRecord) string {
	rec.CallId = attemptOf(ctx)
	if rec.CallId == "" {
		rec.CallId = id.NewShortId()
	}
	r.recordObserved(ctx, rec)
	return RouterCallConcept + ":" + rec.CallId
}

func (r *Router) recordObserved(ctx context.Context, rec CallRecord) {
	r.recordCall(rec)
	attempt := rec.CallId
	if attempt == "" {
		attempt = attemptOf(ctx)
	}
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
