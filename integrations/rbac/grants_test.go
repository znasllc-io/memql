package rbac

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
