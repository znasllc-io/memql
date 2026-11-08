package roletest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
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
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memql "github.com/znasllc-io/memql/component/memql"
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
	// JWT MapClaims decodes numeric dates as float64. Exercise that wire shape,
	// not just the int64 a Go fixture naturally constructs.
	wire, err := json.Marshal(map[string]any{"sub": id, "role": role, "class": "user", "exp": time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(wire, &claims))
	ctx = verifier.AttachToContext(context.Background(), &verifier.VerifiedClaims{ClaimsMap: claims})
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

func TestRoleTestingFromVerifiedOwnerJWT(t *testing.T) {
	service, _ := realService(t)
	owner := seedOperator(t, service, "owner")
	ac, _ := auth.AccessFromContext(owner)
	epoch, err := service.Store.BumpUserRevocationEpoch(auth.ContextWithInternalOrigin(owner), ac.UserId)
	require.NoError(t, err)
	keys, err := identity.NewKeyManager(t.TempDir(), "")
	require.NoError(t, err)
	require.NoError(t, keys.Load())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(identity.BuildJWKS(keys, time.Now()))
	}))
	t.Cleanup(server.Close)
	issuer, err := identity.NewJWTIssuer(keys, identity.Config{Enabled: true, BaseURL: server.URL, JWTAudience: "memql"})
	require.NoError(t, err)
	token, _, err := issuer.IssueAccessToken(identity.IssueInput{UserId: ac.UserId, Role: "owner", RevocationEpoch: epoch}, time.Now())
	require.NoError(t, err)
	config := verifier.Config{BaseURL: server.URL, ExpectedAudience: "memql", JWKSRefreshInterval: time.Hour, JWKSFetchTimeout: 5 * time.Second}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cache, err := verifier.NewJWKSCache(config, logger)
	require.NoError(t, err)
	verify, err := verifier.New(config, cache, nil, logger)
	require.NoError(t, err)
	claims, err := verify.VerifyBearer(context.Background(), token)
	require.NoError(t, err)
	require.IsType(t, float64(0), claims.ClaimsMap["exp"])
	require.IsType(t, float64(0), claims.ClaimsMap["revocation_epoch"])
	ctx := auth.ContextWithAccess(verifier.AttachToContext(context.Background(), claims), ac)
	session, err := service.Start(ctx, "reader")
	require.NoError(t, err)
	require.NoError(t, service.End(ctx, session.ID))
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
	_, err = otherEngine.Execute(viewer, fmt.Sprintf(`mutation createAskConversation(requestId: %s, title: "Role test")`, langparser.QuoteString(uuid.NewString())))
	require.NoError(t, err)
	folder := "v1:library:folder:" + uuid.NewString()
	_, err = engine.Execute(viewer, fmt.Sprintf(`mutation createLibraryFolder(folderId: %s, name: "Test folder")`, langparser.QuoteString(folder)))
	require.NoError(t, err)
	_, err = otherEngine.Execute(viewer, fmt.Sprintf(`mutation renameLibraryFolder(folderId: %s, name: "Edited")`, langparser.QuoteString(folder)))
	require.NoError(t, err)
	stranger := seedOperator(t, one, "owner")
	second, err := one.Start(stranger, "reader")
	require.NoError(t, err)
	otherViewer := personaContext(t, two, otherEngine, second.Token)
	_, err = otherEngine.Execute(otherViewer, fmt.Sprintf(`mutation renameLibraryFolder(folderId: %s, name: "Not mine")`, langparser.QuoteString(folder)))
	require.Error(t, err)
	ac, _ := auth.AccessFromContext(viewer)
	authority, err := auth.ForwardedAuthorityForUser(ac, auth.RoleTestClass, "", session.ExpiresAt, time.Now())
	require.NoError(t, err)
	// The node module tests the protobuf round trip; this lower-level module
	// exercises the receiving replica's shared validation and durable restore.
	require.Equal(t, session.ID, authority.RoleTestSessionID)
	resolved, err := auth.VerifyForwardedAuthority(authority, time.Now())
	require.NoError(t, err)
	forwarded := auth.BindForwardedContext(context.Background(), authority.Principal().Claims, resolved, authority)
	require.NoError(t, auth.ValidateRoleTestContext(forwarded))
	borrowed := auth.ContextWithUserActor(forwarded, session.UserID)
	require.Same(t, forwarded, borrowed)
	detached, cancelDetached, err := auth.DetachUserWork(viewer, session.UserID)
	require.NoError(t, err)
	defer cancelDetached()
	require.NoError(t, auth.ValidateRoleTestContext(detached))
	require.Error(t, auth.ValidateRoleTestContext(auth.ContextWithUserActor(viewer, "another-user")))
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
	require.Error(t, auth.ValidateRoleTestContext(borrowed))
	require.Error(t, auth.ValidateRoleTestContext(detached))
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
	rows, _, err := one.List(owner)
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
		query := fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "read", resourceType: "data", effect: %s, grantedBy: "role-test-regression")`, langparser.QuoteString(grantID), langparser.QuoteString(user), langparser.QuoteString(effect))
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

// ---------------------------------------------------------------------
// Custom and organization roles
// ---------------------------------------------------------------------
//
// Only the role CATALOG is a fixture: the engine here never runs the seed
// materializer, so the role rows are not there to lean on (the choice
// integrations/rbac's access cases make). Every group, membership and grant is
// a row written through the real mutations, and every read the service makes
// goes through its real sources.

const (
	supportLead    = "support-lead"    // custom, rank 150: below a developer, holds what one holds
	releaseCaptain = "release-captain" // custom, rank 350: above a developer, below an owner
	peopleSteward  = "people-steward"  // custom, rank 150: holds update on principal, which a developer lacks
	retiredHelper  = "retired-helper"  // custom, retired
	siteModerator  = "site-moderator"  // organization role, rank 20, scoped to the case's organization
)

type fixtureCatalog struct {
	ranks     map[string]int
	canonical map[string]string
	names     map[string]string
	scopes    map[string]string
	retired   map[string]bool
	grants    map[string]map[auth.VerbResource]bool
}

func (c *fixtureCatalog) resolve(slug string) string {
	return c.canonical[strings.ToLower(strings.TrimSpace(slug))]
}
func (c *fixtureCatalog) Rank(slug string) (int, bool) {
	r, ok := c.ranks[strings.ToLower(strings.TrimSpace(slug))]
	return r, ok
}
func (c *fixtureCatalog) Name(slug string) string { return c.names[c.resolve(slug)] }
func (c *fixtureCatalog) Holds(slug, verb, resource string) bool {
	return c.Active(slug) && c.grants[c.resolve(slug)][auth.VerbResource{Verb: verb, Resource: resource}]
}
func (c *fixtureCatalog) Grants(slug string) []auth.VerbResource {
	if !c.Active(slug) {
		return nil
	}
	out := []auth.VerbResource{}
	for vr := range c.grants[c.resolve(slug)] {
		out = append(out, vr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource+out[i].Verb < out[j].Resource+out[j].Verb })
	return out
}
func (c *fixtureCatalog) Scope(slug string) string { return c.scopes[c.resolve(slug)] }
func (c *fixtureCatalog) Active(slug string) bool {
	s := c.resolve(slug)
	return s != "" && !c.retired[s]
}
func (c *fixtureCatalog) CanonicalSlug(slug string) string { return c.resolve(slug) }
func (c *fixtureCatalog) Slugs() []string {
	out := []string{}
	for slug := range c.names {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// installCatalog installs the ladder with the organization role scoped to
// `organization`, for one case, and returns it so the case can rename a role.
func installCatalog(t *testing.T, organization string) *fixtureCatalog {
	t.Helper()
	vr := func(v, r string) auth.VerbResource { return auth.VerbResource{Verb: v, Resource: r} }
	set := func(pairs ...auth.VerbResource) map[auth.VerbResource]bool {
		m := map[auth.VerbResource]bool{}
		for _, p := range pairs {
			m[p] = true
		}
		return m
	}
	engineers := []auth.VerbResource{vr("read", "data"), vr("create", "data"), vr("update", "data"),
		vr("read", "app:deployables"), vr("execute", "app:deployables/reviews"), vr("read", "principal")}
	c := &fixtureCatalog{
		ranks: map[string]int{"owner": 400, releaseCaptain: 350, "developer": 300, "admin": 200,
			supportLead: 150, peopleSteward: 150, "user": 100, "viewer": 50, retiredHelper: 30, siteModerator: 20},
		names: map[string]string{"owner": "Owner", releaseCaptain: "Release captain", "developer": "Developer", "admin": "Admin",
			supportLead: "Support lead", peopleSteward: "People steward", "user": "Member", "viewer": "Viewer",
			retiredHelper: "Retired helper", siteModerator: "Site moderator"},
		canonical: map[string]string{},
		scopes:    map[string]string{siteModerator: organization},
		retired:   map[string]bool{retiredHelper: true},
		grants: map[string]map[auth.VerbResource]bool{
			"owner":     set(append(engineers, vr("update", "principal"), vr("update", "group"))...),
			"developer": set(engineers...),
			"admin":     set(vr("read", "data"), vr("update", "principal"), vr("update", "group")),
			"user":      set(vr("read", "data"), vr("create", "data"), vr("read", "group")),
			"viewer":    set(vr("read", "data"), vr("read", "group")),
			supportLead: set(vr("read", "data"), vr("update", "data")), releaseCaptain: set(vr("read", "data")),
			peopleSteward: set(vr("read", "data"), vr("update", "principal")), retiredHelper: set(vr("read", "data")),
			siteModerator: set(vr("read", "data"), vr("update", "data")),
		},
	}
	for slug := range c.names {
		c.canonical[slug] = slug
	}
	for alias, slug := range map[string]string{"writer": "user", "reader": "viewer"} {
		c.canonical[alias], c.ranks[alias] = slug, c.ranks[slug]
	}
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(c)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	return c
}

// deploymentExec runs one fixture statement as the cluster acting: the system
// actor under internal origin, which the @serverOnly group, membership and
// grant writers require, and which reads every membership row.
func deploymentExec(t *testing.T, e *memql.MemQLEngine, q string) *memql.ExecuteResult {
	t.Helper()
	res, err := e.Execute(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "role-test-fixture")), q)
	require.NoError(t, err, q)
	return res
}

// organization writes an active organization group at the id ensureAccountGroup
// derives for every account, acct-<account>, and returns the group id.
func organization(t *testing.T, e *memql.MemQLEngine, account, name string) string {
	t.Helper()
	group := "acct-" + account
	deploymentExec(t, e, fmt.Sprintf(`mutation writeGroup(groupId: %s, name: %s, kind: "account", accountId: %s, status: "active")`,
		langparser.QuoteString(group), langparser.QuoteString(name), langparser.QuoteString(account)))
	return group
}

func groupGrant(t *testing.T, e *memql.MemQLEngine, group, verb, resource string) {
	t.Helper()
	id := auth.GrantRowID(auth.SubjectKindGroup, group, verb, resource)
	deploymentExec(t, e, fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "group", subjectId: %s, verb: %s, resourceType: %s, effect: "allow", grantedBy: "role-test-fixture")`,
		langparser.QuoteString(id), langparser.QuoteString(group), langparser.QuoteString(verb), langparser.QuoteString(resource)))
}

// join writes one membership the way somebody other than a role test would.
func join(t *testing.T, e *memql.MemQLEngine, group, account, user, origin, status string) {
	t.Helper()
	deploymentExec(t, e, fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, accountId: %s, userId: %s, origin: %s, status: %s)`,
		langparser.QuoteString(group+"-"+memql.BareShortId(user)), langparser.QuoteString(group), langparser.QuoteString(account),
		langparser.QuoteString(memql.BareShortId(user)), langparser.QuoteString(origin), langparser.QuoteString(status)))
}

// activeGroups is what the engine's authorization reads place a person in.
func activeGroups(t *testing.T, user string) []string {
	t.Helper()
	groups, err := auth.InstalledMembershipSource().(auth.CheckedMembershipSource).ActiveGroupIdsForUserChecked(context.Background(), user)
	require.NoError(t, err)
	out := []string{}
	for _, g := range groups {
		out = append(out, memql.BareShortId(g))
	}
	sort.Strings(out)
	return out
}

// membershipRows is every membership row of a person, newest version per row,
// removed ones included, keyed by bare group id.
func membershipRows(t *testing.T, e *memql.MemQLEngine, user string) map[string]map[string]any {
	t.Helper()
	res := deploymentExec(t, e, fmt.Sprintf(`query groupsForUser(userId: %s, includeRemoved: true)`, langparser.QuoteString(memql.BareShortId(user))))
	out := map[string]map[string]any{}
	for _, row := range memql.MaterializeRows(res) {
		group, _ := row["groupId"].(string)
		out[memql.BareShortId(group)] = row
	}
	return out
}

func lastAudit(t *testing.T, s *Service, action string) identity.AuditEvent {
	t.Helper()
	sink := s.Audit.(*auditSink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for i := len(sink.events) - 1; i >= 0; i-- {
		if sink.events[i].Action == action {
			return sink.events[i]
		}
	}
	t.Fatalf("no %s audit event", action)
	return identity.AuditEvent{}
}

// persona reads a persona's user row as Start and verification read it.
func persona(t *testing.T, s *Service, userID string) identity.UserRow {
	t.Helper()
	u, err := s.Store.LookupUserById(fresh(context.Background()), userID)
	require.NoError(t, err)
	require.NotNil(t, u, userID)
	return *u
}

// sessionRow is what a session's row records: the organization its Start
// wrote (NULL when it wrote none), and whether the session is revoked.
func sessionRow(t *testing.T, s *Service, id string) (account sql.NullString, revoked bool) {
	t.Helper()
	require.NoError(t, s.Store.DirectDB().QueryRowContext(context.Background(),
		`SELECT account_id, revoked_at IS NOT NULL FROM identity_role_test_sessions WHERE id=$1`, id).Scan(&account, &revoked))
	return account, revoked
}

// warnings keeps the message of everything logged at warning level or above.
type warnings struct {
	mu   sync.Mutex
	seen []string
}

func (w *warnings) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelWarn }
func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = append(w.seen, r.Message)
	return nil
}
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnings) WithGroup(string) slog.Handler      { return w }
func (w *warnings) messages() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

// captureWarnings gives the service's Store a logger, which the one in
// realService does not have, and returns what it is warned.
func captureWarnings(s *Service) *warnings {
	w := &warnings{}
	s.Store.Logger = slog.New(w)
	return w
}

// refusingEngine runs every statement on the engine it wraps, save the
// mutations it was last told to refuse, which fail as a refused write does.
type refusingEngine struct {
	identity.EngineExecutor
	mu      sync.Mutex
	refused []string
}

func (e *refusingEngine) refuse(mutations ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refused = mutations
}

func (e *refusingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.mu.Lock()
	refused := e.refused
	e.mu.Unlock()
	for _, m := range refused {
		if strings.HasPrefix(query, "mutation "+m+"(") {
			return nil, fmt.Errorf("%s refused by the test", m)
		}
	}
	return e.EngineExecutor.Execute(ctx, query)
}

// refuseWrites puts a refusingEngine, refusing nothing yet, between the
// service and its engine.
func refuseWrites(s *Service) *refusingEngine {
	e := &refusingEngine{EngineExecutor: s.Store.Engine}
	s.Store.Engine = e
	return e
}

// TestAnOperatorMayNotTestARoleAboveOrHoldingWhatTheyDoNot: a custom role is
// tested on the predefined roles' terms. Rank decides first -- strictly below
// the operator -- and the per-capability rule decides after it: a developer
// cannot start a role holding something the developer does not hold.
func TestAnOperatorMayNotTestARoleAboveOrHoldingWhatTheyDoNot(t *testing.T) {
	service, _ := realService(t)
	installCatalog(t, "org-"+uuid.NewString())
	developer := seedOperator(t, service, "developer")
	owner := seedOperator(t, service, "owner")

	session, err := service.Start(developer, supportLead)
	require.NoError(t, err, "a custom role below the developer that holds nothing the developer lacks")
	require.Equal(t, supportLead, session.Role)
	persona, err := service.Store.LookupUserById(fresh(context.Background()), session.UserID)
	require.NoError(t, err)
	require.Equal(t, "Test Support lead", persona.DisplayName)
	require.Equal(t, supportLead, lastAudit(t, service, "role_test_started").Detail["role"])
	_, err = service.VerifyRoleTest(context.Background(), session.Token)
	require.NoError(t, err)

	for role, why := range map[string]string{
		releaseCaptain: "ranks above a developer",
		peopleSteward:  "holds update on principal, which a developer does not",
		retiredHelper:  "is retired",
	} {
		_, err := service.Start(developer, role)
		require.ErrorIs(t, err, ErrDenied, "a developer may not test a role that %s", why)
	}
	_, err = service.Start(owner, releaseCaptain)
	require.NoError(t, err, "an owner ranks above every custom role")
	_, err = service.Start(owner, peopleSteward)
	require.NoError(t, err, "an owner holds everything")
	_, err = service.Start(owner, retiredHelper)
	require.ErrorIs(t, err, ErrDenied, "a retired role is tested by nobody")
}

// TestAnOrganizationRoleTestIsAMemberOfThatOrganizationOnly: the persona of an
// organization role joins exactly that organization's group, marked as a role
// test, and so sees what that organization's people see and nothing of any
// other. Verification accepts that one membership and nothing else, and every
// other persona still holds none at all.
func TestAnOrganizationRoleTestIsAMemberOfThatOrganizationOnly(t *testing.T) {
	service, engine := realService(t)
	acme, beta := "org-"+uuid.NewString(), "org-"+uuid.NewString()
	installCatalog(t, acme)
	acmeGroup, betaGroup := organization(t, engine, acme, "Acme"), organization(t, engine, beta, "Beta")
	groupGrant(t, engine, acmeGroup, "read", "app:deployables")
	groupGrant(t, engine, acmeGroup, "execute", "app:deployables/reviews")
	developer := seedOperator(t, service, "developer")

	session, err := service.Start(developer, siteModerator)
	require.NoError(t, err, "a developer holds everything the role and its organization's group give")
	require.Equal(t, []string{acmeGroup}, activeGroups(t, session.UserID))
	placed := membershipRows(t, engine, session.UserID)[acmeGroup]
	require.Equal(t, "role-test", placed["origin"])
	require.Equal(t, "active", placed["status"])
	require.Equal(t, acme, memql.BareShortId(fmt.Sprint(placed["accountId"])))
	persona, err := service.Store.LookupUserById(fresh(context.Background()), session.UserID)
	require.NoError(t, err)
	require.Equal(t, "Test Site moderator (Acme)", persona.DisplayName)
	started := lastAudit(t, service, "role_test_started")
	require.Equal(t, siteModerator, started.Detail["role"])
	require.Equal(t, acme, started.Detail["accountId"])
	_, err = service.VerifyRoleTest(context.Background(), session.Token)
	require.NoError(t, err)
	// The engine's own resolution agrees: this persona reaches Acme and no
	// other organization.
	decisions := engine.ResolveOrganizationDecisionsFor(context.Background(), session.UserID, siteModerator, nil)
	require.NotEmpty(t, decisions)
	for _, d := range decisions {
		require.Equal(t, acme, memql.BareShortId(d.AccountID))
	}

	join(t, engine, betaGroup, beta, session.UserID, "added", "active")
	_, err = service.VerifyRoleTest(context.Background(), session.Token)
	require.ErrorIs(t, err, ErrInvalid, "a second organization's group is access its operator never tested")
	join(t, engine, betaGroup, beta, session.UserID, "added", "removed")
	_, err = service.VerifyRoleTest(context.Background(), session.Token)
	require.NoError(t, err)
	join(t, engine, acmeGroup, acme, session.UserID, "added", "active")
	_, err = service.VerifyRoleTest(context.Background(), session.Token)
	require.ErrorIs(t, err, ErrInvalid, "the right group joined some other way is not the test's membership")

	reader, err := service.Start(developer, "reader")
	require.NoError(t, err)
	require.Empty(t, activeGroups(t, reader.UserID))
	join(t, engine, acmeGroup, acme, reader.UserID, "role-test", "active")
	_, err = service.VerifyRoleTest(context.Background(), reader.Token)
	require.ErrorIs(t, err, ErrInvalid, "a global role's persona still holds no membership at all")

	deploymentExec(t, engine, fmt.Sprintf(`mutation writeGroup(groupId: %s, name: "Acme", kind: "account", accountId: %s, status: "archived")`,
		langparser.QuoteString(acmeGroup), langparser.QuoteString(acme)))
	_, err = service.Start(developer, siteModerator)
	require.ErrorIs(t, err, ErrDenied, "an organization with no active group has nothing to test in")
}

// TestAPersonaIsCalledWhatItTests: a persona is named for its role and, for an
// organization role, its organization, and the next Start renames it when
// either was renamed or the role moved to another organization -- before the
// session it issues, and writing that one field of the persona's row alone. A
// rename that fails is logged and the test starts all the same: the name is
// what the persona is called, not what it may do.
func TestAPersonaIsCalledWhatItTests(t *testing.T) {
	service, engine := realService(t)
	acme, beta := "org-"+uuid.NewString(), "org-"+uuid.NewString()
	catalog := installCatalog(t, acme)
	organization(t, engine, acme, "Acme")
	organization(t, engine, beta, "Beta")
	developer := seedOperator(t, service, "developer")
	refusing := refuseWrites(service)
	warned := captureWarnings(service)

	first, err := service.Start(developer, siteModerator)
	require.NoError(t, err)
	before := persona(t, service, first.UserID)
	require.Equal(t, "Test Site moderator (Acme)", before.DisplayName)

	organization(t, engine, acme, "Acme Corp")
	second, err := service.Start(developer, siteModerator)
	require.NoError(t, err)
	require.Equal(t, first.UserID, second.UserID, "one persona per operator and role")
	after := persona(t, service, second.UserID)
	require.Equal(t, "Test Site moderator (Acme Corp)", after.DisplayName, "the organization's group was renamed")
	unchanged := before
	unchanged.DisplayName, unchanged.CreatedAt = after.DisplayName, after.CreatedAt
	require.Equal(t, unchanged, after, "the rename writes the display name and nothing else: role, email, active and the rest stay")
	vc, err := service.VerifyRoleTest(context.Background(), second.Token)
	require.NoError(t, err)
	require.Equal(t, "Test Site moderator (Acme Corp)", vc.Name, "the session Start issued is the renamed persona's")

	catalog.names[siteModerator] = "Content moderator"
	_, err = service.Start(developer, siteModerator)
	require.NoError(t, err)
	require.Equal(t, "Test Content moderator (Acme Corp)", persona(t, service, first.UserID).DisplayName, "the role was renamed")

	installCatalog(t, beta)
	_, err = service.Start(developer, siteModerator)
	require.NoError(t, err)
	require.Equal(t, "Test Site moderator (Beta)", persona(t, service, first.UserID).DisplayName, "the role moved to another organization")
	require.Empty(t, warned.messages())

	refusing.refuse("updateUser")
	organization(t, engine, beta, "Beta Labs")
	refused, err := service.Start(developer, siteModerator)
	require.NoError(t, err, "a rename that fails never stops the test")
	_, err = service.VerifyRoleTest(context.Background(), refused.Token)
	require.NoError(t, err)
	require.Equal(t, "Test Site moderator (Beta)", persona(t, service, first.UserID).DisplayName)
	require.Contains(t, warned.messages(), "role test: renaming a persona failed")
	refusing.refuse()
	_, err = service.Start(developer, siteModerator)
	require.NoError(t, err)
	require.Equal(t, "Test Site moderator (Beta Labs)", persona(t, service, first.UserID).DisplayName, "the next Start renames it")
}

// TestEndingAnOrganizationRoleTestRemovesItsMembership (review focus 3): a
// role test of an organization role ends with no membership left behind --
// ended by its operator or from its own tab -- and replacing the test keeps
// exactly the live run's one membership, moving it when the role's
// organization moved.
func TestEndingAnOrganizationRoleTestRemovesItsMembership(t *testing.T) {
	service, engine := realService(t)
	acme, beta := "org-"+uuid.NewString(), "org-"+uuid.NewString()
	installCatalog(t, acme)
	acmeGroup, betaGroup := organization(t, engine, acme, "Acme"), organization(t, engine, beta, "Beta")
	owner := seedOperator(t, service, "owner")

	first, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	second, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	require.Equal(t, first.UserID, second.UserID, "one persona per operator and role")
	require.Equal(t, []string{acmeGroup}, activeGroups(t, second.UserID))
	require.NoError(t, service.End(owner, first.ID), "ending the run that was replaced")
	require.Equal(t, []string{acmeGroup}, activeGroups(t, second.UserID), "the live run keeps its membership")
	_, err = service.VerifyRoleTest(context.Background(), second.Token)
	require.NoError(t, err)

	require.NoError(t, service.End(owner, second.ID))
	require.Empty(t, activeGroups(t, second.UserID))
	require.Equal(t, "removed", membershipRows(t, engine, second.UserID)[acmeGroup]["status"])
	ended := lastAudit(t, service, "role_test_ended")
	require.Equal(t, siteModerator, ended.Detail["role"])
	require.Equal(t, acme, ended.Detail["accountId"])

	third, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	require.Equal(t, []string{acmeGroup}, activeGroups(t, third.UserID))
	auth.SetRoleTestValidator(service)
	require.NoError(t, service.End(personaContext(t, service, engine, third.Token), third.ID), "ended from the test's own tab")
	require.Empty(t, activeGroups(t, third.UserID))

	installCatalog(t, beta)
	moved, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	require.Equal(t, []string{betaGroup}, activeGroups(t, moved.UserID), "the role's organization moved, and the test moved with it")
	require.NoError(t, service.End(owner, moved.ID))
	require.Empty(t, activeGroups(t, moved.UserID))
}

// TestEndAuditsTheOrganizationTheTestRanIn: Start records on the session the
// organization its test runs in -- an empty one for a global role -- and End
// audits that one, not wherever the role points by the time it ends. A session
// from before Start recorded one has nothing to read, and End falls back to the
// role's current organization, as every End did before.
func TestEndAuditsTheOrganizationTheTestRanIn(t *testing.T) {
	service, engine := realService(t)
	acme, beta := "org-"+uuid.NewString(), "org-"+uuid.NewString()
	installCatalog(t, acme)
	organization(t, engine, acme, "Acme")
	organization(t, engine, beta, "Beta")
	owner := seedOperator(t, service, "owner")

	ran, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	account, _ := sessionRow(t, service, ran.ID)
	require.Equal(t, sql.NullString{String: acme, Valid: true}, account)
	installCatalog(t, beta)
	require.NoError(t, service.End(owner, ran.ID))
	ended := lastAudit(t, service, "role_test_ended")
	require.Equal(t, identity.AuditOutcomeSuccess, ended.Outcome)
	require.Equal(t, siteModerator, ended.Detail["role"])
	require.Equal(t, acme, ended.Detail["accountId"], "the organization the test ran in, not the one its role names now")
	require.Empty(t, activeGroups(t, ran.UserID), "and its membership went, wherever the role went")

	global, err := service.Start(owner, "reader")
	require.NoError(t, err)
	account, _ = sessionRow(t, service, global.ID)
	require.Equal(t, sql.NullString{String: "", Valid: true}, account, "a global role's test runs in no organization, and says so")
	require.NoError(t, service.End(owner, global.ID))
	require.Equal(t, "", lastAudit(t, service, "role_test_ended").Detail["accountId"])

	older, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	_, err = service.Store.DirectDB().ExecContext(context.Background(), `UPDATE identity_role_test_sessions SET account_id=NULL WHERE id=$1`, older.ID)
	require.NoError(t, err)
	installCatalog(t, acme)
	require.NoError(t, service.End(owner, older.ID))
	require.Equal(t, acme, lastAudit(t, service, "role_test_ended").Detail["accountId"],
		"a session with no organization recorded audits its role's current one")
}

// TestADeveloperCannotTestAnOrganizationRoleWhoseGroupGivesWhatTheyLack (R16):
// an organization role's persona holds its role's capabilities AND what its
// organization's group grants, so the developer must hold both. A group
// letting its people manage the organization's groups is beyond a developer,
// who holds no group capability: the test is refused at Start, and a running
// one stops verifying once the group gains it.
func TestADeveloperCannotTestAnOrganizationRoleWhoseGroupGivesWhatTheyLack(t *testing.T) {
	service, engine := realService(t)
	acme := "org-" + uuid.NewString()
	installCatalog(t, acme)
	acmeGroup := organization(t, engine, acme, "Acme")
	groupGrant(t, engine, acmeGroup, "read", "app:deployables")
	developer := seedOperator(t, service, "developer")
	running, err := service.Start(developer, siteModerator)
	require.NoError(t, err)

	groupGrant(t, engine, acmeGroup, "update", "group")
	_, err = service.Start(developer, siteModerator)
	require.ErrorIs(t, err, ErrDenied, "the group gives the persona update on group, which the developer does not hold")
	_, err = service.VerifyRoleTest(context.Background(), running.Token)
	require.ErrorIs(t, err, ErrInvalid, "a running test stops once its group gives what its operator lacks")
	_, err = service.Start(seedOperator(t, service, "owner"), siteModerator)
	require.NoError(t, err, "an owner holds everything the group gives")
}

// TestARefusedStartAuditsTheOrganizationItWasFor: the organization a test of a
// role would run in is the installed role's alone, so every Start audits it --
// a refused one too, whether the role or the operator was refused.
func TestARefusedStartAuditsTheOrganizationItWasFor(t *testing.T) {
	service, engine := realService(t)
	acme := "org-" + uuid.NewString()
	installCatalog(t, acme)
	groupGrant(t, engine, organization(t, engine, acme, "Acme"), "update", "group")
	developer := seedOperator(t, service, "developer")
	ac, _ := auth.AccessFromContext(developer)

	_, err := service.Start(developer, siteModerator)
	require.ErrorIs(t, err, ErrDenied, "the group gives the persona update on group, which the developer does not hold")
	refused := lastAudit(t, service, "role_test_started")
	require.Equal(t, identity.AuditOutcomeFailure, refused.Outcome)
	require.Equal(t, ac.UserId, refused.ActorUserId)
	require.Equal(t, siteModerator, refused.Detail["role"])
	require.Equal(t, acme, refused.Detail["accountId"], "the organization the refused test was for")

	_, err = service.Start(seedOperator(t, service, "admin"), siteModerator)
	require.ErrorIs(t, err, ErrDenied, "an admin tests nothing")
	refused = lastAudit(t, service, "role_test_started")
	require.Equal(t, identity.AuditOutcomeFailure, refused.Outcome)
	require.Equal(t, acme, refused.Detail["accountId"], "refused before the operator was known, and still for that organization")

	_, err = service.Start(developer, "admin")
	require.ErrorIs(t, err, ErrDenied)
	require.Equal(t, "", lastAudit(t, service, "role_test_started").Detail["accountId"], "a global role is for no organization")
}

// TestAnExpiredOrganizationRoleTestKeepsItsMembershipOnlyUntilTheNextStartOrEnd
// (R14): a test that expires without End leaves its membership in place --
// reads write nothing -- until that operator's next Start or End, of any role,
// removes it.
func TestAnExpiredOrganizationRoleTestKeepsItsMembershipOnlyUntilTheNextStartOrEnd(t *testing.T) {
	service, engine := realService(t)
	acme := "org-" + uuid.NewString()
	installCatalog(t, acme)
	acmeGroup := organization(t, engine, acme, "Acme")
	owner := seedOperator(t, service, "owner")

	expired, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	service.Now = func() time.Time { return expired.ExpiresAt.Add(time.Minute) }
	_, err = service.VerifyRoleTest(context.Background(), expired.Token)
	require.ErrorIs(t, err, ErrInvalid)
	rows, _, err := service.List(owner)
	require.NoError(t, err)
	require.Empty(t, rows)
	require.Equal(t, []string{acmeGroup}, activeGroups(t, expired.UserID), "nothing removes it inside a read")
	_, err = service.Start(owner, "reader")
	require.NoError(t, err)
	require.Empty(t, activeGroups(t, expired.UserID), "the operator's next Start removes it")

	service.Now = nil
	lapsed, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	other, err := service.Start(owner, "writer")
	require.NoError(t, err)
	service.Now = func() time.Time { return lapsed.ExpiresAt.Add(time.Minute) }
	require.Equal(t, []string{acmeGroup}, activeGroups(t, lapsed.UserID))
	require.NoError(t, service.End(owner, other.ID))
	require.Empty(t, activeGroups(t, lapsed.UserID), "the operator's next End removes it")
}

// TestAnEndWhoseMembershipStaysHasStillEndedTheTest: End revokes its session
// first, on a statement of its own, so a membership that then fails to go
// neither revives the test nor makes the End a failure. The failure is logged,
// the membership admits nobody, and the operator's next Start or End removes
// it, as it removes an expired test's.
func TestAnEndWhoseMembershipStaysHasStillEndedTheTest(t *testing.T) {
	service, engine := realService(t)
	acme := "org-" + uuid.NewString()
	installCatalog(t, acme)
	acmeGroup := organization(t, engine, acme, "Acme")
	owner := seedOperator(t, service, "owner")
	refusing := refuseWrites(service)
	warned := captureWarnings(service)

	session, err := service.Start(owner, siteModerator)
	require.NoError(t, err)
	refusing.refuse("writeGroupMembership")
	require.NoError(t, service.End(owner, session.ID), "the test ended, whatever became of its membership")
	refusing.refuse()
	_, revoked := sessionRow(t, service, session.ID)
	require.True(t, revoked, "its session is revoked")
	_, err = service.VerifyRoleTest(context.Background(), session.Token)
	require.ErrorIs(t, err, ErrInvalid)
	ended := lastAudit(t, service, "role_test_ended")
	require.Equal(t, identity.AuditOutcomeSuccess, ended.Outcome)
	require.Equal(t, acme, ended.Detail["accountId"])
	require.Contains(t, warned.messages(), "role test: removing an ended test's membership failed")
	require.Equal(t, []string{acmeGroup}, activeGroups(t, session.UserID), "the membership stays")

	_, err = service.Start(owner, "reader")
	require.NoError(t, err)
	require.Empty(t, activeGroups(t, session.UserID), "until the operator's next Start removes it")
}

// TestTheConceptSaysWhenARoleTestMembershipGoes: groupMembership's `origin`,
// which the concept catalog publishes, tells a reader when a role-test
// membership goes the way the tests above prove it: when its test is ended, or
// at the operator's next Start or End once the test has expired. Not "when the
// test ends": an expired test ends with nothing removing it.
func TestTheConceptSaysWhenARoleTestMembershipGoes(t *testing.T) {
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	membership, err := concept.DefaultRegistry().Get("v1:identity:groupMembership")
	require.NoError(t, err)
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(membership.Schemas["definition"], &schema))
	origin := schema.Properties["origin"].Description
	require.Contains(t, origin, "removed when the test is ended, or at the operator's next Start or End after it expires")
	require.NotContains(t, origin, "removed when the test ends")
}

func offeredSlugs(t *testing.T, s *Service, ctx context.Context) []string {
	t.Helper()
	_, roles, err := s.List(ctx)
	require.NoError(t, err)
	out := []string{}
	for _, r := range roles {
		out = append(out, r.Slug)
	}
	return out
}

// everyRole is every role a test could name in the fixture cluster, the
// refused spellings included.
var everyRole = []string{"reader", "writer", "developer", "admin", "owner", "user", "viewer",
	releaseCaptain, peopleSteward, supportLead, retiredHelper, siteModerator}

// acceptedSlugs is the roles Start accepts for this operator, tried one by one.
func acceptedSlugs(t *testing.T, s *Service, ctx context.Context) []string {
	t.Helper()
	out := []string{}
	for _, role := range everyRole {
		if _, err := s.Start(ctx, role); err == nil {
			out = append(out, role)
		} else {
			require.ErrorIs(t, err, ErrDenied, role)
		}
	}
	return out
}

// TestTheOfferedRolesAreExactlyTheOnesStartAccepts: the list answer carries the
// roles THIS operator may test, decided by the gates Start applies, so a client
// draws no test the server would refuse -- an act that is not legal is absent.
// Every role the cluster knows is tried: a listed one starts, an unlisted one is
// refused. The predefined roles first, then the custom ones strongest first.
func TestTheOfferedRolesAreExactlyTheOnesStartAccepts(t *testing.T) {
	service, engine := realService(t)
	acme := "org-" + uuid.NewString()
	installCatalog(t, acme)
	acmeGroup := organization(t, engine, acme, "Acme")
	groupGrant(t, engine, acmeGroup, "read", "app:deployables")

	owner := seedOperator(t, service, "owner")
	require.Equal(t, []string{"reader", "writer", "developer", "admin", releaseCaptain, peopleSteward, supportLead, siteModerator},
		offeredSlugs(t, service, owner), "an owner is offered every role below them")
	require.ElementsMatch(t, offeredSlugs(t, service, owner), acceptedSlugs(t, service, owner))
	_, roles, err := service.List(owner)
	require.NoError(t, err)
	labels := map[string]Role{}
	for _, r := range roles {
		labels[r.Slug] = r
	}
	require.Equal(t, Role{Slug: "reader", Name: "Viewer"}, labels["reader"])
	require.Equal(t, Role{Slug: supportLead, Name: "Support lead"}, labels[supportLead])
	require.Equal(t, Role{Slug: siteModerator, Name: "Site moderator", AccountID: acme, Organization: "Acme"}, labels[siteModerator])

	developer := seedOperator(t, service, "developer")
	require.Equal(t, []string{"reader", "writer", "developer", supportLead, siteModerator}, offeredSlugs(t, service, developer),
		"no admin, no role above a developer, none holding what a developer lacks")
	require.ElementsMatch(t, offeredSlugs(t, service, developer), acceptedSlugs(t, service, developer))

	groupGrant(t, engine, acmeGroup, "update", "group")
	require.Equal(t, []string{"reader", "writer", "developer", supportLead}, offeredSlugs(t, service, developer),
		"an organization role whose group gives what the developer lacks is not offered")
	require.ElementsMatch(t, offeredSlugs(t, service, developer), acceptedSlugs(t, service, developer))

	deploymentExec(t, engine, fmt.Sprintf(`mutation writeGroup(groupId: %s, name: "Acme", kind: "account", accountId: %s, status: "archived")`,
		langparser.QuoteString(acmeGroup), langparser.QuoteString(acme)))
	require.NotContains(t, offeredSlugs(t, service, owner), siteModerator, "an organization with no active group has nothing to test in")
	require.ElementsMatch(t, offeredSlugs(t, service, owner), acceptedSlugs(t, service, owner))

	restricted := seedOperator(t, service, "developer")
	ac, _ := auth.AccessFromContext(restricted)
	deploymentExec(t, engine, fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "read", resourceType: "data", effect: "deny", grantedBy: "role-test-fixture")`,
		langparser.QuoteString(auth.GrantRowID(auth.SubjectKindUser, ac.UserId, "read", "data")), langparser.QuoteString(ac.UserId)))
	require.Empty(t, offeredSlugs(t, service, restricted), "a restricted developer is offered no role, and the answer is not an error")
	require.Empty(t, acceptedSlugs(t, service, restricted))
}

// blinkingGrants fails the grant reads for the subjects it names and answers
// every other read from the real source.
type blinkingGrants struct {
	auth.GrantSource
	users, groups map[string]bool
}

func (b blinkingGrants) ActiveGrantsForUser(ctx context.Context, id string) ([]auth.Grant, error) {
	if b.users[memql.BareShortId(id)] {
		return nil, errors.New("grant read failed")
	}
	return b.GrantSource.ActiveGrantsForUser(ctx, id)
}

func (b blinkingGrants) ActiveGrantsForGroups(ctx context.Context, ids []string) ([]auth.Grant, error) {
	for _, id := range ids {
		if b.groups[memql.BareShortId(id)] {
			return nil, errors.New("grant read failed")
		}
	}
	return b.GrantSource.ActiveGrantsForGroups(ctx, ids)
}

// TestAFailedReadLeavesTheRoleOut: a read that fails while a role is decided
// leaves that role out -- refuse, never admit. Failing to read the
// organization group's grants loses that organization's role and nothing
// else; failing to read the operator's own grants loses every role.
func TestAFailedReadLeavesTheRoleOut(t *testing.T) {
	service, engine := realService(t)
	acme := "org-" + uuid.NewString()
	installCatalog(t, acme)
	acmeGroup := organization(t, engine, acme, "Acme")
	developer := seedOperator(t, service, "developer")
	require.Equal(t, []string{"reader", "writer", "developer", supportLead, siteModerator}, offeredSlugs(t, service, developer))

	source := auth.InstalledGrantSource()
	t.Cleanup(func() { auth.SetGrantSource(source) })
	auth.SetGrantSource(blinkingGrants{GrantSource: source, groups: map[string]bool{acmeGroup: true}})
	require.Equal(t, []string{"reader", "writer", "developer", supportLead}, offeredSlugs(t, service, developer))
	_, err := service.Start(developer, siteModerator)
	require.ErrorIs(t, err, ErrDenied)

	ac, _ := auth.AccessFromContext(developer)
	auth.SetGrantSource(blinkingGrants{GrantSource: source, users: map[string]bool{memql.BareShortId(ac.UserId): true}})
	require.Empty(t, offeredSlugs(t, service, developer))
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
