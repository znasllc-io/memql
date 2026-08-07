package memql

// data_capability_gate.go -- the request-path caller for the coarse
// data-resource capability check (memql#3179, carrying memql#2802).
//
// WHAT THIS IS. auth.Capable(role, verb, auth.ResourceData) is one of the
// seven resource types in the consolidated RBAC model (epic memql#2062). Three
// of them had request-path callers; `data` had none -- auth.CanRead and
// auth.CanWrite existed with zero non-test callers, so nothing consulted the
// data-plane capability before executing a query or a mutation, and `reader`
// and `writer` were indistinguishable at the data plane: a reader could
// execute any mutation the DSL exposes. This file is the caller that stops
// that being true.
//
// WHAT THIS IS NOT -- READ THIS BEFORE TREATING THE ROW-VISIBILITY GAP AS
// CLOSED. This is the COARSE half. auth.Capable(role, auth.VerbRead,
// auth.ResourceData) answers "may this actor read call records" -- never
// "WHICH call records". Row visibility is a separate, unrelated question,
// still answered only by whatever the DSL author wrote in the filter (see
// docs/public/operate/auth/per-row-authz-audit.md). Nothing here narrows a
// result set by row.
//
// WHERE IT SITS, AND WHY. The handler layer, not the executor (the recorded
// memql#3179 ruling). Handlers see only user requests, so this needs NO system
// bypass list at all: node bootstrap (component/node/bootstrap.go, calling
// Engine.Execute with context.Background() and no AccessContext), the
// authoring promote/demote/rearm paths, the authored scheduler, the planner
// reactive loop and the MCP automation runner all reach the engine in-process
// and never send an ExecuteQueryMsg. The executor placement would have needed
// each of them enumerated on a bypass list; this placement sidesteps them
// entirely.
//
// The accepted cost, stated plainly: PARTIAL BY CONSTRUCTION. A DSL-callable
// surface not reached through this handler is not covered, and two things that
// DO reach a handler are deliberately out. The complete residual-bypass set is
// dataPlaneGateExemptions below -- enumerated, each with its reason, asserted
// by TestDataPlaneGateExemptions_AreEnumerated so it cannot grow silently.
//
// SINGLE BUCKET. `data` is one resource, not one per concept (the other half
// of the ruling). Per-concept would need the concept -> resourceType mapping
// that memql#3172 / memql#3174 settle; the single bucket depends on neither.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// dataPlaneGateExemption names one principal or surface deliberately NOT
// covered by the coarse data-plane capability gate, and why.
type dataPlaneGateExemption struct {
	// Name identifies the exempt principal or wire surface.
	Name string
	// Enforced reports whether this file enforces the exemption (an explicit
	// skip in allowDataPlaneAccess) or whether it is simply a surface the gate
	// was never wired into.
	Enforced bool
	// Reason is why it is out. Not decoration: an exemption without a stated
	// reason is an exemption nobody can review.
	Reason string
}

// dataPlaneGateExemptions is THE list -- the complete residual-bypass set for
// the coarse data-plane capability gate, in one place. Anything a `reader` can
// still write through is here or it is a bug.
//
// TestDataPlaneGateExemptions_AreEnumerated pins it exactly, so a future
// exemption has to be added here (with a reason) and to that test, rather than
// appearing as an untracked `if` somewhere in a handler.
var dataPlaneGateExemptions = []dataPlaneGateExemption{
	{
		Name:     "guest-stream",
		Enforced: true,
		Reason: "Guests carry their own authorization dimension -- invitation scope plus " +
			"the partition grant -- and THAT, not the cluster role, is the real gate for " +
			"them. The reader-vs-writer distinction this gate exists to enforce is about " +
			"ordinary authenticated users. Guests have dedicated message types only for " +
			"the invite/join lifecycle (SendGuestInviteMsg, ResolveGuestInviteMsg, " +
			"JoinSpaceAsGuestMsg, cancel/resend); there is no guest-specific message for " +
			"posting into a space, so guest participation necessarily rides " +
			"ExecuteQueryMsg and refusing it would break shipped guest chat. " +
			"Owner ruling, memql#3179.",
	},
	{
		Name:     "CallToolMsg",
		Enforced: false,
		Reason: "Not gated at all -- the gate is wired into handleExecuteQuery only. " +
			"CallToolMsg reaches mutation-backed tools, but that surface is driven by " +
			"machine credentials: class=\"voice_agent\" JWTs carry no role claim and " +
			"resolve to the reader fallback, and proxied agent-node calls arrive as " +
			"forwarded auth, so gating it on the caller's data-plane capability would " +
			"refuse the live voice path. It is a curated, @allowedRoles-gated allowlist " +
			"rather than the whole authored mutation surface, which is why the uncovered " +
			"slice is the smaller one -- but it IS uncovered.",
	},
}

// isGuestStream reports whether this call arrives on a guest-authenticated
// stream, keyed off the claim the guest interceptor sets (GuestAuthClaimKey,
// guest_stream_interceptor.go) and read the same way the guest handlers read it.
//
// Deliberately NOT keyed off role == "reader". The guest interceptor stamps
// `role: "reader"` as a placeholder, so keying on the role would exempt every
// ordinary reader and empty the gate of the exact thing it exists to enforce.
func isGuestStream(ctx context.Context) bool {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok || claims == nil {
		return false
	}
	guest, ok := claims[GuestAuthClaimKey].(map[string]any)
	return ok && guest != nil
}

// dataPlaneRole resolves the role the gate decides on. An absent or
// unrecognised role resolves to the least-privileged VALID role (reader), NOT
// to "holds no capability at all".
//
// Both halves of that matter. Coercing DOWN is fail-closed for writes: an
// unknown actor gets a reader's grants, which do not include create-on-data.
// Coercing to a valid role rather than to nothing keeps reads working, which
// is not hypothetical -- v1:identity:user.role carries a concept-level
// @default that is never applied on insert (memql#2960), so an empty role on a
// real user row is reachable, and auth.FallbackFromClaims already resolves an
// unusable role claim the same way. It also matches the model's own doctrine
// that an unknown slug ranks least-privileged (component/auth/rbac_model.go,
// roleRank).
func dataPlaneRole(access *auth.AccessContext) auth.Role {
	if access == nil {
		return auth.RoleReader
	}
	role := auth.Role(strings.ToLower(strings.TrimSpace(string(access.Role))))
	if !auth.IsValidRole(role) {
		return auth.RoleReader
	}
	return role
}

// allowDataPlaneAccess reports whether the actor on `ctx` may run `query`
// against the data plane, and the refusal message when it may not. `ctx` must
// be the context the query would execute under -- the classification re-parses
// the query exactly as the executor will, and reads the call origin and the
// ambient actor envelope off this context to do it.
//
// The engine is consulted for classification ONLY when the role cannot write.
// A role holding create-on-data may run anything the data plane exposes, so
// there is nothing a classification could change -- which also means the extra
// parse this costs is paid only by the roles that are actually constrained.
func (s *streamSession) allowDataPlaneAccess(ctx context.Context, query string) (bool, string) {
	// Enumerated exemption: guest streams. See dataPlaneGateExemptions
	// ("guest-stream") for the ruling and the reason. Keyed off the guest
	// CLAIM, never off the placeholder role.
	if isGuestStream(ctx) {
		return true, ""
	}

	access, _ := auth.AccessFromContext(ctx)
	role := dataPlaneRole(access)

	// The read half. Every role in the shipped model holds read-on-data, so
	// this refuses nobody today; it is here because "may this actor read at
	// all" is a real question the model answers, and a role authored without
	// the grant (custom roles, v1:rbac:role) must be refused here rather than
	// read silently.
	if !auth.Capable(role, auth.VerbRead, auth.ResourceData) {
		return false, fmt.Sprintf("permission denied: role %q holds no read capability on the data plane", role)
	}

	// The write half -- the reason this file exists.
	if auth.Capable(role, auth.VerbCreate, auth.ResourceData) {
		return true, ""
	}
	if s.service == nil || s.service.engine == nil {
		// No engine to classify against; the execute path reports the real
		// unavailability. Refusing here would report the wrong cause.
		return true, ""
	}
	if verb := s.service.engine.DataVerbFor(ctx, query); verb != auth.VerbRead {
		return false, fmt.Sprintf(
			"permission denied: role %q holds no %s capability on the data plane; this request writes",
			role, verb)
	}
	return true, ""
}
