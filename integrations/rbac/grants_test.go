package rbac

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// EACH GOVERNANCE RULE REFUSES BY ITS CODE (app access grants record, section
// 3, decision D5; epic memql#5297).
//
// One case per refusal, the empty-group case, and the writes that a
// successful set and revoke produce. The rank ladder and the caller's grants
// come from the test catalog; the subject's role, a group's members and an
// existing grant come from the recording engine, which answers the four reads
// these handlers make. Nothing here touches a database, which is the point:
// the guards are the whole product of grants.go.

// grantsEngine is the recording engine with the subject reads filled in: a
// user directory (id -> role), a group directory (id -> its status and
// members) and at most one existing grant, for the revoke cases.
func grantsEngine(t *testing.T) (*Integration, *recordingEngine) {
	t.Helper()
	cat := newTestCatalog()
	// The catalog's `admin` and `owner` hold update-on-principal; give admin
	// and owner a resource to hand out, and developer nothing on principal.
	cat.grants["admin"][componentAuth.VerbResource{Verb: "read", Resource: "app:deployables"}] = true
	cat.grants["owner"][componentAuth.VerbResource{Verb: "read", Resource: "app:deployables"}] = true
	cat.grants["owner"][componentAuth.VerbResource{Verb: "execute", Resource: "app:deployables/publish"}] = true
	i, eng := newRoleIntegration(t, cat)
	eng.users = map[string]string{
		"member":   "writer",
		"admin2":   "admin",
		"dev":      "developer",
		"theowner": "owner",
		"ghost":    "no-such-role",
		"caller":   "admin",
	}
	eng.groups = map[string]testGroup{
		"support":  {status: "active", members: []string{"member", "admin2"}},
		"board":    {status: "active", members: []string{"member", "theowner"}},
		"empty":    {status: "active"},
		"archived": {status: "archived", members: []string{"member"}},
		"haunted":  {status: "active", members: []string{"ghost"}},
	}
	// Grants are consulted through the resolver too; none installed here, so
	// the caller's own capabilities are exactly the catalog's.
	componentAuth.SetGrantSource(nil)
	return i, eng
}

func setArgs(kind, subject, verb, resource, effect string) map[string]any {
	return map[string]any{
		"subjectKind": kind, "subjectId": subject, "verb": verb, "resourceType": resource, "effect": effect,
	}
}

// readGrantDecision decodes {ok, grantId, code} off the single node.
func readGrantDecision(t *testing.T, nodes []memorynodes.MemoryNode, err error) (bool, string, string) {
	t.Helper()
	if err != nil {
		t.Fatalf("handler returned an error rather than a decision: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want exactly one decision node, got %d", len(nodes))
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatalf("decision payload is not JSON: %v", err)
	}
	ok, _ := payload["ok"].(bool)
	code, _ := payload["code"].(string)
	id, _ := payload["grantId"].(string)
	return ok, code, id
}

func setDecision(t *testing.T, i *Integration, ctx context.Context, args map[string]any) (bool, string, string) {
	t.Helper()
	nodes, err := i.handleGrantSet(ctx, args, 0)
	return readGrantDecision(t, nodes, err)
}

func revokeDecision(t *testing.T, i *Integration, ctx context.Context, args map[string]any) (bool, string, string) {
	t.Helper()
	nodes, err := i.handleGrantRevoke(ctx, args, 0)
	return readGrantDecision(t, nodes, err)
}

func TestGrantSetGuards(t *testing.T) {
	const app = "app:deployables"
	for _, tc := range []struct {
		name   string
		caller string
		args   map[string]any
		code   string
	}{
		{
			name: "a caller holding no update-on-principal is refused first", caller: "developer",
			args: setArgs("user", "member", "read", app, "allow"),
			code: codeGrantCallerNotPermitted,
		},
		{
			name: "a caller who does not hold the pair cannot hand it out", caller: "admin",
			args: setArgs("user", "member", "execute", "app:deployables/publish", "allow"),
			code: codeGrantCapabilityNotHeld,
		},
		{
			name: "nor deny it", caller: "admin",
			args: setArgs("user", "member", "execute", "app:deployables/publish", "deny"),
			code: codeGrantCapabilityNotHeld,
		},
		{
			name: "a user subject above the caller is refused", caller: "admin",
			args: setArgs("user", "dev", "read", app, "allow"),
			code: codeGrantSubjectOutranksCaller,
		},
		{
			name: "a user subject whose role ranks nowhere is unresolvable, and denies", caller: "admin",
			args: setArgs("user", "ghost", "read", app, "allow"),
			code: codeGrantSubjectOutranksCaller,
		},
		{
			name: "a group ranks as its highest active member: one containing the owner outranks an admin", caller: "admin",
			args: setArgs("group", "board", "read", app, "deny"),
			code: codeGrantSubjectOutranksCaller,
		},
		{
			name: "a group with an unresolvable member denies", caller: "admin",
			args: setArgs("group", "haunted", "read", app, "allow"),
			code: codeGrantSubjectOutranksCaller,
		},
		{
			name: "yourself is refused", caller: "admin",
			args: setArgs("user", "v1:identity:user:caller", "read", app, "allow"),
			code: codeGrantSelf,
		},
		{
			name: "a user nobody has heard of is unknown", caller: "admin",
			args: setArgs("user", "nobody", "read", app, "allow"),
			code: codeGrantUnknownSubject,
		},
		{
			name: "an archived group is unknown -- it grants nobody anything", caller: "admin",
			args: setArgs("group", "archived", "read", app, "allow"),
			code: codeGrantUnknownSubject,
		},
		{
			name: "a subject kind that is not user or group is invalid", caller: "admin",
			args: setArgs("role", "admin", "read", app, "allow"),
			code: codeGrantInvalid,
		},
		{
			name: "an effect that is not allow or deny is invalid", caller: "admin",
			args: setArgs("user", "member", "read", app, "maybe"),
			code: codeGrantInvalid,
		},
		{
			name: "a verb outside the five is invalid", caller: "admin",
			args: setArgs("user", "member", "grant", app, "allow"),
			code: codeGrantInvalid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := grantsEngine(t)
			ok, code, _ := setDecision(t, i, asRole(tc.caller), tc.args)
			if ok || code != tc.code {
				t.Fatalf("ok=%v code=%q, want refused with %q", ok, code, tc.code)
			}
			if eng.wrote("mutation writeGrant") {
				t.Fatal("a refused set wrote a grant")
			}
			if eng.wrote("mutation createAuditEvent") {
				t.Fatal("a refused set wrote an audit row")
			}
		})
	}
}

func TestGrantSetWritesTheRowAtTheDerivedIdAndAudits(t *testing.T) {
	const app = "app:deployables"
	i, eng := grantsEngine(t)

	ok, code, id := setDecision(t, i, asRole("admin"),
		setArgs("user", "v1:identity:user:member", "read", app, "deny"))
	if !ok || code != "" {
		t.Fatalf("an admin denying a member an app they hold: ok=%v code=%q", ok, code)
	}
	// Stored BARE, at the derived id, with the caller as grantedBy.
	want := componentAuth.GrantRowID("user", "member", "read", app)
	if id != want {
		t.Fatalf("grantId = %s, want the derived id %s", id, want)
	}
	writes := eng.statementsWith("mutation writeGrant")
	if len(writes) != 1 {
		t.Fatalf("want exactly one writeGrant, got %d: %v", len(writes), writes)
	}
	for _, needle := range []string{
		`grantId: "` + want + `"`, `subjectKind: "user"`, `subjectId: "member"`, `verb: "read"`,
		`resourceType: "app:deployables"`, `effect: "deny"`, `grantedBy: "caller"`,
	} {
		if !strings.Contains(writes[0], needle) {
			t.Fatalf("writeGrant lacks %s: %s", needle, writes[0])
		}
	}
	audits := eng.statementsWith("mutation createAuditEvent")
	if len(audits) != 1 {
		t.Fatalf("want exactly one audit row, got %d", len(audits))
	}
	for _, needle := range []string{`action:"grant_set"`, `targetType:"grant"`, `targetId:"` + want + `"`, `"effect":"deny"`} {
		if !strings.Contains(audits[0], needle) {
			t.Fatalf("audit row lacks %s: %s", needle, audits[0])
		}
	}
}

// TestGrantSetGroupRules: an empty group ranks zero (any caller may name it);
// a group whose highest member is a peer of the caller is "no higher" and
// admitted; the owner may name a group containing an owner.
func TestGrantSetGroupRules(t *testing.T) {
	const app = "app:deployables"
	for _, tc := range []struct {
		name   string
		caller string
		group  string
		admit  bool
	}{
		{"an empty group ranks zero", "admin", "empty", true},
		{"a group whose top member is a peer is no higher", "admin", "support", true},
		{"the owner may name a group containing an owner", "owner", "board", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := grantsEngine(t)
			ok, code, _ := setDecision(t, i, asRole(tc.caller),
				setArgs("group", tc.group, "read", app, "allow"))
			if ok != tc.admit {
				t.Fatalf("ok=%v code=%q, want admit=%v", ok, code, tc.admit)
			}
			if tc.admit && !eng.wrote("mutation writeGrant") {
				t.Fatal("an admitted set wrote nothing")
			}
		})
	}
}

func TestGrantRevokeGuardsAndWrite(t *testing.T) {
	const app = "app:deployables"
	existing := func(kind, subject, verb, resource string) *testGrant {
		return &testGrant{
			id:   componentAuth.GrantRowID(kind, subject, verb, resource),
			kind: kind, subject: subject, verb: verb, resource: resource, effect: "allow", active: true,
		}
	}

	t.Run("a caller holding no update-on-principal is refused before the row is read", func(t *testing.T) {
		i, eng := grantsEngine(t)
		eng.grant = existing("user", "member", "read", app)
		ok, code, _ := revokeDecision(t, i, asRole("developer"), map[string]any{"grantId": eng.grant.id})
		if ok || code != codeGrantCallerNotPermitted {
			t.Fatalf("ok=%v code=%q", ok, code)
		}
		if len(eng.statementsWith("query grantById")) != 0 {
			t.Fatal("the row was read before the caller's own authority was checked")
		}
	})
	t.Run("an id naming no active grant is not found", func(t *testing.T) {
		i, eng := grantsEngine(t)
		eng.grant = existing("user", "member", "read", app)
		eng.grant.active = false
		ok, code, _ := revokeDecision(t, i, asRole("admin"), map[string]any{"grantId": eng.grant.id})
		if ok || code != codeGrantNotFound {
			t.Fatalf("ok=%v code=%q", ok, code)
		}
		if eng.wrote("mutation deactivateGrant") {
			t.Fatal("a revoke of an inactive grant wrote")
		}
	})
	t.Run("the pair must be held by the caller to lift it", func(t *testing.T) {
		i, eng := grantsEngine(t)
		eng.grant = existing("user", "member", "execute", "app:deployables/publish")
		ok, code, _ := revokeDecision(t, i, asRole("admin"), map[string]any{"grantId": eng.grant.id})
		if ok || code != codeGrantCapabilityNotHeld {
			t.Fatalf("ok=%v code=%q", ok, code)
		}
	})
	t.Run("the subject must not outrank the caller", func(t *testing.T) {
		i, eng := grantsEngine(t)
		eng.grant = existing("user", "dev", "read", app)
		ok, code, _ := revokeDecision(t, i, asRole("admin"), map[string]any{"grantId": eng.grant.id})
		if ok || code != codeGrantSubjectOutranksCaller {
			t.Fatalf("ok=%v code=%q", ok, code)
		}
	})
	t.Run("a grant naming yourself cannot be lifted by you", func(t *testing.T) {
		i, eng := grantsEngine(t)
		eng.grant = existing("user", "caller", "read", app)
		ok, code, _ := revokeDecision(t, i, asRole("admin"), map[string]any{"grantId": eng.grant.id})
		if ok || code != codeGrantSelf {
			t.Fatalf("ok=%v code=%q", ok, code)
		}
	})
	t.Run("an admitted revoke deactivates at the same id and audits", func(t *testing.T) {
		i, eng := grantsEngine(t)
		eng.grant = existing("user", "member", "read", app)
		ok, code, id := revokeDecision(t, i, asRole("admin"), map[string]any{"grantId": eng.grant.id})
		if !ok || code != "" || id != eng.grant.id {
			t.Fatalf("ok=%v code=%q id=%s", ok, code, id)
		}
		writes := eng.statementsWith("mutation deactivateGrant")
		if len(writes) != 1 || !strings.Contains(writes[0], `grantId: "`+eng.grant.id+`"`) {
			t.Fatalf("deactivateGrant: %v", writes)
		}
		audits := eng.statementsWith("mutation createAuditEvent")
		if len(audits) != 1 || !strings.Contains(audits[0], `action:"grant_revoked"`) || !strings.Contains(audits[0], `targetType:"grant"`) {
			t.Fatalf("audit: %v", audits)
		}
	})
}

// TestEffectiveCapabilitiesAnswersTheCallerOnly: the read takes no subject,
// answers the caller's own set with provenance, and refuses a call carrying
// no caller.
func TestEffectiveCapabilitiesAnswersTheCallerOnly(t *testing.T) {
	i, _ := grantsEngine(t)
	src := &fakeGrantSource{user: map[string][]componentAuth.Grant{
		"caller": {{Verb: "execute", Resource: "app:deployables/publish", Effect: componentAuth.GrantAllow}},
	}}
	componentAuth.SetGrantSource(src)
	t.Cleanup(func() { componentAuth.SetGrantSource(nil) })

	nodes, err := i.handleEffectiveCapabilities(asRole("admin"), map[string]any{"subjectId": "somebody-else"}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%d err=%v", len(nodes), err)
	}
	var payload struct {
		Ok      bool   `json:"ok"`
		Role    string `json:"role"`
		UserId  string `json:"userId"`
		Entries []struct {
			Verb, Resource, Effect, Source string
		} `json:"entries"`
	}
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Ok || payload.Role != "admin" || payload.UserId != "caller" {
		t.Fatalf("payload = %+v", payload)
	}
	by := map[string]struct{ effect, source string }{}
	for _, e := range payload.Entries {
		by[e.Verb+" "+e.Resource] = struct{ effect, source string }{e.Effect, e.Source}
	}
	// Inherited from the role.
	if got := by["read app:deployables"]; got.effect != "allow" || got.source != "role" {
		t.Fatalf("read app:deployables = %+v, want allow from role", got)
	}
	if got := by["delete principal"]; got.effect != "allow" || got.source != "role" {
		t.Fatalf("delete principal = %+v; admin's test catalog holds it -- want allow from role", got)
	}
	// A pair another role holds and this one does not, with nothing granting
	// it: a deny, from the role -- the universe is every pair any role holds.
	if got := by["read data"]; got.effect != "deny" || got.source != "role" {
		t.Fatalf("read data = %+v, want deny from role (admin does not hold it in the test catalog)", got)
	}
	// The caller's own grant, on a pair the catalog's owner holds.
	if got := by["execute app:deployables/publish"]; got.effect != "allow" || got.source != "user" {
		t.Fatalf("execute app:deployables/publish = %+v, want allow from user", got)
	}

	// No caller: refused, no entries.
	nodes, _ = i.handleEffectiveCapabilities(context.Background(), nil, 0)
	var refused struct {
		Ok   bool   `json:"ok"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(nodes[0].Payload, &refused)
	if refused.Ok || refused.Code != codeGrantCallerNotPermitted {
		t.Fatalf("no caller: %+v", refused)
	}
}

type fakeGrantSource struct {
	user  map[string][]componentAuth.Grant
	group map[string][]componentAuth.Grant
}

func (s *fakeGrantSource) ActiveGrantsForUser(_ context.Context, userId string) ([]componentAuth.Grant, error) {
	return s.user[strings.TrimPrefix(userId, "v1:identity:user:")], nil
}

func (s *fakeGrantSource) ActiveGrantsForGroups(_ context.Context, groupIds []string) ([]componentAuth.Grant, error) {
	var out []componentAuth.Grant
	for _, g := range groupIds {
		out = append(out, s.group[g]...)
	}
	return out, nil
}

// Discovery remains alongside the original global decision; a scoped answer
// must never turn a deny into an unscoped permission.
type organizationDiscoveryEngine struct {
	roleEngine
	calledUser string
}

func (e *organizationDiscoveryEngine) ResolveOrganizationCapabilities(ctx context.Context, _ []componentAuth.Decision) []memql.OrganizationCapability {
	ac, _ := componentAuth.AccessFromContext(ctx)
	e.calledUser = ac.UserId
	return []memql.OrganizationCapability{
		{AccountID: "acme", Verb: "read", Resource: "app:deployables", Effect: "allow"},
		{AccountID: "beta", Verb: "read", Resource: "app:deployables", Effect: "deny"},
	}
}
func TestEffectiveCapabilitiesKeepsOrganizationDiscoverySeparate(t *testing.T) {
	i, _ := grantsEngine(t)
	e := &organizationDiscoveryEngine{roleEngine: i.engine}
	i.engine = e
	src := &fakeGrantSource{user: map[string][]componentAuth.Grant{"caller": {{Verb: "read", Resource: "app:deployables", Effect: componentAuth.GrantDeny}}}}
	componentAuth.SetGrantSource(src)
	t.Cleanup(func() { componentAuth.SetGrantSource(nil) })
	nodes, err := i.handleEffectiveCapabilities(asRole("admin"), map[string]any{"subjectId": "forged"}, 0)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%d err=%v", len(nodes), err)
	}
	var payload struct {
		Entries             []struct{ Verb, Resource, Effect string }
		OrganizationEntries []memql.OrganizationCapability `json:"organizationEntries"`
	}
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if memql.BareShortId(e.calledUser) != "caller" || len(payload.OrganizationEntries) != 2 {
		t.Fatalf("wrong discovery identity or decisions: %+v", payload)
	}
	for _, entry := range payload.Entries {
		if entry.Verb == "read" && entry.Resource == "app:deployables" && entry.Effect != "deny" {
			t.Fatal("organization permission replaced the global denial")
		}
	}
}

// ---------------------------------------------------------------------
// effectiveAccessForSubject: what someone can open, and why
// ---------------------------------------------------------------------
//
// AGAINST A REAL ENGINE AND DATABASE, and that is the point of these cases.
// The answer is the engine's own resolver run for somebody other than the
// caller: their role from the catalog, their memberships and their groups'
// organizations from the rows, the grants on both from the rows, and an
// organization role's limits. A double would answer whatever the double said,
// and the screen this read feeds exists because a restatement of the rule
// in the OS (clients/os/src/apps/settings/access/model.ts) answered something
// the engine did not.
//
// Only the role CATALOG is a fixture (the engine here never runs the seed
// materializer, so the role rows are not there to lean on -- the same choice
// component/memql's grant cases make). Every account, group, membership and
// grant is a row written through the real mutations, and every read goes
// through Execute and the DSL declaration, as a client's call does.

// accessModeratorRole is an ORGANIZATION role in the production moderator's
// shape: rank 20, scoped to one account, holding only read and update on data.
// accessLeadRole is an organization role ranked above admin, which a cluster
// floor must still refuse: rank is not the question, the scope is.
const (
	accessModeratorRole = "org-moderator"
	accessLeadRole      = "org-lead"
)

// accessCatalog is the role level of every case: the seeds' ladder in
// miniature, an app or two on each rung, the data verbs, and the two
// organization roles scoped to `account`.
func accessCatalog(account string) *testCatalog {
	vr := func(v, r string) componentAuth.VerbResource { return componentAuth.VerbResource{Verb: v, Resource: r} }
	operators := []componentAuth.VerbResource{
		vr("read", "app:email"), vr("read", "app:deployables"), vr("read", "app:files"), vr("read", "app:logs"),
		vr("read", "app:settings"), vr("read", "app:settings/access"), vr("read", "app:settings/cluster"),
		vr("read", "data"), vr("create", "data"), vr("update", "data"),
		vr("read", "principal"), vr("update", "principal"),
	}
	engineers := append(append([]componentAuth.VerbResource(nil), operators...),
		vr("read", "app:fleet"), vr("execute", "app:deployables/deploy"), vr("execute", "app:deployables/reviews"))
	members := []componentAuth.VerbResource{
		vr("read", "app:email"), vr("read", "app:deployables"), vr("read", "app:files"),
		vr("read", "data"), vr("create", "data"), vr("update", "data"),
	}
	c := &testCatalog{
		ranks: map[string]int{"owner": 400, "developer": 300, accessLeadRole: 250, "admin": 200,
			"user": 100, "writer": 100, "viewer": 50, "reader": 50, accessModeratorRole: 20},
		canonical: map[string]string{"owner": "owner", "developer": "developer", accessLeadRole: accessLeadRole, "admin": "admin",
			"user": "user", "writer": "user", "viewer": "viewer", "reader": "viewer", accessModeratorRole: accessModeratorRole},
		names: map[string]string{"owner": "Owner", "developer": "Developer", accessLeadRole: "Organization lead", "admin": "Admin",
			"user": "Member", "viewer": "Viewer", accessModeratorRole: "Organization moderator"},
		scopes:   map[string]string{accessModeratorRole: account, accessLeadRole: account},
		inactive: map[string]bool{},
		grants:   map[string]map[componentAuth.VerbResource]bool{},
	}
	set := func(slug string, pairs ...componentAuth.VerbResource) {
		m := map[componentAuth.VerbResource]bool{}
		for _, p := range pairs {
			m[p] = true
		}
		c.grants[slug] = m
	}
	set("owner", engineers...)
	set("developer", engineers...)
	set("admin", operators...)
	set("user", members...)
	set("viewer", vr("read", "app:email"), vr("read", "app:files"), vr("read", "data"))
	set(accessLeadRole, vr("read", "data"), vr("update", "data"))
	set(accessModeratorRole, vr("read", "data"), vr("update", "data"))
	return c
}

// accessEngineState backs accessEngine: ONE engine and one pool for the
// package, booted by the first database case. The verdict is recorded inside
// the Once and re-reported by every case through dbtest.Unreachable -- a skip,
// or a failure under MEMQL_REQUIRE_DB=1 -- so the first caller's t does not
// answer for the rest.
var accessEngineState struct {
	once    sync.Once
	eng     *memql.MemQLEngine
	dsn     string
	pingErr error
	bootErr error
}

func accessEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	s := &accessEngineState
	s.once.Do(func() {
		s.dsn = dbtest.DSN()
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(s.dsn))), pgdialect.New())
		if err := db.PingContext(context.Background()); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			return
		}
		eng, err := memql.New(db)
		if err != nil {
			s.bootErr = fmt.Errorf("New: %w", err)
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("Init: %w", err)
			return
		}
		// The integration under test, over the real engine: the builtin
		// reaches it through the DSL declaration's @executor exactly as a
		// client's call does on a node.
		if err := eng.RegisterIntegration(New(eng, nil)); err != nil {
			s.bootErr = fmt.Errorf("RegisterIntegration: %w", err)
			return
		}
		s.eng = eng
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "effectiveAccessForSubject", s.dsn, s.pingErr)
		return nil
	}
	if s.bootErr != nil {
		t.Fatalf("the access engine failed to boot: %v", s.bootErr)
	}
	return s.eng
}

// accessWorld is one case's fixture: the engine with its grant resolution
// installed, and a suffix that keeps this case's rows apart from every other
// case's in the shared database.
type accessWorld struct {
	t      *testing.T
	eng    *memql.MemQLEngine
	suffix string
}

func newAccessWorld(t *testing.T) *accessWorld {
	t.Helper()
	eng := accessEngine(t)
	prevCatalog := componentAuth.InstalledCapabilityCatalog()
	prevGrants, prevMembers := componentAuth.InstalledGrantSource(), componentAuth.InstalledMembershipSource()
	t.Cleanup(func() {
		componentAuth.SetCapabilityCatalog(prevCatalog)
		componentAuth.SetGrantSource(prevGrants)
		componentAuth.SetMembershipSource(prevMembers)
	})
	eng.InstallGrantResolution()
	return &accessWorld{t: t, eng: eng, suffix: fmt.Sprintf("%d", time.Now().UnixNano())}
}

func (w *accessWorld) id(name string) string { return name + "-" + w.suffix }

// installCatalog installs the role level, its organization roles scoped to
// `account`. Before any row is written, so every guard a write meets reads it.
func (w *accessWorld) installCatalog(account string) {
	componentAuth.SetCapabilityCatalog(accessCatalog(account))
}

// exec runs one fixture write as the cluster acting: the system actor under
// internal origin, which the @serverOnly group and grant writers require.
func (w *accessWorld) exec(q string) {
	w.t.Helper()
	ctx := componentAuth.ContextWithInternalOrigin(
		componentAuth.ContextWithAccess(context.Background(), componentAuth.SystemActor("access-db-fixture")))
	ctx = componentAuth.ContextWithToken(ctx, &componentAuth.TokenInfo{Subject: "access-db-fixture"})
	if _, err := w.eng.Execute(ctx, q); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
}

// person writes an active user row holding `role`, and returns its bare id.
func (w *accessWorld) person(name, role string) string {
	id := w.id(name)
	payload, _ := json.Marshal(map[string]any{
		"displayName": name, "primaryEmail": id + "@example.test", "role": role, "active": true,
	})
	w.exec(fmt.Sprintf(`insert("v1:identity:user", id=%s, payload=%s)`, langparser.QuoteString(id), payload))
	return id
}

// organization writes an active account and its account group at the derived
// id acct-<account>, and returns the group's id.
func (w *accessWorld) organization(account string) string {
	payload, _ := json.Marshal(map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
	w.exec(fmt.Sprintf(`insert("v1:accounts:account", id=%s, payload=%s)`, langparser.QuoteString(account), payload))
	group := "acct-" + account
	w.group(group, "account", account)
	return group
}

func (w *accessWorld) group(id, kind, account string) {
	w.exec(fmt.Sprintf(`mutation writeGroup(groupId: %s, name: %s, kind: %s, accountId: %s, status: "active")`,
		langparser.QuoteString(id), langparser.QuoteString(id), langparser.QuoteString(kind), langparser.QuoteString(account)))
}

func (w *accessWorld) join(group, account, user string) {
	w.exec(fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, origin: "added", status: "active", accountId: %s)`,
		langparser.QuoteString(group+"-"+user), langparser.QuoteString(group), langparser.QuoteString(user), langparser.QuoteString(account)))
}

// grant writes one group grant at the derived id and returns the id.
func (w *accessWorld) grant(group, verb, resource, effect string) string {
	return w.grantTo(componentAuth.SubjectKindGroup, group, verb, resource, effect)
}

// grantTo writes one grant on a person or a group at the derived id and
// returns the id.
func (w *accessWorld) grantTo(kind, subject, verb, resource, effect string) string {
	id := componentAuth.GrantRowID(kind, subject, verb, resource)
	w.exec(fmt.Sprintf(`mutation writeGrant(grantId: %s, subjectKind: %s, subjectId: %s, verb: %s, resourceType: %s, effect: %s, grantedBy: "access-db-fixture")`,
		langparser.QuoteString(id), langparser.QuoteString(kind), langparser.QuoteString(subject), langparser.QuoteString(verb),
		langparser.QuoteString(resource), langparser.QuoteString(effect)))
	return id
}

// accessCaller is a signed-in operator as the request path builds one.
func accessCaller(userId string, role componentAuth.Role) context.Context {
	ctx := componentAuth.ContextWithAccess(context.Background(), &componentAuth.AccessContext{
		UserId: "v1:identity:user:" + userId, PrimaryEmail: userId + "@example.test", Role: role,
	})
	return componentAuth.ContextWithToken(ctx, &componentAuth.TokenInfo{Subject: "v1:identity:user:" + userId})
}

// accessAnswer is one row of the read, minus the pair and the organization
// that key it.
type accessAnswer struct {
	held     bool
	source   string
	sourceId string
}

// readAccess calls the builtin as a client does and indexes its rows by
// "<accountId>|<verb> <resourceType>". Two rows answering one key fail the
// case: that is one answer said twice, and a client would draw one of them.
func readAccess(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, kind, id string) (map[string]accessAnswer, error) {
	t.Helper()
	res, err := eng.Execute(ctx, fmt.Sprintf(`builtin effectiveAccessForSubject(subjectKind: %s, subjectId: %s)`,
		langparser.QuoteString(kind), langparser.QuoteString(id)))
	if err != nil {
		return nil, err
	}
	out := map[string]accessAnswer{}
	for _, r := range memql.MaterializeRows(res) {
		str := func(k string) string { s, _ := r[k].(string); return s }
		held, ok := r["held"].(bool)
		if !ok {
			t.Fatalf("a row carries no boolean held: %v", r)
		}
		key := str("accountId") + "|" + str("verb") + " " + str("resourceType")
		if _, dup := out[key]; dup {
			t.Fatalf("two rows answer %s", key)
		}
		out[key] = accessAnswer{held: held, source: str("source"), sourceId: str("sourceId")}
	}
	return out, nil
}

func expectAccess(t *testing.T, rows map[string]accessAnswer, account, verb, resource string, held bool, source, sourceId string) {
	t.Helper()
	got, ok := rows[account+"|"+verb+" "+resource]
	if !ok {
		t.Errorf("no row answers %s %s in organization %q", verb, resource, account)
		return
	}
	if want := (accessAnswer{held: held, source: source, sourceId: sourceId}); got != want {
		t.Errorf("%s %s in organization %q = %+v, want %+v", verb, resource, account, got, want)
	}
}

// expectNoMergedRow: a pair the engine decides per organization is answered in
// each organization and never once for all of them.
func expectNoMergedRow(t *testing.T, rows map[string]accessAnswer, pairs ...string) {
	t.Helper()
	for _, pair := range pairs {
		if got, present := rows["|"+pair]; present {
			t.Errorf("%s has a row with no organization (%+v); the engine decides it per organization, so one merged answer is a wrong one", pair, got)
		}
	}
}

// TestEffectiveAccessForAnOrganizationModerator is the production case the
// read exists for: a person on an organization role, rank 20, holding only the
// data verbs, whose organization's account group grants Deployables and its
// reviews part. They open Deployables and moderate reviews IN that
// organization, from the group; they cannot open Email anywhere; and Logs is
// closed to them by their role's scope, which no grant could lift.
func TestEffectiveAccessForAnOrganizationModerator(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("org")
	w.installCatalog(account)
	group := w.organization(account)
	moderator := w.person("moderator", accessModeratorRole)
	w.join(group, account, moderator)
	w.grant(group, "read", "app:deployables", "allow")
	w.grant(group, "execute", "app:deployables/reviews", "allow")

	rows, err := readAccess(t, w.eng, accessCaller(w.id("admin"), componentAuth.RoleAdmin), "user", moderator)
	if err != nil {
		t.Fatalf("an admin reading an organization moderator: %v", err)
	}
	expectAccess(t, rows, account, "read", "app:deployables", true, "group", group)
	expectAccess(t, rows, account, "execute", "app:deployables/reviews", true, "group", group)
	expectAccess(t, rows, account, "read", "app:email", false, "not-granted", "")
	expectAccess(t, rows, account, "read", "data", true, "role", accessModeratorRole)
	expectAccess(t, rows, "", "read", "app:logs", false, "scoped-role-limit", accessModeratorRole)
	expectNoMergedRow(t, rows, "read app:deployables", "execute app:deployables/reviews", "read app:email")
}

// TestAScopedRoleCannotOpenClusterAppsWhateverItsGroupGrants (review focus 2):
// an organization role on a cluster app -- Logs, a Settings section, Fleet --
// reads "can't open" from the role's limit even though its group allows each
// one. The control is a member of the SAME group on a global role, for whom the
// same grants are live: so it is the scope that refused, not a grant that never
// landed.
func TestAScopedRoleCannotOpenClusterAppsWhateverItsGroupGrants(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("org")
	w.installCatalog(account)
	group := w.organization(account)
	moderator := w.person("moderator", accessModeratorRole)
	member := w.person("member", "writer")
	w.join(group, account, moderator)
	w.join(group, account, member)
	clusterApps := []string{"app:logs", "app:settings/cluster", "app:fleet"}
	for _, resource := range clusterApps {
		w.grant(group, "read", resource, "allow")
	}
	caller := accessCaller(w.id("owner"), componentAuth.RoleOwner)

	rows, err := readAccess(t, w.eng, caller, "user", moderator)
	if err != nil {
		t.Fatalf("reading the organization role: %v", err)
	}
	for _, resource := range clusterApps {
		expectAccess(t, rows, "", "read", resource, false, "scoped-role-limit", accessModeratorRole)
	}

	rows, err = readAccess(t, w.eng, caller, "user", member)
	if err != nil {
		t.Fatalf("reading the member: %v", err)
	}
	expectAccess(t, rows, "", "read", "app:logs", true, "group", group)
	expectAccess(t, rows, "", "read", "app:settings/cluster", true, "group", group)
	// Fleet is the cluster app no grant hands anybody below developer. Its bar
	// is the global role's, and it reads as the role's -- never as the group's
	// grant, and never as an organization role's limit.
	expectAccess(t, rows, "", "read", "app:fleet", false, "role", "writer")
}

// TestEffectiveAccessIsPerOrganization (review focus 1): one person in two
// organizations sees each organization's answer separately. Acme's group
// grants the reviews part and Beta's denies Deployables, so the part is held
// in Acme and not in Beta, and Deployables opens in Acme and not in Beta --
// and neither pair has a merged row that would have to pick one.
func TestEffectiveAccessIsPerOrganization(t *testing.T) {
	w := newAccessWorld(t)
	acme, beta := w.id("acme"), w.id("beta")
	w.installCatalog(acme)
	acmeGroup, betaGroup := w.organization(acme), w.organization(beta)
	person := w.person("both", "writer")
	w.join(acmeGroup, acme, person)
	w.join(betaGroup, beta, person)
	w.grant(acmeGroup, "execute", "app:deployables/reviews", "allow")
	w.grant(betaGroup, "read", "app:deployables", "deny")

	rows, err := readAccess(t, w.eng, accessCaller(w.id("admin"), componentAuth.RoleAdmin), "user", person)
	if err != nil {
		t.Fatalf("an admin reading a person in two organizations: %v", err)
	}
	expectAccess(t, rows, acme, "execute", "app:deployables/reviews", true, "group", acmeGroup)
	expectAccess(t, rows, beta, "execute", "app:deployables/reviews", false, "not-granted", "")
	expectAccess(t, rows, acme, "read", "app:deployables", true, "role", "writer")
	expectAccess(t, rows, beta, "read", "app:deployables", false, "group", betaGroup)
	expectAccess(t, rows, acme, "read", "app:email", true, "role", "writer")
	expectAccess(t, rows, beta, "read", "app:email", true, "role", "writer")
	expectNoMergedRow(t, rows, "read app:deployables", "execute app:deployables/reviews", "read app:email")
	// A pair the engine does not decide per organization keeps one answer.
	expectAccess(t, rows, "", "read", "app:files", true, "role", "writer")
}

// TestEffectiveAccessRefusesSomeoneWhoOutranksTheCaller (review focus 5):
// asked about somebody else, the read keeps the grant reads' floor -- a
// cluster role at admin or above -- and nobody reads a person who outranks
// them. A refusal is an error carrying its code and no rows at all.
func TestEffectiveAccessRefusesSomeoneWhoOutranksTheCaller(t *testing.T) {
	w := newAccessWorld(t)
	w.installCatalog(w.id("org"))
	owner := w.person("owner", "owner")
	developer := w.person("developer", "developer")
	admin := w.person("admin", "admin")
	member := w.person("member", "writer")
	asDeveloper := accessCaller(developer, componentAuth.RoleDeveloper)
	asAdmin := accessCaller(admin, componentAuth.RoleAdmin)

	for _, tc := range []struct {
		name    string
		caller  context.Context
		subject string
		code    string
	}{
		{"a developer reading the owner", asDeveloper, owner, "access_subject_outranks_caller"},
		{"an admin reading a developer", asAdmin, developer, "access_subject_outranks_caller"},
		{"a member reading anybody, below the floor", accessCaller(member, componentAuth.RoleWriter), member, "access_caller_not_permitted"},
		{"an organization role ranked above admin, not a cluster role", accessCaller(w.id("lead"), componentAuth.Role(accessLeadRole)), member, "access_caller_not_permitted"},
		{"a person who is not there", asAdmin, w.id("nobody"), "access_unknown_subject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := readAccess(t, w.eng, tc.caller, "user", tc.subject)
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("err = %v, want a refusal carrying %s", err, tc.code)
			}
			if len(rows) != 0 {
				t.Fatalf("a refusal answered %d rows", len(rows))
			}
		})
	}

	// The floor is the grant reads' and no higher: a developer reads an admin
	// below them, and an admin reads a peer.
	for name, call := range map[string]struct {
		caller  context.Context
		subject string
	}{
		"a developer reading an admin": {asDeveloper, admin},
		"an admin reading a peer":      {asAdmin, w.person("peer", "admin")},
	} {
		rows, err := readAccess(t, w.eng, call.caller, "user", call.subject)
		if err != nil || len(rows) == 0 {
			t.Fatalf("%s: rows=%d err=%v, want the answer", name, len(rows), err)
		}
	}
}

// TestEffectiveAccessForAGroupListsItsOwnGrants: asked about a group, the read
// is the group's own grants -- one row per ACTIVE grant, held from its effect,
// in the group's organization. A revoked grant and another group's grant are
// not its rows, a group tied to no organization answers with none, and a group
// that is not there is a refusal rather than an empty answer.
func TestEffectiveAccessForAGroupListsItsOwnGrants(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("org")
	w.installCatalog(account)
	group := w.organization(account)
	w.grant(group, "read", "app:deployables", "allow")
	w.grant(group, "execute", "app:deployables/reviews", "allow")
	w.grant(group, "read", "app:email", "deny")
	revoked := w.grant(group, "read", "app:files", "allow")
	w.exec(fmt.Sprintf(`mutation deactivateGrant(grantId: %s)`, langparser.QuoteString(revoked)))
	other := w.organization(w.id("other"))
	w.grant(other, "read", "app:logs", "allow")
	caller := accessCaller(w.id("admin"), componentAuth.RoleAdmin)

	rows, err := readAccess(t, w.eng, caller, "group", group)
	if err != nil {
		t.Fatalf("an admin reading an organization's group: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want the group's three active grants and nothing else, got %d: %+v", len(rows), rows)
	}
	expectAccess(t, rows, account, "read", "app:deployables", true, "group", group)
	expectAccess(t, rows, account, "execute", "app:deployables/reviews", true, "group", group)
	expectAccess(t, rows, account, "read", "app:email", false, "group", group)

	team := w.id("team")
	w.group(team, "custom", "")
	w.grant(team, "read", "app:files", "allow")
	rows, err = readAccess(t, w.eng, caller, "group", team)
	if err != nil {
		t.Fatalf("an admin reading a group tied to no organization: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want the one grant, got %d: %+v", len(rows), rows)
	}
	expectAccess(t, rows, "", "read", "app:files", true, "group", team)

	if _, err := readAccess(t, w.eng, caller, "group", w.id("nowhere")); err == nil || !strings.Contains(err.Error(), "access_unknown_subject") {
		t.Fatalf("a group that is not there: err = %v, want access_unknown_subject", err)
	}
}

// TestEffectiveAccessNamesThePersonsDenyOverTheirGroupsAllow: a grant on the
// person is the most specific level, so it decides over their group's. The
// group allows Logs and the reviews part, the person is denied both, and each
// row reads not held, from the person, naming them -- Logs once, the part in
// the organization that decides it. The control is a member of the same group
// with no grant of their own, who holds both from the group: so the group's
// allow was live, and the person's deny is what answered over it.
func TestEffectiveAccessNamesThePersonsDenyOverTheirGroupsAllow(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("org")
	w.installCatalog(account)
	group := w.organization(account)
	denied := w.person("denied", "writer")
	member := w.person("member", "writer")
	w.join(group, account, denied)
	w.join(group, account, member)
	w.grant(group, "read", "app:logs", "allow")
	w.grant(group, "execute", "app:deployables/reviews", "allow")
	w.grantTo(componentAuth.SubjectKindUser, denied, "read", "app:logs", "deny")
	w.grantTo(componentAuth.SubjectKindUser, denied, "execute", "app:deployables/reviews", "deny")
	caller := accessCaller(w.id("admin"), componentAuth.RoleAdmin)

	rows, err := readAccess(t, w.eng, caller, "user", denied)
	if err != nil {
		t.Fatalf("an admin reading a person denied over their group: %v", err)
	}
	expectAccess(t, rows, "", "read", "app:logs", false, "person", denied)
	expectAccess(t, rows, account, "execute", "app:deployables/reviews", false, "person", denied)

	rows, err = readAccess(t, w.eng, caller, "user", member)
	if err != nil {
		t.Fatalf("an admin reading the control member: %v", err)
	}
	expectAccess(t, rows, "", "read", "app:logs", true, "group", group)
	expectAccess(t, rows, account, "execute", "app:deployables/reviews", true, "group", group)
}

// TestEffectiveAccessRefusesAnArchivedGroup: an archived group grants nobody
// anything, so asked about one the read refuses as it does for a group that is
// not there -- rather than listing grants that decide nothing, or answering
// empty, which would say "grants nothing" for a different reason. The control
// is the same group, active, answering its grant.
func TestEffectiveAccessRefusesAnArchivedGroup(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("org")
	w.installCatalog(account)
	group := w.organization(account)
	w.grant(group, "read", "app:deployables", "allow")
	caller := accessCaller(w.id("admin"), componentAuth.RoleAdmin)
	if rows, err := readAccess(t, w.eng, caller, "group", group); err != nil || len(rows) != 1 {
		t.Fatalf("control: the active group answers its one grant: rows=%d err=%v", len(rows), err)
	}

	w.exec(fmt.Sprintf(`mutation writeGroup(groupId: %s, name: %s, kind: "account", accountId: %s, status: "archived", archivedAt: %s)`,
		langparser.QuoteString(group), langparser.QuoteString(group), langparser.QuoteString(account),
		langparser.QuoteString(time.Now().UTC().Format(time.RFC3339))))
	rows, err := readAccess(t, w.eng, caller, "group", group)
	if err == nil || !strings.Contains(err.Error(), "access_unknown_subject") {
		t.Fatalf("an archived group: err = %v, want access_unknown_subject", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refusal answered %d rows", len(rows))
	}
}

// TestAnOrganizationRoleOpensTheSettingsDoorButNotItsSections: an organization
// role's limit bars Settings' SECTIONS -- every one but Access -- and not the
// Settings door itself, which an organization's people open to reach Access.
// With their group allowing both, the door reads held from the group and the
// cluster section reads the role's limit.
func TestAnOrganizationRoleOpensTheSettingsDoorButNotItsSections(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("org")
	w.installCatalog(account)
	group := w.organization(account)
	moderator := w.person("moderator", accessModeratorRole)
	w.join(group, account, moderator)
	w.grant(group, "read", "app:settings", "allow")
	w.grant(group, "read", "app:settings/cluster", "allow")

	rows, err := readAccess(t, w.eng, accessCaller(w.id("admin"), componentAuth.RoleAdmin), "user", moderator)
	if err != nil {
		t.Fatalf("an admin reading an organization moderator: %v", err)
	}
	expectAccess(t, rows, "", "read", "app:settings", true, "group", group)
	expectAccess(t, rows, "", "read", "app:settings/cluster", false, "scoped-role-limit", accessModeratorRole)
}

// unreadableGrants is the installed grant source with some rows unreadable: a
// person's own grants, or a group's, fail the way a database that blinked
// does, and everybody else's come from the rows as before.
type unreadableGrants struct {
	componentAuth.GrantSource
	user  string // the bare id of a person whose own grants cannot be read
	group string // the bare id of a group whose grants cannot be read
}

var errGrantRowsUnreadable = errors.New("the grant rows could not be read")

func (s unreadableGrants) ActiveGrantsForUser(ctx context.Context, userId string) ([]componentAuth.Grant, error) {
	if s.user != "" && memql.BareShortId(userId) == s.user {
		return nil, errGrantRowsUnreadable
	}
	return s.GrantSource.ActiveGrantsForUser(ctx, userId)
}

func (s unreadableGrants) ActiveGrantsForGroups(ctx context.Context, groupIds []string) ([]componentAuth.Grant, error) {
	for _, g := range groupIds {
		if s.group != "" && memql.BareShortId(g) == s.group {
			return nil, errGrantRowsUnreadable
		}
	}
	return s.GrantSource.ActiveGrantsForGroups(ctx, groupIds)
}

// TestEffectiveAccessRefusesWhenAReadBehindTheAnswerFails (review item 1). The
// action guards answer from what they could read when a grant read fails -- a
// database that blinks must not take everybody's access away mid request --
// and they still do. An operator's read of somebody is different: it draws a
// picture, and one drawn from part of the rows is a wrong picture stated with
// confidence. So when any read behind the person's answer fails, the read
// refuses, access_unavailable, and answers no rows.
//
// The last case is the memo's. The request read the same person earlier, with
// nobody listening for a failure: the failure was swallowed and the request's
// grant memo kept what was left. The person's answer must not be served from
// it.
func TestEffectiveAccessRefusesWhenAReadBehindTheAnswerFails(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("org")
	w.installCatalog(account)
	group := w.organization(account)
	member := w.person("member", "writer")
	loner := w.person("loner", "writer")
	w.join(group, account, member)
	w.grant(group, "read", "app:logs", "allow")
	caller := accessCaller(w.id("admin"), componentAuth.RoleAdmin)
	rowsSource := componentAuth.InstalledGrantSource()

	for _, person := range []string{member, loner} {
		if rows, err := readAccess(t, w.eng, caller, "user", person); err != nil || len(rows) == 0 {
			t.Fatalf("control: %s with every read answering: rows=%d err=%v", person, len(rows), err)
		}
	}

	unreadable := func(t *testing.T, src unreadableGrants) {
		t.Helper()
		src.GrantSource = rowsSource
		componentAuth.SetGrantSource(src)
		t.Cleanup(func() { componentAuth.SetGrantSource(rowsSource) })
	}
	refused := func(t *testing.T, ctx context.Context, person string) {
		t.Helper()
		rows, err := readAccess(t, w.eng, ctx, "user", person)
		if err == nil || !strings.Contains(err.Error(), "access_unavailable") {
			t.Fatalf("err = %v with %d rows, want the access_unavailable refusal", err, len(rows))
		}
		if len(rows) != 0 {
			t.Fatalf("a refusal answered %d rows", len(rows))
		}
	}

	t.Run("the person's own grants", func(t *testing.T) {
		unreadable(t, unreadableGrants{user: member})
		refused(t, caller, member)
	})
	t.Run("their group's grants", func(t *testing.T) {
		unreadable(t, unreadableGrants{group: group})
		refused(t, caller, member)
	})
	t.Run("a failure the request memoised before the read began", func(t *testing.T) {
		unreadable(t, unreadableGrants{user: loner})
		request := componentAuth.ContextWithGrantMemo(caller)
		// The entry the person's resolution looks up -- their bare id, in no
		// group -- filled the way an action guard fills it: the failure
		// swallowed, the role's answer standing.
		componentAuth.CapableFor(request, componentAuth.Subject{Role: componentAuth.RoleWriter, UserId: loner}, "read", "app:files")
		refused(t, request, loner)
	})
}
