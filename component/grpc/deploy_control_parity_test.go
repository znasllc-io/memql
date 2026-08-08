package memql

// Parity tests for the streamed DeployControlService bridge (memql#3311).
//
// THE POINT OF THIS FILE. Bridging a role-gated unary service onto an open
// bidirectional stream is a privilege-escalation hole the moment the two paths
// disagree about who may do what. So these tests do not exercise a fake: they
// build the REAL *deploycontrol.Service -- the same type app/ registers as the
// unary DeployControlService -- and drive it through BOTH transports, asserting
// the identical gRPC status code comes back from each.
//
// The test-only import of component/deploycontrol is deliberate and
// direction-correct: deploycontrol sits between `platform` and the servers
// (docs/ci-design.md D3), component/grpc is a server, so the edge points
// downward. It is the same reasoning that put a real-gate test in
// component/identity/admin rather than a fake -- "a fake would assert
// nothing".
//
// The table is keyed by RPC name and checked for completeness against the
// generated DeployControlServiceServer interface, so an RPC added to
// deploy_control.proto without a row here -- i.e. without anyone having
// thought about its gate -- fails the suite.

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
)

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// inertExecutor satisfies deploycontrol.Executor without touching the world.
// The gate rejects before any of these are reached in the denial cases; they
// exist so the admitted cases fail on their own merits (no repo, no cluster)
// rather than shelling out to promote.sh from a unit test.
type inertExecutor struct{}

func (inertExecutor) RunPromote(context.Context, string, string) (string, error) { return "", nil }
func (inertExecutor) RunRollback(context.Context, string, string) (string, error) {
	return "", nil
}
func (inertExecutor) RunRolloutAction(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (inertExecutor) KubectlJSON(context.Context, ...string) ([]byte, error) {
	return []byte("{}"), nil
}
func (inertExecutor) Git(context.Context, ...string) (string, error) { return "", nil }

// recordingAudit captures the audit events the gate emits so the tests can
// assert the blocked event lands (a denial that writes no audit row is a
// silent one).
type recordingAudit struct {
	events []identity.AuditEvent
}

func (r *recordingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	r.events = append(r.events, ev)
}

func newParityService(t *testing.T, audit identity.AuditLogger) *deploycontrol.Service {
	t.Helper()
	svc, err := deploycontrol.NewService(deploycontrol.Options{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:    audit,
		RepoRoot: t.TempDir(),
		Executor: inertExecutor{},
	})
	require.NoError(t, err, "building the real deploy-control service")
	return svc
}

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newDeployControlSession builds a streamSession whose AccessContext is
// pre-seeded with role, wired to the supplied deploy-control service. The
// seeded access short-circuits ensureAccess so no database is needed -- the
// bridge then copies it onto the outgoing context exactly as it does in
// production, which is the plumbing the gate depends on.
func newDeployControlSession(role auth.Role, h DeployControlHandler) (*streamSession, *captureStream) {
	cs := &captureStream{ctx: context.Background()}
	svc := &service{logger: quietTestLogger(), deployControl: h}
	s := &streamSession{
		service: svc,
		stream:  cs,
		logger:  svc.logger,
		access:  &auth.AccessContext{UserId: "v1:identity:user.u1", Role: role},
	}
	s.accessLoaded = true
	return s, cs
}

// unaryCtx mirrors what the unary interceptor stamps for a caller of `role`.
func unaryCtx(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user.u1",
		Role:   role,
	})
}

// -----------------------------------------------------------------------------
// The RPC table
// -----------------------------------------------------------------------------

// gatedRPC describes one DeployControlService RPC for the parity table:
// a VALID request (so the call reaches the gate rather than tripping the
// argument validation that runs before it), the way to invoke it on each
// transport, and the roles the locked matrix denies.
type gatedRPC struct {
	// name must match the generated DeployControlServiceServer method name;
	// the completeness check keys off it.
	name string
	// request is the streamed envelope arm. The unary call re-uses the same
	// request message, so the two paths cannot be handed different inputs.
	request *memqlv1.DeployControlMsg
	// callUnary invokes the RPC directly on the service, returning only the
	// error -- which is the whole comparison surface for the gate.
	callUnary func(context.Context, *deploycontrol.Service, *memqlv1.DeployControlMsg) error
	// deniedRoles are the roles the locked matrix (epic #1871 / memql#1876)
	// must reject with PermissionDenied on BOTH transports.
	deniedRoles []auth.Role
}

// readerAndWriter is the "non-admin" set every deploy RPC must reject: the two
// roles with no engineering or admin power at all.
var readerAndWriter = []auth.Role{auth.RoleWriter, auth.RoleReader}

// deployControlParityTable enumerates every gated RPC.
//
// Denied-role sets encode the LIVE gate, tier by tier:
//   - view (GetDeploymentStatus): admin+ -- so developer is denied too.
//   - cut / deploy (SuggestNextVersion, CutVersion, Deploy): developer+.
//   - the legacy console actions (DeployStaging, Promote, Rollback,
//     RolloutAction): admin+.
//   - RollbackDeployment: OWNER ONLY -- not even admin, per the locked matrix.
func deployControlParityTable() []gatedRPC {
	return []gatedRPC{
		{
			name: "GetDeploymentStatus",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-status",
				Request: &memqlv1.DeployControlMsg_GetDeploymentStatus{
					GetDeploymentStatus: &memqlv1.GetDeploymentStatusRequest{Env: "prod"},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.GetDeploymentStatus(ctx, m.GetGetDeploymentStatus())
				return err
			},
			// The read gate is admin+, so a developer is denied here even
			// though the same developer may cut and deploy.
			deniedRoles: append([]auth.Role{auth.RoleDeveloper}, readerAndWriter...),
		},
		{
			name: "SuggestNextVersion",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-suggest",
				Request: &memqlv1.DeployControlMsg_SuggestNextVersion{
					SuggestNextVersion: &memqlv1.SuggestNextVersionRequest{Env: "prod"},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.SuggestNextVersion(ctx, m.GetSuggestNextVersion())
				return err
			},
			deniedRoles: readerAndWriter,
		},
		{
			name: "DeployStaging",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-deploy-staging",
				Request: &memqlv1.DeployControlMsg_DeployStaging{
					DeployStaging: &memqlv1.DeployStagingRequest{Version: "1.2.3"},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.DeployStaging(ctx, m.GetDeployStaging())
				return err
			},
			deniedRoles: append([]auth.Role{auth.RoleDeveloper}, readerAndWriter...),
		},
		{
			name: "Promote",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-promote",
				Request: &memqlv1.DeployControlMsg_Promote{
					Promote: &memqlv1.PromoteRequest{Version: "1.2.3"},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.Promote(ctx, m.GetPromote())
				return err
			},
			deniedRoles: append([]auth.Role{auth.RoleDeveloper}, readerAndWriter...),
		},
		{
			name: "Rollback",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-rollback",
				Request: &memqlv1.DeployControlMsg_Rollback{
					Rollback: &memqlv1.RollbackRequest{Env: "prod", CommitSha: "deadbeef"},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.Rollback(ctx, m.GetRollback())
				return err
			},
			deniedRoles: append([]auth.Role{auth.RoleDeveloper}, readerAndWriter...),
		},
		{
			name: "RolloutAction",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-rollout",
				Request: &memqlv1.DeployControlMsg_RolloutAction{
					RolloutAction: &memqlv1.RolloutActionRequest{Env: "prod", Rollout: "bff", Action: "promote"},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.RolloutAction(ctx, m.GetRolloutAction())
				return err
			},
			deniedRoles: append([]auth.Role{auth.RoleDeveloper}, readerAndWriter...),
		},
		{
			name: "CutVersion",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-cut",
				Request: &memqlv1.DeployControlMsg_CutVersion{
					CutVersion: &memqlv1.CutVersionRequest{Env: "prod", Bump: "patch"},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.CutVersion(ctx, m.GetCutVersion())
				return err
			},
			deniedRoles: readerAndWriter,
		},
		{
			name: "Deploy",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-deploy",
				Request: &memqlv1.DeployControlMsg_Deploy{
					Deploy: &memqlv1.DeployRequest{DeploymentId: "v1:cluster:deployment.d1"},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.Deploy(ctx, m.GetDeploy())
				return err
			},
			deniedRoles: readerAndWriter,
		},
		{
			name: "RollbackDeployment",
			request: &memqlv1.DeployControlMsg{
				RequestId: "r-rollback-deployment",
				Request: &memqlv1.DeployControlMsg_RollbackDeployment{
					RollbackDeployment: &memqlv1.RollbackDeploymentRequest{
						ToDeploymentId: "v1:cluster:deployment.d0",
					},
				},
			},
			callUnary: func(ctx context.Context, s *deploycontrol.Service, m *memqlv1.DeployControlMsg) error {
				_, err := s.RollbackDeployment(ctx, m.GetRollbackDeployment())
				return err
			},
			// Owner-only: admin AND developer are denied alongside the
			// read-only roles. This is the row that would catch a bridge
			// quietly relaxing rollback to the admin tier.
			deniedRoles: append([]auth.Role{auth.RoleAdmin, auth.RoleDeveloper}, readerAndWriter...),
		},
	}
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// TestDeployControlTableCoversEveryRPC fails when deploy_control.proto grows an
// RPC that nobody added a parity row for. Without it the table is a snapshot of
// whatever existed the day it was written, and the next RPC ships ungated on
// the stream with a green suite.
func TestDeployControlTableCoversEveryRPC(t *testing.T) {
	iface := reflect.TypeOf((*memqlv1.DeployControlServiceServer)(nil)).Elem()
	var declared []string
	for i := 0; i < iface.NumMethod(); i++ {
		name := iface.Method(i).Name
		// The generated interface also carries the embedding guard
		// mustEmbedUnimplemented... which is not an RPC.
		if name == "mustEmbedUnimplementedDeployControlServiceServer" {
			continue
		}
		declared = append(declared, name)
	}
	sort.Strings(declared)

	var covered []string
	for _, rpc := range deployControlParityTable() {
		covered = append(covered, rpc.name)
	}
	sort.Strings(covered)

	assert.Equal(t, declared, covered,
		"every DeployControlService RPC needs a parity row -- an RPC reachable "+
			"over the stream with no test of its gate is a privilege-escalation hole")
}

// TestDeployControlBridgeDispatchesEveryRPC proves the bridge actually reaches
// each RPC. A gate test alone cannot distinguish "denied" from "never
// dispatched", so this drives an admitted (owner) caller through the stream
// handler with a spy and asserts the right method fired.
func TestDeployControlBridgeDispatchesEveryRPC(t *testing.T) {
	for _, rpc := range deployControlParityTable() {
		t.Run(rpc.name, func(t *testing.T) {
			spy := &spyDeployControl{}
			s, cs := newDeployControlSession(auth.RoleOwner, spy)

			require.NoError(t, s.handleDeployControl(
				&memqlv1.MemqlClientMessage{MessageId: "m1"}, rpc.request))

			assert.Equal(t, rpc.name, spy.called, "bridge must dispatch to the named RPC")

			reply := cs.lastSent()
			require.NotNil(t, reply, "bridge must reply")
			res := reply.GetDeployControlResult()
			require.NotNil(t, res, "reply must be a DeployControlResult")
			assert.Equal(t, rpc.name, res.GetRpc())
			assert.Equal(t, rpc.request.GetRequestId(), res.GetRequestId(),
				"request_id must round-trip so a multiplexing client can correlate")
			assert.NotNil(t, res.GetResponse(), "a successful call must fill the response oneof")
		})
	}
}

// TestDeployControlGateParity is the load-bearing test: for every gated RPC and
// every role the matrix denies, the STREAMED path and the UNARY path must both
// answer PermissionDenied -- from the same service instance, so any divergence
// is a bridge bug and not a fixture difference.
func TestDeployControlGateParity(t *testing.T) {
	for _, rpc := range deployControlParityTable() {
		for _, role := range rpc.deniedRoles {
			t.Run(rpc.name+"/"+string(role), func(t *testing.T) {
				// One service instance, both transports. This is what makes
				// the comparison meaningful rather than decorative.
				audit := &recordingAudit{}
				svc := newParityService(t, audit)

				// --- unary path ---
				unaryErr := rpc.callUnary(unaryCtx(role), svc, rpc.request)
				require.Error(t, unaryErr, "unary path must reject %s for %s", rpc.name, role)
				unaryCode := status.Code(unaryErr)

				// --- streamed path ---
				s, cs := newDeployControlSession(role, svc)
				require.NoError(t, s.handleDeployControl(
					&memqlv1.MemqlClientMessage{MessageId: "m1"}, rpc.request))

				reply := cs.lastSent()
				require.NotNil(t, reply, "streamed path must reply")
				assert.Nil(t, reply.GetDeployControlResult(),
					"a denied call must NOT produce a result envelope")
				qe := reply.GetQueryError()
				require.NotNil(t, qe, "streamed denial must land on the QueryError channel")
				streamCode := qe.GetError().GetCode()

				// The parity assertion itself.
				assert.Equal(t, codes.PermissionDenied, unaryCode,
					"%s must be PermissionDenied for %s on the unary path", rpc.name, role)
				assert.Equal(t, codes.PermissionDenied.String(), streamCode,
					"%s must be PermissionDenied for %s on the streamed path", rpc.name, role)
				assert.Equal(t, unaryCode.String(), streamCode,
					"streamed and unary verdicts must be identical for %s / %s", rpc.name, role)

				// Both denials are audited, and identically so: two blocked
				// admin-category events, one per path.
				require.Len(t, audit.events, 2,
					"each denial emits exactly one blocked audit event")
				for _, ev := range audit.events {
					assert.Equal(t, identity.AuditOutcomeBlocked, ev.Outcome)
					assert.Equal(t, identity.AuditCategoryAdmin, ev.Category)
				}
				assert.Equal(t, audit.events[0].Action, audit.events[1].Action,
					"both paths must audit the same verb")
			})
		}
	}
}

// TestDeployControlUnauthenticatedParity: a stream with no resolved access
// fails closed the same way an unauthenticated unary call does. The bridge
// copies whatever AccessContext the session holds onto the outgoing context, so
// "no access" must not read as "no gate".
func TestDeployControlUnauthenticatedParity(t *testing.T) {
	audit := &recordingAudit{}
	svc := newParityService(t, audit)
	req := &memqlv1.DeployControlMsg{
		RequestId: "r-anon",
		Request: &memqlv1.DeployControlMsg_CutVersion{
			CutVersion: &memqlv1.CutVersionRequest{Env: "prod", Bump: "patch"},
		},
	}

	// Unary with a bare context: no actor at all.
	_, unaryErr := svc.CutVersion(context.Background(), req.GetCutVersion())
	require.Error(t, unaryErr)

	// Streamed with a session whose access never resolved.
	cs := &captureStream{ctx: context.Background()}
	s := &streamSession{
		service: &service{logger: quietTestLogger(), deployControl: svc},
		stream:  cs,
		logger:  quietTestLogger(),
	}
	s.accessLoaded = true // access stays nil -> fail closed

	require.NoError(t, s.handleDeployControl(&memqlv1.MemqlClientMessage{MessageId: "m1"}, req))
	qe := cs.lastSent().GetQueryError()
	require.NotNil(t, qe)

	// An AccessContext of zero value still resolves as an actor with an empty
	// role, so the stream lands on PermissionDenied while the actor-less unary
	// call lands on Unauthenticated. Both are refusals with no side effect --
	// what must hold is that neither admits the caller.
	assert.Contains(t, []codes.Code{codes.PermissionDenied, codes.Unauthenticated}, status.Code(unaryErr))
	assert.Contains(t,
		[]string{codes.PermissionDenied.String(), codes.Unauthenticated.String()},
		qe.GetError().GetCode())
}

// TestDeployControlUnwiredNodeReportsUnimplemented: a node that does not host
// DeployControlService (anything but identity) says so, rather than dropping
// the message -- a portal pointed at a bff otherwise hangs with no explanation.
func TestDeployControlUnwiredNodeReportsUnimplemented(t *testing.T) {
	s, cs := newDeployControlSession(auth.RoleOwner, nil)
	req := &memqlv1.DeployControlMsg{
		RequestId: "r1",
		Request: &memqlv1.DeployControlMsg_GetDeploymentStatus{
			GetDeploymentStatus: &memqlv1.GetDeploymentStatusRequest{Env: "prod"},
		},
	}

	require.NoError(t, s.handleDeployControl(&memqlv1.MemqlClientMessage{MessageId: "m1"}, req))

	qe := cs.lastSent().GetQueryError()
	require.NotNil(t, qe)
	assert.Equal(t, codes.Unimplemented.String(), qe.GetError().GetCode())
	assert.Equal(t, "r1", qe.GetRequestId())
}

// TestDeployControlEmptyRequestRejected: an envelope with no request arm is a
// caller error, not a nil-deref.
func TestDeployControlEmptyRequestRejected(t *testing.T) {
	s, cs := newDeployControlSession(auth.RoleOwner, &spyDeployControl{})

	require.NoError(t, s.handleDeployControl(
		&memqlv1.MemqlClientMessage{MessageId: "m1"},
		&memqlv1.DeployControlMsg{RequestId: "r1"}))

	qe := cs.lastSent().GetQueryError()
	require.NotNil(t, qe)
	assert.Equal(t, codes.InvalidArgument.String(), qe.GetError().GetCode())
}

// TestDeployControlIsBadgeRestricted: a live badge grant (shared-terminal
// operator credential) must not be able to drive the deploy surface. Deploy
// control is cluster state whose effects outlive the grant's TTL, so it belongs
// in the same restricted set as credential + session management.
func TestDeployControlIsBadgeRestricted(t *testing.T) {
	s, _ := newDeployControlSession(auth.RoleOwner, &spyDeployControl{})
	s.badgeStamped = true
	s.badgeExpiresAt = time.Now().Add(time.Hour)

	verdict := s.badgeGate(&memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_DeployControl{
			DeployControl: &memqlv1.DeployControlMsg{RequestId: "r1"},
		},
	})
	assert.Equal(t, badgeGateRestricted, verdict)
}

// -----------------------------------------------------------------------------
// Spy
// -----------------------------------------------------------------------------

// spyDeployControl records which RPC the bridge dispatched. It admits
// everything -- the gate is proven against the REAL service above; this one
// exists only to prove the wiring reaches each method.
type spyDeployControl struct {
	memqlv1.UnimplementedDeployControlServiceServer
	called string
}

func (s *spyDeployControl) GetDeploymentStatus(context.Context, *memqlv1.GetDeploymentStatusRequest) (*memqlv1.DeploymentStatus, error) {
	s.called = "GetDeploymentStatus"
	return &memqlv1.DeploymentStatus{Env: "prod"}, nil
}

func (s *spyDeployControl) SuggestNextVersion(context.Context, *memqlv1.SuggestNextVersionRequest) (*memqlv1.SuggestNextVersionResult, error) {
	s.called = "SuggestNextVersion"
	return &memqlv1.SuggestNextVersionResult{NextPatch: "1.2.4"}, nil
}

func (s *spyDeployControl) DeployStaging(context.Context, *memqlv1.DeployStagingRequest) (*memqlv1.ActionResult, error) {
	s.called = "DeployStaging"
	return &memqlv1.ActionResult{Ok: true}, nil
}

func (s *spyDeployControl) Promote(context.Context, *memqlv1.PromoteRequest) (*memqlv1.ActionResult, error) {
	s.called = "Promote"
	return &memqlv1.ActionResult{Ok: true}, nil
}

func (s *spyDeployControl) Rollback(context.Context, *memqlv1.RollbackRequest) (*memqlv1.ActionResult, error) {
	s.called = "Rollback"
	return &memqlv1.ActionResult{Ok: true}, nil
}

func (s *spyDeployControl) RolloutAction(context.Context, *memqlv1.RolloutActionRequest) (*memqlv1.ActionResult, error) {
	s.called = "RolloutAction"
	return &memqlv1.ActionResult{Ok: true}, nil
}

func (s *spyDeployControl) CutVersion(context.Context, *memqlv1.CutVersionRequest) (*memqlv1.ActionResult, error) {
	s.called = "CutVersion"
	return &memqlv1.ActionResult{Ok: true}, nil
}

func (s *spyDeployControl) Deploy(context.Context, *memqlv1.DeployRequest) (*memqlv1.ActionResult, error) {
	s.called = "Deploy"
	return &memqlv1.ActionResult{Ok: true}, nil
}

func (s *spyDeployControl) RollbackDeployment(context.Context, *memqlv1.RollbackDeploymentRequest) (*memqlv1.ActionResult, error) {
	s.called = "RollbackDeployment"
	return &memqlv1.ActionResult{Ok: true}, nil
}
