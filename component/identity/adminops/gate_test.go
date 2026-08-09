package adminops

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
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

// The @serverOnly reads and writes this surface drives are only reachable with
// an internal-origin stamp, and the stamp's whole safety argument is that the
// gate ran first. Asserted behaviourally at the engine seam rather than by
// grepping the source: a reordered argument or a second call site defeats a
// source scan silently (the lesson memql#2991 recorded).
func TestAdmittedWritesStampInternalOrigin(t *testing.T) {
	svc, eng, _ := newTestService(t)
	svc.Engine = &originRecordingEngine{inner: eng}

	// The profile read is the first engine call an admitted caller makes.
	_ = svc.UpdateUserProfile(ctxAs("admin"), UserProfile{UserId: "v1:identity:user:target", DisplayName: "T"})

	rec, _ := svc.Engine.(*originRecordingEngine)
	if len(rec.origins) == 0 {
		t.Fatal("no engine call was made")
	}
	for i, o := range rec.origins {
		if o != auth.OriginInternal {
			t.Errorf("engine call %d (%s) ran with origin %v, want internal",
				i, firstWords(rec.inner.queries[i]), o)
		}
	}
}

type originRecordingEngine struct {
	inner   *recordingEngine
	origins []auth.CallOrigin
}

func (e *originRecordingEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	return e.inner.Execute(ctx, q)
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
