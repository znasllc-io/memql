package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowauthz_write.go -- memql#3079, Phase 5 of memql#2803.
//
// The write-side counterpart to Phase 3 (#3076, read side). One concept
// declaration now governs both directions: a concept that declares
// `@rowAuthz(owner="F")` is filtered on read AND guarded on update/delete,
// rather than each mutation having to say so itself.
//
// # Why in the engine rather than per-mutation
//
// 120+ of the tree's 215 mutations take a caller-supplied target id with
// nothing relating it to actor.userId, and the mutation grammar cannot express
// such a relation -- zero mutations carry a filter, and an `update` block takes
// `id:` plus field assignments and nothing else. Asking every mutation to
// restate the concept's own declaration is 120 separate authorization
// judgments, and a missing one is indistinguishable from a deliberate
// omission. That is where drift enters.
//
// # Scope: UPDATE / DELETE, not INSERT
//
// This guards a write that has a TARGET ROW. An insert has none, so its problem
// is stamping rather than guarding -- that is memql#3059 (raw `insert()`
// bypasses accept/stamp, so an owner tier is still forgeable), which stays
// independently open. The two together are what make a declared tier mean
// something end to end.

// writeAuthzDecision is what the guard concluded, for the error text and for
// tests that need to distinguish "allowed because internal" from "allowed
// because owner".
type writeAuthzDecision string

const (
	writeAuthzNoDeclaration writeAuthzDecision = "no-declaration"
	writeAuthzOwner         writeAuthzDecision = "owner"
	writeAuthzClusterOwner  writeAuthzDecision = "cluster-owner"
	writeAuthzInternal      writeAuthzDecision = "internal-origin"
	writeAuthzPublicTier    writeAuthzDecision = "public-tier"
	writeAuthzRefused       writeAuthzDecision = "refused"
)

// assertRowAuthzWrite refuses an update/delete whose target row is not owned by
// the actor, for a concept that declares an owner tier.
//
// priorPayload is the PERSISTED row, not the caller's delta, and that is the
// whole point: the check must read ownership from what is stored, never from
// what the caller supplied. Reading the delta would let a caller assert
// ownership in the same request that changes it.
//
// existed reports whether the target row was found. requirePrior distinguishes
// the two callers of the shared write chokepoint: true is update()/delete()
// (a row is expected), false is insert() (a create is legitimate).
//
// That distinction is load-bearing. A genuine CREATE reaches this chokepoint
// with existed == false and no target row to own, so refusing it would break
// every create on a declared concept -- the guard would look like enforcement
// and behave like an outage. Guarding a create's OWNERSHIP STAMP is a
// different problem with a different fix, and it is memql#3059.
func (e *MemQLEngine) assertRowAuthzWrite(
	ctx context.Context,
	conceptMeta *memorynodes.Concept,
	targetID string,
	priorPayload map[string]any,
	existed bool,
	requirePrior bool,
) (writeAuthzDecision, error) {
	decl := rowAuthzDeclOf(conceptMeta)
	if decl == nil {
		return writeAuthzNoDeclaration, nil
	}
	if !existed && !requirePrior {
		// A create. No target row, nothing to own, nothing this guard can
		// say. See memql#3059 for the stamping half.
		return writeAuthzNoDeclaration, nil
	}

	// INTERNAL ORIGIN, checked first and scoped to THIS write.
	//
	// Server-side Go that constructs its own context may write on a user's
	// behalf. The escape is deliberately the origin signal and NOT a role:
	// component/auth/call_origin.go states the doctrine -- "Do NOT derive this
	// from claims, a role, or anything the caller can influence" -- because a
	// role is a database fact about a user row, which makes a role-based
	// bypass spoofable by anyone who can obtain that role. memql#2991 is that
	// failure in the concrete: `updateUser` let a caller name any user AND any
	// role, on the row holding that caller's own role.
	//
	// PER-WRITE, not per-request. memql#2989 built and REFUTED the route of
	// stamping internal origin onto a request-derived context: it opens every
	// guarded construct for the rest of that request. The shape to copy is
	// #3072's -- a context the trusted caller constructs for one operation.
	// Nothing here widens that; this only READS the origin already on ctx.
	if auth.OriginFromContext(ctx) == auth.OriginInternal {
		return writeAuthzInternal, nil
	}

	access, ok := auth.AccessFromContext(ctx)
	if !ok || access == nil {
		// No actor and not internal. Fail CLOSED -- an unauthenticated write
		// to a concept that declares an owner is exactly what this guards.
		return writeAuthzRefused, fmt.Errorf(
			"write to %q refused: the concept declares a row-owner tier and the call carries no "+
				"authenticated actor (memql#3079)", conceptMeta.Name)
	}

	// CLUSTER OWNER bypasses; ADMIN DOES NOT. Stated rather than inferred, as
	// the ruling requires.
	//
	// The declaration vocabulary itself draws this line: the language has a
	// `clusterOwner` tier and no `admin` tier, so an admin bypass would grant
	// a power the DSL cannot express and therefore cannot declare, review, or
	// revoke per concept. And the two directions are not symmetric in cost --
	// an admin wrongly refused is a visible, recoverable error that can be
	// answered with an explicit declaration or a server-side path, while an
	// admin wrongly allowed is a silent cross-tenant write. #2991 is the
	// worked example of a role-based write bypass going wrong.
	if access.IsClusterOwner() {
		return writeAuthzClusterOwner, nil
	}

	switch decl.Tier {
	case langparser.RowAuthzPublic:
		// `public` names no owner, so there is no ownership to check. It is a
		// statement about visibility, and this guard is about ownership --
		// enforcing anything here would be inventing a rule the declaration
		// does not make.
		return writeAuthzPublicTier, nil

	case langparser.RowAuthzClusterOwner:
		// Reached only when the actor is NOT the cluster owner (the bypass
		// above returns first), so this is always a refusal.
		return writeAuthzRefused, fmt.Errorf(
			"write to %q refused: the concept declares the clusterOwner tier and the actor is "+
				"not the cluster owner (memql#3079)", conceptMeta.Name)

	case langparser.RowAuthzGranted:
		// Deciding whether a grant reaches THIS row needs the relationship
		// semantics Phase 4 is scoped to build. Fail CLOSED rather than
		// guessing: an unenforceable declaration must not read as permission.
		// No concept in the tree declares this tier today, so the blast radius
		// is zero until Phase 4 lands.
		return writeAuthzRefused, fmt.Errorf(
			"write to %q refused: the concept declares the granted tier (spec %q), whose "+
				"per-row evaluation needs relationship semantics that do not exist yet "+
				"(memql#2803 Phase 4). Refusing rather than assuming the grant reaches this row "+
				"(memql#3079)", conceptMeta.Name, decl.Spec)

	case langparser.RowAuthzOwned:
		if !existed {
			// Reached only on the update()/delete() path (a create returned
			// above), so this is a TARGETED write whose target is missing.
			//
			// A MISSING TARGET ROW MUST NOT READ AS AUTHORIZED. Falling
			// through would be the fail-OPEN direction, and #2982's analyzer
			// had to be fixed once for exactly that (e486d0f5, "the analyzer
			// failed OPEN on lowered AST nodes").
			//
			// executeWrite's own requirePrior check refuses this a few lines
			// later with a caller-facing "use insert()" message. This arm is
			// deliberate defence in depth: the ordering of those two checks is
			// not something a future edit should be able to silently invert.
			return writeAuthzRefused, fmt.Errorf(
				"write to %q refused: target row %q does not exist, and the concept declares a "+
					"row-owner tier -- a missing row cannot be shown to be owned by the actor "+
					"(memql#3079)", conceptMeta.Name, targetID)
		}
		owner, err := rowOwnerValue(decl, targetID, priorPayload)
		if err != nil {
			return writeAuthzRefused, fmt.Errorf("write to %q refused: %w (memql#3079)", conceptMeta.Name, err)
		}
		if owner != "" && owner == strings.TrimSpace(access.UserId) {
			return writeAuthzOwner, nil
		}
		// Deliberately does NOT echo the row's owner back to the caller: that
		// would turn a refusal into an ownership oracle over rows the caller
		// cannot read.
		return writeAuthzRefused, fmt.Errorf(
			"write to %q refused: row %q is not owned by the actor (memql#3079)",
			conceptMeta.Name, targetID)

	default:
		// Same fail-closed posture the read side takes for an unrecognised
		// tier: the engine cannot say what was declared, so it refuses rather
		// than writing.
		return writeAuthzRefused, fmt.Errorf(
			"write to %q refused: unrecognised row-authz tier %q (memql#3079)",
			conceptMeta.Name, decl.Tier)
	}
}

// rowOwnerValue reads the owning user id for a row under decl.
//
// For the SELF-OWNED form (memql#3029) the row IS the owner, so the identity
// comes from the row's own id rather than from a payload field -- matching
// InjectedPredicate, which renders `row.id==actor.userId` for that form.
func rowOwnerValue(decl *langparser.RowAuthzDecl, targetID string, payload map[string]any) (string, error) {
	if strings.TrimSpace(decl.Owner) == langparser.RowAuthzSelfOwnedField {
		return shortIdOf(targetID), nil
	}
	field := strings.TrimSpace(decl.Owner)
	if field == "" {
		return "", fmt.Errorf("the owned tier declares no owner field")
	}
	raw, present := payload[field]
	if !present {
		// The declaration names a field the stored row does not carry. Fail
		// closed: this is a tree defect, and treating "no owner recorded" as
		// "anyone may write" is the wrong direction for it.
		return "", fmt.Errorf("the stored row carries no %q field to check ownership against", field)
	}
	s, isString := raw.(string)
	if !isString {
		return "", fmt.Errorf("the stored row's %q is %T, not a user id string", field, raw)
	}
	return strings.TrimSpace(s), nil
}

// shortIdOf reduces a canonical `{concept}:{shortId}` to its trailing segment,
// so a self-owned row stored as `v1:identity:user:u_123` compares against the
// actor's bare `u_123`. A bare id passes through unchanged.
func shortIdOf(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.LastIndex(id, ":"); i >= 0 {
		return strings.TrimSpace(id[i+1:])
	}
	return id
}

// rowAuthzDeclOf reads the declaration off a concept, mirroring the read
// side's rowAuthzDeclFor but taking the concept the write path already holds
// instead of looking it up by name again.
func rowAuthzDeclOf(c *memorynodes.Concept) *langparser.RowAuthzDecl {
	if c == nil {
		return nil
	}
	return c.RowAuthz
}
