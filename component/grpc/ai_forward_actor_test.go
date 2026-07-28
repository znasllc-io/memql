package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// ai_forward_actor_test.go -- memql#2876.
//
// The AI-forward worker path attached the caller's CLAIMS
// (auth.ContextWithForwardedClaims sets TokenInfo + Claims) but never an
// AccessContext -- and every engine actor surface reads
// auth.AccessFromContext, not the claims:
//
//	component/memql/executor.go            resolveActorPath
//	component/memql/spec_evaluator.go
//	component/memql/mutation_templates.go
//	component/memql/result_cache_policy.go
//	component/automations/actor_envelope_binding.go
//
// So DSL executed on the worker side of a BFF -> worker hop runs with NO
// actor: under the deny-on-nil default (#2801) `actor.userId` resolves to ""
// and `isClusterOwner` to false, so every self-scoped read returns zero rows
// and every mutation stamps `createdBy: ""`. The comment on that path says
// "so worker-side ACLs work"; those ACLs are exactly what does not.
//
// THE DEFECT IS STILL OPEN. memql#2876 is parked: the obvious repair -- attach
// an AccessContext resolved from the forwarded claims -- is a NET REGRESSION,
// because it removes the deny-on-nil protection above while the badge role
// ceiling (#2513) cannot be carried reliably. See the note on
// auth.WithForwardedAuthorityContext for the measurement and the shape a real
// fix needs. These tests pin what is TRUE today so the next attempt starts from
// evidence rather than from the same wrong assumption.
//
// Unlike #2814's QueryForward -- receive-side machinery that never had a
// producer, and was removed outright rather than repaired -- this path is
// LIVE: every BFF -> Voice / BFF -> Agent forward goes through it
// (handleAiChat, handleCallTool, handleAgentGenerateTurn).
//
// # Why these tests assert what they assert
//
// Through auth.AccessFromContext and auth.ActorEnvelopeMap -- the envelope the
// DSL actually binds -- rather than through UserIdentityFromContext. That
// distinction is the whole defect: UserIdentityFromContext PASSED on the
// broken code, because claims were attached; it is a proxy that reports
// success while the engine still sees no actor.

// TestForwardedClaimsAloneLeaveTheEngineWithNoActor pins the defect itself, so
// the reason the fix is needed cannot be forgotten.
//
// It asserts the property of auth.ContextWithForwardedClaims directly: claims
// are attached, an identity is derivable -- and AccessFromContext is still
// nil. Anyone tempted to "simplify" the fix by relying on the claims alone can
// read this and see why that does not work.
func TestForwardedClaimsAloneLeaveTheEngineWithNoActor(t *testing.T) {
	ctx := auth.ContextWithForwardedClaims(context.Background(), map[string]string{
		"sub":  "v1:identity:user:alice",
		"role": "owner",
	})

	// The proxy that made this look fine.
	id, err := auth.UserIdentityFromContext(ctx)
	if err != nil || id.Subject != "v1:identity:user:alice" {
		t.Fatalf("fixture: expected a derivable identity, got %+v err=%v", id, err)
	}

	// What the engine actually reads.
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		t.Fatalf("ContextWithForwardedClaims must NOT set an AccessContext by itself "+
			"(got %+v). If this ever starts returning one, the fix in handleAiForward "+
			"is redundant and should be removed rather than left as a second source of truth.", ac)
	}

	// And the envelope the DSL binds is the deny-all one.
	ac, _ := auth.AccessFromContext(ctx)
	env := auth.ActorEnvelopeMap(ac)
	if env["userId"] != "" {
		t.Errorf("actor.userId = %q, want empty -- this is the zero-rows symptom", env["userId"])
	}
	if env["isClusterOwner"] == true {
		t.Error("actor.isClusterOwner is true with no AccessContext; the deny-on-nil default (#2801) is not holding")
	}
}

// TestForwardedAuthorityContextCarriesTheBadgeCeiling covers the half that
// makes attaching an AccessContext SAFE rather than an escalation.
//
// applyBadgeRoleCeiling (memql#2513) clamps a badge session's stored role down
// to the terminal's ceiling, keyed on the `class` and `role_ceiling` claims.
// auth.UserIdentity carries neither -- Subject/Email/PhoneNumber/Role/
// FirstName/LastName only -- so ForwardedClaimsFromIdentity dropped both, and
// a receiver resolving the principal from the database would have got the
// UNCLAMPED stored role.
//
// The direction matters: the PRINCIPAL fails closed (an unresolvable subject
// yields no actor) while the CEILING fails open, and an absent `class` is
// indistinguishable from "not a badge session" -- so the receiver cannot
// detect the loss. Attaching an AccessContext without carrying these two would
// have converted today's zero-rows bug into a privilege escalation.
func TestForwardedAuthorityContextCarriesTheBadgeCeiling(t *testing.T) {
	verified := map[string]any{
		"sub":          "v1:identity:user:alice",
		"role":         "owner",
		"class":        "badge",
		"role_ceiling": "reader",
		// Must NOT cross: not an input to any decision the receiver reapplies.
		"some_vendor_claim": "leak-me",
	}

	m := auth.WithForwardedAuthorityContext(
		auth.ForwardedClaimsFromIdentity(auth.UserIdentity{
			Subject: "v1:identity:user:alice",
			Role:    "owner",
		}),
		verified,
	)

	if m["class"] != "badge" {
		t.Errorf("class did not cross the hop (got %q) -- applyBadgeRoleCeiling keys on it, "+
			"so without it a badge session resolves its unclamped stored role on the worker", m["class"])
	}
	if m["role_ceiling"] != "reader" {
		t.Errorf("role_ceiling did not cross the hop (got %q)", m["role_ceiling"])
	}
	if _, leaked := m["some_vendor_claim"]; leaked {
		t.Error("an unrelated claim crossed the hop -- the carrier must be an ALLOWLIST, " +
			"not a copy of the claim set, or every future issuer claim lands on a peer message")
	}

	// End to end through the receiving side's rebuild: the two claims must
	// survive TokenInfoFromForwardedClaims, which is what LoadFromClaims reads.
	tok := auth.TokenInfoFromForwardedClaims(m)
	if tok == nil {
		t.Fatal("TokenInfoFromForwardedClaims returned nil for a populated map")
	}
	if got := tok.Claims["class"]; got != "badge" {
		t.Errorf("class did not survive the TokenInfo rebuild: %v", got)
	}
	if got := tok.Claims["role_ceiling"]; got != "reader" {
		t.Errorf("role_ceiling did not survive the TokenInfo rebuild: %v", got)
	}
}

// TestForwardedAuthorityContextIsAbsentForNonBadgeSessions guards the other
// direction: a normal session must not acquire a spurious `class`, because
// applyBadgeRoleCeiling clamps to RoleReader on a badge grant with an
// unusable ceiling. Stamping `class=badge` on everything would fail closed on
// every ordinary forward.
func TestForwardedAuthorityContextIsAbsentForNonBadgeSessions(t *testing.T) {
	m := auth.WithForwardedAuthorityContext(
		auth.ForwardedClaimsFromIdentity(auth.UserIdentity{
			Subject: "v1:identity:user:bob",
			Role:    "writer",
		}),
		map[string]any{"sub": "v1:identity:user:bob", "role": "writer"},
	)

	if v, present := m["class"]; present {
		t.Errorf("class=%q was stamped on a non-badge session; applyBadgeRoleCeiling would "+
			"then clamp it to reader for want of a ceiling, failing every ordinary forward closed", v)
	}
	if v, present := m["role_ceiling"]; present {
		t.Errorf("role_ceiling=%q was stamped on a non-badge session", v)
	}
}

// TestWithForwardedAuthorityContextHandlesNilInputs is the boring-but-load-
// bearing case: the producer calls this on every forward, including ones whose
// stream carries no claims yet.
func TestWithForwardedAuthorityContextHandlesNilInputs(t *testing.T) {
	if m := auth.WithForwardedAuthorityContext(nil, nil); m == nil {
		t.Error("WithForwardedAuthorityContext(nil, nil) returned nil; the producer assigns the result unconditionally")
	}
	m := auth.WithForwardedAuthorityContext(map[string]string{"sub": "x"}, nil)
	if m["sub"] != "x" {
		t.Error("an existing entry was dropped when claims were nil")
	}
}
