package auth

import (
	"context"
	"slices"
	"strings"

	"github.com/visionarys-io/memql/core/common"
)

// TokenInfo represents structured token information extracted from JWT claims.
type TokenInfo struct {
	Subject string
	Scopes  []string
	Claims  map[string]any
}

// TokenInfoFromContext extracts TokenInfo from the context.
// Falls back to building TokenInfo from claims if not directly available.
func TokenInfoFromContext(ctx context.Context) *TokenInfo {
	if ctx == nil {
		return nil
	}

	// Try to get directly stored TokenInfo first
	if value, ok := ctx.Value(TokenInfoContextKey).(*TokenInfo); ok {
		return value
	}

	// Fall back to building from claims
	claims, ok := ClaimsFromContext(ctx)
	if !ok {
		return nil
	}

	return BuildTokenInfo(claims)
}

// ContextWithToken returns a new context with the provided TokenInfo.
func ContextWithToken(ctx context.Context, token *TokenInfo) context.Context {
	if ctx == nil || token == nil {
		return ctx
	}
	return context.WithValue(ctx, TokenInfoContextKey, token)
}

// BuildTokenInfo constructs TokenInfo from a claims map.
func BuildTokenInfo(claims map[string]any) *TokenInfo {
	if len(claims) == 0 {
		return nil
	}

	token := &TokenInfo{
		Claims: make(map[string]any, len(claims)),
	}

	for key, value := range claims {
		token.Claims[key] = value
	}

	if subject, ok := claimString(claims, "sub"); ok {
		token.Subject = subject
	} else if email, ok := claimString(claims, "email"); ok {
		token.Subject = email
	}

	token.Scopes = normalizeScopes(extractScopes(claims))
	return token
}

func extractScopes(claims map[string]any) []string {
	if scopes, ok := claimString(claims, "scope"); ok {
		return strings.Fields(scopes)
	}
	if raw, ok := claims["scopes"]; ok {
		switch typed := raw.(type) {
		case []string:
			return typed
		case []any:
			out := make([]string, 0, len(typed))
			for _, entry := range typed {
				if str, ok := entry.(string); ok {
					out = append(out, str)
				}
			}
			return out
		}
	}
	if raw, ok := claims["scp"]; ok {
		switch typed := raw.(type) {
		case []string:
			return typed
		case []any:
			out := make([]string, 0, len(typed))
			for _, entry := range typed {
				if str, ok := entry.(string); ok {
					out = append(out, str)
				}
			}
			return out
		case string:
			return strings.Fields(typed)
		}
	}
	return nil
}

func normalizeScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		trimmed := strings.TrimSpace(scope)
		if trimmed == "" {
			continue
		}
		normalized = append(normalized, trimmed)
	}
	slices.Sort(normalized)
	normalized = slices.Compact(normalized)
	return normalized
}

func claimString(claims map[string]any, key string) (string, bool) {
	if claims == nil {
		return "", false
	}
	raw, ok := claims[key]
	if !ok || raw == nil {
		return "", false
	}
	str, ok := raw.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(str)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// RequireScope checks if the token has the required scope.
// Returns the actor identifier if authorized, or ErrUnauthorized if not.
func RequireScope(ctx context.Context, scope string) (string, error) {
	token := TokenInfoFromContext(ctx)
	if token == nil {
		return "", common.ErrUnauthorized
	}

	if slices.Contains(token.Scopes, scope) {
		return ActorFromToken(token), nil
	}

	return "", common.ErrUnauthorized
}

// ActorFromToken extracts the actor identifier from a TokenInfo.
func ActorFromToken(token *TokenInfo) string {
	if token == nil {
		return "mcp"
	}

	identity := buildIdentityFromTokenInfo(token)
	if strings.TrimSpace(identity.Email) != "" {
		return identity.Email
	}
	if strings.TrimSpace(identity.Subject) != "" {
		return identity.Subject
	}

	return "mcp"
}

// ActorFromContext extracts the authenticated actor identifier from the context.
// Returns an empty string when no authenticated actor is attached.
// When delegation context is present, produces a compound identifier:
//
//	agent@email[delegation:ID,on-behalf-of:human@email]
//
// For synthetic identities with a guardian:
//
//	agent@email[delegation:ID,on-behalf-of:aria@co.com,guardian:alice@co.com]
func ActorFromContext(ctx context.Context) string {
	token := TokenInfoFromContext(ctx)
	if token == nil {
		return ""
	}

	actor := strings.TrimSpace(ActorFromToken(token))

	if dc, ok := DelegationFromContext(ctx); ok && dc != nil {
		delegatingEmail := strings.TrimSpace(dc.DelegatingIdentity.Email)
		if delegatingEmail == "" {
			delegatingEmail = dc.DelegatingIdentity.Subject
		}

		suffix := "delegation:" + dc.DelegationId + ",on-behalf-of:" + delegatingEmail

		if dc.GuardianSubject != "" {
			suffix += ",guardian:" + dc.GuardianSubject
		}

		return actor + "[" + suffix + "]"
	}

	return actor
}

// buildIdentityFromTokenInfo creates a minimal identity for actor extraction.
func buildIdentityFromTokenInfo(token *TokenInfo) UserIdentity {
	id := UserIdentity{
		Subject: strings.TrimSpace(token.Subject),
	}

	id.Email = firstNonEmptyFromClaims(token.Claims,
		"email",
		"preferred_username",
		"user",
	)
	if id.Email == "" {
		id.Email = id.Subject
	}

	return id
}

func firstNonEmptyFromClaims(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := claimString(claims, key); ok && v != "" {
			return v
		}
	}
	return ""
}
