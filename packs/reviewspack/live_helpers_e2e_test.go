package reviewspack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// seedCtx is the engine's own seeding identity, as its account tests use it:
// internal origin and a synthetic owner, so a fixture row is written without
// standing in for anyone.
func seedCtx() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "reviews-e2e-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "reviews-e2e-seeder"})
}

// seedSite writes a storefront site the merchant owns, bound to the store: the
// site every store-wide reviews act names, and the organization gate judges.
func seedSite(t *testing.T, eng *memql.MemQLEngine, siteID, owner, storeID string) {
	t.Helper()
	seedSiteIn(t, eng, siteID, owner, storeID, "")
}

// seedSiteIn is seedSite for a site that belongs to an organization.
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

// seedStorePerson makes a store's merchant one of the people of the store's
// organization: the organization and its account group, and the merchant in
// it as a member on the member rung. A store's removal approver is one of
// those people.
//
// WRITER, the member rung's compiled name: this engine installs no role
// catalog, so the catalog's own slug for the rung ("user") holds nothing here,
// and an organization's store is judged on what its people hold there.
func seedStorePerson(t *testing.T, eng *memql.MemQLEngine, account, userID string) {
	t.Helper()
	seedPersonIn(t, eng, userID, userID, string(auth.RoleWriter), true, seedOrganization(t, eng, account), account, "added")
}

// grantGroup gives a group one allow grant, at the id the grant writer derives.
func grantGroup(t *testing.T, eng *memql.MemQLEngine, groupID, verb, resource string) {
	t.Helper()
	call := `writeGrant(grantId: ` + quote(auth.GrantRowID(auth.SubjectKindGroup, groupID, verb, resource)) +
		`, subjectKind: "group", subjectId: ` + quote(groupID) + `, verb: ` + quote(verb) + `, resourceType: ` + quote(resource) +
		`, effect: "allow", grantedBy: "reviews-e2e-seeder")`
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("grant the group: %v", err)
	}
}

// grantReviewsPart gives one person the reviews part, the way a store's
// moderator is given it: a grant on top of a role that does not hold it.
func grantReviewsPart(t *testing.T, eng *memql.MemQLEngine, userID string) {
	t.Helper()
	call := `writeGrant(grantId: ` + quote("grant-reviews-"+userID) + `, subjectKind: "user", subjectId: ` + quote(userID) +
		`, verb: "execute", resourceType: "app:deployables/reviews", effect: "allow", grantedBy: "reviews-e2e-seeder")`
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("grant the reviews part: %v", err)
	}
}

// seedMirror writes one mirrored Shopify row the way the connector does:
// Shopify owns these rows, so only the connector's actor writes them.
func seedMirror(t *testing.T, eng *memql.MemQLEngine, concept, id string, payload map[string]any) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload["updatedAt"], payload["syncedAt"] = now, now
	raw, _ := json.Marshal(payload)
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "shopify"))
	if _, err := eng.Execute(ctx, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(concept), langparser.QuoteString(id), string(raw))); err != nil {
		t.Fatalf("seed %s: %v", concept, err)
	}
}
