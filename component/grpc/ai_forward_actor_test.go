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
// THE DEFECT IS CLOSED, in memql#3205. HandleForwardedRequest now verifies a
// mandatory ForwardedAuthority and binds the AccessContext it asserts, so the
// engine sees the caller's actor on the worker side.
//
// What survives here is the DEFECT PIN below: claims ALONE still leave the
// engine with no actor. That has to stay true -- it is the property the whole
// contract rests on, so if someone re-attaches claims and calls it done, this
// test fails.
//
// The naive repair the original note warned about -- resolve an AccessContext
// from the forwarded claims -- was a net regression, because it removed the
// deny-on-nil protection above while the badge ceiling (#2513) could not be
// carried reliably. What shipped resolves nothing on the receiver: it verifies
// an assertion sourced from the producer's post-rotate session state, and
// refuses when it cannot prove the ceiling was applied. See
// docs/internal/design/mesh-forwarded-auth-contract.md.
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

// The three tests that stood here -- TestForwardedAuthorityContextCarriesTheBadgeCeiling,
// TestForwardedAuthorityContextIsAbsentForNonBadgeSessions and
// TestWithForwardedAuthorityContextHandlesNilInputs -- are DELETED with the
// carrier they covered (auth.WithForwardedAuthorityContext), memql#3205.
//
// They were not merely obsolete, they were misleading. The first asserted "the
// packed map contains these two strings" without ever calling the function that
// SOURCES them, so it passed against the very defect it was written for: the
// source was the stream context, which cannot see a mid-stream badge grant. The
// second asserted the ABSENCE of those keys for a non-badge session -- i.e. it
// pinned as correct the exact property that makes an unstated ceiling
// undetectable.
//
// The replacements live where the two halves actually are:
//   - component/auth/forward_authority_test.go   the verifier's rule table
//   - forwarded_authority_source_test.go         the SOURCE, driven through a
//     real handleRotateAuth rotation, which is the assertion the deleted tests
//     could not make.
