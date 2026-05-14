package auth

import "context"

// DelegationResolver looks up active delegations for an authenticated
// subject. It is called by auth middleware after token validation to
// inject DelegationContext into the request context when applicable.
//
// The implementation is provided by the identity integration and
// injected into the auth middleware at bootstrap time.
type DelegationResolver interface {
	// ResolveActiveDelegation returns the active delegation for the given
	// subject, or nil if no active delegation exists. The subject
	// typically comes from the validated JWT "sub" claim.
	ResolveActiveDelegation(ctx context.Context, subject string) (*DelegationContext, error)
}
