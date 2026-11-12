package wholesalepack_test

// seed_helpers_db_test.go -- organizations, sites and people for the database
// tests.
//
// A DELIBERATE COPY of packs/reviewspack/live_helpers_e2e_test.go's helpers of
// the same names: one pack's test files cannot be imported by another's, and a
// pack may not import another pack.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// seedCtx is the engine's own seeding identity: internal origin and a synthetic
// owner, so a fixture row is written without standing in for anyone.
// Copied from packs/reviewspack/live_helpers_e2e_test.go.
func seedCtx() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "wholesale-e2e-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "wholesale-e2e-seeder"})
}

// seedSiteIn writes a storefront site the merchant owns, bound to the store,
// that belongs to an organization (none when accountID is empty).
// Copied from packs/reviewspack/live_helpers_e2e_test.go.
func seedSiteIn(t *testing.T, eng *memql.MemQLEngine, siteID, owner, storeID, accountID string) {
	t.Helper()
	// The store first: a storefront may be bound only to a store row that
	// exists and its writer can read.
	store, _ := json.Marshal(map[string]any{"domain": storeID + ".myshopify.com"})
	if _, err := eng.Execute(seedCtx(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString("v1:shopify:store"), langparser.QuoteString(storeID), string(store))); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	site := map[string]any{
		"ownerUserId": owner, "title": siteID, "hostname": siteID + ".example.test",
		"status": "live", "kind": "shopify_storefront", "bundleRef": "blob://sites/" + siteID + "/v1/",
		"binding": map[string]any{"storeId": storeID},
	}
	if accountID != "" {
		site["accountId"] = accountID
	}
	payload, _ := json.Marshal(site)
	q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString("v1:platform:site"),
		langparser.QuoteString(siteID), string(payload))
	if _, err := eng.Execute(seedCtx(), q); err != nil {
		t.Fatalf("seed site: %v", err)
	}
}

// seedOrganization writes an active organization and its account group, at the
// id integrations/groups derives (acct-<account>), and answers the group's id.
// Copied from packs/reviewspack/live_helpers_e2e_test.go.
func seedOrganization(t *testing.T, eng *memql.MemQLEngine, account string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
	if _, err := eng.Execute(seedCtx(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString("v1:accounts:account"), langparser.QuoteString(account), string(payload))); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	group := "acct-" + account
	seedGroup(t, eng, group, "account", account)
	return group
}

// seedGroup writes an active group tied to an organization.
// Copied from packs/reviewspack/live_helpers_e2e_test.go.
func seedGroup(t *testing.T, eng *memql.MemQLEngine, groupID, kind, account string) {
	t.Helper()
	call := `mutation writeGroup(groupId: ` + quote(groupID) + `, name: ` + quote(groupID) + `, kind: ` + quote(kind) +
		`, accountId: ` + quote(account) + `, status: "active")`
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("seed group: %v", err)
	}
}

// seedPersonIn writes a user row holding role and places the person in a group
// of the organization, with the membership's origin.
// Copied from packs/reviewspack/live_helpers_e2e_test.go.
func seedPersonIn(t *testing.T, eng *memql.MemQLEngine, userID, name, role string, active bool, groupID, account, origin string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"displayName": name, "primaryEmail": userID + "@example.test", "role": role, "active": active})
	if _, err := eng.Execute(seedCtx(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString("v1:identity:user"), langparser.QuoteString(userID), string(payload))); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	call := `mutation writeGroupMembership(membershipId: ` + quote(groupID+"-"+userID) + `, groupId: ` + quote(groupID) +
		`, userId: ` + quote(userID) + `, origin: ` + quote(origin) + `, status: "active", accountId: ` + quote(account) + `)`
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// seedStorePerson makes a store's person one of the people of the store's
// organization: the organization and its account group, and the person in it
// as a member on the member rung (writer, the rung's compiled name).
// Copied from packs/reviewspack/live_helpers_e2e_test.go.
func seedStorePerson(t *testing.T, eng *memql.MemQLEngine, account, userID string) {
	t.Helper()
	seedPersonIn(t, eng, userID, userID, string(auth.RoleWriter), true, seedOrganization(t, eng, account), account, "added")
}

// grantGroup gives a group one allow grant, at the id the grant writer derives.
// Copied from packs/reviewspack/live_helpers_e2e_test.go.
func grantGroup(t *testing.T, eng *memql.MemQLEngine, groupID, verb, resource string) {
	t.Helper()
	call := `writeGrant(grantId: ` + quote(auth.GrantRowID(auth.SubjectKindGroup, groupID, verb, resource)) +
		`, subjectKind: "group", subjectId: ` + quote(groupID) + `, verb: ` + quote(verb) + `, resourceType: ` + quote(resource) +
		`, effect: "allow", grantedBy: "wholesale-e2e-seeder")`
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("grant the group: %v", err)
	}
}

// quote renders a MemQL string literal through the engine's own escaping.
func quote(s string) string { return langparser.QuoteString(s) }
