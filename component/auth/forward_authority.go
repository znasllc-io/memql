package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The mesh forwarded-auth contract (memql#3205, carrying memql#2876 and
// absorbing memql#2814's "design it once for all forwards").
//
// # What this buys, and what it does not
//
// The mesh wire is ALREADY peer-authenticated: only a class="node" JWT may open
// NodeService.Stream. So this contract is not authentication of the producer,
// and it cannot defend against a fully compromised mesh node -- such a node can
// assert anything. Overclaiming here is how the previous attempt got its threat
// model wrong. What it does buy:
//
//  1. COMPLETENESS. A producer that forgets to establish authority fails loudly
//     instead of silently escalating. "No badge" is a VALUE, never a missing key.
//  2. INTEGRITY. The receiver re-runs the exact clamp the producer claims to
//     have run (RoleAtMost) and refuses on disagreement, so a producer bug that
//     skips the clamp is caught rather than trusted.
//  3. INDEPENDENT EXPIRY. The receiver enforces `exp` itself, so an expired
//     badge cannot be honoured because a producer's session state went stale.
//  4. NO SECOND, WEAKER RESOLUTION PATH. FallbackFromClaims -- which lifts
//     `role` straight off a map -- becomes unreachable from wire-supplied input
//     on the receiver. It remains reachable on the PRODUCER, where its input is
//     the verifier's own output.
//  5. NO WIRE-DRIVEN @serverOnly EXECUTION. The receiver never calls
//     LoadFromClaims, so `userByIdSystem` is never run under an internal-origin
//     stamp keyed by a wire-supplied subject.
//
// # Why the assertion, and not the claims map
//
// The claims map is still carried, and still has a job: ActorFromContext reads
// TokenInfo for `createdBy` attribution, and a mutation with no actor hard-fails.
// But it is NEVER a source of authorization any more. The two carriers are
// derived from one value (ForwardedAuthority.Principal) and cross-checked on
// receipt, so they cannot drift.

// ForwardedAuthorityVersion is the contract version every producer stamps and
// every receiver requires. A receiver refuses anything else rather than
// guessing at a producer it does not implement.
const ForwardedAuthorityVersion = "v1"

// ForwardedPrincipalKind distinguishes a forward made on behalf of an end user
// from one a node makes for itself. The zero value is deliberately invalid: an
// unset field must be loud.
type ForwardedPrincipalKind uint8

const (
	ForwardedPrincipalUnspecified ForwardedPrincipalKind = iota
	ForwardedPrincipalUser
	ForwardedPrincipalSystem
)

func (k ForwardedPrincipalKind) String() string {
	switch k {
	case ForwardedPrincipalUser:
		return "user"
	case ForwardedPrincipalSystem:
		return "system"
	default:
		return "unspecified"
	}
}

// The credential classes a forward may assert. Closed on purpose: an unknown
// class is REFUSED rather than treated as an ordinary user session, so adding a
// credential type is a loud change instead of one that silently lands in the
// most permissive bucket.
//
// These mirror the identity service's class claim where one exists, plus the
// three synthetic principals that have no JWT class of their own: the no-auth
// dev shim, the operator stream, and a node acting for itself.
const (
	ForwardedClassUser           = "user"
	ForwardedClassBadge          = "badge"
	ForwardedClassPat            = "pat"
	ForwardedClassServiceAccount = "service_account"
	ForwardedClassVoiceAgent     = "voice_agent"
	ForwardedClassWorkerToken    = "worker_token"
	ForwardedClassNode           = "node"
	ForwardedClassGuest          = "guest"
	ForwardedClassLocalDev       = "local_dev"
	ForwardedClassOperator       = "operator"
	ForwardedClassSystem         = "system"
)

func knownForwardedClass(class string) bool {
	switch class {
	case ForwardedClassUser, ForwardedClassBadge, ForwardedClassPat,
		ForwardedClassServiceAccount, ForwardedClassVoiceAgent,
		ForwardedClassWorkerToken, ForwardedClassNode, ForwardedClassGuest,
		ForwardedClassLocalDev, ForwardedClassOperator, ForwardedClassSystem:
		return true
	default:
		return false
	}
}

// canonicalUserIdPrefix is the shape a real user id takes. A SYSTEM principal
// must not wear one: downstream owner resolution keys on exactly this prefix,
// so a system assertion carrying a user id would be adopted as that user's
// owner id.
const canonicalUserIdPrefix = "v1:identity:user:"

// ForwardedAuthority is the mandatory authorization assertion on every mesh
// forward. It carries the producer's ALREADY-RESOLVED, ALREADY-CLAMPED decision
// -- not the inputs to one. The receiver verifies and binds it verbatim.
type ForwardedAuthority struct {
	Version string
	Kind    ForwardedPrincipalKind

	// The resolved AccessContext, field for field.
	Subject      string
	PrimaryEmail string
	Role         Role
	// IdentityId mirrors AccessContext.IdentityId. Neither LoadFromClaims nor
	// FallbackFromClaims populates that field today, so this is always empty in
	// practice -- carried rather than dropped so the mesh keeps parity with the
	// direct path if it ever is populated, and so its emptiness is a fact about
	// the resolver rather than about this contract.
	IdentityId string

	// CredentialClass is never empty on a valid assertion. This is the field
	// that makes an unstated ceiling DETECTABLE: "not a badge session" is the
	// value ForwardedClassUser, not the absence of a key.
	CredentialClass string

	// RoleCeiling is non-empty IFF CredentialClass is badge. ExpiresAt is
	// non-zero for any class that expires mid-stream (today: badge only).
	RoleCeiling Role
	ExpiresAt   time.Time

	// Audit only -- never an input to a decision.
	OriginNodeId   string
	OriginNodeType string
	AssertedAt     time.Time
}

// Refusal sentinels. One per rule, so a log line, a metric label and a test
// assertion all name the same failure.
var (
	ErrForwardMissingAuthority       = errors.New("forwarded authority is absent")
	ErrForwardUnsupportedContract    = errors.New("forwarded authority contract version is not supported")
	ErrForwardMissingPrincipalKind   = errors.New("forwarded authority has no principal kind")
	ErrForwardMissingSubject         = errors.New("forwarded authority has no subject")
	ErrForwardMissingClass           = errors.New("forwarded authority has no credential class")
	ErrForwardUnknownClass           = errors.New("forwarded authority credential class is not recognised")
	ErrForwardInvalidRole            = errors.New("forwarded authority role is not a valid role")
	ErrForwardSystemRoleTooHigh      = errors.New("forwarded system principal asserts more than reader")
	ErrForwardSystemImpersonatesUser = errors.New("forwarded system principal names a canonical user id")
	ErrForwardUserClaimsSystemClass  = errors.New("forwarded user principal asserts the system credential class")
	ErrForwardBadgeMissingCeiling    = errors.New("forwarded badge authority has no role ceiling")
	ErrForwardBadgeMissingExpiry     = errors.New("forwarded badge authority has no expiry")
	ErrForwardCeilingNotApplied      = errors.New("forwarded authority role exceeds its own ceiling")
	ErrForwardStrayCeiling           = errors.New("forwarded authority carries a ceiling its class will not enforce")
	ErrForwardAuthorityExpired       = errors.New("forwarded authority has expired")
)

// VerifyForwardedAuthority is the single gateway from the wire to an
// AccessContext. Every rule fails closed, and nothing binds unless all of them
// pass.
//
// It performs NO database access, NO claims resolution, and NO fallback. That
// is the point: the receiver's only authorization input is the assertion, so
// there is exactly one path and it is checkable.
func VerifyForwardedAuthority(a ForwardedAuthority, now time.Time) (*AccessContext, error) {
	if a.Version != ForwardedAuthorityVersion {
		return nil, fmt.Errorf("%w: %q", ErrForwardUnsupportedContract, a.Version)
	}
	if a.Kind == ForwardedPrincipalUnspecified {
		return nil, ErrForwardMissingPrincipalKind
	}
	if strings.TrimSpace(a.Subject) == "" {
		return nil, ErrForwardMissingSubject
	}
	if a.CredentialClass == "" {
		return nil, ErrForwardMissingClass
	}
	if !knownForwardedClass(a.CredentialClass) {
		return nil, fmt.Errorf("%w: %q", ErrForwardUnknownClass, a.CredentialClass)
	}
	if !IsValidRole(a.Role) {
		return nil, fmt.Errorf("%w: %q", ErrForwardInvalidRole, a.Role)
	}

	switch a.Kind {
	case ForwardedPrincipalSystem:
		if a.CredentialClass != ForwardedClassSystem {
			return nil, fmt.Errorf("%w: %q", ErrForwardUnknownClass, a.CredentialClass)
		}
		// Reader is the no-widening choice. These hops send the invalid string
		// "system" today, which IsValidRole rejects and FallbackFromClaims
		// clamps to reader -- and RoleLevel ranks writer(2) ABOVE reader(3),
		// lower being more privileged, so anything else would GRANT the mesh's
		// system hops more than they have ever had.
		if a.Role != RoleReader {
			return nil, fmt.Errorf("%w: %q", ErrForwardSystemRoleTooHigh, a.Role)
		}
		if strings.HasPrefix(strings.TrimSpace(a.Subject), canonicalUserIdPrefix) {
			return nil, fmt.Errorf("%w: %q", ErrForwardSystemImpersonatesUser, a.Subject)
		}
	case ForwardedPrincipalUser:
		if a.CredentialClass == ForwardedClassSystem {
			return nil, ErrForwardUserClaimsSystemClass
		}
	}

	if a.CredentialClass == ForwardedClassBadge {
		if !IsValidRole(a.RoleCeiling) {
			return nil, fmt.Errorf("%w: %q", ErrForwardBadgeMissingCeiling, a.RoleCeiling)
		}
		// THE PROOF OF CLAMP. RoleAtMost is the exact function the producer's
		// clamp went through (applyBadgeRoleCeiling), so re-running it here is a
		// true check that it was applied rather than a restatement of the claim.
		// It inherits that function's coarseness by construction: RoleLevel maps
		// admin and developer to one level, so an admin under a developer
		// ceiling passes -- identical to the direct path, deliberately.
		if RoleAtMost(a.Role, a.RoleCeiling) != a.Role {
			return nil, fmt.Errorf("%w: role %q exceeds ceiling %q", ErrForwardCeilingNotApplied, a.Role, a.RoleCeiling)
		}
		if a.ExpiresAt.IsZero() {
			return nil, ErrForwardBadgeMissingExpiry
		}
	} else if a.RoleCeiling != "" {
		// A ceiling nothing will enforce must not be present, or an assertion
		// can LOOK clamped while its class says the ceiling is never checked.
		return nil, fmt.Errorf("%w: class %q ceiling %q", ErrForwardStrayCeiling, a.CredentialClass, a.RoleCeiling)
	}

	// Expiry, for badge and any future expiring class alike. Same comparison as
	// the direct path's badgeGate, so the two agree at the same instant.
	if !a.ExpiresAt.IsZero() && !now.Before(a.ExpiresAt) {
		return nil, fmt.Errorf("%w at %s", ErrForwardAuthorityExpired, a.ExpiresAt.UTC().Format(time.RFC3339))
	}

	return &AccessContext{
		UserId:       a.Subject,
		PrimaryEmail: a.PrimaryEmail,
		Role:         a.Role,
		IdentityId:   a.IdentityId,
	}, nil
}

// ForwardedPrincipal is the ONE value a producer hands to the forwarder. Claims
// are DERIVED from the authority rather than supplied beside it, which is what
// makes "claims without an authority" inexpressible at the call site.
type ForwardedPrincipal struct {
	Authority ForwardedAuthority
	Claims    map[string]string
}

// Principal derives the wire pair. The claims map exists only for `createdBy`
// attribution (ActorFromContext -> TokenInfo); it is not an authorization input
// on the receiver.
func (a ForwardedAuthority) Principal() ForwardedPrincipal {
	claims := map[string]string{
		forwardedClaimSubject: a.Subject,
		forwardedClaimRole:    string(a.Role),
	}
	if a.PrimaryEmail != "" {
		claims[forwardedClaimEmail] = a.PrimaryEmail
	}
	if a.CredentialClass == ForwardedClassLocalDev {
		claims[forwardedClaimLocalDev] = "1"
	}
	return ForwardedPrincipal{Authority: a, Claims: claims}
}

// ForwardedAuthorityForUser projects a RESOLVED session decision onto the wire.
//
// `ac` MUST be the session's POST-ROTATE AccessContext, and class / ceiling /
// expiresAt MUST come from the session's post-rotate credential stamp -- never
// from the gRPC stream context. A stream context is fixed at stream-open while
// badge grants arrive MID-STREAM via RotateAuth, which cannot mutate it. Sourced
// from there, a mid-stream badge session forwards no class and no ceiling, the
// receiver cannot distinguish that from "not a badge session", and the
// operator's UNCLAMPED stored role is resolved. That is memql#2876's measured
// escalation, and the reason this function takes the pieces separately rather
// than reading a context itself.
func ForwardedAuthorityForUser(ac *AccessContext, class string, ceiling Role, expiresAt time.Time, now time.Time) (ForwardedAuthority, error) {
	if ac == nil {
		return ForwardedAuthority{}, fmt.Errorf("forwarded authority: no resolved access context for this session")
	}
	if strings.TrimSpace(ac.UserId) == "" {
		return ForwardedAuthority{}, fmt.Errorf("forwarded authority: resolved access context has no subject")
	}
	if class == "" {
		class = ForwardedClassUser
	}
	if !knownForwardedClass(class) {
		return ForwardedAuthority{}, fmt.Errorf("forwarded authority: unknown credential class %q", class)
	}
	if class != ForwardedClassBadge {
		// Never ship a ceiling the receiver will refuse as stray.
		ceiling = ""
		expiresAt = time.Time{}
	}
	role := ac.Role
	if !IsValidRole(role) {
		role = RoleReader
	}
	return ForwardedAuthority{
		Version:         ForwardedAuthorityVersion,
		Kind:            ForwardedPrincipalUser,
		Subject:         ac.UserId,
		PrimaryEmail:    ac.PrimaryEmail,
		Role:            role,
		IdentityId:      ac.IdentityId,
		CredentialClass: class,
		RoleCeiling:     ceiling,
		ExpiresAt:       expiresAt,
		AssertedAt:      now,
	}, nil
}

// ForwardedAuthorityForSystem is the ONLY way to forward without an end-user
// principal. It replaces every nil claims map in the tree.
//
// Role is fixed at reader and the subject may not look like a canonical user id
// -- see the SYSTEM rules in VerifyForwardedAuthority for why both are the
// no-widening choice.
func ForwardedAuthorityForSystem(subject string, now time.Time) (ForwardedAuthority, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return ForwardedAuthority{}, fmt.Errorf("forwarded authority: a system principal needs a subject")
	}
	if strings.HasPrefix(subject, canonicalUserIdPrefix) {
		return ForwardedAuthority{}, fmt.Errorf("forwarded authority: a system principal may not name the user id %q", subject)
	}
	return ForwardedAuthority{
		Version:         ForwardedAuthorityVersion,
		Kind:            ForwardedPrincipalSystem,
		Subject:         subject,
		Role:            RoleReader,
		CredentialClass: ForwardedClassSystem,
		AssertedAt:      now,
	}, nil
}
