package rbac

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// A DEVELOPER SETS A CLIENT UP WITHOUT THE OWNER (design record 2026-11-25,
// developer access). The case that started it: Jesús, a developer, gives the
// Fylo organization's group the apps its client needs -- Files and sharing,
// Deployables, Email -- and then reads what that client can open, to support
// them. Until the developer role held `update` on `principal`, every grant here
// was refused grant_caller_not_permitted and only the owner could do it.
//
// THE ROLES ARE THE SEEDS' OWN, read from dsl/rbac/seeds.memql, so this case
// fails when a seed row it relies on is removed -- not a ladder in miniature
// that would keep passing. The one role added is the organization's moderator,
// scoped to the organization, as production's `fylo-moderator` is.

const onboardModeratorRole = "client-moderator"

var seedCapabilityRow = regexp.MustCompile(`seed capability \S+ \{\s*roleSlug:\s*"([^"]+)"\s+verb:\s*"([^"]+)"\s+resourceType:\s*"([^"]+)"`)

// seededCatalogWithModerator is the predefined ladder with every capability row
// the seeds give it, plus one organization role scoped to `account`.
func seededCatalogWithModerator(t *testing.T, account string) *testCatalog {
	t.Helper()
	src, err := os.ReadFile("../../dsl/rbac/seeds.memql")
	if err != nil {
		t.Fatalf("reading the seeds: %v", err)
	}
	c := &testCatalog{
		ranks: map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100,
			"viewer": 50, "reader": 50, onboardModeratorRole: 20},
		canonical: map[string]string{"owner": "owner", "developer": "developer", "admin": "admin", "user": "user",
			"writer": "user", "viewer": "viewer", "reader": "viewer", onboardModeratorRole: onboardModeratorRole},
		names: map[string]string{"owner": "Owner", "developer": "Developer", "admin": "Admin", "user": "Member",
			"viewer": "Viewer", onboardModeratorRole: "Client moderator"},
		scopes:   map[string]string{onboardModeratorRole: account},
		inactive: map[string]bool{},
		grants:   map[string]map[componentAuth.VerbResource]bool{},
	}
	rows := seedCapabilityRow.FindAllStringSubmatch(string(src), -1)
	if len(rows) < 100 {
		t.Fatalf("read %d capability rows from the seeds; the pattern no longer matches them", len(rows))
	}
	for _, m := range rows {
		if c.grants[m[1]] == nil {
			c.grants[m[1]] = map[componentAuth.VerbResource]bool{}
		}
		c.grants[m[1]][componentAuth.VerbResource{Verb: m[2], Resource: m[3]}] = true
	}
	c.grants[onboardModeratorRole] = map[componentAuth.VerbResource]bool{
		{Verb: "read", Resource: "data"}: true, {Verb: "update", Resource: "data"}: true,
	}
	return c
}

// The apps a client's group needs, as the Users > Access screen writes them,
// and the organization each is answered in: Deployables and Email are decided
// per organization (the engine's organization gate), Files and its share part
// are a person's own and answered once, with no organization.
var clientApps = []struct{ verb, resource, inAccount string }{
	{"read", "app:files", ""},
	{"execute", "app:files/share", ""},
	{"read", "app:deployables", "account"},
	{"read", "app:email", "account"},
}

func TestADeveloperGivesAClientGroupItsAppsAndReadsWhatTheClientCanOpen(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("fylo")
	componentAuth.SetCapabilityCatalog(seededCatalogWithModerator(t, account))
	group := w.organization(account)
	client := w.person("carlos", onboardModeratorRole)
	w.join(group, account, client)
	developer := accessCaller(w.id("jesus"), componentAuth.RoleDeveloper)
	w.person("jesus", "developer")

	for _, app := range clientApps {
		res, err := w.eng.Execute(developer, fmt.Sprintf(
			`builtin grantSet(subjectKind: "group", subjectId: %s, verb: %s, resourceType: %s, effect: "allow")`,
			langparser.QuoteString(group), langparser.QuoteString(app.verb), langparser.QuoteString(app.resource)))
		if err != nil {
			t.Fatalf("grantSet %s %s: %v", app.verb, app.resource, err)
		}
		rows := memql.MaterializeRows(res)
		if len(rows) != 1 {
			t.Fatalf("grantSet %s %s answered %d rows", app.verb, app.resource, len(rows))
		}
		if ok, _ := rows[0]["ok"].(bool); !ok {
			t.Fatalf("a developer was refused %s %s on a client's group: %v", app.verb, app.resource, rows[0])
		}
	}

	// SUPPORT: the developer reads, through the engine's own resolver, what the
	// client can open in their organization -- each app from the group.
	got, err := readAccess(t, w.eng, developer, "user", client)
	if err != nil {
		t.Fatalf("a developer reading a client's access: %v", err)
	}
	for _, app := range clientApps {
		in := ""
		if app.inAccount != "" {
			in = account
		}
		expectAccess(t, got, in, app.verb, app.resource, true, "group", group)
	}
	// And nothing it was not given: the organization role's scope still holds.
	expectAccess(t, got, "", "read", "app:logs", false, "scoped-role-limit", onboardModeratorRole)
}

// THE OWNER STAYS OUT OF REACH. A group with the owner in it ranks as the owner,
// and nobody below an owner writes a grant for it -- grantSet's third rule,
// unchanged. Production consequence, stated: if the owner is ever a MEMBER of a
// client's group, only the owner can change that group's apps.
func TestADeveloperCannotGrantToAGroupTheOwnerIsIn(t *testing.T) {
	w := newAccessWorld(t)
	account := w.id("fylo")
	componentAuth.SetCapabilityCatalog(seededCatalogWithModerator(t, account))
	group := w.organization(account)
	w.join(group, account, w.person("owner", "owner"))
	developer := accessCaller(w.id("jesus"), componentAuth.RoleDeveloper)
	w.person("jesus", "developer")

	res, err := w.eng.Execute(developer, fmt.Sprintf(
		`builtin grantSet(subjectKind: "group", subjectId: %s, verb: "read", resourceType: "app:files", effect: "allow")`,
		langparser.QuoteString(group)))
	if err != nil {
		t.Fatalf("grantSet: %v", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("grantSet answered %d rows", len(rows))
	}
	if ok, _ := rows[0]["ok"].(bool); ok {
		t.Fatal("a developer wrote a grant for a group the owner is in")
	}
	if code, _ := rows[0]["code"].(string); code != codeGrantSubjectOutranksCaller {
		t.Fatalf("refused with %q, want %q", code, codeGrantSubjectOutranksCaller)
	}
}
