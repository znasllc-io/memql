package deploycontrol

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
)

// refusal_test.go is the coverage for memql#3334: a refused deploy-control
// call returns the id of the blocked audit event it wrote.
//
// The suite is deliberately split by the question the issue asked:
//
//	TestRefusalIsAuditedAndReturnsThatEventsId   -- (1) refusals ARE audited,
//	                                                   and the id now comes back
//	TestRefusalAuditIdIsIdenticalOnBothPaths     -- (3) the acceptance property:
//	                                                   unary and streamed agree
//	TestNonRefusalFailuresCarryNoAuditId         -- the negative: "" is honest,
//	                                                   not a dropped id

// TestRefusalIsAuditedAndReturnsThatEventsId establishes the empirical answer
// to question 1 of the issue and pins the fix to it in one assertion: the
// blocked event the gate writes and the id the caller receives are THE SAME
// event.
//
// Asserting the id is non-empty would not be enough -- a fresh random token
// would satisfy that while correlating with nothing. The check that matters is
// equality with the emitted event's CorrelationId, which is the column an
// admin resolves the operator's quoted reference against.
func TestRefusalIsAuditedAndReturnsThatEventsId(t *testing.T) {
	// One case per gate helper, so a change to any tier is caught. rollback
	// (owner-only) is checked against ADMIN specifically: admin is denied
	// there and admitted everywhere else, which is the row an operator is most
	// likely to hit legitimately and be surprised by.
	cases := []struct {
		name string
		role auth.Role
		verb string
		call func(ctx context.Context, svc *Service) error
		code codes.Code
	}{
		{
			name: "authorize/promote refuses a writer",
			role: auth.RoleWriter,
			verb: "deployment_console_promote",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.Promote(ctx, &memqlv1.PromoteRequest{Version: "1.2.3"})
				return err
			},
			code: codes.PermissionDenied,
		},
		{
			name: "authorizeDeploy/cut_version refuses a reader",
			role: auth.RoleReader,
			verb: "deployment_console_cut_version",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.CutVersion(ctx, &memqlv1.CutVersionRequest{Env: "prod", Bump: "patch"})
				return err
			},
			code: codes.PermissionDenied,
		},
		{
			name: "authorizeOwner/rollback_deployment refuses an ADMIN",
			role: auth.RoleAdmin,
			verb: "deployment_console_rollback_deployment",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.RollbackDeployment(ctx, &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "d1"})
				return err
			},
			code: codes.PermissionDenied,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, audit, _ := newParityService(t)

			err := c.call(ctxWithRole(c.role), svc)
			if got := status.Code(err); got != c.code {
				t.Fatalf("code = %v, want %v", got, c.code)
			}

			if len(audit.events) != 1 {
				t.Fatalf("want exactly one audit event, got %d", len(audit.events))
			}
			ev := audit.events[0]
			if ev.Outcome != identity.AuditOutcomeBlocked {
				t.Errorf("outcome = %q, want blocked", ev.Outcome)
			}
			if ev.Action != c.verb {
				t.Errorf("audit action = %q, want %q", ev.Action, c.verb)
			}
			if ev.CorrelationId == "" {
				t.Fatal("the blocked event carries no correlation id; there is nothing to return")
			}

			got := AuditEventIdFromError(err)
			if got == "" {
				t.Fatal("refusal carried no audit event id (memql#3334 regressed)")
			}
			if got != ev.CorrelationId {
				t.Errorf("returned id %q is not the emitted event %q -- a reference that resolves to nothing is worse than none",
					got, ev.CorrelationId)
			}
		})
	}

	// The unauthenticated edge takes the other branch of authorizeWith, and it
	// is the branch a browser can actually present.
	t.Run("unauthenticated caller", func(t *testing.T) {
		svc, audit, _ := newParityService(t)

		_, err := svc.Rollback(context.Background(), &memqlv1.RollbackRequest{Env: "prod", CommitSha: "abc1234"})
		if got := status.Code(err); got != codes.Unauthenticated {
			t.Fatalf("code = %v, want Unauthenticated", got)
		}
		if len(audit.events) != 1 {
			t.Fatalf("want exactly one audit event, got %d", len(audit.events))
		}
		if got, want := AuditEventIdFromError(err), audit.events[0].CorrelationId; got != want {
			t.Errorf("audit id = %q, want %q", got, want)
		}
	})
}

// TestRefusalAuditIdIsIdenticalOnBothPaths is the issue's acceptance criterion
// stated as a test: "behaviour and docs agree, on both the streamed and unary
// paths".
//
// The two surfaces carry the id by DIFFERENT mechanisms -- a gRPC status
// detail on the unary path, an envelope field on the streamed one, because a
// multiplexed stream has no status to hang a detail off. That is exactly the
// kind of split where one side quietly stops working, so the test drives the
// same refusal through both and requires the same id out of each.
func TestRefusalAuditIdIsIdenticalOnBothPaths(t *testing.T) {
	for _, c := range parityCases() {
		// reader is denied by every row of the matrix.
		ctx := ctxWithRole(auth.RoleReader)
		t.Run(c.rpc, func(t *testing.T) {
			unarySvc, unaryAudit, _ := newParityService(t)
			unaryErr := c.unary(ctx, unarySvc)
			if got := status.Code(unaryErr); got != codes.PermissionDenied {
				t.Fatalf("precondition: unary %s = %v, want PermissionDenied", c.rpc, got)
			}
			unaryID := AuditEventIdFromError(unaryErr)

			streamSvc, streamAudit, _ := newParityService(t)
			res := Dispatch(ctx, streamSvc, c.stream())
			if got := codes.Code(res.GetErrorCode()); got != codes.PermissionDenied {
				t.Fatalf("precondition: streamed %s = %v, want PermissionDenied", c.rpc, got)
			}
			streamID := res.GetAuditEventId()

			if unaryID == "" {
				t.Errorf("%s: unary refusal carries no audit id", c.rpc)
			}
			if streamID == "" {
				t.Errorf("%s: streamed refusal carries no audit id -- DeployControlResult.audit_event_id is empty", c.rpc)
			}
			// Each path wrote its OWN event (separate services), so the ids
			// differ between them by construction. What must match is each
			// path's id against the event that path emitted.
			if len(unaryAudit.events) != 1 || unaryID != unaryAudit.events[0].CorrelationId {
				t.Errorf("%s: unary id %q does not name its emitted event %+v", c.rpc, unaryID, unaryAudit.events)
			}
			if len(streamAudit.events) != 1 || streamID != streamAudit.events[0].CorrelationId {
				t.Errorf("%s: streamed id %q does not name its emitted event %+v", c.rpc, streamID, streamAudit.events)
			}
			// A refusal that carries an id must still not look like a result.
			if res.GetOk() || res.GetResult() != nil {
				t.Errorf("%s: refusal carried a result: ok=%v result=%T", c.rpc, res.GetOk(), res.GetResult())
			}
		})
	}
}

// TestNonRefusalFailuresCarryNoAuditId is the negative half, and it is load
// bearing: an empty audit_event_id has to MEAN "no event was written", or a
// consumer cannot tell a silent drop from an honest absence.
//
// Two shapes qualify. An argument rejection runs BEFORE the gate on several
// RPCs, so it is refused without an actor ever being resolved and without an
// audit event. A node with no service never reached the gate at all.
func TestNonRefusalFailuresCarryNoAuditId(t *testing.T) {
	t.Run("invalid argument is not audited", func(t *testing.T) {
		svc, audit, _ := newParityService(t)

		res := Dispatch(ctxWithRole(auth.RoleOwner), svc, &memqlv1.DeployControlMsg{
			Request: &memqlv1.DeployControlMsg_Promote{Promote: &memqlv1.PromoteRequest{Version: ""}},
		})
		if got := codes.Code(res.GetErrorCode()); got != codes.InvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument", got)
		}
		if len(audit.events) != 0 {
			t.Fatalf("an argument rejection must write no audit event, got %+v", audit.events)
		}
		if res.GetAuditEventId() != "" {
			t.Errorf("audit_event_id = %q, want empty -- nothing was audited", res.GetAuditEventId())
		}
	})

	t.Run("no service on this node", func(t *testing.T) {
		res := Dispatch(context.Background(), nil, &memqlv1.DeployControlMsg{
			Request: &memqlv1.DeployControlMsg_Promote{Promote: &memqlv1.PromoteRequest{Version: "1.0.0"}},
		})
		if got := codes.Code(res.GetErrorCode()); got != codes.Unimplemented {
			t.Fatalf("code = %v, want Unimplemented", got)
		}
		if res.GetAuditEventId() != "" {
			t.Errorf("audit_event_id = %q, want empty", res.GetAuditEventId())
		}
	})

	t.Run("AuditEventIdFromError on a plain error", func(t *testing.T) {
		if got := AuditEventIdFromError(nil); got != "" {
			t.Errorf("nil error = %q, want empty", got)
		}
		if got := AuditEventIdFromError(status.Error(codes.Internal, "boom")); got != "" {
			t.Errorf("detail-less status = %q, want empty", got)
		}
		if got := AuditEventIdFromError(context.Canceled); got != "" {
			t.Errorf("non-status error = %q, want empty", got)
		}
	})
}

// TestPermittedActionsKeepTheirIdOnActionResult guards the direction #3334
// must not disturb. The permitted path has always returned its audit id on
// ActionResult.audit_event_id; the new envelope field is for refusals only and
// must stay empty on success, or a consumer reading it would see two ids for
// one call and have to guess which is authoritative.
func TestPermittedActionsKeepTheirIdOnActionResult(t *testing.T) {
	for _, c := range parityCases() {
		if !c.action {
			continue
		}
		t.Run(c.rpc, func(t *testing.T) {
			svc, audit, _ := newParityService(t)
			res := Dispatch(ctxWithRole(auth.RoleOwner), svc, c.stream())
			if !res.GetOk() {
				t.Fatalf("owner %s: ok=false %q", c.rpc, res.GetErrorMessage())
			}
			if got := res.GetAuditEventId(); got != "" {
				t.Errorf("%s: envelope audit_event_id = %q on a PERMITTED call, want empty "+
					"(the id lives on ActionResult.audit_event_id there)", c.rpc, got)
			}
			if len(audit.events) != 1 {
				t.Fatalf("%s: want one audit event, got %d", c.rpc, len(audit.events))
			}
			if got, want := res.GetAction().GetAuditEventId(), audit.events[0].CorrelationId; got != want {
				t.Errorf("%s: ActionResult.audit_event_id = %q, want %q", c.rpc, got, want)
			}
		})
	}
}
