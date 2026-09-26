package airoute

import (
	"context"
	"time"
)

// CallObservation is public execution evidence from the actual provider attempt.
// It contains no messages, credentials, tool results or private model reasoning.
type CallObservation struct {
	ID                string    `json:"id"`
	Phase             string    `json:"phase"`
	StartedAt         time.Time `json:"startedAt"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	Vendor            string    `json:"vendor,omitempty"`
	Policy            string    `json:"policy,omitempty"`
	Rule              string    `json:"rule,omitempty"`
	Door              string    `json:"door,omitempty"`
	Modality          string    `json:"modality,omitempty"`
	ExecutionSurface  string    `json:"executionSurface,omitempty"`
	ServedModel       string    `json:"servedModel,omitempty"`
	CacheKind         string    `json:"cacheKind,omitempty"`
	InputTokens       int       `json:"inputTokens"`
	OutputTokens      int       `json:"outputTokens"`
	TokensEstimated   bool      `json:"tokensEstimated"`
	TotalCost         float64   `json:"totalCost"`
	PricingConfigured bool      `json:"pricingConfigured"`
	Billing           string    `json:"billing,omitempty"`
	ElapsedMS         int       `json:"elapsedMs"`
	FirstTokenMS      int       `json:"firstTokenMs"`
	Error             string    `json:"error,omitempty"`
}
type observationKey struct{}
type Observer func(CallObservation)

// WithObserver is installed on the executing node after authenticating a call;
// wire adapters forward observations as data rather than forwarding callbacks.
func WithObserver(ctx context.Context, observe Observer) context.Context {
	return context.WithValue(ctx, observationKey{}, observe)
}
func Observe(ctx context.Context, event CallObservation) {
	if observe, ok := ctx.Value(observationKey{}).(Observer); ok {
		observe(event)
	}
}
