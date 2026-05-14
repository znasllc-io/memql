// Package auth provides shared authentication utilities for MemQL.
// Both the per-node identity verifier and the no-auth dev shim
// produce contexts shaped by helpers in this package.
package auth

import (
	"context"
	"maps"
)

type contextKey string

const (
	// ClaimsContextKey is the context key for storing JWT claims.
	// This key is used by middleware to inject claims and by RBAC/identity
	// functions to extract them.
	ClaimsContextKey contextKey = "authorizer_claims"

	// TokenInfoContextKey is the context key for storing TokenInfo.
	// This provides structured access to token information.
	TokenInfoContextKey contextKey = "mcpTokenInfo"

	// DelegationContextKey is the context key for storing DelegationContext.
	// Present when an agent acts under delegated authority from an identity.
	DelegationContextKey contextKey = "delegationContext"
)

// ClaimsFromContext extracts JWT claims from the request context.
// Returns the claims map and a boolean indicating if claims were found.
func ClaimsFromContext(ctx context.Context) (map[string]any, bool) {
	if ctx == nil {
		return nil, false
	}

	claims, ok := ctx.Value(ClaimsContextKey).(map[string]any)
	return claims, ok && claims != nil
}

// ContextWithClaims returns a new context with the provided claims.
func ContextWithClaims(ctx context.Context, claims map[string]any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	if claims == nil {
		return ctx
	}

	return context.WithValue(ctx, ClaimsContextKey, claims)
}

// DelegationFromContext extracts the DelegationContext from the request context.
// Returns nil and false when no delegation is present.
func DelegationFromContext(ctx context.Context) (*DelegationContext, bool) {
	if ctx == nil {
		return nil, false
	}

	dc, ok := ctx.Value(DelegationContextKey).(*DelegationContext)
	return dc, ok && dc != nil
}

// ContextWithDelegation returns a new context with the provided DelegationContext.
func ContextWithDelegation(ctx context.Context, dc *DelegationContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	if dc == nil {
		return ctx
	}

	return context.WithValue(ctx, DelegationContextKey, dc)
}

// CloneClaims creates a shallow copy of a claims map.
func CloneClaims(claims map[string]any) map[string]any {
	if claims == nil {
		return nil
	}

	cloned := make(map[string]any, len(claims))
	maps.Copy(cloned, claims)

	return cloned
}
