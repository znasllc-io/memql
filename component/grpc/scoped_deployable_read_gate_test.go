package memql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// An organization role can read its own deployables without holding any
// targetless data authority. The wire gate must let these collection readers
// ask the existing row guards; changing the role to global is not a remedy.
func TestScopedDeployableReadsPassTheTransportGate(t *testing.T) {
	eng := gateTestEngine(t)
	old := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(organizationGroupsWireCatalog{role: "store-moderator", account: "store-a"})
	t.Cleanup(func() { auth.SetCapabilityCatalog(old) })
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "moderator", Role: "store-moderator"})
	if eng.GlobalDataCapable(ctx, auth.VerbRead) {
		t.Fatal("precondition: an organization role must have no global data read")
	}
	s, cs := gateTestSession(t, eng, "store-moderator")
	// The receiving session has only verified forwarded identity. No local
	// organization grant or scope memo is transferred from the first node.
	wire, err := auth.ForwardedAuthorityForUser(s.access, auth.ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.access, err = auth.VerifyForwardedAuthority(wire, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, query string }{
		{"directory", `builtin deployableDirectoryPage()`},
		{"directory-own", `builtin deployableDirectoryPage(resource: "deployables", accountId: "store-a")`},
		{"directory-other", `builtin deployableDirectoryPage(resource: "deployables", accountId: "store-b")`},
		{"statistics", `builtin siteStatistics(period: "7d")`},
		{"statistics-accounts", `builtin siteStatistics(period: "7d", accountIds: ["store-a", "store-b"])`},
		{"statistics-sites", `builtin siteStatistics(period: "7d", siteIds: ["site-a", "site-b"])`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			driveGateQuery(t, s, tc.id, tc.query)
			// This engine has no database. Its eventual execution error proves
			// the real wire handler passed the data gate; an empty immediate
			// response alone would not prove admission or successful execution.
			outcome := awaitQueryError(t, cs, tc.id)
			if strings.Contains(outcome.GetMessage(), "holds no read capability on the data plane") {
				t.Fatalf("organization-aware read was stopped before its row guard: %s", outcome.GetMessage())
			}
			if !strings.Contains(outcome.GetMessage(), "memory engine database not configured") {
				t.Fatalf("expected admitted request to reach the DB-less engine, got: %s", outcome.GetMessage())
			}
		})
	}
	for _, query := range []string{
		`builtin deployableDirectoryPage(resource: "sources")`,
		`builtin deployableDirectoryPage(resource: "unknown")`,
		`query agentById(agentId: "private")`,
		`createAgent(agentId: "new", ownerUserId: "moderator", name: "Private")`,
	} {
		if allowed, _ := s.allowDataPlaneAccess(ctx, query); allowed {
			t.Errorf("collection-read exception admitted unscoped work: %s", query)
		}
	}
	if eng.GlobalDataCapable(ctx, auth.VerbRead) {
		t.Fatal("collection admission changed the role's global authority")
	}
}
