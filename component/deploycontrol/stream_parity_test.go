package deploycontrol

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
)

// stream_parity_test.go is the security test for the #3311 bridge.
//
// Bridging DeployControlService onto MemqlService.Stream adds a SECOND way to
// reach an owner-only action. If the two surfaces ever enforce the role matrix
// differently, the bridge is a privilege-escalation hole: a caller who cannot
// roll back over the unary service reaches the same rollback through the
// WebSocket. The bridge is built so that cannot happen -- Dispatch calls the
// same DeployControlServiceServer methods the unary path serves, so there is
// one gate rather than two that agree today -- and these tests are what keeps
// it that way as RPCs are added.
//
// The locked role matrix (epic #1871 / #1876), read off service.go's gate
// helpers:
//
//	suggest / cut_version / deploy       -> owner, admin, developer  (authorizeDeploy)
//	get_status / deploy_staging /
//	  promote / rollback / rollout_action -> owner, admin            (authorize)
//	rollback_deployment                  -> owner ONLY, not even admin (authorizeOwner)
//
// Note the "get_status" row. Earlier summary tables (issues #3311 / #3312 and
// deployment-console.md) said any role may view; the SHIPPED unary gate on
// GetDeploymentStatus has been owner/admin since #728, and #3311 preserved it
// rather than loosening a read gate on a deploy-control surface. memql#3332
// settled that in favour of the CODE and corrected the docs, so this table and
// the operator guide now agree with what runs. Do not "fix" the gate to match
// an old table: loosening it is a deliberate product decision to make on BOTH
// paths at once, and TestGetDeploymentStatusStaysOwnerAdminOnBothPaths is here
// to make sure it cannot happen by accident.

// allRoles is the full role spectrum the gate discriminates over. Every role
// not listed in a case's allowed set must be denied, on both paths.
var allRoles = []auth.Role{
	auth.RoleOwner,
	auth.RoleAdmin,
	auth.RoleDeveloper,
	auth.RoleWriter,
	auth.RoleReader,
}

// parityCase is one bridged RPC: the streamed envelope, the equivalent unary
// call, the roles the gate admits, and the audit verb the action stamps.
type parityCase struct {
	// rpc is the DeployControlServiceServer method name. It is matched
	// against the interface's method set by TestEveryRpcHasAParityCase, so an
	// RPC added to the service without a case here fails the suite rather
	// than shipping ungated.
	rpc string
	// stream builds the bridged envelope for this RPC.
	stream func() *memqlv1.DeployControlMsg
	// unary invokes the same RPC directly on the service, the way a dialed
	// gRPC client reaches it.
	unary func(ctx context.Context, svc *Service) error
	// allowed is the set of roles the gate admits.
	allowed []auth.Role
	// auditVerb is the deployment_console_<verb> suffix the action stamps.
	// Empty for the two reads, which are not audited on success.
	auditVerb string
	// action is true for the seven write RPCs -- the ones that must return an
	// audit event id in ActionResult.
	action bool

	// invalidStream / invalidUnary build the SAME RPC carrying an argument the
	// request parser rejects. They exist for the four RPCs memql#3457
	// re-ordered -- DeployStaging, Promote, Rollback, RolloutAction -- and
	// drive TestBelowFloorCallerWithAnInvalidArgumentIsRefusedAndAudited.
	//
	// Nil elsewhere, deliberately and NOT as an oversight. The remaining five
	// RPCs still validate before their gate; #3457 scoped itself to the four
	// whose ordering it changed, so a nil here records "out of this issue's
	// scope", not "already correct". TestBelowFloorInvalidArgumentCoverage
	// pins the count so the set cannot silently shrink.
	invalidStream func() *memqlv1.DeployControlMsg
	invalidUnary  func(ctx context.Context, svc *Service) error
}

func parityCases() []parityCase {
	// Arguments are deliberately VALID everywhere. Several RPCs validate
	// argument shape BEFORE the auth gate, so a malformed request would be
	// rejected with InvalidArgument and the test would pass while testing
	// nothing about the gate.
	ownerAdmin := []auth.Role{auth.RoleOwner, auth.RoleAdmin}
	deployTier := []auth.Role{auth.RoleOwner, auth.RoleAdmin, auth.RoleDeveloper}
	ownerOnly := []auth.Role{auth.RoleOwner}

	return []parityCase{
		{
			rpc: "GetDeploymentStatus",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_GetDeploymentStatus{
					GetDeploymentStatus: &memqlv1.GetDeploymentStatusRequest{Env: "staging"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.GetDeploymentStatus(ctx, &memqlv1.GetDeploymentStatusRequest{Env: "staging"})
				return err
			},
			allowed:   ownerAdmin,
			auditVerb: "get_status",
		},
		{
			rpc: "SuggestNextVersion",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_SuggestNextVersion{
					SuggestNextVersion: &memqlv1.SuggestNextVersionRequest{Env: "staging"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.SuggestNextVersion(ctx, &memqlv1.SuggestNextVersionRequest{Env: "staging"})
				return err
			},
			allowed:   deployTier,
			auditVerb: "suggest_version",
		},
		{
			rpc: "DeployStaging",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_DeployStaging{
					DeployStaging: &memqlv1.DeployStagingRequest{Version: "1.2.3"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.DeployStaging(ctx, &memqlv1.DeployStagingRequest{Version: "1.2.3"})
				return err
			},
			allowed:   ownerAdmin,
			auditVerb: "deploy_staging",
			action:    true,
			// An empty version is the shape the request parser rejects.
			invalidStream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_DeployStaging{
					DeployStaging: &memqlv1.DeployStagingRequest{},
				}}
			},
			invalidUnary: func(ctx context.Context, svc *Service) error {
				_, err := svc.DeployStaging(ctx, &memqlv1.DeployStagingRequest{})
				return err
			},
		},
		{
			rpc: "Promote",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_Promote{
					Promote: &memqlv1.PromoteRequest{Version: "1.2.3"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.Promote(ctx, &memqlv1.PromoteRequest{Version: "1.2.3"})
				return err
			},
			allowed:   ownerAdmin,
			auditVerb: "promote",
			action:    true,
			invalidStream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_Promote{
					Promote: &memqlv1.PromoteRequest{},
				}}
			},
			invalidUnary: func(ctx context.Context, svc *Service) error {
				_, err := svc.Promote(ctx, &memqlv1.PromoteRequest{})
				return err
			},
		},
		{
			rpc: "Rollback",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_Rollback{
					Rollback: &memqlv1.RollbackRequest{Env: "staging", CommitSha: "abc1234"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.Rollback(ctx, &memqlv1.RollbackRequest{Env: "staging", CommitSha: "abc1234"})
				return err
			},
			allowed:   ownerAdmin,
			auditVerb: "rollback",
			action:    true,
			// An env outside {staging, prod}: rejected by the same validator
			// that runs on a permitted caller, with a valid sha beside it so
			// the env is unambiguously what fails.
			invalidStream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_Rollback{
					Rollback: &memqlv1.RollbackRequest{Env: "nowhere", CommitSha: "abc1234"},
				}}
			},
			invalidUnary: func(ctx context.Context, svc *Service) error {
				_, err := svc.Rollback(ctx, &memqlv1.RollbackRequest{Env: "nowhere", CommitSha: "abc1234"})
				return err
			},
		},
		{
			rpc: "RolloutAction",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_RolloutAction{
					RolloutAction: &memqlv1.RolloutActionRequest{Env: "staging", Rollout: "bff", Action: "promote"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.RolloutAction(ctx, &memqlv1.RolloutActionRequest{Env: "staging", Rollout: "bff", Action: "promote"})
				return err
			},
			allowed:   ownerAdmin,
			auditVerb: "rollout_action",
			action:    true,
			// The third of RolloutAction's three validators: an action that is
			// neither promote nor abort.
			invalidStream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_RolloutAction{
					RolloutAction: &memqlv1.RolloutActionRequest{Env: "staging", Rollout: "bff", Action: "bogus"},
				}}
			},
			invalidUnary: func(ctx context.Context, svc *Service) error {
				_, err := svc.RolloutAction(ctx, &memqlv1.RolloutActionRequest{Env: "staging", Rollout: "bff", Action: "bogus"})
				return err
			},
		},
		{
			rpc: "CutVersion",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_CutVersion{
					CutVersion: &memqlv1.CutVersionRequest{Env: "staging", Bump: "patch"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.CutVersion(ctx, &memqlv1.CutVersionRequest{Env: "staging", Bump: "patch"})
				return err
			},
			allowed:   deployTier,
			auditVerb: "cut_version",
			action:    true,
		},
		{
			rpc: "Deploy",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_Deploy{
					Deploy: &memqlv1.DeployRequest{DeploymentId: "d1"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.Deploy(ctx, &memqlv1.DeployRequest{DeploymentId: "d1"})
				return err
			},
			allowed:   deployTier,
			auditVerb: "deploy",
			action:    true,
		},
		{
			rpc: "RollbackDeployment",
			stream: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_RollbackDeployment{
					RollbackDeployment: &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "d1"},
				}}
			},
			unary: func(ctx context.Context, svc *Service) error {
				_, err := svc.RollbackDeployment(ctx, &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "d1"})
				return err
			},
			allowed:   ownerOnly,
			auditVerb: "rollback_deployment",
			action:    true,
		},
	}
}

func roleAllowed(c parityCase, role auth.Role) bool {
	for _, r := range c.allowed {
		if r == role {
			return true
		}
	}
	return false
}

// newParityService builds a fresh service per invocation so the two paths
// never observe each other's side effects. The deployment fixture is a
// SUCCEEDED record so the allowed-role legs of the parity check reach real
// work rather than short-circuiting on "not found" -- the denied legs never
// get that far, which is itself part of what is asserted.
func newParityService(t *testing.T) (*Service, *fakeAudit, *fakeEngine) {
	t.Helper()
	audit := &fakeAudit{}
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "d1", "status": "succeeded", "version": "1.2.0",
			"imageDigest": "sha256:abc", "provider": "azure", "environment": "staging",
		}),
	}}
	svc := newTestServiceWithEngine(t, &fakeExecutor{promoteOut: "SUCCESS"}, audit, eng)
	return svc, audit, eng
}

// TestStreamedAndUnaryGateParity is THE test the bridge exists to be safe
// under: for every RPC and every role, the streamed path and the unary path
// return the SAME gRPC code -- and specifically PermissionDenied for every
// role the matrix denies.
//
// Comparing the full role cross-product rather than only the denial cases is
// deliberate. A bridge that denied everyone would pass a denial-only check
// while being useless; a bridge that admitted everyone fails the denial rows.
// Requiring the codes to match in both directions pins the surfaces together.
func TestStreamedAndUnaryGateParity(t *testing.T) {
	for _, c := range parityCases() {
		for _, role := range allRoles {
			t.Run(c.rpc+"/"+string(role), func(t *testing.T) {
				ctx := ctxWithRole(role)

				unarySvc, _, _ := newParityService(t)
				unaryCode := status.Code(c.unary(ctx, unarySvc))

				streamSvc, _, _ := newParityService(t)
				res := Dispatch(ctx, streamSvc, c.stream())
				streamCode := codes.Code(res.GetErrorCode())

				if unaryCode != streamCode {
					t.Fatalf("gate drift on %s for role %s: unary = %v, streamed = %v (%q)",
						c.rpc, role, unaryCode, streamCode, res.GetErrorMessage())
				}

				if roleAllowed(c, role) {
					if streamCode == codes.PermissionDenied || streamCode == codes.Unauthenticated {
						t.Fatalf("%s must ADMIT role %s, got %v", c.rpc, role, streamCode)
					}
					return
				}
				// The denial assertion, stated explicitly rather than inferred
				// from the equality above: a denied role gets PermissionDenied,
				// on both paths.
				if unaryCode != codes.PermissionDenied {
					t.Errorf("%s unary must DENY role %s with PermissionDenied, got %v", c.rpc, role, unaryCode)
				}
				if streamCode != codes.PermissionDenied {
					t.Errorf("%s streamed must DENY role %s with PermissionDenied, got %v", c.rpc, role, streamCode)
				}
				// A denied bridged call must never look like a result.
				if res.GetOk() || res.GetResult() != nil {
					t.Errorf("%s streamed denial for %s carried a result: ok=%v result=%T",
						c.rpc, role, res.GetOk(), res.GetResult())
				}
			})
		}
	}
}

// TestGetDeploymentStatusStaysOwnerAdminOnBothPaths pins the one row of the
// matrix that has been argued about (memql#3332).
//
// The docs and issues #3311 / #3312 said "View: any" for two releases while
// the code said owner/admin. The decision recorded in #3332 is that the CODE
// is authoritative -- loosening a read gate on a deploy-control surface is the
// dangerous direction, and #728 tightened it deliberately -- and the docs were
// corrected to match. This test is what stops the next reader of an old table
// from "fixing" the code back: it names the roles explicitly rather than
// deriving them from parityCases, so relaxing the gate cannot be done by
// editing one `allowed` slice and watching everything stay green.
//
// It asserts on BOTH transports, because parity is the property the bridge
// exists to preserve: admitting a developer on the stream while the unary
// service refuses one is precisely the privilege-escalation hole #3311 was
// built to be free of.
func TestGetDeploymentStatusStaysOwnerAdminOnBothPaths(t *testing.T) {
	statusMsg := func() *memqlv1.DeployControlMsg {
		return &memqlv1.DeployControlMsg{Request: &memqlv1.DeployControlMsg_GetDeploymentStatus{
			GetDeploymentStatus: &memqlv1.GetDeploymentStatusRequest{Env: "staging"},
		}}
	}

	t.Run("refused", func(t *testing.T) {
		// Developer is the load-bearing case: developer MAY cut and deploy,
		// so "can act" does not imply "can read the status".
		for _, role := range []auth.Role{auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader} {
			t.Run(string(role), func(t *testing.T) {
				unarySvc, _, unaryEng := newParityService(t)
				_, err := unarySvc.GetDeploymentStatus(ctxWithRole(role), &memqlv1.GetDeploymentStatusRequest{Env: "staging"})
				if got := status.Code(err); got != codes.PermissionDenied {
					t.Errorf("unary GetDeploymentStatus for %s = %v, want PermissionDenied", role, got)
				}

				streamSvc, _, streamEng := newParityService(t)
				res := Dispatch(ctxWithRole(role), streamSvc, statusMsg())
				if got := codes.Code(res.GetErrorCode()); got != codes.PermissionDenied {
					t.Errorf("streamed GetDeploymentStatus for %s = %v, want PermissionDenied", role, got)
				}
				if res.GetOk() || res.GetDeploymentStatus() != nil {
					t.Errorf("streamed denial for %s leaked a status payload: ok=%v result=%T",
						role, res.GetOk(), res.GetResult())
				}
				// Fails closed: a refused read never reaches the overlay or
				// kubectl, so there is nothing to leak even by timing.
				if len(unaryEng.queries) != 0 || len(streamEng.queries) != 0 {
					t.Errorf("refused read touched the engine: unary=%v streamed=%v",
						unaryEng.queries, streamEng.queries)
				}
			})
		}
	})

	t.Run("admitted", func(t *testing.T) {
		// The other half: a gate that refused everyone would satisfy the
		// denial rows above while making the console useless.
		for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
			t.Run(string(role), func(t *testing.T) {
				unarySvc, _, _ := newParityService(t)
				_, err := unarySvc.GetDeploymentStatus(ctxWithRole(role), &memqlv1.GetDeploymentStatusRequest{Env: "staging"})
				if got := status.Code(err); got == codes.PermissionDenied || got == codes.Unauthenticated {
					t.Errorf("unary GetDeploymentStatus must ADMIT %s, got %v", role, got)
				}

				streamSvc, _, _ := newParityService(t)
				res := Dispatch(ctxWithRole(role), streamSvc, statusMsg())
				if got := codes.Code(res.GetErrorCode()); got == codes.PermissionDenied || got == codes.Unauthenticated {
					t.Errorf("streamed GetDeploymentStatus must ADMIT %s, got %v (%q)",
						role, got, res.GetErrorMessage())
				}
			})
		}
	})
}

// TestStreamedDenialEmitsBlockedAuditLikeUnary checks the other half of the
// gate's contract: a denial is RECORDED. The unary path emits exactly one
// blocked admin-category audit event on refusal, and the bridged path must
// emit the identical event -- otherwise a WebSocket caller could probe the
// deploy surface without leaving the trail a dialed caller leaves.
func TestStreamedDenialEmitsBlockedAuditLikeUnary(t *testing.T) {
	for _, c := range parityCases() {
		// reader is denied by every case in the matrix.
		ctx := ctxWithRole(auth.RoleReader)
		t.Run(c.rpc, func(t *testing.T) {
			unarySvc, unaryAudit, unaryEng := newParityService(t)
			if code := status.Code(c.unary(ctx, unarySvc)); code != codes.PermissionDenied {
				t.Fatalf("precondition: unary %s for reader = %v, want PermissionDenied", c.rpc, code)
			}

			streamSvc, streamAudit, streamEng := newParityService(t)
			if res := Dispatch(ctx, streamSvc, c.stream()); codes.Code(res.GetErrorCode()) != codes.PermissionDenied {
				t.Fatalf("precondition: streamed %s for reader = %v, want PermissionDenied",
					c.rpc, codes.Code(res.GetErrorCode()))
			}

			if len(unaryAudit.events) != 1 || len(streamAudit.events) != 1 {
				t.Fatalf("want exactly one audit event per path, got unary=%d streamed=%d",
					len(unaryAudit.events), len(streamAudit.events))
			}
			u, s := unaryAudit.events[0], streamAudit.events[0]
			if u.Outcome != identity.AuditOutcomeBlocked || s.Outcome != identity.AuditOutcomeBlocked {
				t.Errorf("outcomes = unary %q / streamed %q, want blocked on both", u.Outcome, s.Outcome)
			}
			if u.Action != s.Action {
				t.Errorf("audit action drift: unary %q vs streamed %q", u.Action, s.Action)
			}
			if want := "deployment_console_" + c.auditVerb; s.Action != want {
				t.Errorf("streamed audit action = %q, want %q", s.Action, want)
			}
			if s.Category != identity.AuditCategoryAdmin {
				t.Errorf("streamed audit category = %q, want %q", s.Category, identity.AuditCategoryAdmin)
			}
			if s.ActorRole != string(auth.RoleReader) {
				t.Errorf("streamed audit actorRole = %q, want reader", s.ActorRole)
			}
			// The gate fails CLOSED: a denied call never reaches the engine on
			// either path. This is what makes the denial a refusal rather than
			// a rollback-after-the-fact.
			if len(unaryEng.queries) != 0 || len(streamEng.queries) != 0 {
				t.Errorf("denied call touched the engine: unary=%v streamed=%v",
					unaryEng.queries, streamEng.queries)
			}
		})
	}
}

// TestBelowFloorCallerWithAnInvalidArgumentIsRefusedAndAudited closes the one
// hole in "every denied attempt is audited" (memql#3457).
//
// Four RPCs used to validate their arguments BEFORE calling the gate, so a
// caller below the role floor who sent a bad argument got INVALID_ARGUMENT and
// left NO trail. The attempt achieved nothing -- the argument was invalid, so
// no deployment moved -- which is exactly why it was easy to leave alone. But
// the value of an audit trail on a privileged surface is that it records
// ATTEMPTS, and a below-floor caller hammering Rollback with junk is precisely
// the pattern the trail exists to surface. It was also the failure mode nobody
// could see: the caller read a plain argument error and the admin reading the
// trail read nothing at all.
//
// Gate-first is the safer order on its own terms too -- an unauthorized caller
// now learns nothing from the shape of the argument parser.
//
// Both surfaces are asserted, because #3311's parity property is what makes the
// bridge safe: a fix that reached only the unary path would leave the browser
// path (which is the one an operator actually uses) unaudited.
func TestBelowFloorCallerWithAnInvalidArgumentIsRefusedAndAudited(t *testing.T) {
	for _, c := range parityCases() {
		if c.invalidStream == nil {
			continue
		}
		t.Run(c.rpc, func(t *testing.T) {
			// Precondition, and the row of #3457's table that must NOT move: a
			// caller AT or above the floor still gets InvalidArgument for the
			// same request, and still writes no audit event (the gate audits
			// denials, not admissions).
			okSvc, okAudit, _ := newParityService(t)
			if code := status.Code(c.invalidUnary(ctxWithRole(auth.RoleOwner), okSvc)); code != codes.InvalidArgument {
				t.Fatalf("owner + invalid argument on %s = %v, want InvalidArgument still", c.rpc, code)
			}
			if len(okAudit.events) != 0 {
				t.Errorf("an admitted caller's argument error wrote %d audit events, want 0", len(okAudit.events))
			}

			// reader is denied by every case in the matrix.
			ctx := ctxWithRole(auth.RoleReader)

			unarySvc, unaryAudit, unaryEng := newParityService(t)
			unaryErr := c.invalidUnary(ctx, unarySvc)
			if code := status.Code(unaryErr); code != codes.PermissionDenied {
				t.Fatalf("unary %s for a below-floor caller with a bad argument = %v, want PermissionDenied "+
					"(the gate must run before the argument parser)", c.rpc, code)
			}

			streamSvc, streamAudit, streamEng := newParityService(t)
			res := Dispatch(ctx, streamSvc, c.invalidStream())
			if code := codes.Code(res.GetErrorCode()); code != codes.PermissionDenied {
				t.Fatalf("streamed %s for a below-floor caller with a bad argument = %v, want PermissionDenied",
					c.rpc, code)
			}
			if res.GetOk() || res.GetResult() != nil {
				t.Errorf("streamed refusal carried a result: ok=%v result=%T", res.GetOk(), res.GetResult())
			}

			// The point of the whole exercise: the attempt is RECORDED, once,
			// on both surfaces, attributed to the caller who made it.
			if len(unaryAudit.events) != 1 || len(streamAudit.events) != 1 {
				t.Fatalf("want exactly one blocked audit event per surface, got unary=%d streamed=%d",
					len(unaryAudit.events), len(streamAudit.events))
			}
			u, s := unaryAudit.events[0], streamAudit.events[0]
			if u.Outcome != identity.AuditOutcomeBlocked || s.Outcome != identity.AuditOutcomeBlocked {
				t.Errorf("outcomes = unary %q / streamed %q, want blocked on both", u.Outcome, s.Outcome)
			}
			if want := "deployment_console_" + c.auditVerb; u.Action != want || s.Action != want {
				t.Errorf("audit action = unary %q / streamed %q, want %q", u.Action, s.Action, want)
			}
			if u.ActorRole != string(auth.RoleReader) || s.ActorRole != string(auth.RoleReader) {
				t.Errorf("audit actorRole = unary %q / streamed %q, want reader", u.ActorRole, s.ActorRole)
			}

			// And the caller can quote it (memql#3334): the refusal is a
			// refusal in full, id included, not a degraded one.
			if got := AuditEventIdFromError(unaryErr); got != u.CorrelationId {
				t.Errorf("unary refusal audit id = %q, want %q", got, u.CorrelationId)
			}
			if got := res.GetAuditEventId(); got != s.CorrelationId {
				t.Errorf("streamed refusal audit id = %q, want %q", got, s.CorrelationId)
			}

			// Fails closed, as the valid-argument denial does.
			if len(unaryEng.queries) != 0 || len(streamEng.queries) != 0 {
				t.Errorf("denied call touched the engine: unary=%v streamed=%v",
					unaryEng.queries, streamEng.queries)
			}
		})
	}
}

// TestBelowFloorInvalidArgumentCoverage pins the SET the test above walks.
//
// Without it the loop degrades silently: drop an invalidStream and the suite
// stays green while testing one fewer RPC. The four are the ones memql#3457
// re-ordered; adding a fifth means fixing that RPC's ordering too, and this
// count is what forces the two to move together.
func TestBelowFloorInvalidArgumentCoverage(t *testing.T) {
	want := map[string]bool{
		"DeployStaging": true, "Promote": true, "Rollback": true, "RolloutAction": true,
	}
	got := map[string]bool{}
	for _, c := range parityCases() {
		if c.invalidStream == nil {
			if c.invalidUnary != nil {
				t.Errorf("%s declares invalidUnary without invalidStream; the pair must move together", c.rpc)
			}
			continue
		}
		if c.invalidUnary == nil {
			t.Errorf("%s declares invalidStream without invalidUnary; the pair must move together", c.rpc)
		}
		got[c.rpc] = true
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("gate-before-validation coverage = %v, want %v", got, want)
	}
}

// TestStreamedActionsReturnAuditEventId asserts the bridged write path returns
// the audit id the unary path returns. The console shows it to the operator
// and support correlates on it; a bridge that ran the action but dropped the
// id would be silently lossy.
func TestStreamedActionsReturnAuditEventId(t *testing.T) {
	for _, c := range parityCases() {
		if !c.action {
			continue
		}
		// Every action admits owner, so one role covers the whole set.
		t.Run(c.rpc, func(t *testing.T) {
			svc, audit, _ := newParityService(t)
			res := Dispatch(ctxWithRole(auth.RoleOwner), svc, c.stream())
			if !res.GetOk() {
				t.Fatalf("owner %s: ok=false code=%v %q", c.rpc, codes.Code(res.GetErrorCode()), res.GetErrorMessage())
			}
			action := res.GetAction()
			if action == nil {
				t.Fatalf("%s: streamed result carried no ActionResult (got %T)", c.rpc, res.GetResult())
			}
			if action.GetAuditEventId() == "" {
				t.Fatalf("%s: ActionResult.audit_event_id is empty", c.rpc)
			}
			// Exactly one event, and it is the one whose id came back.
			if len(audit.events) != 1 {
				t.Fatalf("%s: want exactly one audit event, got %d", c.rpc, len(audit.events))
			}
			ev := audit.events[0]
			if ev.CorrelationId != action.GetAuditEventId() {
				t.Errorf("%s: audit_event_id %q does not match the emitted event %q",
					c.rpc, action.GetAuditEventId(), ev.CorrelationId)
			}
			if want := "deployment_console_" + c.auditVerb; ev.Action != want {
				t.Errorf("%s: audit action = %q, want %q", c.rpc, ev.Action, want)
			}
			if ev.Category != identity.AuditCategoryAdmin {
				t.Errorf("%s: audit category = %q, want %q", c.rpc, ev.Category, identity.AuditCategoryAdmin)
			}
		})
	}
}

// TestStreamedReadsReturnTheirTypedResult pins the reply oneof: the two reads
// must land in their own result fields, not in the generic action slot. A
// consumer switching on the discriminant would otherwise silently miss them.
func TestStreamedReadsReturnTheirTypedResult(t *testing.T) {
	svc, _, _ := newParityService(t)
	res := Dispatch(ctxWithRole(auth.RoleOwner), svc, &memqlv1.DeployControlMsg{
		RequestId: "req-1",
		Request: &memqlv1.DeployControlMsg_SuggestNextVersion{
			SuggestNextVersion: &memqlv1.SuggestNextVersionRequest{Env: "staging"},
		},
	})
	if !res.GetOk() {
		t.Fatalf("owner SuggestNextVersion: %v %q", codes.Code(res.GetErrorCode()), res.GetErrorMessage())
	}
	if res.GetRequestId() != "req-1" {
		t.Errorf("request_id = %q, want req-1 (the caller correlates on it)", res.GetRequestId())
	}
	if res.GetNextVersion() == nil {
		t.Fatalf("SuggestNextVersion landed in %T, want DeployControlResult_NextVersion", res.GetResult())
	}
	// The fixture's succeeded 1.2.0 staging deployment is the version the
	// proposals are computed from, which also proves the bridged call reached
	// the real service rather than a stub.
	if got := res.GetNextVersion().GetNextPatch(); got != "1.2.1" {
		t.Errorf("nextPatch = %q, want 1.2.1", got)
	}
}

// recordingServer implements DeployControlServiceServer and records which
// method the dispatcher called. It exists so the routing table is checked
// independently of the gate: a case wired to the wrong method (Promote
// dispatched to DeployStaging, say) would pass every gate assertion above,
// because the two share a role tier.
type recordingServer struct {
	memqlv1.UnimplementedDeployControlServiceServer
	called []string
}

func (r *recordingServer) GetDeploymentStatus(context.Context, *memqlv1.GetDeploymentStatusRequest) (*memqlv1.DeploymentStatus, error) {
	r.called = append(r.called, "GetDeploymentStatus")
	return &memqlv1.DeploymentStatus{}, nil
}

func (r *recordingServer) SuggestNextVersion(context.Context, *memqlv1.SuggestNextVersionRequest) (*memqlv1.SuggestNextVersionResult, error) {
	r.called = append(r.called, "SuggestNextVersion")
	return &memqlv1.SuggestNextVersionResult{}, nil
}

func (r *recordingServer) DeployStaging(context.Context, *memqlv1.DeployStagingRequest) (*memqlv1.ActionResult, error) {
	r.called = append(r.called, "DeployStaging")
	return &memqlv1.ActionResult{}, nil
}

func (r *recordingServer) Promote(context.Context, *memqlv1.PromoteRequest) (*memqlv1.ActionResult, error) {
	r.called = append(r.called, "Promote")
	return &memqlv1.ActionResult{}, nil
}

func (r *recordingServer) Rollback(context.Context, *memqlv1.RollbackRequest) (*memqlv1.ActionResult, error) {
	r.called = append(r.called, "Rollback")
	return &memqlv1.ActionResult{}, nil
}

func (r *recordingServer) RolloutAction(context.Context, *memqlv1.RolloutActionRequest) (*memqlv1.ActionResult, error) {
	r.called = append(r.called, "RolloutAction")
	return &memqlv1.ActionResult{}, nil
}

func (r *recordingServer) CutVersion(context.Context, *memqlv1.CutVersionRequest) (*memqlv1.ActionResult, error) {
	r.called = append(r.called, "CutVersion")
	return &memqlv1.ActionResult{}, nil
}

func (r *recordingServer) Deploy(context.Context, *memqlv1.DeployRequest) (*memqlv1.ActionResult, error) {
	r.called = append(r.called, "Deploy")
	return &memqlv1.ActionResult{}, nil
}

func (r *recordingServer) RollbackDeployment(context.Context, *memqlv1.RollbackDeploymentRequest) (*memqlv1.ActionResult, error) {
	r.called = append(r.called, "RollbackDeployment")
	return &memqlv1.ActionResult{}, nil
}

func TestDispatchRoutesEachRequestToItsOwnRpc(t *testing.T) {
	for _, c := range parityCases() {
		t.Run(c.rpc, func(t *testing.T) {
			rec := &recordingServer{}
			res := Dispatch(context.Background(), rec, c.stream())
			if !res.GetOk() {
				t.Fatalf("dispatch %s: ok=false %q", c.rpc, res.GetErrorMessage())
			}
			if len(rec.called) != 1 || rec.called[0] != c.rpc {
				t.Fatalf("dispatch %s routed to %v", c.rpc, rec.called)
			}
		})
	}
}

// TestEveryRpcHasAParityCase is the guard that makes the suite self-extending.
// It reads the DeployControlServiceServer method set off the generated
// interface, so an RPC added to deploy_control.proto WITHOUT a parity case
// here fails immediately -- which is the only way a newly added, ungated RPC
// gets caught before it ships on both surfaces.
func TestEveryRpcHasAParityCase(t *testing.T) {
	iface := reflect.TypeOf((*memqlv1.DeployControlServiceServer)(nil)).Elem()

	var rpcs []string
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		// The generated interface carries the forward-compat embed marker
		// alongside the real RPCs.
		if strings.HasPrefix(name, "mustEmbedUnimplemented") {
			continue
		}
		rpcs = append(rpcs, name)
	}
	// Without this the whole guard passes vacuously if the reflection ever
	// stops seeing methods (a generator change, a renamed embed marker): an
	// empty rpcs list makes "nothing missing" trivially true, which is the
	// exact failure mode a coverage guard must not have.
	if len(rpcs) < 9 {
		t.Fatalf("reflection found only %d DeployControlService RPCs (%v); "+
			"the coverage guard cannot work with an empty method set", len(rpcs), rpcs)
	}

	covered := map[string]bool{}
	for _, c := range parityCases() {
		if covered[c.rpc] {
			t.Errorf("duplicate parity case for %s", c.rpc)
		}
		covered[c.rpc] = true
	}

	var missing []string
	for _, rpc := range rpcs {
		if !covered[rpc] {
			missing = append(missing, rpc)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("DeployControlService RPCs with no parity case: %v.\n"+
			"Every bridged RPC needs one, or the streamed path can ship an ungated action.", missing)
	}

	// The reverse direction too: a case naming an RPC that no longer exists
	// is dead weight that would quietly stop testing anything.
	known := map[string]bool{}
	for _, rpc := range rpcs {
		known[rpc] = true
	}
	for name := range covered {
		if !known[name] {
			t.Errorf("parity case %q names no DeployControlService RPC", name)
		}
	}
}

// TestDispatchRefusesAnEmptyOrUnservedEnvelope covers the two shapes a bridged
// caller can produce that have no RPC behind them. Both must fail loudly: an
// empty ok=true reply would read to a client as "the action ran".
func TestDispatchRefusesAnEmptyOrUnservedEnvelope(t *testing.T) {
	t.Run("no request set", func(t *testing.T) {
		res := Dispatch(context.Background(), &recordingServer{}, &memqlv1.DeployControlMsg{RequestId: "r"})
		if res.GetOk() || codes.Code(res.GetErrorCode()) != codes.InvalidArgument {
			t.Fatalf("empty request: ok=%v code=%v", res.GetOk(), codes.Code(res.GetErrorCode()))
		}
		if res.GetRequestId() != "r" {
			t.Errorf("request_id must survive a rejection, got %q", res.GetRequestId())
		}
	})

	t.Run("no service on this node", func(t *testing.T) {
		res := Dispatch(context.Background(), nil, &memqlv1.DeployControlMsg{
			Request: &memqlv1.DeployControlMsg_Promote{Promote: &memqlv1.PromoteRequest{Version: "1.0.0"}},
		})
		if res.GetOk() || codes.Code(res.GetErrorCode()) != codes.Unimplemented {
			t.Fatalf("nil service: ok=%v code=%v", res.GetOk(), codes.Code(res.GetErrorCode()))
		}
	})
}

// TestDispatchDeniesAnUnauthenticatedCaller pins the fail-closed edge: a
// context with no resolved actor is Unauthenticated, not an accidental pass.
// The bridged surface is reachable from a browser, so "no actor" is a shape a
// real caller can present.
func TestDispatchDeniesAnUnauthenticatedCaller(t *testing.T) {
	for _, c := range parityCases() {
		t.Run(c.rpc, func(t *testing.T) {
			unarySvc, _, _ := newParityService(t)
			unaryCode := status.Code(c.unary(context.Background(), unarySvc))

			streamSvc, _, _ := newParityService(t)
			res := Dispatch(context.Background(), streamSvc, c.stream())
			streamCode := codes.Code(res.GetErrorCode())

			if unaryCode != codes.Unauthenticated || streamCode != codes.Unauthenticated {
				t.Fatalf("%s with no actor: unary = %v, streamed = %v, want Unauthenticated on both",
					c.rpc, unaryCode, streamCode)
			}
		})
	}
}
