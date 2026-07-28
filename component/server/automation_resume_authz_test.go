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

// resumeRequest is the minimal well-formed body. executionId is non-empty so a
// 400 cannot be mistaken for the 403 under test -- the authorization check runs
// BEFORE the body validation, and these tests must not depend on that order.
func resumeRequest() PostAutomationResumeRequestObject {
	return PostAutomationResumeRequestObject{
		Body: &PostAutomationResumeJSONRequestBody{ExecutionId: "exec-2908-probe"},
	}
}

func ctxWithRole(role auth.Role) context.Context {
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
		{"no AccessContext at all (unauthenticated node, memql#2937)", context.Background()},
		{"reader", ctxWithRole(auth.RoleReader)},
		{"writer", ctxWithRole(auth.RoleWriter)},
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

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			resp, err := s.PostAutomationResume(ctxWithRole(role), resumeRequest())
			if err != nil {
				t.Fatalf("handler returned a transport error: %v", err)
			}
			if _, refused := resp.(PostAutomationResume403JSONResponse); refused {
				t.Fatalf("role %q was refused; owner and admin must be able to resume (memql#2908)", role)
			}
			if _, ok := resp.(PostAutomationResume404JSONResponse); !ok {
				t.Fatalf("role %q: got %T, want the scheduler-not-configured 404 that proves the "+
					"request passed authorization", role, resp)
			}
		})
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
