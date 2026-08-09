package webauthn

// Passkey MANAGEMENT operations (memql#3409): rename, revoke, and the
// sign-in-route count that stands between a revoke and a lockout.
//
// Split from store.go on purpose. store.go is what the CEREMONIES need
// -- create a row, resolve one by credential id, list a user's
// authenticators for the exclusion set -- and every one of those runs
// with an attested credential in hand. Nothing here has that: these are
// driven by a form post on /me/devices, so the authorization is the
// caller's session and the resolution of "which row" has to happen
// before, not during.
//
// That split is the reason for the ONE rule this file repeats: the
// caller resolves the target out of ListForSelf (userId==actor.userId)
// and passes an id it has already seen there. Neither Rename nor Revoke
// re-checks ownership, and they deliberately cannot -- both write under
// the system credential actor, which is precisely the actor for whom
// `actor.userId` names nobody.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"google.golang.org/protobuf/types/known/structpb"
)

// magicLinkIdentityType / passkeyIdentityType mirror the identityType
// enum values on v1:identity:identity. Only the two that are SIGN-IN
// routes appear here -- see signInIdentitiesForSelf in
// dsl/identity/queries.memql for why the other eight variants are not.
const (
	magicLinkIdentityType = "magic_link"
	passkeyIdentityType   = "passkey"
)

// SignInRoute is one active way the caller could get back into their
// account: a magic-link credential or a passkey.
//
// It is deliberately NOT a Row. A Row carries a passkey's credential
// material and backup posture; a route carries only enough to count the
// set and to name what is left in a sentence a user can act on.
type SignInRoute struct {
	ID           string
	UserId       string
	IdentityType string
	Label        string
	Active       bool
}

// IsPasskey / IsMagicLink classify a route without every caller
// re-spelling the enum value.
func (r SignInRoute) IsPasskey() bool   { return r.IdentityType == passkeyIdentityType }
func (r SignInRoute) IsMagicLink() bool { return r.IdentityType == magicLinkIdentityType }

// Rename changes a passkey's display label.
//
// OWNERSHIP IS THE CALLER'S OBLIGATION, and it is not checkable here:
// the write runs under the system credential actor (a passkey row is in
// the memql#2513 machine-credential set, so a user actor's write is
// rejected outright), and under that actor `actor.userId` resolves to no
// person at all. Resolve the target from ListForSelf first -- that read
// IS the ownership check, and it is structural rather than a comparison
// somebody can forget to write.
func (s *Store) Rename(ctx context.Context, identityId, label string) error {
	if s == nil || s.Engine == nil {
		return errors.New("webauthn.Store: engine not wired")
	}
	identityId = strings.TrimSpace(identityId)
	label = strings.TrimSpace(label)
	if identityId == "" || label == "" {
		return errors.New("webauthn.Store.Rename: identityId and label are both required")
	}
	q := fmt.Sprintf(`mutation renamePasskeyIdentity(identityId:%s,label:%s)`,
		dslString(bareSlug(identityId)), dslString(label))
	if _, err := s.Engine.Execute(identity.ContextWithSystemCredentialActor(ctx), q); err != nil {
		return fmt.Errorf("webauthn.Store.Rename: %w", err)
	}
	return nil
}

// Revoke deactivates a passkey by flipping active=false.
//
// SOFT, and the softness is load-bearing rather than a convention
// inherited from the sibling credential stores. Revoking a row does not
// make the authenticator forget its private key, so the credential id
// must stay taken: register/finish refuses an id that already exists
// ANYWHERE, revoked rows included, and a hard delete would free the id
// for a second enrolment that quietly re-points a still-live
// authenticator at a fresh row.
//
// Same ownership obligation as Rename, for the same reason.
//
// What this function does NOT do is decide whether the revoke is safe.
// The last-credential question is answered one layer up, where there is
// a user to warn; a store method that refused on its own would leave the
// caller unable to say what the user would be left with.
func (s *Store) Revoke(ctx context.Context, identityId string) error {
	if s == nil || s.Engine == nil {
		return errors.New("webauthn.Store: engine not wired")
	}
	identityId = strings.TrimSpace(identityId)
	if identityId == "" {
		return errors.New("webauthn.Store.Revoke: identityId is required")
	}
	q := fmt.Sprintf(`mutation revokePasskeyIdentity(identityId:%s)`, dslString(bareSlug(identityId)))
	if _, err := s.Engine.Execute(identity.ContextWithSystemCredentialActor(ctx), q); err != nil {
		return fmt.Errorf("webauthn.Store.Revoke: %w", err)
	}
	return nil
}

// ListSignInRoutesForSelf returns every ACTIVE credential the caller
// could sign in with -- magic-link and passkey, and nothing else.
//
// SELF-SCOPED with no userId parameter, like ListForSelf: ctx MUST carry
// an AccessContext for the caller or `actor.userId` resolves to "" and
// the query yields zero rows. A zero count is exactly the input that
// makes the caller warn about a lockout, so an unstamped context here
// would produce a warning that fires for everybody -- which is the
// failure mode that teaches users to click through warnings. Callers
// treat a query ERROR and an empty result differently for the same
// reason.
func (s *Store) ListSignInRoutesForSelf(ctx context.Context) ([]SignInRoute, error) {
	if s == nil || s.Engine == nil {
		return nil, errors.New("webauthn.Store: engine not wired")
	}
	const q = `query signInIdentitiesForSelf()`
	out := []SignInRoute{}
	cursor := ""
	for i := 0; i < maxPageWalk; i++ {
		nodes, next, err := s.executeAndExtractPage(ctx, q, cursor)
		if err != nil {
			return nil, fmt.Errorf("webauthn.Store.ListSignInRoutesForSelf: %w", err)
		}
		for _, n := range nodes {
			if r := signInRouteFromNode(n); r != nil {
				out = append(out, *r)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// signInRouteFromNode projects an identitySummary node into a route.
// Rows whose active flag is false are dropped rather than carried with
// Active=false: the query already filters on isActiveRecord, so a false
// here would mean the projection disagreed with the filter, and counting
// it as a route would be the one mistake that matters.
func signInRouteFromNode(n *memqlv1.MemoryNode) *SignInRoute {
	if n == nil || n.Payload == nil {
		return nil
	}
	fields := n.Payload.GetFields()
	if fields == nil {
		return nil
	}
	str := func(k string) string {
		v, ok := fields[k]
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(v.GetStringValue())
	}
	active := true
	if v, ok := fields["active"]; ok && v != nil {
		if _, isBool := v.GetKind().(*structpb.Value_BoolValue); isBool {
			active = v.GetBoolValue()
		} else if sv := strings.TrimSpace(v.GetStringValue()); sv != "" {
			active = strings.EqualFold(sv, "true")
		}
	}
	if !active {
		return nil
	}
	identityType := strings.TrimSpace(str("identityType"))
	if identityType != magicLinkIdentityType && identityType != passkeyIdentityType {
		return nil
	}
	return &SignInRoute{
		ID:           firstNonEmpty(str("id"), n.GetId()),
		UserId:       str("userId"),
		IdentityType: identityType,
		Label:        str("label"),
		Active:       true,
	}
}
