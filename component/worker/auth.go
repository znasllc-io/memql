package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// AuthClaimKey is the claims-map key under which the worker stream
// interceptor stows the authenticated worker identity. The server's
// Stream RPC reads it via identityFromContext below.
const AuthClaimKey = "worker.identity"

// ContextWithWorkerIdentity returns a new context carrying the
// supplied worker identity inside the auth claim map. Used by the
// gRPC stream interceptor when it admits a mql_wkr_<token>.
func ContextWithWorkerIdentity(ctx context.Context, ident *WorkerIdentity) context.Context {
	if ctx == nil || ident == nil {
		return ctx
	}
	subject := "worker:" + ident.IdentityId
	claims := map[string]any{
		"sub":         subject,
		"role":        "worker",
		AuthClaimKey:  identityToClaims(ident),
		"ownerUserId": ident.OwnerUserId,
	}
	tok := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, tok)
	return ctx
}

// identityFromContext is the inverse: the server reads the claims
// map and reconstitutes the WorkerIdentity payload.
func identityFromContext(ctx context.Context) (*WorkerIdentity, error) {
	if ctx == nil {
		return nil, errors.New("worker: missing context")
	}
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return nil, errors.New("worker: claims not available")
	}
	raw, ok := claims[AuthClaimKey]
	if !ok {
		return nil, errors.New("worker: identity claim missing")
	}
	asMap, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("worker: identity claim malformed")
	}
	ident := &WorkerIdentity{}
	if v, ok := asMap["identityId"].(string); ok {
		ident.IdentityId = v
	}
	if v, ok := asMap["ownerUserId"].(string); ok {
		ident.OwnerUserId = v
	}
	if v, ok := asMap["active"].(bool); ok {
		ident.Active = v
	}
	if ident.IdentityId == "" || ident.OwnerUserId == "" {
		return nil, fmt.Errorf("worker: identity claim missing required fields")
	}
	return ident, nil
}

func identityToClaims(ident *WorkerIdentity) map[string]any {
	if ident == nil {
		return nil
	}
	out := map[string]any{
		"identityId":  ident.IdentityId,
		"ownerUserId": ident.OwnerUserId,
		"active":      ident.Active,
	}
	if !ident.ExpiresAt.IsZero() {
		out["expiresAt"] = ident.ExpiresAt.Unix()
	}
	return out
}

// IsWorkerSubject reports whether a context carries a worker-scoped
// authentication. Used by the auth-side path-check that rejects
// worker tokens on non-worker RPCs.
func IsWorkerSubject(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return false
	}
	if _, has := claims[AuthClaimKey]; has {
		return true
	}
	if sub, ok := claims["sub"].(string); ok && strings.HasPrefix(sub, "worker:") {
		return true
	}
	return false
}
