package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The mesh forwarded-auth contract (memql#3205).
//
// The use case: a BFF receives a request from a client, authenticates it
// (identity-issued JWT, or the no-auth dev shim), and then forwards some part
// of the work to a worker node (Voice / Agent / Cognition). The worker needs an
// actor, or every actor-gated construct it executes silently returns zero rows
// and every row it writes is stamped createdBy:"".
//
// # What crosses the hop: the DECISION, not the inputs to it
//
// The sender has already resolved the caller -- ensureAccess loaded the user
// row, applyBadgeRoleCeiling clamped the role, handleRotateAuth absorbed
// whatever arrived mid-stream. ForwardedAuthority carries THAT ANSWER. The
// receiver re-derives nothing, so there is nothing for it to get wrong.
//
// This inverts the predecessor, which shipped a map<string,string> of raw JWT
// claims and had the worker rebuild the decision from them. It carried the
// principal but not `class` / `role_ceiling`, so a forwarded badge session
// resolved its UNCLAMPED stored role. Measured, operator stored role `owner`,
// terminal ceiling `reader`:
//
//	DIRECT     role="reader" isClusterOwner=false
//	FORWARDED  role="owner"  isClusterOwner=true
//
// Carrying those two claims OPTIONALLY does not fix it: an absent `class` is
// indistinguishable from "not a badge session", so the receiver cannot detect
// the loss. Only a mandatory, explicit assertion can -- which is why `kind`
// below has no zero value the receiver will accept.
//
// The full rationale, the four kinds, and the receiver rules:
// docs/internal/design/mesh-forwarded-auth-contract.md

// AuthorityKind names what the sender is asserting about the principal behind
// a forwarded request. It is mandatory on every forward: "no badge" is a VALUE,
// never a missing key.
type AuthorityKind string

const (
	// AuthorityKindUser is an ordinary end-user principal.
	AuthorityKindUser AuthorityKind = "user"

	// AuthorityKindBadge is a shared-terminal operator grant (memql#2513).
	// Role is already clamped to the terminal's ceiling; BadgeExpires is
	// mandatory and the receiver refuses the request once it has passed.
	AuthorityKindBadge AuthorityKind = "badge"

	// AuthorityKindSystem is a named service principal for work with no end
	// user behind it (greet-on-join, post-approval Plan dispatch). The
	// receiver pins the ROLE and validates the SUBJECT against its own
	// allowlist, so the sender chooses among known system actors but cannot
	// invent one and cannot elevate.
	//
	// This path genuinely needs an actor -- memql#1107 is the bug where it had
	// none and the agent's taskstamp Stamper failed with "no actor found in
	// context".
	AuthorityKindSystem AuthorityKind = "system"

	// AuthorityKindInternal carries no principal at all. The receiver binds
	// NO actor, and any actor-gated construct reached on this path fails
	// closed exactly as it does today. Used by the forwards that legitimately
	// have no caller: the planner's AgentPreemptTurn (flips a pause flag keyed
	// by request id) and cognition's client-tool relay (ClientToolResult,
	// resolved against a waiter keyed by call id). Neither persists anything.
	AuthorityKindInternal AuthorityKind = "internal"
)

// The system actors permitted to cross a mesh forward.
//
// A system forward names WHICH actor it is, but only from this closed set. The
// distinction is load-bearing for audit: the planner and cognition deliberately
// carry different ids so a stamped row can be attributed to the integration
// that produced it. Collapsing them into a single pinned constant would keep
// the safety property and destroy that, so the contract CONSTRAINS the subject
// rather than fixing it.
//
// The set lives here, on the RECEIVING side, precisely so the wire cannot
// extend it. The values match the long-standing per-integration constants, so
// rows stamped by system-initiated turns keep the same createdBy.
const (
	SystemActorCognition = "system:cognition-integration"
	SystemActorPlanner   = "system:planner-integration"
)

var systemActorAllowlist = map[string]struct{}{
	SystemActorCognition: {},
	SystemActorPlanner:   {},
}

// IsKnownSystemActor reports whether id names a system actor the receiver will
// accept on a forward.
func IsKnownSystemActor(id string) bool {
	_, ok := systemActorAllowlist[id]
	return ok
}

// SystemActorRole is the role the receiver pins for AuthorityKindSystem. The
// sender never names it.
//
// Reader, deliberately, and not a widening: the predecessor sent role:"system"
// on the wire, "system" is not in IsValidRole, and FallbackFromClaims clamps an
// unrecognised role to RoleReader. So reader is what this path already resolved
// to -- pinning it preserves the behaviour while removing the sender's ability
// to assert a role at all.
const SystemActorRole = RoleReader

// ErrAuthorityMissing is returned when a forwarded request carries no
// authority. This is the contract's central refusal: the receiver refuses when
// it cannot PROVE the ceiling was applied, rather than inferring safety from
// absence.
var ErrAuthorityMissing = errors.New("forwarded request carries no authority")

// ForwardedAuthority is the transport-agnostic form of the contract. The proto
// encoding (nodev1.ForwardedAuthority) is converted at the grpc boundary so
// this package stays free of a transport dependency.
type ForwardedAuthority struct {
	// Kind is mandatory. The zero value is not accepted.
	Kind AuthorityKind

	// UserId / PrimaryEmail / Role are the sender's resolved AccessContext.
	// Meaningful for Kind user and badge. For system, UserId selects from the
	// allowlist and Role is ignored; for internal, all three are ignored.
	UserId       string
	PrimaryEmail string

	// Role is POST-ceiling and final. The receiver never re-clamps it and
	// never receives the `class` / `role_ceiling` with which it could try.
	Role Role

	// BadgeExpires is the grant's exp. Required and non-zero for
	// AuthorityKindBadge; the receiver refuses once it has passed. Without it
	// an expired grant would be rejected on the direct stream (badgeGate) and
	// honored on every forwarded AiChat / CallTool.
	BadgeExpires time.Time

	// LocalDev marks a principal synthesised by the no-auth dev shim, so the
	// provenance is visible in worker logs. No authorization meaning.
	LocalDev bool

	// FirstName / LastName are PROVENANCE ONLY -- they feed
	// component/metadata's identity.displayName, which the engine stamps onto
	// every row a mutation writes. They are never an authorization input:
	// Validate ignores them and AccessContext() does not read them.
	//
	// Carried because dropping them silently changed audit metadata -- a row
	// written by a worker on a forwarded turn lost the name the same user's
	// rows carry when written on the direct path.
	FirstName string
	LastName  string
}

// PrincipalAuthority builds the authority for a real caller from the sender's
// already-resolved AccessContext.
//
// badgeExpires is the session's current badge expiry (zero for every non-badge
// credential); a non-zero value selects AuthorityKindBadge, which is what makes
// expiry enforceable on the receiving node.
//
// The AccessContext passed here MUST be the session's post-rotate value
// (streamSession.ensureAccess / currentAccess), never one derived from the
// stream context: a gRPC stream's context is fixed at stream-open while badge
// grants arrive MID-STREAM via RotateAuth, so a context-sourced authority
// forwards the pre-rotation, unclamped role.
func PrincipalAuthority(ac *AccessContext, badgeExpires time.Time, localDev bool) (*ForwardedAuthority, error) {
	if ac == nil || ac.UserId == "" {
		return nil, errors.New("cannot forward a principal authority without a resolved AccessContext")
	}
	kind := AuthorityKindUser
	if !badgeExpires.IsZero() {
		kind = AuthorityKindBadge
	}
	return &ForwardedAuthority{
		Kind:         kind,
		UserId:       ac.UserId,
		PrimaryEmail: ac.PrimaryEmail,
		Role:         ac.Role,
		BadgeExpires: badgeExpires,
		LocalDev:     localDev,
	}, nil
}

// WithDisplayName attaches provenance-only name fields. Separate from
// PrincipalAuthority because an AccessContext does not carry names -- the
// producer reads them off the session identity, which is not an authorization
// source and must not be mistaken for one.
func (a *ForwardedAuthority) WithDisplayName(first, last string) *ForwardedAuthority {
	if a == nil {
		return nil
	}
	a.FirstName = first
	a.LastName = last
	return a
}

// SystemAuthority builds the authority for work with no end user behind it.
// actorId must be one of the allowlisted system actors; the receiver rejects
// anything else, and pins the role regardless of what is sent.
func SystemAuthority(actorId string) *ForwardedAuthority {
	return &ForwardedAuthority{Kind: AuthorityKindSystem, UserId: actorId}
}

// InternalAuthority builds the authority for a forward that needs no actor at
// all. The receiver accepts it and binds none.
func InternalAuthority() *ForwardedAuthority {
	return &ForwardedAuthority{Kind: AuthorityKindInternal}
}

// Validate is the receiver's refusal gate. A non-nil error means the request
// must be refused, not degraded: this contract has no "accept without an
// actor" fallback for a principal-bearing kind, because that is precisely the
// silent-zero-rows failure it replaces.
func (a *ForwardedAuthority) Validate(now time.Time) error {
	if a == nil {
		return ErrAuthorityMissing
	}
	switch a.Kind {
	case AuthorityKindInternal:
		return nil

	case AuthorityKindSystem:
		if !IsKnownSystemActor(a.UserId) {
			// The sender picks among known service principals; it does not
			// get to name one. An unknown id is a refusal rather than a
			// fallback to some default actor, which would let a caller
			// launder work through whichever actor the default happened to
			// be.
			return fmt.Errorf("forwarded system authority names unknown actor %q", a.UserId)
		}
		return nil

	case AuthorityKindUser, AuthorityKindBadge:
		if a.UserId == "" {
			return fmt.Errorf("forwarded authority kind %q carries no user id", a.Kind)
		}
		if !IsValidRole(a.Role) {
			// The sender resolves the role; an unrecognised one means the
			// two sides disagree about the role set. Refuse rather than
			// clamp -- a silent clamp here would hide a real mismatch.
			return fmt.Errorf("forwarded authority carries unrecognised role %q", a.Role)
		}
		if a.Kind == AuthorityKindBadge {
			if a.BadgeExpires.IsZero() {
				// A badge grant whose expiry did not cross cannot be gated
				// on the worker at all. Refuse: an unenforceable ceiling is
				// the exact hole this contract closes.
				return errors.New("forwarded badge authority carries no expiry")
			}
			if !now.Before(a.BadgeExpires) {
				return fmt.Errorf("forwarded badge authority expired at %s",
					a.BadgeExpires.UTC().Format(time.RFC3339))
			}
		}
		return nil

	default:
		return fmt.Errorf("forwarded authority carries unknown kind %q", a.Kind)
	}
}

// AccessContext returns the actor this authority binds, or nil when it binds
// none (AuthorityKindInternal). Call Validate first.
func (a *ForwardedAuthority) AccessContext() *AccessContext {
	if a == nil {
		return nil
	}
	switch a.Kind {
	case AuthorityKindSystem:
		// Subject constrained by the allowlist (checked in Validate); role
		// pinned here, so the wire cannot influence it at all.
		if !IsKnownSystemActor(a.UserId) {
			return nil
		}
		return &AccessContext{
			UserId:       a.UserId,
			PrimaryEmail: a.UserId,
			Role:         SystemActorRole,
		}
	case AuthorityKindUser, AuthorityKindBadge:
		return &AccessContext{
			UserId:       a.UserId,
			PrimaryEmail: a.PrimaryEmail,
			Role:         a.Role,
		}
	default:
		return nil
	}
}

// Identity returns the UserIdentity the receiving session should carry. Empty
// for AuthorityKindInternal.
func (a *ForwardedAuthority) Identity() UserIdentity {
	ac := a.AccessContext()
	if ac == nil {
		return UserIdentity{}
	}
	return UserIdentity{
		Subject:   ac.UserId,
		Email:     ac.PrimaryEmail,
		Role:      string(ac.Role),
		FirstName: a.FirstName,
		LastName:  a.LastName,
	}
}

// synthesizedClaims builds the claims map the receiving context exposes.
//
// These are DERIVED from the decision, never received. Claims-reading
// consumers on the worker (auth.UserFromContext, resolveRoleFromClaims,
// BuildTokenInfo) keep working, while a sender-asserted sub/role pair stays
// unrepresentable -- there is no claims field on the wire to put one in.
//
// `class` and `role_ceiling` are deliberately absent: Role is already final,
// and re-introducing them would hand a downstream applyBadgeRoleCeiling the
// inputs to clamp a second time.
func (a *ForwardedAuthority) synthesizedClaims() map[string]any {
	ac := a.AccessContext()
	if ac == nil {
		return nil
	}
	claims := map[string]any{
		"sub":  ac.UserId,
		"role": string(ac.Role),
	}
	if ac.PrimaryEmail != "" {
		claims["email"] = ac.PrimaryEmail
	}
	// Provenance only. component/metadata reads these back through
	// UserIdentityFromContext to stamp identity.displayName on written rows;
	// no authorization decision consults them.
	if a.FirstName != "" {
		claims["given_name"] = a.FirstName
	}
	if a.LastName != "" {
		claims["family_name"] = a.LastName
	}
	if a.LocalDev {
		claims["memql_local_dev"] = "1"
	}
	return claims
}

type forwardedAuthorityCtxKey struct{}

// ContextWithForwardedAuthority attaches everything the worker's engine and
// handlers read for actor resolution: the AccessContext (which the predecessor
// never set -- the defect), plus a TokenInfo and claims derived from it so
// UserIdentityFromContext and the claims-reading consumers resolve the same
// principal.
//
// It also stashes the authority itself, so a node that forwards AGAIN can
// re-assert it verbatim. This matters for the two-hop chain BFF -> cognition ->
// agent, where handleCallTool and handleAgentGenerateTurn run: rebuilding the
// authority from the AccessContext at hop two would preserve the clamped role
// but silently drop BadgeExpires, leaving the final node unable to enforce
// expiry. Re-forwarding the validated original keeps kind, ceiling and expiry
// intact across any number of hops.
//
// The stash happens for every kind, including AuthorityKindInternal -- it
// records "this request arrived asserting no principal", which a downstream hop
// must re-assert rather than upgrade. Only the actor bindings are skipped for
// internal: that kind binds no actor by design, and stamping an empty one would
// be indistinguishable from the bug this replaces.
func ContextWithForwardedAuthority(ctx context.Context, a *ForwardedAuthority) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, forwardedAuthorityCtxKey{}, a)

	ac := a.AccessContext()
	if ac == nil {
		return ctx
	}
	ctx = ContextWithAccess(ctx, ac)
	if claims := a.synthesizedClaims(); claims != nil {
		if tok := BuildTokenInfo(claims); tok != nil {
			ctx = ContextWithToken(ctx, tok)
		}
		ctx = ContextWithClaims(ctx, claims)
	}
	return ctx
}

// ForwardedAuthorityFromContext returns the authority this request arrived
// with, or nil when it did not arrive over a mesh forward. A node that forwards
// onward should prefer this over rebuilding one, so the assertion the edge made
// -- including a badge grant's expiry -- survives the next hop.
func ForwardedAuthorityFromContext(ctx context.Context) *ForwardedAuthority {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(forwardedAuthorityCtxKey{}).(*ForwardedAuthority)
	return a
}
