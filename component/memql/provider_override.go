package memql

import (
	"context"
	"strings"
)

// providerOverrideKey carries an optional LLM provider name that the tool
// loop's InvokeAIChatWithFilteredTools passes as the
// AIInvocation.ProviderOverride. It lets a caller route one specific turn to
// a registered provider other than the template default -- the planner's
// agent loop escalates a stuck plan to a stronger reasoning tier this way,
// without changing the global default.
type providerOverrideKey struct{}

// WithProviderOverride returns a child context carrying the provider override
// name. Empty or whitespace-only names are ignored, so a caller that resolved
// no provider does not have to branch.
func WithProviderOverride(ctx context.Context, provider string) context.Context {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return ctx
	}
	return context.WithValue(ctx, providerOverrideKey{}, provider)
}

// ProviderOverrideFromContext returns the provider override previously
// attached via WithProviderOverride, or "" if none was set.
func ProviderOverrideFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(providerOverrideKey{}).(string); ok {
		return v
	}
	return ""
}
