package adminops

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// The gate, verified rather than asserted (memql#3324).
//
// The server-rendered /admin/* console refused a caller below owner/admin with
// a 403 AND an `admin_auth_forbidden` audit event carrying the failure reason
// `role_not_admin` (component/identity/admin/auth.go). Moving those writes onto
// the stream must not lose either half, and "the handler calls authorize()" is
// not evidence -- these tests drive every operation with every role and read
// what actually came out.
//
// The engine here REFUSES EVERY CALL. That is the point: if any operation
// reaches the engine for a caller below the floor, the refusal test fails on
// the engine's own counter rather than on a message string, so a future
// operation that forgets to gate cannot pass by returning a plausible error.

// recordingEngine fails every Execute and counts the attempts.
type recordingEngine struct {
	calls   int
	queries []string
	// reply, when set, is returned for a query whose prefix matches. Used by
	// the positive-control test to let an admitted caller get past the read.
	err error
}

func (e *recordingEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.calls++
	e.queries = append(e.queries, q)
	if e.err != nil {
		return nil, e.err
	}
	return nil, errTestEngine
}

var errTestEngine = &engineErr{}

type engineErr struct{}

func (*engineErr) Error() string { return "test engine: no database" }

// capturingAudit records every event the service emits.
type capturingAudit struct{ events []identity.AuditEvent }

func (a *capturingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	a.events = append(a.events, ev)
}

func newTestService(t *testing.T) (*Service, *recordingEngine, *capturingAudit) {
	t.Helper()
	eng := &recordingEngine{}
	audit := &capturingAudit{}
	svc, err := New(&Service{Engine: eng, Audit: audit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, eng, audit
}

// ctxAs builds a stream context carrying a resolved AccessContext, exactly as
// component/grpc's handler stamps it from the session.
func ctxAs(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId:       "v1:identity:user:caller",
		PrimaryEmail: "caller@example.test",
		Role:         role,
		IdentityId:   "v1:identity:identity:caller",
	})
}

// operations is every gated write, named as the audit trail names it. A new
// operation added to the service without a row here is invisible to these
// tests, which is why the table is the file's first citizen.
var operations = []struct {
	name string
	call func(*Service, context.Context) Result
}{
	{"update_user_profile", func(s *Service, ctx context.Context) Result {
		return s.UpdateUserProfile(ctx, UserProfile{UserId: "v1:identity:user:target", DisplayName: "Target"})
	}},
	{"set_user_role", func(s *Service, ctx context.Context) Result {
		return s.SetUserRole(ctx, "v1:identity:user:target", "owner")
	}},
	{"suspend_user", func(s *Service, ctx context.Context) Result {
		return s.SetUserSuspended(ctx, "v1:identity:user:target", true, "policy")
	}},
	{"reinstate_user", func(s *Service, ctx context.Context) Result {
		return s.SetUserSuspended(ctx, "v1:identity:user:target", false, "")
	}},
	{"revoke_pat", func(s *Service, ctx context.Context) Result {
		return s.RevokePersonalAccessToken(ctx, "v1:identity:identity:pat")
	}},
	{"revoke_node_token", func(s *Service, ctx context.Context) Result {
		return s.RevokeNodeToken(ctx, "v1:identity:identity:node")
	}},
	{"update_cluster_settings", func(s *Service, ctx context.Context) Result {
		return s.UpdateClusterSettings(ctx, ClusterSettings{
			RegistrationMode: "open", InternalDefaultRole: "writer",
		})
	}},
}

// Every role below owner/admin is refused, at every operation, with
// PERMISSION_DENIED and an audited `admin_auth_forbidden` -- and the engine is
// never touched.
func TestEveryWriteRefusesBelowOwnerOrAdmin(t *testing.T) {
	for _, role := range []auth.Role{"reader", "writer", "developer", ""} {
		for _, op := range operations {
			t.Run(string(role)+"/"+op.name, func(t *testing.T) {
				svc, eng, audit := newTestService(t)

				res := op.call(svc, ctxAs(role))

				if res.OK {
					t.Fatalf("role %q was permitted to %s", role, op.name)
				}
				if res.Code != CodePermissionDenied {
					t.Errorf("code = %d, want %d (PERMISSION_DENIED)", res.Code, CodePermissionDenied)
				}
				if eng.calls != 0 {
					t.Errorf("engine was reached %d time(s) for a refused caller: %v", eng.calls, eng.queries)
				}
				if len(audit.events) != 1 {
					t.Fatalf("want exactly 1 audit event, got %d", len(audit.events))
				}
				ev := audit.events[0]
				if ev.Action != "admin_auth_forbidden" {
					t.Errorf("audit action = %q, want admin_auth_forbidden", ev.Action)
				}
				if ev.Category != identity.AuditCategoryAdmin {
					t.Errorf("audit category = %q, want admin", ev.Category)
				}
				if ev.Outcome != identity.AuditOutcomeBlocked {
					t.Errorf("audit outcome = %q, want blocked", ev.Outcome)
				}
				if ev.FailureReason != "role_not_admin" {
					t.Errorf("audit failure reason = %q, want role_not_admin", ev.FailureReason)
				}
				if ev.ActorRole != string(role) {
					t.Errorf("audit actor role = %q, want %q", ev.ActorRole, role)
				}
				// The id an operator quotes when arguing about a denial.
				if res.AuditEventId == "" || res.AuditEventId != ev.CorrelationId {
					t.Errorf("result audit id %q does not match the emitted event's correlation id %q",
						res.AuditEventId, ev.CorrelationId)
				}
			})
		}
	}
}

// A stream that resolved no actor at all fails CLOSED -- UNAUTHENTICATED, not
// "treat the empty role as a reader and carry on".
func TestUnauthenticatedCallerIsRefusedAndAudited(t *testing.T) {
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			svc, eng, audit := newTestService(t)

			res := op.call(svc, context.Background())

			if res.OK || res.Code != CodeUnauthenticated {
				t.Fatalf("ok=%v code=%d, want refusal with %d", res.OK, res.Code, CodeUnauthenticated)
			}
			if eng.calls != 0 {
				t.Errorf("engine reached %d time(s) with no actor", eng.calls)
			}
			if len(audit.events) != 1 || audit.events[0].FailureReason != "no_authenticated_actor" {
				t.Fatalf("want one no_authenticated_actor event, got %+v", audit.events)
			}
		})
	}
}

// Positive control. Without it every assertion above is satisfied by a service
// that refuses everyone, and the tests would keep passing if the gate were
// replaced by `return false`.
func TestOwnerAndAdminReachTheEngine(t *testing.T) {
	for _, role := range []auth.Role{"owner", "admin"} {
		for _, op := range operations {
			t.Run(string(role)+"/"+op.name, func(t *testing.T) {
				svc, eng, audit := newTestService(t)

				res := op.call(svc, ctxAs(role))

				if eng.calls == 0 {
					t.Fatalf("role %q never reached the engine for %s -- the gate refused an admitted caller",
						role, op.name)
				}
				// The write fails (the test engine has no database), which is
				// the expected shape here: what matters is that the failure is
				// NOT the role gate and is NOT audited as blocked.
				if res.Code == CodePermissionDenied || res.Code == CodeUnauthenticated {
					t.Fatalf("role %q was refused by the gate: %s", role, res.ErrorMessage)
				}
				for _, ev := range audit.events {
					if ev.Action == "admin_auth_forbidden" {
						t.Fatalf("an admitted %q caller was audited as forbidden", role)
					}
					if ev.Outcome == identity.AuditOutcomeBlocked {
						t.Fatalf("an admitted %q caller produced a blocked audit event: %+v", role, ev)
					}
				}
				if len(audit.events) == 0 {
					t.Fatalf("an admitted %q caller's %s wrote no audit event at all", role, op.name)
				}
			})
		}
	}
}

// The @serverOnly reads AND writes this surface drives are only reachable with
// an internal-origin stamp, and the stamp's whole safety argument is that the
// gate ran first.
//
// THIS TEST IS THE RELOCATED memql#2991 GUARD. It used to live in
// component/identity/admin as TestAdminUpdateUserStampsInternalOrigin, pinned
// on the templ console's one call site; that call site is gone and this is the
// only path to `updateUser` now, so the guard moves rather than disappearing.
//
// Behavioural at the Engine seam, not a source scan, for the reasons that file
// recorded at length: a scan was defeated three separate ways in review -- a
// second unstamped call site (only the first matched), a comment naming the
// helper with the real call unstamped, and reordering the fmt args so nothing
// matched and the test SKIPPED, which reads as a pass. Recording EVERY call's
// origin is indifferent to all three.
func TestAdmittedWritesStampInternalOrigin(t *testing.T) {
	rec := &originRecordingEngine{}
	audit := &capturingAudit{}
	svc, err := New(&Service{Engine: rec, Audit: audit})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res := svc.UpdateUserProfile(ctxAs("admin"), UserProfile{
		UserId:      "v1:identity:user:target",
		DisplayName: "Target Person",
	})
	if !res.OK {
		t.Fatalf("the write failed against the stub engine: %s", res.ErrorMessage)
	}

	// Both halves must be present: the @serverOnly READ that finds the row and
	// the @serverOnly WRITE that changes it. Asserting only over "every call"
	// would stay green if the write never happened at all.
	var sawRead, sawWrite bool
	for i, call := range rec.calls {
		if strings.Contains(call.query, "userByIdSystem(") {
			sawRead = true
		}
		if strings.Contains(call.query, "updateUser(") {
			sawWrite = true
		}
		if call.origin.IsInternal() {
			continue
		}
		t.Errorf("call %d of %d ran with origin %v, not internal: %s\n"+
			"@serverOnly is enforced as `fn.ServerOnly && "+
			"!auth.OriginFromContext(ctx).IsInternal()`, so this call is REFUSED at runtime "+
			"and fails in the console rather than in any test (memql#2991).",
			i+1, len(rec.calls), call.origin, firstWords(call.query))
	}
	if !sawRead {
		t.Error("no call named userByIdSystem -- if the read was renamed, move this guard with it")
	}
	if !sawWrite {
		t.Error("no call named updateUser -- if the mutation was renamed, move this guard with it")
	}
}

// originRecordingEngine answers the profile read with one row and records the
// origin of every call. A slice rather than a map keyed by construct: a map is
// last-write-wins, so an unstamped call placed BEFORE a stamped one would be
// overwritten and vanish -- the mirror of the "only sees the first" flaw the
// source scan this replaces had.
type originRecordingEngine struct {
	calls []struct {
		query  string
		origin auth.CallOrigin
	}
}

func (e *originRecordingEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.calls = append(e.calls, struct {
		query  string
		origin auth.CallOrigin
	}{q, auth.OriginFromContext(ctx)})

	if strings.Contains(q, "userByIdSystem(") {
		return &memqlengine.ExecuteResult{
			Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
				Id: "v1:identity:user:target",
				Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"primaryEmail": structpb.NewStringValue("target@example.test"),
					"role":         structpb.NewStringValue("reader"),
					"active":       structpb.NewBoolValue(true),
				}},
			}}},
		}, nil
	}
	return &memqlengine.ExecuteResult{}, nil
}

func firstWords(q string) string {
	if i := strings.IndexByte(q, '('); i > 0 {
		return q[:i]
	}
	return q
}

// A Service without an audit sink is refused at construction. An unaudited
// admin write surface looks like a working gate and leaves nothing behind to
// check it by.
func TestServiceRequiresAnAuditSink(t *testing.T) {
	if _, err := New(&Service{Engine: &recordingEngine{}}); err == nil {
		t.Fatal("New accepted a Service with no audit sink")
	}
	if _, err := New(&Service{Audit: &capturingAudit{}}); err == nil {
		t.Fatal("New accepted a Service with no engine")
	}
}
