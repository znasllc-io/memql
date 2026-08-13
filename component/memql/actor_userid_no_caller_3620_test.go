package memql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// memql#3620 -- `actor.userId` must REFUSE against an envelope that names
// nobody, on BOTH the read and the write path.
//
// # What was wrong
//
// auth.ActorEnvelopeValue(nil, "userId") answers ("", true), and both binding
// surfaces took that at face value:
//
//	read   resolveActorReferences(ctx, `ownerUserId == actor.userId`)
//	       -> err=<nil>, term becomes `payload #>> '{ownerUserId}' = ''`
//	write  a mutation stamping `ownerUserId: actor.userId`
//	       -> payload["ownerUserId"] = ""
//
// Neither is an empty result or a skipped write. `F == ""` is a legal
// predicate that MATCHES, so the read returns every row whose owner is the
// empty string; the write MINTS such a row. The two compose: the write is what
// creates the row and the read is what hands it to a caller who never
// authenticated.
//
// # Why the asymmetry is the finding
//
// The same emptiness was ALREADY treated as DENY three doors down --
// refuseRowAuthzWithoutActor and rowAuthzAdmits both refuse an empty caller
// (memql#3172), and stampRowAuthzOwner refuses to stamp one on the raw
// insert() path (memql#3175). But stampRowAuthzOwner early-returns on
// FromTemplate, which is exactly what these 75 mutations are, and no concept
// schema rejects an empty owner (`@required` means present, not non-empty --
// see zeroValueFor's comment in executor_mutation.go). So the value surfaces
// treated as writable what the gate surfaces treated as nobody.
//
// Refusing costs nothing: every legitimate caller has a user id. The paths
// that did not -- the automation executor and the harness reconciler -- were
// writing rows owned by nobody, and were given actors rather than exemptions.

// ---------------------------------------------------------------------------
// READ half
// ---------------------------------------------------------------------------

// ownerTerm builds `ownerUserId == actor.userId` -- the shape 71 owned-tier
// reads in the corpus carry, and the shape the row-authz injector produces.
func ownerTerm() *ComparisonExpression {
	return &ComparisonExpression{
		Field:    FieldReference{Raw: "ownerUserId", Parts: []string{"ownerUserId"}},
		Operator: OpEq,
		Value:    &ActorReference{Path: "userId"},
	}
}

func TestActorUserIdInAFilterRefusesWithNoCaller(t *testing.T) {
	resolved, err := resolveActorReferences(context.Background(), ownerTerm())
	if err == nil {
		cmp, _ := resolved.(*ComparisonExpression)
		bound := "<not a comparison>"
		if cmp != nil {
			bound, _ = cmp.Value.(string)
			bound = "\"" + bound + "\""
		}
		t.Fatalf("resolveActorReferences bound actor.userId to %s with no AccessContext.\n"+
			"That term compiles to `payload #>> '{ownerUserId}' = ?` with an empty argument and "+
			"nothing else narrows the query, so it returns every row owned by nobody -- to a "+
			"caller who never authenticated (memql#3620).", bound)
	}
	if !errors.Is(err, auth.ErrActorEnvelopeNoCaller) {
		t.Fatalf("refusal must carry auth.ErrActorEnvelopeNoCaller so both binding surfaces "+
			"report one identity; got %v", err)
	}
	if !strings.Contains(err.Error(), "actor.userId") {
		t.Errorf("the refusal must name the path it refused; got %q", err.Error())
	}
}

// The positive control. Without it the test above is satisfied by a resolver
// that refuses everything.
func TestActorUserIdInAFilterBindsARealCaller(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "user-3620", Role: auth.RoleWriter})

	resolved, err := resolveActorReferences(ctx, ownerTerm())
	require.NoError(t, err)
	cmp, ok := resolved.(*ComparisonExpression)
	require.True(t, ok)
	require.Equal(t, "user-3620", cmp.Value,
		"an authenticated caller must still bind normally")
}

// A WHITESPACE-ONLY user id is the same absence wearing a different string.
// Comparing an owner column against a run of spaces names no more of a caller
// than comparing it against the empty string does, and the row gate trims
// before comparing (rowauthz_enforce.go) -- so a resolver that admitted it
// would disagree with the gate all over again.
func TestActorUserIdInAFilterRefusesABlankCaller(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "   ", Role: auth.RoleWriter})

	_, err := resolveActorReferences(ctx, ownerTerm())
	require.Error(t, err, "a blank user id names nobody just as a nil AccessContext does")
	require.ErrorIs(t, err, auth.ErrActorEnvelopeNoCaller)
}

// The refusal is scoped to userId, and that scope is deliberate rather than
// incidental -- see auth.ActorEnvelopeBind's comment. role and isClusterOwner
// FAIL a gate when absent (so the absence is already safe), identityId is
// empty for essentially every authenticated caller (LoadFromClaims never
// populates it), and primaryEmail is legitimately blank for machine
// credentials. Refusing those would refuse the normal case.
func TestOnlyUserIdRefusesOnTheFilterPath(t *testing.T) {
	for _, path := range []string{"role", "identityId", "primaryEmail", "isClusterOwner", "now"} {
		t.Run(path, func(t *testing.T) {
			expr := &ComparisonExpression{
				Field:    FieldReference{Raw: "probe", Parts: []string{"probe"}},
				Operator: OpEq,
				Value:    &ActorReference{Path: path},
			}
			if _, err := resolveActorReferences(context.Background(), expr); err != nil {
				t.Fatalf("actor.%s must still resolve against an absent caller: %v", path, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WRITE half
// ---------------------------------------------------------------------------

// ownedTierMutation loads a REAL shipped mutation whose concept declares
// `@rowAuthz(owner="ownerUserId")` and whose insert stamps that field from
// actor.userId -- so this measures the shipped corpus, not a fixture that
// merely resembles it.
func ownedTierMutation(t *testing.T) (*MemQLEngine, *FunctionMutationTemplate) {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fnRegistry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, fnRegistry, concept.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	fn, err := fnRegistry.Get("mintAction")
	require.NoError(t, err, "mintAction must load from dsl/actions/mutations.memql")
	require.NotNil(t, fn.MutationTemplate)
	return &MemQLEngine{}, fn.MutationTemplate
}

// payloadField reads one field out of a rendered node's JSON payload.
func payloadField(t *testing.T, node MutationNode, field string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(node.PayloadRaw), &payload); err != nil {
		t.Fatalf("unmarshal rendered payload %q: %v", node.PayloadRaw, err)
	}
	s, _ := payload[field].(string)
	return s
}

func mintArgs() map[string]any {
	return map[string]any{
		"actionId":         "act-3620",
		"slug":             "probe",
		"intent":           "probe the owner stamp",
		"inputFingerprint": "fp-3620",
		"calls":            []any{},
	}
}

func TestOwnedTierMutationRefusesToStampAnEmptyOwner(t *testing.T) {
	eng, tmpl := ownedTierMutation(t)

	node, err := eng.renderMutationTemplate(context.Background(), tmpl, mintArgs())
	if err == nil {
		owner := payloadField(t, node, "ownerUserId")
		t.Fatalf("rendering succeeded with no AccessContext and stamped ownerUserId=%q.\n"+
			"The row is then readable and writable by nobody, while wearing the exact value an "+
			"unauthenticated owned-tier read compares against -- so the read half hands it "+
			"straight back. stampRowAuthzOwner cannot catch this: it early-returns on "+
			"FromTemplate, which every named mutation is (memql#3620).", owner)
	}
	require.ErrorIs(t, err, auth.ErrActorEnvelopeNoCaller)
}

// The positive control for the write half.
func TestOwnedTierMutationStampsARealCaller(t *testing.T) {
	eng, tmpl := ownedTierMutation(t)
	ctx := auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "user-3620", Role: auth.RoleWriter})

	node, err := eng.renderMutationTemplate(ctx, tmpl, mintArgs())
	require.NoError(t, err)
	owner := payloadField(t, node, "ownerUserId")
	require.Contains(t, owner, "user-3620",
		"an authenticated caller must still have their id stamped as the owner")
}

// ---------------------------------------------------------------------------
// The system-actor paths that must KEEP working
// ---------------------------------------------------------------------------

// The seed materializer runs from a startup goroutine under a context with
// claims + TokenInfo and NO AccessContext (systemActorContext, this package).
// It stays legal because the mutations it drives take `ownerUserId` as an ARG
// -- the materializer passes the target user explicitly for a @scope("perUser")
// seed -- so none of them reads actor.userId.
//
// That is a property of the corpus, not of the materializer, and nothing else
// states it. Adding `ownerUserId: actor.userId` to any create<Concept> the
// materializer drives would break the startup sweep at boot; this fails first.
func TestSeedMaterializerSystemActorStillRendersItsMutations(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fnRegistry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, fnRegistry, concept.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	eng := &MemQLEngine{}
	ctx := systemActorContext(context.Background())

	if _, ok := auth.AccessFromContext(ctx); ok {
		t.Fatal("the materializer context is supposed to carry NO AccessContext -- " +
			"this test measures nothing if it acquires one")
	}

	cases := []struct {
		mutation string
		args     map[string]any
	}{
		{"createAgent", map[string]any{
			"agentId": "agent-3620", "ownerUserId": "user-seeded", "name": "Seeded",
		}},
		{"createRole", map[string]any{
			"roleId": "role-3620", "slug": "probe", "name": "Probe", "rank": 1,
		}},
		{"createCapability", map[string]any{
			"capabilityId": "cap-3620", "roleSlug": "probe",
			"verb": "read", "resourceType": "data",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.mutation, func(t *testing.T) {
			fn, err := fnRegistry.Get(tc.mutation)
			require.NoError(t, err)
			require.NotNil(t, fn.MutationTemplate)
			_, err = eng.renderMutationTemplate(ctx, fn.MutationTemplate, tc.args)
			require.NoError(t, err,
				"%s must still render under the seed materializer's system actor; "+
					"if it now reads actor.userId, the startup seed sweep fails at boot", tc.mutation)
		})
	}
}

// The other half of the automation guard. component/automations'
// TestSystemActorStampsABindableAccessContext pins that contextWithSystemActor
// now produces this envelope shape; this pins that dsl/forge's audit mutation
// -- the one construct in the corpus an automation reaches that reads
// actor.userId -- renders under it.
//
// Before memql#3620 the automation executor stamped claims + TokenInfo only, so
// routeRequest and recordTransition wrote `actorUserId: ""` on a REQUIRED field
// carrying `@relationship(target=user)`, on every routed request and every
// status transition. The refusal turns that into a boot-visible failure; the
// executor's third surface is what keeps it working.
func TestForgeAuditMutationRendersUnderTheAutomationPrincipal(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fnRegistry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, fnRegistry, concept.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	fn, err := fnRegistry.Get("recordRequestEvent")
	require.NoError(t, err, "recordRequestEvent must load from dsl/forge/mutations.memql")

	// Byte-for-byte the envelope component/automations' contextWithSystemActor
	// now builds for a background trigger.
	const principal = "system:automation:routeRequest"
	ctx := auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: principal, Role: auth.RoleReader})

	eng := &MemQLEngine{}
	node, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"eventId":   "evt-3620",
		"requestId": "req-3620",
		"kind":      "routed",
	})
	require.NoError(t, err,
		"the forge audit automations must still persist; a refusal here means every routed "+
			"request and every status transition loses its audit row")
	require.Contains(t, payloadField(t, node, "actorUserId"), "routeRequest",
		"the audit row must name the principal that produced the event")
}

// The seeded owner must be the SEED's target user, not silently dropped. Pinned
// alongside the refusal so "it renders" cannot degrade into "it renders an
// unowned row".
func TestSeedMaterializerStampsTheSeededOwner(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fnRegistry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, fnRegistry, concept.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	fn, err := fnRegistry.Get("createAgent")
	require.NoError(t, err)

	eng := &MemQLEngine{}
	node, err := eng.renderMutationTemplate(systemActorContext(context.Background()),
		fn.MutationTemplate, map[string]any{
			"agentId": "agent-3620b", "ownerUserId": "user-seeded", "name": "Seeded",
		})
	require.NoError(t, err)
	owner := payloadField(t, node, "ownerUserId")
	require.Contains(t, owner, "user-seeded",
		"the perUser seed's target owner must survive; an empty owner here is the same "+
			"orphan row memql#3620 is about, arriving by a different door")
}
