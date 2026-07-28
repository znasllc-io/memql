package server

// automation_resume_authz_test.go -- POST /automations/resume is an operator
// action and must be gated as one (memql#2908).
//
// # Why the gate is in the handler
//
// Resuming re-executes a checkpoint's remaining steps through the PRODUCTION
// step registry, and Executor.ResumeFrom calls contextWithSystemActor, which
// REPLACES whatever caller identity reached the handler with the automation's
// own system actor. So the caller's identity is not merely unchecked
// downstream -- it is discarded. Nothing below this point can authorize.
//
// Middleware alone is not sufficient, which is the property the nil case below
// pins (memql#2937): the identity binary skips the verifier middleware
// entirely (app/config.go returns early on !verifierRequired) while still
// registering this route against a real scheduler, and it is the publicly
// fronted listener. On that node a request arrives with NO AccessContext at
// all. If absent-actor were treated as anything but a refusal, the gate would
// be decorative exactly where it matters most.
//
// # Scope
//
// This asserts ROLE, not per-execution ownership. "You may resume what you
// triggered" is not expressible today: the checkpoint records
// triggerContext.triggeredBy as a KIND ("schedule" / "event" / "manual"), and
// the row's createdBy is the automation's system actor, not the human who
// triggered it. Narrowing to the triggering principal needs a field first.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// resumeRequest is the minimal well-formed body. Body is non-nil so the one
// 400 path AHEAD of the gate (request.Body == nil) cannot be mistaken for the
// 403 under test, and executionId is non-empty so the 400 path BEHIND it is
// not reached either.
func resumeRequest() PostAutomationResumeRequestObject {
	return PostAutomationResumeRequestObject{
		Body: &PostAutomationResumeJSONRequestBody{ExecutionId: "exec-2908-probe"},
	}
}

// ctxWithClaims builds the context shape the REAL HTTP path produces.
//
// This is the load-bearing detail. verifier.AttachToContext calls
// ContextWithClaims + ContextWithToken and NOT ContextWithAccess, and no HTTP
// middleware anywhere calls ContextWithAccess -- so on the wire
// auth.AccessFromContext is nil for every caller, cluster owners included.
//
// An earlier revision of this test built its context with ContextWithAccess, a
// call nothing on the HTTP path makes. It passed green while a real owner JWT
// got 403: the gate did not secure the endpoint, it killed it. Tests that
// fabricate a context shape production never produces prove only that the
// function compiles.
func ctxWithClaims(role auth.Role) context.Context {
	return auth.ContextWithClaims(context.Background(), map[string]any{
		"sub":  "v1:identity:user:probe-2908",
		"role": string(role),
	})
}

// ctxWithAccess is the OTHER shape the handler must still honour: a caller that
// arrives with an AccessContext already resolved (as the gRPC path does).
func ctxWithAccess(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:probe-2908",
		Role:   role,
	})
}

// TestPostAutomationResume_RefusesWithoutOwnerOrAdmin is the gate.
//
// The nil-AccessContext case is the load-bearing one: it is the identity node,
// where no auth middleware runs at all.
func TestPostAutomationResume_RefusesWithoutOwnerOrAdmin(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"nothing at all (unauthenticated node, memql#2937)", context.Background()},
		{"claims: reader", ctxWithClaims(auth.RoleReader)},
		{"claims: writer", ctxWithClaims(auth.RoleWriter)},
		{"claims: no role", auth.ContextWithClaims(context.Background(), map[string]any{"sub": "v1:identity:user:x"})},
		{"AccessContext: reader", ctxWithAccess(auth.RoleReader)},
	}

	// A server with NO scheduler wired. Any of these cases reaching past the
	// authorization check would land on the "scheduler not configured" 404, so
	// a 403 here proves the refusal happened at the gate and not incidentally.
	s := &Server{}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp, err := s.PostAutomationResume(tc.ctx, resumeRequest())
			if err != nil {
				t.Fatalf("handler returned a transport error: %v", err)
			}
			if _, ok := resp.(PostAutomationResume403JSONResponse); !ok {
				t.Fatalf("resume was NOT refused for %s: got %T, want PostAutomationResume403JSONResponse.\n"+
					"Resuming re-executes steps through the production registry under the automation's "+
					"system actor, so an unauthorized caller must never reach the scheduler (memql#2908).",
					tc.name, resp)
			}
		})
	}
}

// TestPostAutomationResume_AdmitsOwnerAndAdmin pins the other half. Without it
// the gate could be satisfied by refusing everything, which would pass the test
// above while breaking the endpoint.
//
// These reach the "scheduler not configured" 404 because this Server has none
// wired -- which is exactly the proof that they got PAST authorization.
func TestPostAutomationResume_AdmitsOwnerAndAdmin(t *testing.T) {
	s := &Server{}

	shapes := []struct {
		name string
		mk   func(auth.Role) context.Context
	}{
		// The one that matters: claims-only is what the HTTP middleware
		// actually attaches. If this regresses, owner and admin are locked out
		// of the endpoint on every node.
		{"claims (the real HTTP shape)", ctxWithClaims},
		{"pre-resolved AccessContext", ctxWithAccess},
	}

	for _, sh := range shapes {
		for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
			t.Run(sh.name+"/"+string(role), func(t *testing.T) {
				resp, err := s.PostAutomationResume(sh.mk(role), resumeRequest())
				if err != nil {
					t.Fatalf("handler returned a transport error: %v", err)
				}
				if _, refused := resp.(PostAutomationResume403JSONResponse); refused {
					t.Fatalf("%s role %q was REFUSED. Owner and admin must be able to resume; "+
						"a gate that refuses everyone has killed the endpoint, not secured it "+
						"(memql#2908)", sh.name, role)
				}
				if _, ok := resp.(PostAutomationResume404JSONResponse); !ok {
					t.Fatalf("%s role %q: got %T, want the scheduler-not-configured 404 that proves "+
						"the request passed authorization", sh.name, role, resp)
				}
			})
		}
	}
}

// TestIsOwnerOrAdmin_NilIsFalse pins the helper's fail-closed property
// directly, so a refactor cannot quietly make an absent actor permissive.
func TestIsOwnerOrAdmin_NilIsFalse(t *testing.T) {
	if isOwnerOrAdmin(nil) {
		t.Fatal("isOwnerOrAdmin(nil) must be false: a node with no auth middleware supplies no " +
			"AccessContext, and that must refuse rather than admit (memql#2908, #2937)")
	}
	for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter} {
		if isOwnerOrAdmin(&auth.AccessContext{Role: role}) {
			t.Fatalf("isOwnerOrAdmin(%q) must be false", role)
		}
	}
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		if !isOwnerOrAdmin(&auth.AccessContext{Role: role}) {
			t.Fatalf("isOwnerOrAdmin(%q) must be true", role)
		}
	}
}
