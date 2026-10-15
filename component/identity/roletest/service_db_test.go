package roletest

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
	memql "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"google.golang.org/protobuf/proto"
)

func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type auditSink struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (s *auditSink) Log(_ context.Context, e identity.AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}
func realService(t *testing.T) (*Service, *memql.MemQLEngine) {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "role-test sessions", dbtest.DSN(), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	engine, err := memql.New(db)
	require.NoError(t, err)
	engine.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, engine.Init(concept.DefaultRegistry()))
	oldGrants, oldMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	engine.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetGrantSource(oldGrants)
		auth.SetMembershipSource(oldMembers)
		auth.SetRoleTestValidator(nil)
	})
	service := &Service{Store: &identity.Store{Engine: engine, DirectDB: func() *sql.DB { return db.DB }}, Audit: &auditSink{}}
	return service, engine
}
func seedOperator(t *testing.T, s *Service, role string) context.Context {
	t.Helper()
	id := "v1:identity:user:" + uuid.NewString()
	ctx := identity.ContextWithSystemCredentialActor(context.Background())
	require.NoError(t, s.Store.CreateUserOnFirstLogin(auth.ContextWithInternalOrigin(ctx), id, "Operator", "operator-"+uuid.NewString()+"@example.test", role, false, identity.UserProfileSeed{}))
	ctx = verifier.AttachToContext(context.Background(), &verifier.VerifiedClaims{ClaimsMap: map[string]any{"sub": id, "role": role, "class": "user", "exp": time.Now().Add(time.Hour).Unix()}})
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: id, Role: auth.Role(role)})
}
func personaContext(t *testing.T, s *Service, e *memql.MemQLEngine, token string) context.Context {
	t.Helper()
	vc, err := s.VerifyRoleTest(context.Background(), token)
	require.NoError(t, err)
	ctx := verifier.AttachToContext(context.Background(), vc)
	resolver := &auth.IdentityResolver{Engine: auth.QueryRunnerFunc(func(ctx context.Context, query string) (any, error) {
		result, err := e.Execute(ctx, query)
		if err != nil || result == nil {
			return nil, err
		}
		return result.OutputPayload(), nil
	})}
	ac, err := resolver.LoadFromClaims(ctx, vc.ClaimsMap)
	require.NoError(t, err)
	return auth.ContextWithAccess(ctx, ac)
}

func TestRealPersonaAcrossReplicasAndRevocation(t *testing.T) {
	one, engine := realService(t)
	two, otherEngine := realService(t)
	owner := seedOperator(t, one, "owner")
	session, err := one.Start(owner, "reader")
	require.NoError(t, err)
	require.True(t, auth.IsRoleTestUser(session.UserID))
	vc, err := two.VerifyRoleTest(context.Background(), session.Token)
	require.NoError(t, err)
	require.Equal(t, "reader", vc.Role)
	auth.SetRoleTestValidator(two)
	viewer := personaContext(t, two, otherEngine, session.Token)
	_, err = otherEngine.Execute(viewer, fmt.Sprintf(`mutation createAskConversation(requestId: %q, title: "Role test")`, uuid.NewString()))
	require.NoError(t, err)
	folder := "v1:library:folder:" + uuid.NewString()
	_, err = engine.Execute(viewer, fmt.Sprintf(`mutation createLibraryFolder(folderId: %q, name: "Test folder")`, folder))
	require.NoError(t, err)
	_, err = otherEngine.Execute(viewer, fmt.Sprintf(`mutation renameLibraryFolder(folderId: %q, name: "Edited")`, folder))
	require.NoError(t, err)
	stranger := seedOperator(t, one, "owner")
	second, err := one.Start(stranger, "reader")
	require.NoError(t, err)
	otherViewer := personaContext(t, two, otherEngine, second.Token)
	_, err = otherEngine.Execute(otherViewer, fmt.Sprintf(`mutation renameLibraryFolder(folderId: %q, name: "Not mine")`, folder))
	require.Error(t, err)
	ac, _ := auth.AccessFromContext(viewer)
	authority, err := auth.ForwardedAuthorityForUser(ac, auth.RoleTestClass, "", session.ExpiresAt, time.Now())
	require.NoError(t, err)
	wire, err := proto.Marshal(node.ForwardedAuthorityToProto(authority, "replica-one", "bff"))
	require.NoError(t, err)
	received := new(nodev1.ForwardedAuthority)
	require.NoError(t, proto.Unmarshal(wire, received))
	authority = node.ForwardedAuthorityFromProto(received)
	require.Equal(t, session.ID, authority.RoleTestSessionID)
	resolved, err := auth.VerifyForwardedAuthority(authority, time.Now())
	require.NoError(t, err)
	forwarded := auth.BindForwardedContext(context.Background(), authority.Principal().Claims, resolved, authority)
	require.NoError(t, auth.ValidateRoleTestContext(forwarded))
	grant, err := auth.CaptureExecutionAuthority(forwarded)
	require.NoError(t, err)
	require.Equal(t, session.ID, grant["roleTestSessionId"])
	resolver := auth.NewIdentityResolver(auth.QueryRunnerFunc(func(ctx context.Context, query string) (any, error) {
		result, err := otherEngine.Execute(ctx, query)
		if err != nil || result == nil {
			return nil, err
		}
		return result.OutputPayload(), nil
	}), nil)
	_, err = auth.ContextWithPersistedOwner(context.Background(), session.UserID, grant, resolver)
	require.NoError(t, err, "durable work restores only the bounded test authority")
	_, err = auth.ContextWithPersistedOwner(context.Background(), session.UserID, nil, resolver)
	require.Error(t, err, "missing grant must never upgrade a test persona")

	_, err = one.Start(viewer, "reader")
	require.ErrorIs(t, err, ErrDenied)
	require.ErrorIs(t, one.End(otherViewer, session.ID), ErrDenied)
	_, _, err = (&identity.JWTIssuer{}).IssueAccessToken(identity.IssueInput{UserId: session.UserID}, time.Now())
	require.ErrorContains(t, err, "test personas")
	require.NoError(t, one.End(owner, session.ID))
	_, err = two.VerifyRoleTest(context.Background(), session.Token)
	require.ErrorIs(t, err, ErrInvalid)
	require.Error(t, auth.ValidateRoleTestContext(forwarded))
	_, err = auth.ContextWithPersistedOwner(context.Background(), session.UserID, grant, resolver)
	require.Error(t, err, "revoked tests cannot resume queued work")
	_, err = otherEngine.Execute(viewer, `mutation createAskConversation(requestId: "after-end", title: "Refused")`)
	require.Error(t, err)
	require.Error(t, auth.ValidateRoleTestContext(auth.ContextWithInternalOrigin(forwarded)), "internal work must retain the test boundary")
}

func TestRoleBoundaryAndConcurrentProvisioning(t *testing.T) {
	one, _ := realService(t)
	two, _ := realService(t)
	developer := seedOperator(t, one, "developer")
	for _, role := range []string{"owner", "admin", "unknown"} {
		_, err := one.Start(developer, role)
		require.ErrorIs(t, err, ErrDenied)
	}
	for _, role := range []string{"reader", "writer", "developer"} {
		_, err := one.Start(developer, role)
		if err != nil {
			subject, _ := auth.SubjectFromContext(developer)
			for _, d := range auth.EffectiveCapabilities(developer, auth.Subject{Role: auth.Role(role)}) {
				if d.Held && !auth.CapableFor(developer, subject, d.Verb, d.Resource) {
					t.Logf("missing capability for %s: %s %s", role, d.Verb, d.Resource)
				}
			}
		}
		require.NoError(t, err, role)
	}
	for _, role := range []string{"reader", "writer", "admin"} {
		_, err := one.Start(seedOperator(t, one, role), "reader")
		require.ErrorIs(t, err, ErrDenied)
	}
	owner := seedOperator(t, one, "owner")
	_, err := one.Start(owner, "admin")
	require.NoError(t, err)
	_, err = one.Start(owner, "owner")
	require.ErrorIs(t, err, ErrDenied)
	var wg sync.WaitGroup
	sessions := make([]Session, 2)
	errs := make([]error, 2)
	for i, s := range []*Service{one, two} {
		wg.Add(1)
		go func(i int, s *Service) { defer wg.Done(); sessions[i], errs[i] = s.Start(owner, "reader") }(i, s)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, sessions[0].UserID, sessions[1].UserID)
	valid := 0
	for _, session := range sessions {
		if _, err := one.VerifyRoleTest(context.Background(), session.Token); err == nil {
			valid++
		}
	}
	require.Equal(t, 1, valid)
	rows, err := one.List(owner)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	latest, err := one.Start(owner, "reader")
	require.NoError(t, err)
	two.Now = func() time.Time { return latest.ExpiresAt }
	_, err = two.VerifyRoleTest(context.Background(), latest.Token)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestOperatorRevocationEndsItsTestSessions(t *testing.T) {
	one, _ := realService(t)
	two, _ := realService(t)
	owner := seedOperator(t, one, "owner")
	session, err := one.Start(owner, "admin")
	require.NoError(t, err)
	ac, _ := auth.AccessFromContext(owner)
	_, err = one.Store.BumpUserRevocationEpoch(auth.ContextWithInternalOrigin(owner), ac.UserId)
	require.NoError(t, err)
	_, err = two.VerifyRoleTest(context.Background(), session.Token)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = one.Start(owner, "reader")
	require.ErrorIs(t, err, ErrDenied, "an already-revoked operator credential cannot start a new test")
}

func TestDeveloperRestrictionAndPersonaGrantInvalidateTests(t *testing.T) {
	service, engine := realService(t)
	developer := seedOperator(t, service, "developer")
	session, err := service.Start(developer, "reader")
	require.NoError(t, err)
	ac, _ := auth.AccessFromContext(developer)
	write := func(user, effect string) {
		t.Helper()
		grantID := auth.GrantRowID(auth.SubjectKindUser, user, auth.VerbRead, auth.ResourceData)
		query := fmt.Sprintf(`mutation writeGrant(grantId: %q, subjectKind: "user", subjectId: %q, verb: "read", resourceType: "data", effect: %q, grantedBy: "role-test-regression")`, grantID, user, effect)
		_, err := engine.Execute(auth.ContextWithInternalOrigin(identity.ContextWithSystemCredentialActor(context.Background())), query)
		require.NoError(t, err)
	}
	write(ac.UserId, "deny")
	_, err = service.Start(developer, "reader")
	require.ErrorIs(t, err, ErrDenied)
	_, err = service.VerifyRoleTest(context.Background(), session.Token)
	require.ErrorIs(t, err, ErrInvalid)
	owner := seedOperator(t, service, "owner")
	persona, err := service.Start(owner, "reader")
	require.NoError(t, err)
	write(persona.UserID, "allow")
	_, err = service.VerifyRoleTest(context.Background(), persona.Token)
	require.ErrorIs(t, err, ErrInvalid, "a test persona must never inherit extra grants")
}

type durableAudit struct {
	t    *testing.T
	sink *identity.EngineAuditSink
}

func (a durableAudit) Log(ctx context.Context, event identity.AuditEvent) {
	a.t.Helper()
	require.NoError(a.t, a.sink.WriteAuditEvent(ctx, event))
}
func TestRoleTestsWriteDurableAuditWithoutCredentials(t *testing.T) {
	service, engine := realService(t)
	service.Audit = durableAudit{t, &identity.EngineAuditSink{Engine: engine, Logger: engine.Logger}}
	owner := seedOperator(t, service, "owner")
	session, err := service.Start(owner, "reader")
	require.NoError(t, err)
	require.NoError(t, service.End(owner, session.ID))
	var count int
	var payloads string
	err = service.Store.DirectDB().QueryRowContext(context.Background(), `SELECT count(*), string_agg(payload::text, ' ') FROM "MemoryNodes" WHERE concept='v1:identity:auditEvent' AND payload->>'targetId'=$1`, session.ID).Scan(&count, &payloads)
	require.NoError(t, err)
	require.Equal(t, 2, count)
	require.Contains(t, payloads, "role_test_started")
	require.Contains(t, payloads, "role_test_ended")
	require.NotContains(t, payloads, session.Token)
}
