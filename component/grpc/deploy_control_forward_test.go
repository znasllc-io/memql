package memql

// deploy_control_forward_test.go is the cross-node coverage for memql#3380.
//
// The rule these tests exist to enforce is not "the forward works". It is that
// the DEPLOY ROLE MATRIX survives the hop. rollback_deployment is owner-only --
// not even admin (memql#1876, docs/public/operate/deployment-console.md) -- and
// the hop is exactly where that could quietly stop being true, in two ways:
//
//   - the caller could be dropped, and the receiving node could resolve an
//     actor from the connection instead. The connection belongs to the BFF and
//     carries its class="node" credential, so "resolve from the connection"
//     resolves a machine, not the human who pressed the button.
//   - the caller could be carried as data the receiver trusts, so that saying
//     "role: owner" on the wire made it so.
//
// Neither is a hypothetical: CLAUDE.md's multi-node rule exists because green
// single-node tests have repeatedly certified code that broke in the mesh. So
// every assertion below runs against a handler with NO local state from the
// originating node -- it sees only the wire envelope -- and the service behind
// it is a REAL *deploycontrol.Service, so the gate under test is the shipped
// gate rather than a restatement of it.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// recordingAudit captures the audit events the deploy-control service emits.
// The outcome is the second, independent witness for every gate assertion: a
// refusal writes exactly one "blocked" event, an admitted call does not.
type recordingAudit struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (a *recordingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *recordingAudit) all() []identity.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]identity.AuditEvent(nil), a.events...)
}

// refusingExecutor stands in for the shell-out boundary. Every method fails,
// which is fine and deliberate: no assertion below depends on a deploy
// succeeding, only on whether the caller got PAST the gate. A permissive fake
// would risk a test passing because the action was a no-op.
type refusingExecutor struct{}

func (refusingExecutor) RunPromote(context.Context, string, string) (string, error) {
	return "", context.Canceled
}
func (refusingExecutor) RunRollback(context.Context, string, string) (string, error) {
	return "", context.Canceled
}
func (refusingExecutor) RunRolloutAction(context.Context, string, string, string) (string, error) {
	return "", context.Canceled
}
func (refusingExecutor) KubectlJSON(context.Context, ...string) ([]byte, error) {
	return nil, context.Canceled
}
func (refusingExecutor) Git(context.Context, ...string) (string, error) {
	return "", context.Canceled
}

// newForwardFixture builds the RECEIVING half exactly as the identity node
// builds it: one real *deploycontrol.Service, wrapped in the mesh handler.
func newForwardFixture(t *testing.T) (*DeployControlForwardHandler, *recordingAudit) {
	t.Helper()
	audit := &recordingAudit{}
	svc, err := deploycontrol.NewService(deploycontrol.Options{
		Logger:   testLogger(),
		Audit:    audit,
		RepoRoot: t.TempDir(),
		Executor: refusingExecutor{},
	})
	if err != nil {
		t.Fatalf("building deploy-control service: %v", err)
	}
	h := NewDeployControlForwardHandler(svc, testLogger())
	if h == nil {
		t.Fatal("NewDeployControlForwardHandler returned nil for a non-nil service")
	}
	return h, audit
}

// principalForRole builds the assertion the ORIGINATING node would build for a
// session resolved to `role`, through the same auth constructor
// streamSession.forwardedPrincipal uses.
func principalForRole(t *testing.T, role auth.Role) auth.ForwardedPrincipal {
	t.Helper()
	a, err := auth.ForwardedAuthorityForUser(
		&auth.AccessContext{
			UserId:       "v1:identity:user:" + string(role) + "-person",
			PrimaryEmail: string(role) + "@example.test",
			Role:         role,
		},
		auth.ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("building a %s principal: %v", role, err)
	}
	return a.Principal()
}

// runHop drives one full round-trip through the code the mesh runs: the
// producer's encoder, the receiving handler, and the consumer's decoder. Only
// peer.Connection.Send is absent -- it needs a live gRPC stream, and it carries
// no logic.
//
// The handler is invoked with context.Background(): a receiving node has NONE
// of the originating session's state, and passing a bare context is what makes
// that true in the test rather than merely intended.
func runHop(
	t *testing.T,
	h *DeployControlForwardHandler,
	principal auth.ForwardedPrincipal,
	msg *memqlv1.DeployControlMsg,
) (*memqlv1.DeployControlResult, error) {
	t.Helper()
	return runHopOnContext(t, context.Background(), h, principal, msg)
}

func runHopOnContext(
	t *testing.T,
	ctx context.Context,
	h *DeployControlForwardHandler,
	principal auth.ForwardedPrincipal,
	msg *memqlv1.DeployControlMsg,
) (*memqlv1.DeployControlResult, error) {
	t.Helper()

	req, err := buildDeployControlForwardRequest("hop-1", principal, msg, "bff-a", "bff")
	if err != nil {
		t.Fatalf("encoding the forward: %v", err)
	}

	var replies []*nodev1.DeployControlForwardResponse
	h.HandleForwardedRequest(ctx, req, func(m *nodev1.NodeServerMessage) error {
		replies = append(replies, m.GetDeployControlForwardResponse())
		return nil
	})
	if len(replies) != 1 {
		t.Fatalf("want exactly one reply (the originating handler is parked on it), got %d", len(replies))
	}
	return decodeDeployControlForwardResponse(replies[0])
}

// rollbackMsg is the owner-only action, and therefore the sharpest probe of the
// matrix. Arguments are VALID: several RPCs validate shape BEFORE the gate, and
// a malformed request would be rejected with InvalidArgument, passing the test
// while testing nothing about authorization.
func rollbackMsg() *memqlv1.DeployControlMsg {
	return &memqlv1.DeployControlMsg{
		RequestId: "req-rollback",
		Request: &memqlv1.DeployControlMsg_RollbackDeployment{
			RollbackDeployment: &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "d-good"},
		},
	}
}

// -----------------------------------------------------------------------------
// The role matrix over the hop
// -----------------------------------------------------------------------------

// TestForwardedRollbackIsOwnerOnly is the acceptance test named in the issue:
// rollback refused for an admin caller and admitted for an owner, verified
// against a FORWARDED call rather than a direct one.
//
// "Admitted" is asserted as "not PermissionDenied", not as "succeeded": the
// fixture has no engine, so an owner's rollback fails later on `no engine
// wired`. That is the correct assertion boundary -- this test is about the
// gate, and asserting on the outcome of the shell-out would couple it to the
// deploy machinery it is not testing. The audit outcome is the independent
// witness that the two callers took genuinely different paths.
func TestForwardedRollbackIsOwnerOnly(t *testing.T) {
	t.Run("admin is refused", func(t *testing.T) {
		h, audit := newForwardFixture(t)

		res, err := runHop(t, h, principalForRole(t, auth.RoleAdmin), rollbackMsg())
		if err != nil {
			t.Fatalf("the hop itself must succeed; a refusal travels INSIDE the result: %v", err)
		}
		if res.GetOk() {
			t.Fatal("an admin rollback came back ok=true over the mesh: the owner-only gate did not survive the hop")
		}
		if got := codes.Code(res.GetErrorCode()); got != codes.PermissionDenied {
			t.Fatalf("error code = %v, want PermissionDenied (got message %q)", got, res.GetErrorMessage())
		}
		// The caller's request_id is echoed, so the SDK's stream-keyed
		// dispatcher can route the refusal back to the pending call.
		if res.GetRequestId() != "req-rollback" {
			t.Errorf("request_id = %q, want the caller's own id echoed back", res.GetRequestId())
		}

		events := audit.all()
		if len(events) != 1 || events[0].Outcome != identity.AuditOutcomeBlocked {
			t.Fatalf("want exactly one blocked audit event, got %+v", events)
		}
		if events[0].ActorRole != string(auth.RoleAdmin) {
			t.Errorf("audit actor role = %q, want admin -- the refusal must be attributed to the ORIGINATING caller",
				events[0].ActorRole)
		}
	})

	t.Run("owner is admitted", func(t *testing.T) {
		h, audit := newForwardFixture(t)

		res, err := runHop(t, h, principalForRole(t, auth.RoleOwner), rollbackMsg())
		if err != nil {
			t.Fatalf("hop: %v", err)
		}
		if got := codes.Code(res.GetErrorCode()); got == codes.PermissionDenied {
			t.Fatalf("an OWNER was refused rollback over the mesh: %q", res.GetErrorMessage())
		}

		events := audit.all()
		if len(events) != 1 {
			t.Fatalf("want exactly one audit event, got %+v", events)
		}
		if events[0].Outcome == identity.AuditOutcomeBlocked {
			t.Fatalf("owner rollback was audited as blocked: %+v", events[0])
		}
		if events[0].ActorRole != string(auth.RoleOwner) {
			t.Errorf("audit actor role = %q, want owner", events[0].ActorRole)
		}
		if events[0].ActorUserId != "v1:identity:user:owner-person" {
			t.Errorf("audit actor = %q, want the originating caller's subject", events[0].ActorUserId)
		}
	})

	// The rest of the spectrum, for the same action. developer sits between the
	// two interesting cases -- it may cut and deploy forward but not roll back
	// -- so a hop that leaked "can deploy" into "can roll back" shows up here.
	for _, role := range []auth.Role{auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader} {
		role := role
		t.Run(string(role)+" is refused", func(t *testing.T) {
			h, _ := newForwardFixture(t)
			res, err := runHop(t, h, principalForRole(t, role), rollbackMsg())
			if err != nil {
				t.Fatalf("hop: %v", err)
			}
			if got := codes.Code(res.GetErrorCode()); got != codes.PermissionDenied {
				t.Fatalf("%s rollback error code = %v, want PermissionDenied", role, got)
			}
		})
	}
}

// TestForwardedDeployTierMatchesTheLocalMatrix walks the other two gate tiers
// so the hop is not merely correct for the one action the issue names.
//
//	get_status              -> owner, admin              (authorize)
//	cut_version             -> owner, admin, developer   (authorizeDeploy)
func TestForwardedDeployTierMatchesTheLocalMatrix(t *testing.T) {
	cases := []struct {
		name    string
		msg     func() *memqlv1.DeployControlMsg
		allowed map[auth.Role]bool
	}{
		{
			name: "get_status (owner/admin)",
			msg: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{
					RequestId: "req-status",
					Request: &memqlv1.DeployControlMsg_GetDeploymentStatus{
						GetDeploymentStatus: &memqlv1.GetDeploymentStatusRequest{Env: "staging"},
					},
				}
			},
			allowed: map[auth.Role]bool{auth.RoleOwner: true, auth.RoleAdmin: true},
		},
		{
			name: "cut_version (owner/admin/developer)",
			msg: func() *memqlv1.DeployControlMsg {
				return &memqlv1.DeployControlMsg{
					RequestId: "req-cut",
					Request: &memqlv1.DeployControlMsg_CutVersion{
						CutVersion: &memqlv1.CutVersionRequest{Env: "prod", Bump: "patch"},
					},
				}
			},
			allowed: map[auth.Role]bool{
				auth.RoleOwner: true, auth.RoleAdmin: true, auth.RoleDeveloper: true,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			for _, role := range []auth.Role{
				auth.RoleOwner, auth.RoleAdmin, auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader,
			} {
				h, _ := newForwardFixture(t)
				res, err := runHop(t, h, principalForRole(t, role), tc.msg())
				if err != nil {
					t.Fatalf("%s hop: %v", role, err)
				}
				denied := codes.Code(res.GetErrorCode()) == codes.PermissionDenied
				if tc.allowed[role] && denied {
					t.Errorf("%s was refused but the matrix admits it: %q", role, res.GetErrorMessage())
				}
				if !tc.allowed[role] && !denied {
					t.Errorf("%s was admitted but the matrix refuses it", role)
				}
			}
		})
	}
}

// TestDeployControlForwardIgnoresTheForwardingNodesOwnCredential is the test
// the issue's sharpest requirement asks for, stated as a property rather than
// as a code shape.
//
// The receiving handler runs on the NodeService stream's context, which belongs
// to the FORWARDING node. Here that ambient context is stacked in the
// attacker's favour: it already carries a fully-resolved OWNER AccessContext,
// exactly as it would if a future edit resolved the actor from the connection,
// or reused a context some other part of the identity node had already stamped.
// The wire assertion says admin. Admin must lose the rollback.
//
// If this test ever fails, the hop has started reading an actor from the
// ambient context, and rollback has silently widened from owner to anyone who
// can reach a bff.
func TestDeployControlForwardIgnoresTheForwardingNodesOwnCredential(t *testing.T) {
	h, audit := newForwardFixture(t)

	poisoned := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId:       "v1:identity:user:the-bff-itself",
		PrimaryEmail: "bff@cluster.internal",
		Role:         auth.RoleOwner,
	})

	res, err := runHopOnContext(t, poisoned, h, principalForRole(t, auth.RoleAdmin), rollbackMsg())
	if err != nil {
		t.Fatalf("hop: %v", err)
	}
	if got := codes.Code(res.GetErrorCode()); got != codes.PermissionDenied {
		t.Fatalf("an owner-shaped ambient context let an ADMIN roll back (code %v): the receiver is "+
			"resolving the actor from the connection, not from the assertion", got)
	}
	events := audit.all()
	if len(events) != 1 {
		t.Fatalf("want one audit event, got %+v", events)
	}
	if events[0].ActorUserId != "v1:identity:user:admin-person" {
		t.Errorf("audit actor = %q, want the ORIGINATING caller (the ambient owner must not win)",
			events[0].ActorUserId)
	}
}

// TestDeployControlForwardRefusesAnUnprovableAuthority pins the fail-closed
// half. An envelope with no assertion is refused BEFORE the payload is even
// decoded, and refused as a transport error rather than dropped -- a drop would
// park the originating handler until its ceiling expires.
func TestDeployControlForwardRefusesAnUnprovableAuthority(t *testing.T) {
	h, audit := newForwardFixture(t)

	payload, err := proto.Marshal(rollbackMsg())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var replies []*nodev1.DeployControlForwardResponse
	h.HandleForwardedRequest(context.Background(),
		&nodev1.DeployControlForwardRequest{RequestId: "hop-x", DeployControlMsg: payload},
		func(m *nodev1.NodeServerMessage) error {
			replies = append(replies, m.GetDeployControlForwardResponse())
			return nil
		})

	if len(replies) != 1 {
		t.Fatalf("a refusal must still be ANSWERED (the caller is parked on it); got %d replies", len(replies))
	}
	if replies[0].GetErrorCode() != "forwarded_authority_refused" {
		t.Errorf("error code = %q, want forwarded_authority_refused", replies[0].GetErrorCode())
	}
	if _, derr := decodeDeployControlForwardResponse(replies[0]); derr == nil {
		t.Error("the consumer must surface a transport refusal as an error, not as a result")
	}
	if got := audit.all(); len(got) != 0 {
		t.Errorf("an unprovable forward must never reach the service, so it must write no audit event; got %+v", got)
	}
}

// TestDeployControlForwardRefusesAClaimedSystemPrincipal covers the other
// direction of the same fail-closed rule: the assertion is present but asserts
// something the contract forbids, so it never becomes an actor.
func TestDeployControlForwardRefusesAClaimedSystemPrincipal(t *testing.T) {
	h, _ := newForwardFixture(t)

	// A user principal claiming the system credential class -- the shape
	// auth.ErrForwardUserClaimsSystemClass exists to reject.
	bad := auth.ForwardedAuthority{
		Version:         auth.ForwardedAuthorityVersion,
		Kind:            auth.ForwardedPrincipalUser,
		Subject:         "v1:identity:user:someone",
		Role:            auth.RoleOwner,
		CredentialClass: auth.ForwardedClassSystem,
	}
	payload, err := proto.Marshal(rollbackMsg())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var replies []*nodev1.DeployControlForwardResponse
	h.HandleForwardedRequest(context.Background(), &nodev1.DeployControlForwardRequest{
		RequestId:        "hop-y",
		DeployControlMsg: payload,
		Authority:        node.ForwardedAuthorityToProto(bad, "bff-a", "bff"),
	}, func(m *nodev1.NodeServerMessage) error {
		replies = append(replies, m.GetDeployControlForwardResponse())
		return nil
	})

	if len(replies) != 1 || replies[0].GetErrorCode() != "forwarded_authority_refused" {
		t.Fatalf("want one forwarded_authority_refused reply, got %+v", replies)
	}
}

// -----------------------------------------------------------------------------
// Originating side
// -----------------------------------------------------------------------------

// TestDeployControlOnANodeWithoutTheServiceForwards is the regression test for
// the reported bug, expressed in the operator's terms.
//
// Against the pre-fix code this FAILS: a bff has no deploy-control service, so
// deploycontrol.Dispatch short-circuits and every control comes back
// UNIMPLEMENTED -- the exact symptom of memql#3380. With the forward wired, a
// bff no longer claims the surface does not exist. Here no identity peer is
// reachable, so it reports UNAVAILABLE instead: a retryable "could not reach
// the deploy node", which is a different operator instruction entirely.
func TestDeployControlOnANodeWithoutTheServiceForwards(t *testing.T) {
	logger := testLogger()
	identityNode := &node.Identity{ID: "test-bff", Type: node.NodeTypeBFF}
	peerMgr := node.NewPeerManager(identityNode, logger)

	cs := newCaptureStream(t)
	sess := &streamSession{
		service: &service{
			logger: logger,
			// The bug's precondition: no local deploy-control service...
			deployControlHandler: nil,
			// ...and the fix: a route to the node that has one.
			deployControlForwarder: NewDeployControlForwardRouter(peerMgr, logger),
		},
		stream:       cs,
		logger:       logger,
		access:       &auth.AccessContext{UserId: "v1:identity:user:op", Role: auth.RoleOwner},
		accessLoaded: true,
	}

	env := &memqlv1.MemqlClientMessage{MessageId: "m1", Payload: &memqlv1.MemqlClientMessage_DeployControl{
		DeployControl: rollbackMsg(),
	}}
	if err := sess.handleDeployControl(env, env.GetDeployControl()); err != nil {
		t.Fatalf("handler returned an error; that tears down the whole stream: %v", err)
	}

	// The forward is dispatched on a goroutine so a ten-minute promote.sh
	// cannot stall the browser's multiplexed stream, so the reply is awaited.
	res := awaitDeployControlResult(t, cs)
	if got := codes.Code(res.GetErrorCode()); got == codes.Unimplemented {
		t.Fatal("a bff still answers UNIMPLEMENTED: the deploy surface is not being forwarded (memql#3380)")
	}
	if got := codes.Code(res.GetErrorCode()); got != codes.Unavailable {
		t.Fatalf("error code = %v, want Unavailable with no identity peer reachable (message %q)",
			got, res.GetErrorMessage())
	}
	if !strings.Contains(res.GetErrorMessage(), "identity node") {
		t.Errorf("message %q should name the node that owns the surface", res.GetErrorMessage())
	}
	if res.GetRequestId() != "req-rollback" {
		t.Errorf("request_id = %q, want the caller's id echoed so the SDK can route the failure",
			res.GetRequestId())
	}
}

// TestDeployControlStillAnswersUnimplementedWithNoRoute pins the other half of
// the routing decision: a node with neither a service nor a forwarder is
// unchanged. Every non-bff, non-identity binary is in that state, and the
// honest answer there is still "this node does not have this surface".
func TestDeployControlStillAnswersUnimplementedWithNoRoute(t *testing.T) {
	logger := testLogger()
	cs := newCaptureStream(t)
	sess := &streamSession{
		service:      &service{logger: logger},
		stream:       cs,
		logger:       logger,
		access:       &auth.AccessContext{UserId: "v1:identity:user:op", Role: auth.RoleOwner},
		accessLoaded: true,
	}
	env := &memqlv1.MemqlClientMessage{MessageId: "m1", Payload: &memqlv1.MemqlClientMessage_DeployControl{
		DeployControl: rollbackMsg(),
	}}
	if err := sess.handleDeployControl(env, env.GetDeployControl()); err != nil {
		t.Fatalf("handleDeployControl: %v", err)
	}
	res := awaitDeployControlResult(t, cs)
	if got := codes.Code(res.GetErrorCode()); got != codes.Unimplemented {
		t.Fatalf("error code = %v, want Unimplemented on a node with no service and no route", got)
	}
}

// TestDeployControlForwardRouterWithoutAPeer covers the router's own
// no-route behaviour, which is what the handler above depends on.
func TestDeployControlForwardRouterWithoutAPeer(t *testing.T) {
	peerMgr := node.NewPeerManager(&node.Identity{ID: "bff-1", Type: node.NodeTypeBFF}, testLogger())
	r := NewDeployControlForwardRouter(peerMgr, testLogger())

	if r.Available() {
		t.Error("Available() is true with no identity peer registered")
	}
	if _, err := r.Forward(context.Background(), principalForRole(t, auth.RoleOwner), rollbackMsg()); err == nil {
		t.Fatal("Forward must error rather than hang when no identity peer is reachable")
	}
}

// TestDeployControlForwardRouterDispatchRoutesByRequestId proves the reply
// reaches the right parked caller and is decoded through the shipped decoder.
func TestDeployControlForwardRouterDispatchRoutesByRequestId(t *testing.T) {
	peerMgr := node.NewPeerManager(&node.Identity{ID: "bff-1", Type: node.NodeTypeBFF}, testLogger())
	r := NewDeployControlForwardRouter(peerMgr, testLogger())

	ch := make(chan *nodev1.DeployControlForwardResponse, 1)
	r.mu.Lock()
	r.inflight["hop-7"] = ch
	r.mu.Unlock()

	encoded, err := proto.Marshal(&memqlv1.DeployControlResult{RequestId: "req-1", Ok: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// An unrelated reply must not disturb the parked caller.
	r.Dispatch(&nodev1.DeployControlForwardResponse{RequestId: "hop-other", DeployControlResult: encoded})
	r.Dispatch(&nodev1.DeployControlForwardResponse{RequestId: "hop-7", DeployControlResult: encoded})

	select {
	case resp := <-ch:
		out, derr := decodeDeployControlForwardResponse(resp)
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		if !out.GetOk() || out.GetRequestId() != "req-1" {
			t.Errorf("decoded reply = %+v, want the caller's ok result", out)
		}
	case <-time.After(time.Second):
		t.Fatal("the parked caller never received its reply")
	}
}

// awaitDeployControlResult waits for the async forward goroutine to answer.
func awaitDeployControlResult(t *testing.T, cs *captureStream) *memqlv1.DeployControlResult {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if msg := cs.lastSent(); msg != nil {
			if res := msg.GetDeployControlResult(); res != nil {
				return res
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no DeployControlResult was sent on the stream")
	return nil
}

var _ = slog.Default
