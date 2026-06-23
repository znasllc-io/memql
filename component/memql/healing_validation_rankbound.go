package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// Blast-radius -> minimum role rank required to VALIDATE a healed override
// (E4.5 / memql#2143). Mirrors the Epic 1 base ranks (owner=400 >
// developer=300 > admin=200 > user=100): a bigger blast radius demands a
// higher-ranked validator. The ranks are read from the live v1:rbac:role
// catalog at validation time (lookupRoleRankBySlug) so a re-ranked or custom
// role resolves correctly; these constants are the THRESHOLD the actor's
// resolved rank must meet, keyed off the canonical base slugs.
const (
	// healRankUser is the minimum rank for a personal-blast-radius heal.
	healRankUser = 100
	// healRankAdmin is the minimum rank for a shared-blast-radius heal.
	healRankAdmin = 200
	// healRankDeveloper is the minimum rank for a spine_adjacent heal.
	healRankDeveloper = 300
)

// blastRadiusMinRank maps a healedOverride blastRadius to the minimum role
// rank required to validate it. An unknown / empty radius is treated as the
// most permissive (personal) since the concept default is personal, but the
// gate still requires at least the user rank -- a validation always needs an
// authenticated, ranked principal.
func blastRadiusMinRank(blastRadius string) int {
	switch strings.ToLower(strings.TrimSpace(blastRadius)) {
	case "spine_adjacent":
		return healRankDeveloper
	case "shared":
		return healRankAdmin
	default: // "personal" or unset
		return healRankUser
	}
}

// validateHealingValidationRankBound is the E4.5 (memql#2143) blast-radius-
// scaled validation guard. It gates the human-validation transition of a
// healed override -- the write that flips valid=false -> true (accepting the
// heal so it becomes resolution-eligible). Validation effort is scaled by the
// override's blastRadius: a bigger blast radius requires a higher-ranked
// validator (personal -> user, shared -> admin, spine_adjacent -> developer;
// owner always allowed). This is the role-gating Epic 4 ties to Epic 1's RBAC
// ranks: the wider a heal's reach, the more trust required to accept it.
//
// It only gates the ACCEPT transition (valid going from not-true to true):
//   - merged valid != true: pass (a propose, a reject, or a non-validation
//     edit -- none of these make a heal resolution-eligible).
//   - prior valid already true: pass (re-writing an already-accepted override
//     is not a new acceptance; the base-immutability + ownership gates still
//     apply elsewhere).
//   - the false -> true transition: require the actor's resolved role rank to
//     meet blastRadiusMinRank(blastRadius). A system actor and an owner are
//     always allowed (owner is self-managed and outranks every blast radius).
//
// Mirrors validateRbacCustomRoleRankBound's rank-resolution shape
// (resolveActorRank / lookupRoleRankBySlug) and fails CLOSED: an unresolved
// actor rank is 0, below every threshold, so the validation is denied.
func (e *MemQLEngine) validateHealingValidationRankBound(ctx context.Context, payload map[string]any, priorValid bool, actor string) error {
	if payload == nil {
		return nil
	}

	mergedValid := boolFromAny(payload["valid"])
	if !mergedValid {
		// Not an acceptance (propose / reject / non-validation edit).
		return nil
	}
	if priorValid {
		// Already accepted; this write is not a NEW acceptance.
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	// A system actor (e.g. the SeedMaterializer re-materializing a base
	// override) bypasses the human-validation rank gate.
	if isSystemActor(identity, actor) {
		return nil
	}
	// The owner can validate any blast radius. Resolve owner-ness directly
	// from the identity slug FIRST so the owner pass-path never hits the DB
	// (resolveActorRank's catalog lookup would otherwise run for every actor).
	if strings.EqualFold(strings.TrimSpace(identity.Role), "owner") {
		return nil
	}

	actorRank, _ := e.resolveActorRank(ctx, identity)

	blastRadius := stringFromAny(payload["blastRadius"])
	required := blastRadiusMinRank(blastRadius)
	if actorRank >= required {
		return nil
	}

	baseConstructId := strings.TrimSpace(stringFromAny(payload["baseConstructId"]))
	return fmt.Errorf(
		"v1:healing:healedOverride: validating a %q-blast-radius heal for %q requires role rank >= %d, but the actor's rank is %d -- "+
			"validation effort is blast-radius-scaled (personal->user, shared->admin, spine_adjacent->developer; owner always allowed). "+
			"A wider-reaching heal needs a higher-ranked validator. See dsl/healing/concepts.memql:healedOverride.blastRadius (E4.5 / memql#2143).",
		blastRadiusOrDefault(blastRadius), baseConstructId, required, actorRank,
	)
}

// blastRadiusOrDefault returns the blast radius for an error message,
// defaulting to "personal" (the concept default) when empty.
func blastRadiusOrDefault(blastRadius string) string {
	b := strings.ToLower(strings.TrimSpace(blastRadius))
	if b == "" {
		return "personal"
	}
	return b
}
