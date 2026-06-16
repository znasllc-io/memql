package mcp

// Capability-tier gate (MCP epic #1529 §2-3, Phase 2 #1532) -- "Gate A". A
// coarse, per-deployment switch (MEMQL_MCP_MODE) deciding which CLASSES of
// operation the server permits at all, orthogonal to the per-construct role
// gate ("Gate B", Tool.IsAllowedForRole / CanAuthor / CanRunInline). A call is
// allowed only if the tier enables the op class AND the caller's role passes
// the construct gate. Both are enforced server-side.

import (
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// Tier is the per-deployment capability tier set by MEMQL_MCP_MODE.
type Tier int

const (
	TierSealed    Tier = iota // Tier 1: execute only embedded named constructs
	TierAuthoring             // Tier 2 (default): + define new constructs
	TierInline                // Tier 3: + ad-hoc inline DSL
)

// DefaultTier is the mode a deployment runs in when MEMQL_MCP_MODE is unset.
const DefaultTier = TierAuthoring

// ParseTier maps MEMQL_MCP_MODE to a Tier; unknown/empty -> DefaultTier.
func ParseTier(mode string) Tier {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "sealed":
		return TierSealed
	case "inline":
		return TierInline
	default: // "authoring", "", or anything unrecognized
		return DefaultTier
	}
}

func (t Tier) String() string {
	switch t {
	case TierSealed:
		return "sealed"
	case TierInline:
		return "inline"
	default:
		return "authoring"
	}
}

// opClass is the capability class an MCP operation belongs to.
type opClass int

const (
	classExec   opClass = iota // Tier 1: reflected tools + run_query/mutation/automation
	classAuthor                // Tier 2: define
	classInline                // Tier 3: query (inline) + inline definitions
)

// tierAllows reports whether the tier enables an operation class (Gate A).
func tierAllows(t Tier, c opClass) bool {
	switch c {
	case classExec:
		return true // every tier
	case classAuthor:
		return t == TierAuthoring || t == TierInline
	case classInline:
		return t == TierInline
	}
	return false
}

// Config carries the MCP-specific knobs the server enforces: the acting role
// (Gate B), the capability tier (Gate A), and the acting user (the session
// owner that authored constructs are scoped to, Phase 3 #1533).
type Config struct {
	ActingRole string
	ActingUser string
	Tier       Tier
}

// roleCanAuthor / roleCanRunInline bridge the acting-role string to the auth
// capability predicates (Gate B for the define / inline op classes). owner or
// developer only; admin/writer/reader cannot author or run inline.
func roleCanAuthor(role string) bool {
	return auth.CanAuthor(auth.UserContext{Role: auth.Role(role)})
}

func roleCanRunInline(role string) bool {
	return auth.CanRunInline(auth.UserContext{Role: auth.Role(role)})
}

// roleCanPromote reports whether the role may promote a session-authored
// construct into the durable/shared schema. Promotion is OWNER-ONLY (design §5:
// "promotion ... is a separate, higher-privileged action -- owner only"): a
// developer can author + run inline, but only an owner makes a construct
// permanent. This is the higher bar than CanAuthor on purpose.
func roleCanPromote(role string) bool {
	return strings.EqualFold(strings.TrimSpace(role), string(auth.RoleOwner))
}

// authoringGate is Gate A + Gate B for the `define` op (Tier 2): the deployment
// must enable the authoring class AND the role must pass CanAuthor. Returns the
// allow decision plus a typed denial message for the refused case.
func authoringGate(role string, tier Tier) (bool, string) {
	if !tierAllows(tier, classAuthor) {
		return false, "define is not permitted: deployment tier is " + tier.String() + " (set MEMQL_MCP_MODE=authoring or inline)"
	}
	if !roleCanAuthor(role) {
		return false, "define requires the owner or developer role"
	}
	return true, ""
}

// promoteGate is Gate A + Gate B for the `promote` op: the authoring class must
// be enabled (a sealed deployment promotes nothing) AND the role must be owner.
func promoteGate(role string, tier Tier) (bool, string) {
	if !tierAllows(tier, classAuthor) {
		return false, "promote is not permitted: deployment tier is " + tier.String() + " (set MEMQL_MCP_MODE=authoring or inline)"
	}
	if !roleCanPromote(role) {
		return false, "promote requires the owner role (developers may author + run inline, but only an owner makes a construct durable)"
	}
	return true, ""
}

// gateInline enforces both gates for the inline `query` op (Tier 3). The actual
// inline path lands in Phase 5 (#1535).
func gateInline(role string, tier Tier) map[string]any {
	if !tierAllows(tier, classInline) {
		return errorResult("inline query is not permitted: deployment tier is " + tier.String() + " (set MEMQL_MCP_MODE=inline)")
	}
	if !roleCanRunInline(role) {
		return errorResult("inline query requires the owner or developer role")
	}
	return errorResult("inline query is permitted but not yet implemented (lands in Phase 5, memql#1535)")
}
