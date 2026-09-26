package auth

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type executionAuthorityKey struct{}
type executionAuthorityStamp struct {
	authority ForwardedAuthority
	err       error
}

// ContextWithExecutionAuthority carries the stream's current, post-rotation
// credential stamp to durable intake. A refusal is preserved for that intake;
// ordinary reads do not acquire an unrelated forwarding requirement.
func ContextWithExecutionAuthority(ctx context.Context, authority ForwardedAuthority, err error) context.Context {
	return context.WithValue(ctx, executionAuthorityKey{}, executionAuthorityStamp{authority, err})
}

// CaptureExecutionAuthority records only a ceiling, never a bearer credential.
// This value is stamped by intake, outside the user-controlled goal input.
func CaptureExecutionAuthority(ctx context.Context) (map[string]any, error) {
	access, ok := AccessFromContext(ctx)
	if !ok || access == nil || access.Synthetic || access.Unranked || !IsValidRole(access.Role) {
		return nil, fmt.Errorf("work requires a resolved user authority")
	}
	class, expiry := ForwardedClassUser, time.Time{}
	forwarded, present := ForwardedAuthorityFromContext(ctx)
	if stamp, ok := ctx.Value(executionAuthorityKey{}).(executionAuthorityStamp); ok {
		if stamp.err != nil {
			return nil, fmt.Errorf("work credential authority is unavailable: %w", stamp.err)
		}
		forwarded, present = stamp.authority, true
	}
	if present {
		verified, err := VerifyForwardedAuthority(forwarded, time.Now())
		if err != nil || verified.UserId != access.UserId || verified.Role != access.Role {
			return nil, fmt.Errorf("work caller authority does not match its forwarding assertion")
		}
		class, expiry = forwarded.CredentialClass, forwarded.ExpiresAt
	}
	if class != ForwardedClassUser && class != ForwardedClassBadge && class != ForwardedClassPat {
		return nil, fmt.Errorf("work requires a user, personal token or badge authority")
	}
	result := map[string]any{"roleCeiling": string(access.Role), "credentialClass": class}
	if !expiry.IsZero() {
		result["expiresAt"] = expiry.UTC().Format(time.RFC3339Nano)
	}
	return result, nil
}

// ContextWithPersistedOwner restores an authorized journal's owner on a new
// replica. A recorded ceiling can only narrow the CURRENT stored role. Neither
// a maintenance parent nor a model-supplied input lends authority. Restricted
// badge grants keep their expiry and ceiling across the durable hop.
func ContextWithPersistedOwner(ctx context.Context, ownerUserId string, grant map[string]any, resolver *IdentityResolver) (context.Context, error) {
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		return nil, fmt.Errorf("auth: a persisted owner is required to assert forwarded authority")
	}
	// Work without a captured interactive grant (including background
	// responsibilities) has only a writer-scoped assertion. It must never
	// acquire owner authority just because its owner holds that role.
	access := &AccessContext{UserId: owner, Role: RoleWriter}
	class, ceiling, expires := ForwardedClassUser, Role(""), time.Time{}
	if len(grant) != 0 {
		storedCeiling, _ := grant["roleCeiling"].(string)
		class, _ = grant["credentialClass"].(string)
		if !IsValidRole(Role(storedCeiling)) || (class != ForwardedClassUser && class != ForwardedClassBadge && class != ForwardedClassPat) {
			return nil, fmt.Errorf("auth: invalid persisted work authority")
		}
		if class == ForwardedClassBadge {
			value, _ := grant["expiresAt"].(string)
			var err error
			expires, err = time.Parse(time.RFC3339Nano, value)
			if err != nil || !time.Now().Before(expires) {
				return nil, fmt.Errorf("auth: persisted work badge authority expired")
			}
			ceiling = Role(storedCeiling)
		}
		var err error
		access, err = resolver.LoadFromClaims(ctx, map[string]any{"sub": owner})
		if err != nil {
			return nil, fmt.Errorf("auth: cannot resolve work owner: %w", err)
		}
		if !IsValidRole(access.Role) {
			return nil, fmt.Errorf("auth: work owner has no valid role")
		}
		access.Role = RoleAtMost(access.Role, Role(storedCeiling))
	}
	now := time.Now()
	authority, err := ForwardedAuthorityForUser(access, class, ceiling, expires, now)
	if err != nil {
		return nil, err
	}
	verified, err := VerifyForwardedAuthority(authority, now)
	if err != nil {
		return nil, err
	}
	return BindForwardedContext(ctx, authority.Principal().Claims, verified, authority), nil
}

// QueryRunnerFunc adapts an engine's shaped result without introducing a
// dependency from auth back onto the engine.
type QueryRunnerFunc func(context.Context, string) (any, error)

func (f QueryRunnerFunc) ExecuteShaped(ctx context.Context, query string) (any, error) {
	return f(ctx, query)
}
