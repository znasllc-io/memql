package rbac

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// EACH GUARD REFUSES BY ITS CODE (epic memql#5166, section D).
//
// The codes are the contract, not the sentences: a client renders the refusal
// it got and offers the operator the fix for it, so a guard that reported a
// different code -- or ran in a different order -- would send them to the wrong
// place. Every case below asserts the code, and the ones that should NOT write
// assert that nothing was written.

// ---------------------------------------------------------------------
// doubles
// ---------------------------------------------------------------------

// testCatalog is a hand-built catalog satisfying the wider reader these guards
// need. Its Slugs() carries a DEACTIVATED role, because the rank-taken and
// slug-taken guards must both see one.
type testCatalog struct {
	ranks     map[string]int
	canonical map[string]string
	names     map[string]string
	grants    map[string]map[componentAuth.VerbResource]bool
	scopes    map[string]string
	inactive  map[string]bool
}

func (c *testCatalog) Rank(slug string) (int, bool) { r, ok := c.ranks[slug]; return r, ok }
func (c *testCatalog) Name(slug string) string      { return c.names[c.CanonicalSlug(slug)] }
func (c *testCatalog) Holds(slug, verb, resource string) bool {
	canonical := c.CanonicalSlug(slug)
	if canonical == "" || c.inactive[canonical] {
		return false
	}
	return c.grants[canonical][componentAuth.VerbResource{Verb: verb, Resource: resource}]
}

func (c *testCatalog) Grants(slug string) []componentAuth.VerbResource {
	out := []componentAuth.VerbResource{}
	for vr := range c.grants[c.CanonicalSlug(slug)] {
		out = append(out, vr)
	}
	return out
}
func (c *testCatalog) Scope(slug string) string { return c.scopes[c.CanonicalSlug(slug)] }
func (c *testCatalog) Active(slug string) bool {
	canonical := c.CanonicalSlug(slug)
	return canonical != "" && !c.inactive[canonical]
}
func (c *testCatalog) CanonicalSlug(slug string) string { return c.canonical[slug] }
func (c *testCatalog) Slugs() []string {
	out := make([]string, 0, len(c.names))
	for slug := range c.names {
		out = append(out, slug)
	}
	return out
}

func newTestCatalog() *testCatalog {
	c := &testCatalog{
		ranks:     map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100, "retired-lead": 175},
		canonical: map[string]string{"owner": "owner", "developer": "developer", "admin": "admin", "user": "user", "writer": "user", "retired-lead": "retired-lead"},
		names:     map[string]string{"owner": "Owner", "developer": "Developer", "admin": "Admin", "user": "Member", "retired-lead": "Retired Lead"},
		scopes:    map[string]string{},
		inactive:  map[string]bool{"retired-lead": true},
		grants:    map[string]map[componentAuth.VerbResource]bool{},
	}
	set := func(slug string, pairs ...componentAuth.VerbResource) {
		m := map[componentAuth.VerbResource]bool{}
		for _, p := range pairs {
			m[p] = true
		}
		c.grants[slug] = m
	}
	vr := func(v, r string) componentAuth.VerbResource {
		return componentAuth.VerbResource{Verb: v, Resource: r}
	}
	set("owner",
		vr("read", "principal"), vr("create", "principal"), vr("update", "principal"), vr("delete", "principal"),
		vr("read", "role"), vr("create", "role"), vr("update", "role"), vr("delete", "role"))
	set("admin",
		vr("read", "principal"), vr("create", "principal"), vr("update", "principal"), vr("delete", "principal"),
		vr("read", "role"), vr("create", "role"), vr("update", "role"))
	set("developer", vr("read", "principal"), vr("read", "role"))
	set("user", vr("read", "data"))
	return c
}

// recordingEngine records every statement and answers the two reads the
// handlers make. Asserting against the rendered TEXT is the point: a double
// that recorded method names would never notice a statement that will not
// parse.
type recordingEngine struct {
	statements []string
	role       *roleRow
	grants     []componentAuth.VerbResource

	// The subject reads the grant builtins make (grants_test.go): a user
	// directory (bare id -> role slug), a group directory, and at most one
	// existing grant for the revoke cases.
	users  map[string]string
	groups map[string]testGroup
	grant  *testGrant
}

type testGroup struct {
	status  string
	members []string
}

type testGrant struct {
	id, kind, subject, verb, resource, effect string
	active                                    bool
}

// bareTestId strips the canonical prefixes the handlers may pass, so the
// directories above are keyed by the bare id alone.
func bareTestId(id string) string {
	for _, prefix := range []string{"v1:identity:user:", "v1:identity:group:", "v1:rbac:grant:"} {
		id = strings.TrimPrefix(id, prefix)
	}
	return id
}

// quotedArg reads the first quoted argument value out of a rendered statement.
func quotedArg(q, name string) string {
	idx := strings.Index(q, name+": \"")
	if idx < 0 {
		return ""
	}
	rest := q[idx+len(name)+3:]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func stringNode(id string, fields map[string]*structpb.Value) *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{Id: id, Payload: &structpb.Struct{Fields: fields}}
}

func (e *recordingEngine) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	e.statements = append(e.statements, q)
	switch {
	case strings.HasPrefix(q, "query roleBySlug"):
		if e.role == nil {
			return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: "v1:rbac:role:" + e.role.slug,
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"slug":        structpb.NewStringValue(e.role.slug),
				"name":        structpb.NewStringValue(e.role.name),
				"description": structpb.NewStringValue(e.role.description),
				"accountId":   structpb.NewStringValue(e.role.accountId),
				"rank":        structpb.NewNumberValue(float64(e.role.rank)),
				"predefined":  structpb.NewBoolValue(e.role.predefined),
			}},
		}}}}, nil
	case strings.HasPrefix(q, "query userByIdSystem"):
		id := bareTestId(quotedArg(q, "userId"))
		role, ok := e.users[id]
		if !ok {
			return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
			stringNode("v1:identity:user:"+id, map[string]*structpb.Value{"role": structpb.NewStringValue(role)}),
		}}}, nil
	case strings.HasPrefix(q, "query groupById"):
		id := bareTestId(quotedArg(q, "groupId"))
		g, ok := e.groups[id]
		if !ok {
			return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
			stringNode("v1:identity:group:"+id, map[string]*structpb.Value{"status": structpb.NewStringValue(g.status)}),
		}}}, nil
	case strings.HasPrefix(q, "query membersOfGroup"):
		id := bareTestId(quotedArg(q, "groupId"))
		nodes := []*memqlv1.MemoryNode{}
		for _, m := range e.groups[id].members {
			nodes = append(nodes, stringNode("v1:identity:groupMembership:"+id+"-"+m, map[string]*structpb.Value{
				"groupId": structpb.NewStringValue(id), "userId": structpb.NewStringValue(m),
			}))
		}
		return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil
	case strings.HasPrefix(q, "query grantById"):
		id := bareTestId(quotedArg(q, "grantId"))
		if e.grant == nil || e.grant.id != id {
			return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
			stringNode("v1:rbac:grant:"+id, map[string]*structpb.Value{
				"subjectKind":  structpb.NewStringValue(e.grant.kind),
				"subjectId":    structpb.NewStringValue(e.grant.subject),
				"verb":         structpb.NewStringValue(e.grant.verb),
				"resourceType": structpb.NewStringValue(e.grant.resource),
				"effect":       structpb.NewStringValue(e.grant.effect),
				"active":       structpb.NewBoolValue(e.grant.active),
			}),
		}}}, nil
	case strings.HasPrefix(q, "query capabilitiesForRole"):
		nodes := make([]*memqlv1.MemoryNode, 0, len(e.grants))
		for idx, g := range e.grants {
			nodes = append(nodes, &memqlv1.MemoryNode{
				Id: "cap-" + g.Verb,
				Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
					"verb":         structpb.NewStringValue(g.Verb),
					"resourceType": structpb.NewStringValue(g.Resource),
					"effect":       structpb.NewStringValue("allow"),
				}},
			})
			_ = idx
		}
		return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil
	}
	return &memql.ExecuteResult{}, nil
}

func (e *recordingEngine) wrote(prefix string) bool {
	for _, s := range e.statements {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func (e *recordingEngine) statementsWith(needle string) []string {
	var out []string
	for _, s := range e.statements {
		if strings.Contains(s, needle) {
			out = append(out, s)
		}
	}
	return out
}

func newRoleIntegration(t *testing.T, cat *testCatalog) (*Integration, *recordingEngine) {
	t.Helper()
	componentAuth.SetCapabilityCatalog(cat)
	t.Cleanup(func() { componentAuth.SetCapabilityCatalog(nil) })
	eng := &recordingEngine{}
	// No bunDB: the holder count refuses rather than reporting zero, which is
	// asserted by name below.
	return New(eng, nil), eng
}

func asRole(slug string) context.Context {
	return componentAuth.ContextWithAccess(context.Background(), &componentAuth.AccessContext{
		UserId:       "v1:identity:user:caller",
		PrimaryEmail: "caller@example.test",
		Role:         componentAuth.Role(slug),
	})
}

// readDecision decodes the single node a handler returns.
func readDecision(t *testing.T, nodes []memorynodes.MemoryNode, err error) (bool, string) {
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
	return ok, code
}

// ---------------------------------------------------------------------
// roleCreate
// ---------------------------------------------------------------------

func TestRoleCreateGuards(t *testing.T) {
	grant := func(verb, resource string) map[string]any {
		return map[string]any{"verb": verb, "resource": resource}
	}

	for _, tc := range []struct {
		name   string
		caller string
		args   map[string]any
		code   string
	}{
		{
			name: "a caller holding no create-on-role is refused", caller: "developer",
			args: map[string]any{"slug": "lead", "name": "Lead", "rank": 150,
				"grants": []any{grant("read", "principal")}},
			code: codeNotAuthorized,
		},
		{
			name: "a malformed slug is refused before anything is read", caller: "admin",
			args: map[string]any{"slug": "Lead Role", "name": "Lead", "rank": 150,
				"grants": []any{grant("read", "principal")}},
			code: codeInvalidSlug,
		},
		{
			name: "a slug another role holds is refused", caller: "owner",
			args: map[string]any{"slug": "admin", "name": "Admin Two", "rank": 150,
				"grants": []any{grant("read", "principal")}},
			code: codeSlugTaken,
		},
		{
			// THE ALIAS HALF. `writer` names no role's slug and every ordinary
			// user row carries it, so a create that only checked slugs would
			// mint a role whose name silently re-points every member.
			name: "a slug another role claims as an ALIAS is refused", caller: "owner",
			args: map[string]any{"slug": "writer", "name": "Writer", "rank": 150,
				"grants": []any{grant("read", "principal")}},
			code: codeSlugTaken,
		},
		{
			// D8. A retired role keeps its name.
			name: "a DEACTIVATED role's slug is still taken", caller: "owner",
			args: map[string]any{"slug": "retired-lead", "name": "Lead Again", "rank": 150,
				"grants": []any{grant("read", "principal")}},
			code: codeSlugTaken,
		},
		{
			name: "a rank at the caller's own is refused", caller: "admin",
			args: map[string]any{"slug": "peer", "name": "Peer", "rank": 200,
				"grants": []any{grant("read", "principal")}},
			code: codeRankNotBelowCaller,
		},
		{
			name: "a rank above the caller's is refused", caller: "admin",
			args: map[string]any{"slug": "boss", "name": "Boss", "rank": 350,
				"grants": []any{grant("read", "principal")}},
			code: codeRankNotBelowCaller,
		},
		{
			name: "a rank an existing rung holds is refused", caller: "owner",
			args: map[string]any{"slug": "twin", "name": "Twin", "rank": 200,
				"grants": []any{grant("read", "principal")}},
			code: codeRankTaken,
		},
		{
			// D8 again, on the other axis: a retired role keeps its rung too.
			name: "a rank a DEACTIVATED role holds is refused", caller: "owner",
			args: map[string]any{"slug": "twin", "name": "Twin", "rank": 175,
				"grants": []any{grant("read", "principal")}},
			code: codeRankTaken,
		},
		{
			// D6, and the record's own example: an admin holds create-on-role
			// but not delete-on-role, so it cannot hand delete-on-role out.
			name: "a grant the caller does not hold is refused", caller: "admin",
			args: map[string]any{"slug": "lead", "name": "Lead", "rank": 150,
				"grants": []any{grant("delete", "role")}},
			code: codeGrantNotHeld,
		},
		{
			name: "a grant with an unknown verb is refused", caller: "admin",
			args: map[string]any{"slug": "lead", "name": "Lead", "rank": 150,
				"grants": []any{grant("manage", "principal")}},
			code: codeInvalidGrant,
		},
		{
			name: "a role with no grants is refused", caller: "admin",
			args: map[string]any{"slug": "lead", "name": "Lead", "rank": 150, "grants": []any{}},
			code: codeInvalidGrant,
		},
		{
			// The record's own success case: an admin creating a rank-150 role
			// with create-on-principal.
			name: "an admin may create a rank-150 role with create on principal", caller: "admin",
			args: map[string]any{"slug": "support-lead", "name": "Support Lead", "rank": 150,
				"grants": []any{grant("create", "principal")}},
			code: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newRoleIntegration(t, newTestCatalog())

			nodes, err := i.handleRoleCreate(asRole(tc.caller), tc.args, 0)
			ok, code := readDecision(t, nodes, err)

			if code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
			if tc.code == "" {
				if !ok {
					t.Fatal("a permitted create reported ok=false")
				}
				if !eng.wrote("mutation createRole") {
					t.Fatalf("no role was written; statements=%v", eng.statements)
				}
				if len(eng.statementsWith("mutation createCapability")) != 1 {
					t.Fatalf("want one capability row per grant, got %v",
						eng.statementsWith("mutation createCapability"))
				}
				return
			}
			if ok {
				t.Fatal("a refused create reported ok=true")
			}
			// A REFUSAL WRITES NOTHING. A guard that refuses after writing the
			// role row leaves a role with no grants, which is worse than either
			// outcome it was choosing between.
			if eng.wrote("mutation createRole") || eng.wrote("mutation createCapability") {
				t.Fatalf("a refused create still wrote: %v", eng.statements)
			}
		})
	}
}

// TestRoleCreateWritesTheRoleAndItsGrantsTogether. "One internal-origin write"
// is section D's phrase, and what it has to mean in practice is that the role
// row and every grant row land from the same call under the same origin --
// createRole and createCapability are both @serverOnly, so a path that stamped
// one and not the other would half-write the role and refuse the rest.
func TestRoleCreateWritesTheRoleAndItsGrantsTogether(t *testing.T) {
	i, eng := newRoleIntegration(t, newTestCatalog())

	_, err := i.handleRoleCreate(asRole("owner"), map[string]any{
		"slug": "support-lead", "name": "Support Lead", "rank": 150,
		"description": "Handles support",
		"grants": []any{
			map[string]any{"verb": "read", "resource": "principal"},
			map[string]any{"verb": "update", "resource": "principal"},
		},
	}, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	roles := eng.statementsWith("mutation createRole")
	if len(roles) != 1 {
		t.Fatalf("want one role write, got %d: %v", len(roles), roles)
	}
	if !strings.Contains(roles[0], `predefined: false`) || !strings.Contains(roles[0], `active: true`) {
		t.Errorf("a created role must be predefined:false active:true -- got %s", roles[0])
	}
	if !strings.Contains(roles[0], `slug: "support-lead"`) || !strings.Contains(roles[0], `rank: 150`) {
		t.Errorf("the role write does not carry the slug and rank: %s", roles[0])
	}
	caps := eng.statementsWith("mutation createCapability")
	if len(caps) != 2 {
		t.Fatalf("want one capability row per grant (2), got %d: %v", len(caps), caps)
	}
	for _, c := range caps {
		if !strings.Contains(c, `active: true`) || !strings.Contains(c, `effect: "allow"`) {
			t.Errorf("a granted capability must be an active allow: %s", c)
		}
	}
}

// ---------------------------------------------------------------------
// roleUpdate
// ---------------------------------------------------------------------

func TestRoleUpdateRefusesAPredefinedRole(t *testing.T) {
	i, eng := newRoleIntegration(t, newTestCatalog())
	eng.role = &roleRow{slug: "admin", name: "Admin", rank: 200, predefined: true}

	nodes, err := i.handleRoleUpdate(asRole("owner"), map[string]any{
		"slug": "admin", "name": "Administrator",
	}, 0)
	ok, code := readDecision(t, nodes, err)

	if ok || code != codePredefinedImmutable {
		t.Fatalf("ok=%v code=%q, want a %s refusal (D7)", ok, code, codePredefinedImmutable)
	}
	if eng.wrote("mutation createRole") {
		t.Fatal("a predefined role was written despite the refusal")
	}
}

func TestRoleUpdateRetiresARemovedGrantRatherThanDeletingIt(t *testing.T) {
	i, eng := newRoleIntegration(t, newTestCatalog())
	eng.role = &roleRow{slug: "support-lead", name: "Support Lead", rank: 150}
	eng.grants = []componentAuth.VerbResource{
		{Verb: "read", Resource: "principal"},
		{Verb: "update", Resource: "principal"},
	}

	_, err := i.handleRoleUpdate(asRole("owner"), map[string]any{
		"slug":   "support-lead",
		"grants": []any{map[string]any{"verb": "read", "resource": "principal"}},
	}, 0)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	var retired []string
	for _, s := range eng.statementsWith("mutation createCapability") {
		if strings.Contains(s, "active: false") {
			retired = append(retired, s)
		}
	}
	if len(retired) != 1 {
		t.Fatalf("want exactly one grant written inactive, got %d: %v", len(retired),
			eng.statementsWith("mutation createCapability"))
	}
	if !strings.Contains(retired[0], `verb: "update"`) {
		t.Errorf("the wrong grant was retired: %s", retired[0])
	}
	// THE ROW IS NOT DELETED. It is the record of what this role could do at a
	// point in time, and an audit that can only see the current grant set
	// cannot answer what somebody was allowed to do last month.
	if eng.wrote("mutation deleteCapability") {
		t.Error("a removed grant was deleted rather than deactivated")
	}
}

func TestRoleUpdateRefusesAGrantTheCallerDoesNotHold(t *testing.T) {
	i, eng := newRoleIntegration(t, newTestCatalog())
	eng.role = &roleRow{slug: "support-lead", name: "Support Lead", rank: 150}

	nodes, err := i.handleRoleUpdate(asRole("admin"), map[string]any{
		"slug":   "support-lead",
		"grants": []any{map[string]any{"verb": "delete", "resource": "role"}},
	}, 0)
	ok, code := readDecision(t, nodes, err)

	if ok || code != codeGrantNotHeld {
		t.Fatalf("ok=%v code=%q, want %s -- an edit must apply the same subset guard a create does",
			ok, code, codeGrantNotHeld)
	}
}

// ---------------------------------------------------------------------
// roleDeactivate
// ---------------------------------------------------------------------

func TestRoleDeactivateRefusesAPredefinedRole(t *testing.T) {
	i, _ := newRoleIntegration(t, newTestCatalog())
	eng := i.engine.(*recordingEngine)
	eng.role = &roleRow{slug: "admin", name: "Admin", rank: 200, predefined: true}

	nodes, err := i.handleRoleDeactivate(asRole("owner"), map[string]any{"slug": "admin"}, 0)
	ok, code := readDecision(t, nodes, err)

	if ok || code != codePredefinedImmutable {
		t.Fatalf("ok=%v code=%q, want %s", ok, code, codePredefinedImmutable)
	}
}

// TestRoleDeactivateRefusesWhenItCannotCountHolders is the fail-closed half of
// the holder guard, and the one an implementation gets wrong by returning zero.
// A node with no database cannot tell whether anybody holds the role, and
// "cannot tell" must not read as "nobody does" on the one path whose whole job
// is refusing to strand people.
func TestRoleDeactivateRefusesWhenItCannotCountHolders(t *testing.T) {
	i, _ := newRoleIntegration(t, newTestCatalog())
	eng := i.engine.(*recordingEngine)
	eng.role = &roleRow{slug: "support-lead", name: "Support Lead", rank: 150}

	_, err := i.handleRoleDeactivate(asRole("owner"), map[string]any{"slug": "support-lead"}, 0)
	if err == nil {
		t.Fatal("a node that cannot count holders reported success; it must refuse")
	}
	if eng.wrote("mutation createRole") {
		t.Fatal("the role was retired despite the holder count being unavailable")
	}
}

func TestRoleDeactivateRefusesAnUnknownRole(t *testing.T) {
	i, _ := newRoleIntegration(t, newTestCatalog())

	nodes, err := i.handleRoleDeactivate(asRole("owner"), map[string]any{"slug": "ghost"}, 0)
	ok, code := readDecision(t, nodes, err)

	if ok || code != codeUnknownRole {
		t.Fatalf("ok=%v code=%q, want %s", ok, code, codeUnknownRole)
	}
}

// TestRoleAuthoringRefusesWithNoCatalog. Authoring against the compiled mirror
// would resolve the caller's grant set from five hardcoded profiles rather than
// from what this cluster says they hold, and would let a slug the rows already
// carry be minted again.
func TestRoleAuthoringRefusesWithNoCatalog(t *testing.T) {
	componentAuth.SetCapabilityCatalog(nil)
	i := New(&recordingEngine{}, nil)

	nodes, err := i.handleRoleCreate(asRole("owner"), map[string]any{
		"slug": "lead", "name": "Lead", "rank": 150,
		"grants": []any{map[string]any{"verb": "read", "resource": "principal"}},
	}, 0)
	ok, code := readDecision(t, nodes, err)

	if ok || code != codeCatalogUnavailable {
		t.Fatalf("ok=%v code=%q, want %s", ok, code, codeCatalogUnavailable)
	}
}
