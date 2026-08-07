package node

import (
	"time"

	"github.com/znasllc-io/memql/component/auth"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// forwarded_authority.go -- the ONLY adaptation between the wire message and
// component/auth's plain-Go ForwardedAuthority.
//
// component/auth cannot import component/node (node -> identity/verifier ->
// auth already exists), so the mapping lives on this side. It sits in
// component/node rather than in component/grpc because there are now TWO
// forwards carrying the assertion -- the AI forward (component/grpc) and the
// workbench forward (integrations/workbench) -- and both import this package
// while neither imports the other.
//
// One mapping, not two (memql#3219). A second copy is how the field set drifts:
// the copy that forgets to carry role_ceiling produces an assertion that looks
// clamped and is not, and the receiver's proof-of-clamp is only as good as the
// producer's completeness.

// ForwardedAuthorityToProto projects the plain-Go assertion onto the wire,
// stamping the audit-only origin fields from the sending node.
func ForwardedAuthorityToProto(a auth.ForwardedAuthority, originNodeId, originNodeType string) *nodev1.ForwardedAuthority {
	out := &nodev1.ForwardedAuthority{
		ContractVersion: a.Version,
		Subject:         a.Subject,
		PrimaryEmail:    a.PrimaryEmail,
		Role:            string(a.Role),
		IdentityId:      a.IdentityId,
		CredentialClass: a.CredentialClass,
		RoleCeiling:     string(a.RoleCeiling),
		OriginNodeId:    originNodeId,
		OriginNodeType:  originNodeType,
		// Provenance only (memql#3221) -- see the field comments on
		// auth.ForwardedAuthority. Carried so identity.displayName lands the
		// same on a row this hop writes as on one the user writes directly.
		FirstName: a.FirstName,
		LastName:  a.LastName,
	}
	switch a.Kind {
	case auth.ForwardedPrincipalUser:
		out.PrincipalKind = nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_USER
	case auth.ForwardedPrincipalSystem:
		out.PrincipalKind = nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_SYSTEM
	default:
		out.PrincipalKind = nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_UNSPECIFIED
	}
	if !a.ExpiresAt.IsZero() {
		out.ExpiresAtUnix = a.ExpiresAt.Unix()
	}
	if !a.AssertedAt.IsZero() {
		out.AssertedAtUnix = a.AssertedAt.Unix()
	}
	return out
}

// ForwardedAuthorityFromProto reads the wire assertion back. A nil message
// yields the zero value, which auth.VerifyForwardedAuthority refuses: an ABSENT
// assertion and a malformed one take the same path deliberately, because the
// contract's premise is that absence is never read as safe.
func ForwardedAuthorityFromProto(p *nodev1.ForwardedAuthority) auth.ForwardedAuthority {
	if p == nil {
		return auth.ForwardedAuthority{}
	}
	a := auth.ForwardedAuthority{
		Version:         p.GetContractVersion(),
		Subject:         p.GetSubject(),
		PrimaryEmail:    p.GetPrimaryEmail(),
		Role:            auth.Role(p.GetRole()),
		IdentityId:      p.GetIdentityId(),
		CredentialClass: p.GetCredentialClass(),
		RoleCeiling:     auth.Role(p.GetRoleCeiling()),
		OriginNodeId:    p.GetOriginNodeId(),
		OriginNodeType:  p.GetOriginNodeType(),
		// Provenance only. Read back so the worker's metadata collector can
		// stamp identity.displayName; VerifyForwardedAuthority never looks at
		// them and the AccessContext it returns has nowhere to put them.
		FirstName: p.GetFirstName(),
		LastName:  p.GetLastName(),
	}
	switch p.GetPrincipalKind() {
	case nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_USER:
		a.Kind = auth.ForwardedPrincipalUser
	case nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_SYSTEM:
		a.Kind = auth.ForwardedPrincipalSystem
	}
	if v := p.GetExpiresAtUnix(); v > 0 {
		a.ExpiresAt = time.Unix(v, 0)
	}
	if v := p.GetAssertedAtUnix(); v > 0 {
		a.AssertedAt = time.Unix(v, 0)
	}
	return a
}
