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
// The locked role matrix (epic #1871 / #1876):
//
//	view (get_status)                    -> owner, admin
//	suggest / cut version / deploy       -> owner, admin, developer
//	rollback (both flavours)             -> owner ONLY, not even admin
//
// Note the "view" row: the issue's summary table says any role may view, but
// the SHIPPED unary gate on GetDeploymentStatus is owner/admin (#728). Parity
// with the unary path is the property under test, so the table records what
// the code enforces. Loosening the read gate is a deliberate change to make on
// BOTH paths at once -- which, because they share the gate, is one edit.

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
