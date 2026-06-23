package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// agent.kind enum values. Mirrors the enum declared at
// dsl/agents/concepts.memql:agent.kind. Listed here so the actor-scope
// guard can name the safe-for-user value in its error message without
// inlining a string literal.
const (
	agentKindAssistant  = "assistant"
	agentKindSpecialist = "specialist"
	agentKindSystem     = "system"
)

// validateAgentKindActorScope rejects user-actor agent writes that try
// to stamp `kind in ("system", "specialist")`. Those kinds are reserved
// for platform-internal callers running under a system actor:
//
//   - The SeedMaterializer (`system:seedMaterializer`) writes
//     `kind="system"` when materializing platform agents (MemQL Planner,
//     MemQL Trainer).
//   - The planner integration (`system:planner`) writes
//     `kind="specialist"` when auto-provisioning a specialist for a Plan's
//     capability gap (integrations/agents/factory.go:buildCreateAgentArgs).
//
// User-actor callers -- the CoPresent CreateAgentModal flow in
// particular -- may only land `kind="assistant"` (or omit the field;
// createAgent's default coalesces to "assistant"). Without
// this gate, a user actor could craft a raw insert or pass
// `kind: "system"` through the mutation arg and silently create a
// row that downstream consumers (utterance routing, dedupe, the
// Agents panel filter) treat as platform infrastructure.
//
// The mutation surface itself stays permissive on `kind` so the
// SeedMaterializer + planner paths can keep writing their values; the
// rejection layer sits above the row write to keep the DSL grammar
// small (no per-arg actor preconditions).
//
// Contract:
//
//   - payload missing the kind field: pass. Updates that don't touch
//     kind are unaffected; the prior row's value is preserved by the
//     update merger upstream.
//   - kind="" or kind="assistant": pass for any actor.
//   - kind="system" or kind="specialist": pass only if the caller is a
//     system actor (UserIdentity.Role=="system" OR actor begins with
//     "system:"). Otherwise reject with a typed error naming the
//     allowed value.
//   - Any other kind value: pass. The concept's enum schema check
//     catches the malformed write downstream; this guard has no
//     opinion on it.
//
// Companion to validateAgentLockedItems (locked-skills / domains /
// tools on the same concept); both run inline in executeInsert() for
// every write to v1:agents:agent.
//
// Closes znasllc-io/memql#403.
func (e *MemQLEngine) validateAgentKindActorScope(ctx context.Context, payload map[string]any, actor string) error {
	if payload == nil {
		return nil
	}
	rawKind, present := payload["kind"]
	if !present {
		return nil
	}
	kind := strings.ToLower(strings.TrimSpace(stringFromAny(rawKind)))
	switch kind {
	case "", agentKindAssistant:
		return nil
	case agentKindSystem, agentKindSpecialist:
		// fall through to the actor check.
	default:
		// Unknown values are caught by the concept's enum schema check
		// inside Concept.Create. Don't double-error here.
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}
	return fmt.Errorf(
		"v1:agents:agent: kind=%q requires a system actor (the SeedMaterializer or planner integration); "+
			"user-actor createAgent calls may only set kind to %q or omit the field. "+
			"See dsl/agents/concepts.memql:agent.kind for the platform-identity contract.",
		kind, agentKindAssistant,
	)
}
