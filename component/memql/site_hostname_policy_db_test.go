package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// site_hostname_policy_db_test.go -- memql#4344, the half that has to run
// against a real engine.
//
// The unit tests beside this file drive applySiteOwnerStamp and
// validateUserSiteHostname directly. That is necessary and not sufficient:
// every one of them would keep passing if executeWrite stopped calling either,
// and the failure would be silent -- rows landing owner-less, hostnames landing
// unchecked, and a suite reporting green. So the properties that matter are
// proven HERE, through `eng.Execute` on the real mutation path, which is the
// same standard memql#3207 set for the sibling slug guard.
//
// Postgres-gated like its neighbours (sharedReadMergeEngine skips when no DB is
// reachable). CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1, so
// a skip there is a failure rather than a green.

// siteTestDomain is the domain these tests configure the cluster to serve. Set
// through MEMQL_DOMAIN, which is where siteHostnamePolicyDomain reads it and
// where every other domain-shaped value in the cluster comes from.
const siteTestDomain = "policy-test.example"

// userSiteCtx is a cluster developer, used by hostname and attribution tests.
// Organization-isolation tests below use ordinary members instead: cluster
// operators intentionally have standing access to every organization.
func userSiteCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.RoleDeveloper,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// installSiteOrganizationCapabilities admits ordinary members to the exact app
// actions exercised below, so a cross-organization refusal tests row scope.
func installSiteOrganizationCapabilities(t *testing.T, extra ...auth.VerbResource) {
	t.Helper()
	grants := map[auth.VerbResource]bool{
		{Verb: auth.VerbRead, Resource: auth.ResourceData}:            true,
		{Verb: auth.VerbCreate, Resource: auth.ResourceData}:          true,
		{Verb: auth.VerbUpdate, Resource: auth.ResourceData}:          true,
		{Verb: auth.VerbRead, Resource: "app:deployables"}:            true,
		{Verb: auth.VerbExecute, Resource: "app:deployables/sources"}: true,
		{Verb: auth.VerbExecute, Resource: "app:deployables/publish"}: true,
		{Verb: auth.VerbExecute, Resource: "app:deployables/retire"}:  true,
	}
	for _, permission := range extra {
		grants[permission] = true
	}
	auth.SetCapabilityCatalog(&capabilityFake{
		ranks:  map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100, "viewer": 50},
		grants: map[string]map[auth.VerbResource]bool{"owner": grants, "developer": grants, "user": grants, "writer": grants},
	})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
}

func siteOrganizationMemberCtx(t *testing.T, eng *MemQLEngine, userID, accountID string) context.Context {
	t.Helper()
	seedPrincipal(t, eng, userID, auth.RoleWriter)
	if err := organizationInsert(t, eng, groupSeedCtx(), conceptAccountsAccount, accountID, map[string]any{"name": accountID, "status": "active", "domainStatus": "unverified"}); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	seedGroup(t, eng, "group-"+accountID, accountID, "account", accountID)
	seedMembership(t, eng, "group-"+accountID, userID, "active")
	return rankActorCtx(userID, auth.RoleWriter)
}

// systemSiteCtx is the SeedMaterializer's shape: a `system:` actor carrying a
// cluster-owner AccessContext (systemActorContext, memql#3711).
func systemSiteCtx() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId:    "system:site-policy-test",
		Role:      auth.RoleOwner,
		Synthetic: true,
		Unranked:  true,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "system:site-policy-test"})
}

// runSiteMutation invokes a named mutation in the kind-prefixed, named-args
// call form (`mutation <name>(k: v, ...)`, #2335) and RETURNS the error rather
// than asserting on it -- half these tests are about a refusal, and runMutation
// (executor_mutation_readmerge_db_test.go) requires success.
//
// It does NOT layer a caller identity the way runMutation does: every test here
// passes its own actor deliberately, and stamping a fixture identity over the
// top is what would make a cross-user refusal test pass by comparing one caller
// against itself.
func runSiteMutation(t *testing.T, ctx context.Context, eng *MemQLEngine, name string, args map[string]any) (string, error) {
	t.Helper()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(args))
	for _, k := range keys {
		vb, err := json.Marshal(args[k])
		if err != nil {
			t.Fatalf("marshal arg %q: %v", k, err)
		}
		parts = append(parts, k+": "+string(vb))
	}
	res, err := eng.Execute(ctx, "mutation "+name+"("+strings.Join(parts, ", ")+")")
	if err != nil {
		return "", err
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return "", nil
	}
	return res.Bundle.Nodes[0].Id, nil
}

// createSiteRaw runs the real createSite mutation and returns its error, so a
// test can assert on a REFUSAL rather than only on a success.
func createSiteRaw(t *testing.T, ctx context.Context, eng *MemQLEngine, args map[string]any) (string, error) {
	t.Helper()
	return runSiteMutation(t, ctx, eng, "createSite", args)
}

// A user's own deployable: created at <slug>.<domain>, and the row lands owned
// by them. This is the criterion "createSite stamps ownerUserId from the actor",
// measured where it is actually stamped.
func TestSiteCreateStampsTheActorAsOwnerThroughTheMutationPath(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	slug := "own-" + uniqueSuffix("site")
	id := "site-own-" + uniqueSuffix("site")
	caller := "user-site-own-" + uniqueSuffix("site")

	ctx := userSiteCtx(caller)
	storedId, err := createSiteRaw(t, ctx, eng, map[string]any{
		"siteId":    id,
		"hostname":  slug + "." + siteTestDomain,
		"bundleRef": "blob://sites/" + id + "/v1/",
	})
	if err != nil {
		t.Fatalf("a user could not create a deployable at <slug>.<domain>: %v", err)
	}

	payload := latestPayload(t, ctx, db, conceptPlatformSite, storedId)
	owner := stringFromAny(payload["ownerUserId"])
	if !sameRowAuthzOwner(owner, caller) {
		t.Fatalf("the created site's ownerUserId is %q, want the caller %q. Without the stamp the "+
			"row is cluster-owned and the person who created it cannot see it at all", owner, caller)
	}
}

// THE POLICY, through the mutation path. Each of these is a hostname a user
// must not be able to claim, and the refusal has to come from the guard rather
// than from something incidental -- so the message is checked too.
func TestSiteCreateRefusesAUserHostnameOutsideThePolicy(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	caller := userSiteCtx("user-site-policy-" + uniqueSuffix("site"))
	cases := []struct {
		name     string
		hostname string
		want     string
	}{
		{"a reserved front-door label", "identity." + siteTestDomain, "reserved"},
		{"the portal's own label", "portal." + siteTestDomain, "reserved"},
		{"a squat label", "www." + siteTestDomain, "reserved"},
		{"the apex", siteTestDomain, "apex"},
		{"two labels under the domain", "shop.eu." + siteTestDomain, "more than one label"},
		{"another domain entirely", "shop.somewhere-else.test", "not under this cluster's domain"},
		{"a slug under the minimum", "ab." + siteTestDomain, "not a usable site name"},
		// Refused one layer EARLIER, by createSite's own @pattern -- the SHAPE
		// half of the policy, which is all a mutation body can express. Kept in
		// this table on purpose: the two layers are both real, and a reader
		// needs to see which one answers rather than assume one covers
		// everything.
		{"a slug with an illegal character", "sh_op." + siteTestDomain, "does not match pattern"},
		{"an uppercase hostname", "SHOP." + siteTestDomain, "does not match pattern"},
		{"a slug over the maximum", strings.Repeat("a", 41) + "." + siteTestDomain, "not a usable site name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := createSiteRaw(t, caller, eng, map[string]any{
				"siteId":    "site-refuse-" + uniqueSuffix(tc.name),
				"hostname":  tc.hostname,
				"bundleRef": "blob://sites/x/v1/",
			})
			if err == nil {
				t.Fatalf("a user claimed %q through createSite. The one `*.%s` Ingress rule is what "+
					"routes a site with no operator step, and the reserved labels are what stop a "+
					"user being handed the front door's own traffic", tc.hostname, siteTestDomain)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal did not come from the hostname policy (wanted %q in the "+
					"message): %v", tc.want, err)
			}
		})
	}
}

// A cluster owner is exempt from the SHAPE rule: a custom apex or a second
// domain is a legitimate operator deployment arriving with its own DNS and its
// own hand-issued Certificate (memql#4224).
func TestSiteCreateLetsAClusterOwnerUseACustomHostname(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	root := "root-site-" + uniqueSuffix("site")
	id := "site-custom-" + uniqueSuffix("site")
	ctx := clusterOwnerCtx(root)

	storedId, err := createSiteRaw(t, ctx, eng, map[string]any{
		"siteId":    id,
		"hostname":  "www.a-customers-own-domain-" + uniqueSuffix("site") + ".test",
		"bundleRef": "blob://sites/" + id + "/v1/",
	})
	if err != nil {
		t.Fatalf("a cluster owner was refused a custom hostname: %v\n"+
			"Refusing this would make the cluster unable to host anything but <slug>.<domain>", err)
	}

	// A real operator remains the recorded owner. Only synthetic seed actors
	// create unowned system sites.
	payload := latestPayload(t, ctx, db, conceptPlatformSite, storedId)
	if owner := strings.TrimSpace(stringFromAny(payload["ownerUserId"])); BareShortId(owner) != root {
		t.Fatalf("a real cluster owner lost persisted attribution: %q, want %q", owner, root)
	}
}

// HOSTNAME UNIQUENESS binds every caller, including a cluster owner: two live
// rows on one hostname make siteByHostname's answer depend on row order, which
// is a routing defect rather than a permission one.
func TestSiteCreateRefusesADuplicateHostnameEvenForAClusterOwner(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	slug := "dup-" + uniqueSuffix("site")
	hostname := slug + "." + siteTestDomain
	first := "site-dup-a-" + uniqueSuffix("site")

	owner := userSiteCtx("user-site-dup-" + uniqueSuffix("site"))
	if _, err := createSiteRaw(t, owner, eng, map[string]any{
		"siteId":    first,
		"hostname":  hostname,
		"bundleRef": "blob://sites/" + first + "/v1/",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	// A second USER cannot take it...
	second := userSiteCtx("user-site-dup2-" + uniqueSuffix("site"))
	_, err := createSiteRaw(t, second, eng, map[string]any{
		"siteId":    "site-dup-b-" + uniqueSuffix("site"),
		"hostname":  hostname,
		"bundleRef": "blob://sites/b/v1/",
	})
	if err == nil {
		t.Fatal("a second user claimed a hostname another site already answers on")
	}
	if !strings.Contains(err.Error(), "already served by site") {
		t.Fatalf("the refusal did not come from the uniqueness probe: %v", err)
	}

	// ...and neither can a CLUSTER OWNER. The shape rule is waived for them;
	// this one is not, because it is about whether the cluster can serve the
	// row at all.
	_, err = createSiteRaw(t, clusterOwnerCtx("root-dup-"+uniqueSuffix("site")), eng, map[string]any{
		"siteId":    "site-dup-c-" + uniqueSuffix("site"),
		"hostname":  hostname,
		"bundleRef": "blob://sites/c/v1/",
	})
	if err == nil {
		t.Fatal("a cluster owner created a SECOND live site on an occupied hostname. Uniqueness is " +
			"not a permission question -- the edge resolves a request Host to one row, so two rows " +
			"make which site answers depend on row order")
	}
}

// The uniqueness probe must not refuse a row's own re-write. A mutation carries
// a BARE id while storage holds the concept-qualified one, so a raw comparison
// makes every update of an existing site a duplicate of itself -- and the first
// casualty would be the SeedMaterializer's re-write of the portal row on boot.
func TestSiteUpdateOfItsOwnHostnameIsNotADuplicate(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	slug := "self-" + uniqueSuffix("site")
	id := "site-self-" + uniqueSuffix("site")
	ctx := userSiteCtx("user-site-self-" + uniqueSuffix("site"))

	args := map[string]any{
		"siteId":    id,
		"hostname":  slug + "." + siteTestDomain,
		"bundleRef": "blob://sites/" + id + "/v1/",
	}
	if _, err := createSiteRaw(t, ctx, eng, args); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := createSiteRaw(t, ctx, eng, args); err != nil {
		t.Fatalf("re-running createSite on the SAME id was refused: %v\n"+
			"That is the shape the SeedMaterializer takes on every boot after the first", err)
	}
	if _, err := runSiteMutation(t, ctx, eng, "updateSiteBundle", map[string]any{
		"siteId":    id,
		"bundleRef": "blob://sites/" + id + "/v2/",
	}); err != nil {
		t.Fatalf("publishing a new bundle to an existing site was refused: %v", err)
	}
}

// CROSS-ORGANIZATION WRITES. A member with the required app grants may
// publish their organization's deployable, but cannot republish another's.
func TestSiteCrossUserWritesAreRefusedThroughTheMutationPath(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	slug := "cross-" + uniqueSuffix("site")
	id := "site-cross-" + uniqueSuffix("site")
	installSiteOrganizationCapabilities(t)
	accountID := "account-owner-" + uniqueSuffix("site")
	ownerCtx := siteOrganizationMemberCtx(t, eng, "user-site-owner-"+uniqueSuffix("site"), accountID)
	strangerCtx := siteOrganizationMemberCtx(t, eng, "user-site-stranger-"+uniqueSuffix("site"), "account-stranger-"+uniqueSuffix("site"))

	if _, err := createSiteRaw(t, ownerCtx, eng, map[string]any{
		"siteId":    id,
		"accountId": accountID,
		"hostname":  slug + "." + siteTestDomain,
		"bundleRef": "blob://sites/" + id + "/v1/",
	}); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	for _, tc := range []struct {
		mutation string
		args     map[string]any
	}{
		{"updateSiteBundle", map[string]any{"siteId": id, "bundleRef": "blob://evil/v1/"}},
		{"updateSiteStatus", map[string]any{"siteId": id, "status": "disabled"}},
		{"deleteSite", map[string]any{"siteId": id}},
	} {
		t.Run(tc.mutation, func(t *testing.T) {
			if _, err := runSiteMutation(t, strangerCtx, eng, tc.mutation, tc.args); err == nil {
				t.Fatalf("%s succeeded against another organization's deployable", tc.mutation)
			}
		})
	}

	// The owner still writes their own, and a cluster owner writes anybody's.
	if _, err := runSiteMutation(t, ownerCtx, eng, "updateSiteStatus", map[string]any{
		"siteId": id, "status": "live",
	}); err != nil {
		t.Fatalf("the site's own owner was refused: %v", err)
	}
	if _, err := runSiteMutation(t, clusterOwnerCtx("root-cross-"+uniqueSuffix("site")), eng,
		"updateSiteStatus", map[string]any{"siteId": id, "status": "disabled"}); err != nil {
		t.Fatalf("a cluster owner was refused a write onto a user's site: %v", err)
	}
}

// THE READ SIDE. A member sees their organization's deployables; a cluster
// owner sees every organization. Membership and standing access both matter.
func TestSitesAllScopesToTheCallerAndOpensForAClusterOwner(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	mineSlug := "mine-" + uniqueSuffix("site")
	theirsSlug := "theirs-" + uniqueSuffix("site")
	mineId := "site-mine-" + uniqueSuffix("site")
	theirsId := "site-theirs-" + uniqueSuffix("site")

	installSiteOrganizationCapabilities(t)
	mineAccount := "account-mine-" + uniqueSuffix("site")
	theirsAccount := "account-theirs-" + uniqueSuffix("site")
	me := siteOrganizationMemberCtx(t, eng, "user-site-me-"+uniqueSuffix("site"), mineAccount)
	them := siteOrganizationMemberCtx(t, eng, "user-site-them-"+uniqueSuffix("site"), theirsAccount)

	storedMine, err := createSiteRaw(t, me, eng, map[string]any{
		"siteId": mineId, "accountId": mineAccount, "hostname": mineSlug + "." + siteTestDomain,
		"bundleRef": "blob://sites/" + mineId + "/v1/",
	})
	if err != nil {
		t.Fatalf("create mine: %v", err)
	}
	storedTheirs, err := createSiteRaw(t, them, eng, map[string]any{
		"siteId": theirsId, "accountId": theirsAccount, "hostname": theirsSlug + "." + siteTestDomain,
		"bundleRef": "blob://sites/" + theirsId + "/v1/",
	})
	if err != nil {
		t.Fatalf("create theirs: %v", err)
	}

	mine := sitesAllIds(t, me, eng)
	if !mine[storedMine] {
		t.Fatal("sitesAll did not return the caller's OWN deployable. Under a plain clusterOwner " +
			"tier this is exactly what a user sees: nothing they made")
	}
	if mine[storedTheirs] {
		t.Fatal("sitesAll returned another user's deployable to an ordinary caller")
	}

	root := sitesAllIds(t, clusterOwnerCtx("root-all-"+uniqueSuffix("site")), eng)
	for _, id := range []string{storedMine, storedTheirs} {
		if !root[id] {
			t.Fatalf("sitesAll did not return %q to a CLUSTER OWNER. \"List every site in this "+
				"cluster\" is the portal's primary screen, and it is the read a plain owner= tier "+
				"makes not merely unimplemented but inexpressible", id)
		}
	}
}

// A site created by a SYSTEM actor -- the SeedMaterializer's shape -- lands
// cluster-owned, and stays cluster-owned when it is re-materialized. Both boots
// matter: the first-ever materialization is the only one with no prior row, so
// a guard that only got the create right would hand site #1 an owner on the
// second boot and nothing would say so.
func TestSiteSeededBySystemActorStaysClusterOwnedAcrossReMaterialization(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	id := "site-seed-" + uniqueSuffix("site")
	ctx := systemSiteCtx()
	args := map[string]any{
		"siteId":      id,
		"hostname":    "portal." + siteTestDomain,
		"bundleRef":   "file:///app/os",
		"status":      "live",
		"apiProxy":    true,
		"systemOwned": true,
		"title":       "MemQL Portal",
	}

	storedId, err := createSiteRaw(t, ctx, eng, args)
	if err != nil {
		t.Fatalf("the seed-shaped create was refused: %v\n"+
			"The hostname is portal.<domain>, which is RESERVED for users -- a guard that applied "+
			"the user policy to the seeder would fail every boot", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := createSiteRaw(t, ctx, eng, args); err != nil {
			t.Fatalf("re-materialization %d was refused: %v", i+1, err)
		}
		payload := latestPayload(t, ctx, db, conceptPlatformSite, storedId)
		if owner := strings.TrimSpace(stringFromAny(payload["ownerUserId"])); owner != "" {
			t.Fatalf("after re-materialization %d the seeded site is owned by %q, want "+
				"CLUSTER-OWNED (empty). The portal is the platform's row -- an owner here means "+
				"site #1 belongs to whatever principal happened to boot the cluster", i+1, owner)
		}
	}
}

// sitesAllIds runs the real sitesAll query under one actor and returns the row
// ids it returned, as a set.
//
// Ids rather than hostnames deliberately: the id is what the shape projects
// under `row.id` regardless of how siteFull evolves, and comparing it to the
// id createSite handed back keeps the assertion about ROW ADMISSION rather than
// about a projection.
func sitesAllIds(t *testing.T, ctx context.Context, eng *MemQLEngine) map[string]bool {
	t.Helper()
	res, err := eng.Execute(ctx, "query sitesAll()")
	if err != nil {
		t.Fatalf("sitesAll: %v", err)
	}
	out := map[string]bool{}
	if res == nil || res.Bundle == nil {
		return out
	}
	for _, n := range res.Bundle.Nodes {
		if n == nil {
			continue
		}
		if id := strings.TrimSpace(n.GetId()); id != "" {
			out[id] = true
		}
	}
	return out
}

// A shopify_storefront deployable, with its binding, round-trips through the
// real mutation and lands on the row.
//
// Two things this is actually about. `kind` is a closed enum, so a value the
// concept does not declare is refused at schema validation -- and the storefront
// value is the one memql#4344 adds. And `binding` is an `object` arg: it is the
// only structured argument createSite takes, so it is the one whose shape a
// unit test cannot vouch for.
//
// THE BINDING NAMES A STORE ROW NOW (epic memql#5530). It used to carry a COPY
// of the store's domain and its Storefront token reference, and this test used
// to assert both round-tripped. Both are on v1:shopify:store, the edge resolves
// them through it at serve time, and the guard beside the hostname policy
// REFUSES the retired shape outright -- so the write below is made as a cluster
// owner against a store row this test seeds, which is the only shape that is
// legal at all.
func TestSiteCreateAcceptsAShopifyStorefrontWithItsBinding(t *testing.T) {
	// Reach the store row ownership guard with explicit app action authority.
	installSiteOrganizationCapabilities(t, auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/store"})
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	slug := "shop-" + uniqueSuffix("site")
	id := "site-shop-" + uniqueSuffix("site")
	storeId := "store-" + uniqueSuffix("site")
	// A CLUSTER OWNER, because v1:shopify:store is @rowAuthz(clusterOwner) and
	// the guard admits only a store the CALLER can read.
	ctx := systemSiteCtx()

	if _, err := runSiteMutation(t, ctx, eng, "createStore", map[string]any{
		"storeId":            storeId,
		"domain":             "example-store.myshopify.com",
		"storefrontTokenRef": "SHOPIFY_STOREFRONT_TOKEN",
	}); err != nil {
		t.Fatalf("seeding the store this storefront fronts: %v", err)
	}

	storedId, err := createSiteRaw(t, ctx, eng, map[string]any{
		"siteId":    id,
		"hostname":  slug + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/" + id + "/v1/",
		"binding":   map[string]any{"storeId": storeId},
	})
	if err != nil {
		t.Fatalf("a shopify_storefront site was refused: %v", err)
	}

	payload := latestPayload(t, ctx, db, conceptPlatformSite, storedId)
	if got := stringFromAny(payload["kind"]); got != "shopify_storefront" {
		t.Fatalf("kind = %q, want shopify_storefront", got)
	}
	binding, ok := payload["binding"].(map[string]any)
	if !ok {
		t.Fatalf("binding did not round-trip as an object: %#v", payload["binding"])
	}
	if got := stringFromAny(binding["storeId"]); got != storeId {
		t.Fatalf("binding.storeId = %q, want %q -- the binding NAMES the store row; "+
			"the domain and the Storefront token reference live on it", got, storeId)
	}
	// AND NOTHING ELSE. A copy beside the reference would be the duplication
	// this epic removed, re-created by the one mutation that writes a binding.
	if len(binding) != 1 {
		t.Fatalf("binding carries %d keys, want exactly storeId: %#v", len(binding), binding)
	}

	// THE RETIRED SHAPE IS REFUSED, and by the same caller that just
	// succeeded -- so the refusal is about the SHAPE rather than about who
	// asked. Pre-release means no shim: a write still carrying the copy is a
	// caller nobody migrated.
	if _, err := createSiteRaw(t, ctx, eng, map[string]any{
		"siteId":    "site-legacy-" + uniqueSuffix("site"),
		"hostname":  "legacy-" + uniqueSuffix("site") + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/legacy/v1/",
		"binding": map[string]any{
			"storeDomain":        "example-store.myshopify.com",
			"storefrontTokenRef": "SHOPIFY_STOREFRONT_TOKEN",
		},
	}); err == nil {
		t.Fatal("the retired {storeDomain, storefrontTokenRef} binding was accepted")
	} else if !strings.Contains(err.Error(), "storeId") {
		t.Errorf("the refusal does not say what to write instead: %v", err)
	}

	// A STORE THE CALLER CANNOT READ IS REFUSED. This is the substantive half
	// of the tier reconciliation (issue memql#5538): binding a storefront to a
	// store you may not read would publish that store's Storefront token under
	// your own hostname, because the edge resolves it into a document served
	// unauthenticated to every visitor.
	//
	// The caller is an ADMIN, the rung directly below v1:shopify:store's
	// developer read floor (Connect Shopify, D3), so a floor set one rung too
	// low would fail here. It used to be a developer, who since D3 reads every
	// store -- and so may bind to one, which the next case pins.
	//
	// Each refusal below must be the one it claims, so the catalog installed
	// above is re-cut per role: the admin holds every grant the organization
	// boundary (memql#5598) asks of a binding create, the store part included,
	// which leaves the read floor as the only thing refusing them; the
	// developer holds the same set, the store part included, as the seed
	// gives it (Connect Shopify 003).
	installed := auth.InstalledCapabilityCatalog().(*capabilityFake)
	auth.SetCapabilityCatalog(&capabilityFake{ranks: installed.ranks, grants: map[string]map[auth.VerbResource]bool{
		"owner":     installed.grants["owner"],
		"admin":     installed.grants["owner"],
		"developer": installed.grants["owner"],
	}})
	adminCtx := rankActorCtx("admin-site-shop-"+uniqueSuffix("site"), auth.RoleAdmin)
	if _, err := createSiteRaw(t, adminCtx, eng, map[string]any{
		"siteId":    "site-unreadable-" + uniqueSuffix("site"),
		"hostname":  "unreadable-" + uniqueSuffix("site") + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/unreadable/v1/",
		"binding":   map[string]any{"storeId": storeId},
	}); err == nil {
		t.Fatal("an admin bound a storefront to a store below whose read floor they sit")
	} else if !strings.Contains(err.Error(), storeId) {
		t.Errorf("the refusal does not name the store it refused: %v", err)
	}

	// A DEVELOPER BINDS AT CREATE (Connect Shopify, D3). A developer reads
	// every store, and holds the store part (execute app:deployables/store)
	// that binding also asks for -- so createSite with a binding is admitted,
	// by the write seam (validateSiteStoreBindingChange) and by the
	// organization boundary's own check at the site's organization alike.
	// Until the store part was seeded on developer this case was REFUSED with
	// capability_not_held; the refusal for a developer WITHOUT the part is
	// pinned by TestAWriteThatKeepsTheBindingNeedsNoStorePart, under a deny.
	devCtx := userSiteCtx("user-site-shop-" + uniqueSuffix("site"))
	devSite, err := createSiteRaw(t, devCtx, eng, map[string]any{
		"siteId":    "site-developer-" + uniqueSuffix("site"),
		"hostname":  "developer-" + uniqueSuffix("site") + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/developer/v1/",
		"binding":   map[string]any{"storeId": storeId},
	})
	if err != nil {
		t.Fatalf("a developer holding the store part was refused a binding at create: %v", err)
	}
	devBinding, _ := latestPayload(t, ctx, db, conceptPlatformSite, devSite)["binding"].(map[string]any)
	if got := stringFromAny(devBinding["storeId"]); got != storeId {
		t.Fatalf("the developer's storefront binding.storeId = %q, want %q", got, storeId)
	}

	// A kind the concept does not declare is refused, which is what makes the
	// enum a decision rather than a label: android / ios / macos deliberately
	// have no value (design D5).
	if _, err := createSiteRaw(t, ctx, eng, map[string]any{
		"siteId":    "site-android-" + uniqueSuffix("site"),
		"hostname":  "android-" + uniqueSuffix("site") + "." + siteTestDomain,
		"kind":      "android",
		"bundleRef": "blob://sites/x/v1/",
	}); err == nil {
		t.Fatal("kind \"android\" was accepted. Android / iOS / macOS are artifact DISTRIBUTION, " +
			"not hostname-resolved web surfaces -- a value the edge cannot resolve would be the " +
			"wrong kind of additive (design D5)")
	}
}

// TestAWriteThatKeepsTheBindingNeedsNoStorePart pins the other half of the
// capability check: it judges a CHANGE of binding, not every write to a bound
// site. The guard sees the merged row, so a check on "the payload names a
// store" would demand the store part for a settings edit or a publish.
//
// The caller is a developer WITHOUT the store part, which since the part is
// seeded on developer means a developer an owner has barred from it by name:
// a user DENY of execute app:deployables/store (access-model.md, "Grants to
// people and groups"). So this also proves a per-person deny is honoured on
// the paths no binding mutation guards -- createSite, which takes a binding
// and declares no capability, and a raw insert(), which names no construct.
// The site is the developer's own, created unbound, and then bound by server
// code, which is the only way the barred developer comes to hold a bound site.
// Both bindings: the serving one and the preview one (Connect Shopify 009),
// which names a store the same way.
func TestAWriteThatKeepsTheBindingNeedsNoStorePart(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	eng.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetGrantSource(nil)
		auth.SetMembershipSource(nil)
	})

	suffix := uniqueSuffix("keepbinding")
	storeId := "store-" + suffix
	if _, err := runSiteMutation(t, systemSiteCtx(), eng, "createStore", map[string]any{
		"storeId": storeId,
		"domain":  "keep-" + suffix + ".myshopify.com",
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	dev := "user-keep-" + suffix
	seedPrincipal(t, eng, dev, auth.RoleDeveloper)
	writeGrant(t, eng, auth.SubjectKindUser, dev, auth.VerbExecute, "app:deployables/store", auth.GrantDeny)
	devCtx := userSiteCtx(dev)

	// The CREATE path: a binding at createSite is a change from nothing.
	if _, err := createSiteRaw(t, devCtx, eng, map[string]any{
		"siteId":    "site-keep-denied-" + suffix,
		"hostname":  "keep-denied-" + suffix + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/keep-denied/v1/",
		"binding":   map[string]any{"storeId": storeId},
	}); err == nil {
		t.Fatal("a developer denied the store part bound a storefront at create")
	} else if !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Errorf("the refusal is not the capability refusal: %v", err)
	}

	siteId := "site-keep-" + suffix
	storedSite, err := createSiteRaw(t, devCtx, eng, map[string]any{
		"siteId":    siteId,
		"hostname":  "keep-" + suffix + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/" + siteId + "/v1/",
	})
	if err != nil {
		t.Fatalf("the developer's unbound storefront: %v", err)
	}
	// Server code is the SeedMaterializer's shape: a synthetic actor, which
	// the organization boundary (memql#5598) exempts, under internal origin,
	// which the row write guard and the capability gate exempt.
	if _, err := runSiteMutation(t, auth.ContextWithInternalOrigin(systemSiteCtx()), eng, "updateSiteStoreBinding", map[string]any{
		"siteId":  siteId,
		"storeId": storeId,
	}); err != nil {
		t.Fatalf("bind it as server code: %v", err)
	}
	devStore := seedStore(t, eng, "store-dev-"+suffix, true)
	if _, err := runSiteMutation(t, auth.ContextWithInternalOrigin(systemSiteCtx()), eng, "updateSitePreviewBinding", map[string]any{
		"siteId":  siteId,
		"storeId": devStore,
	}); err != nil {
		t.Fatalf("point its preview as server code: %v", err)
	}

	if _, err := runSiteMutation(t, devCtx, eng, "updateSiteSettings", map[string]any{
		"siteId":   siteId,
		"settings": map[string]any{"theme": "dark"},
	}); err != nil {
		t.Fatalf("a settings edit on a site whose bindings it does not touch was refused: %v", err)
	}

	// Re-pointing it is a change, and a raw insert names no construct, so the
	// capability must be asked at the write seam, not on the mutation.
	other := "store-other-" + suffix
	if _, err := runSiteMutation(t, systemSiteCtx(), eng, "createStore", map[string]any{
		"storeId": other,
		"domain":  "other-" + suffix + ".myshopify.com",
	}); err != nil {
		t.Fatalf("seed second store: %v", err)
	}
	if err := rawSiteWrite(devCtx, eng, siteId, map[string]any{"binding": map[string]any{"storeId": other}}); err == nil {
		t.Fatal("a raw insert re-pointed a storefront's binding without the store part")
	} else if !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Errorf("the refusal is not the capability refusal: %v", err)
	}

	// The preview binding is the same: re-pointing it is a change. (Clearing
	// is judged by neither of this seam's checks, but the organization
	// boundary asks the store part for it too, which this developer lacks at
	// the site's organization; TestAPreviewBindingNeedsAStoreTheCallerCanRead
	// clears as a caller the boundary admits.)
	otherDev := seedStore(t, eng, "store-dev-other-"+suffix, true)
	if err := rawSiteWrite(devCtx, eng, siteId, map[string]any{"previewBinding": map[string]any{"storeId": otherDev}}); err == nil {
		t.Fatal("a raw insert re-pointed a storefront's preview binding without the store part")
	} else if !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Errorf("the refusal is not the capability refusal: %v", err)
	}
	// And the named mutation, whose own @requiresCapability asks the same.
	if _, err := runSiteMutation(t, devCtx, eng, "updateSiteStoreBinding", map[string]any{
		"siteId":  siteId,
		"storeId": other,
	}); err == nil {
		t.Fatal("updateSiteStoreBinding re-pointed a storefront for a developer denied the store part")
	} else if !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Errorf("the refusal is not the capability refusal: %v", err)
	}
	binding, _ := latestPayload(t, systemSiteCtx(), db, conceptPlatformSite, storedSite)["binding"].(map[string]any)
	if got := stringFromAny(binding["storeId"]); got != storeId {
		t.Fatalf("a refused re-point still moved the binding: storeId = %q, want %q", got, storeId)
	}
}

// TestADeveloperBindsTheirStorefrontToAStoreAnOwnerRegistered is the case the
// store part on developer exists for (Connect Shopify design, section 8; D3):
// a developer attaches a storefront they own to a store a cluster owner
// registered, through the same mutation the Store panel calls. Two gates, and
// a developer now passes both -- the part (execute app:deployables/store) and
// the readability of the store (v1:shopify:store's developer read floor).
func TestADeveloperBindsTheirStorefrontToAStoreAnOwnerRegistered(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("devbind")
	storeId := "store-" + suffix
	if _, err := runSiteMutation(t, systemSiteCtx(), eng, "createStore", map[string]any{
		"storeId":            storeId,
		"domain":             "devbind-" + suffix + ".myshopify.com",
		"storefrontTokenRef": "DEVBIND_STOREFRONT_TOKEN",
	}); err != nil {
		t.Fatalf("an owner registering the store: %v", err)
	}
	dev := "user-devbind-" + suffix
	seedPrincipal(t, eng, dev, auth.RoleDeveloper)
	devCtx := userSiteCtx(dev)
	siteId := "site-devbind-" + suffix
	storedSite, err := createSiteRaw(t, devCtx, eng, map[string]any{
		"siteId":    siteId,
		"hostname":  "devbind-" + suffix + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/" + siteId + "/v1/",
	})
	if err != nil {
		t.Fatalf("the developer's unbound storefront: %v", err)
	}

	if _, err := runSiteMutation(t, devCtx, eng, "updateSiteStoreBinding", map[string]any{
		"siteId":  siteId,
		"storeId": storeId,
	}); err != nil {
		t.Fatalf("a developer was refused binding their own storefront to a store they can read: %v", err)
	}
	binding, _ := latestPayload(t, devCtx, db, conceptPlatformSite, storedSite)["binding"].(map[string]any)
	if got := stringFromAny(binding["storeId"]); got != storeId {
		t.Fatalf("binding.storeId = %q, want %q", got, storeId)
	}
}

// TestAPreviewBindingNeedsAStoreTheCallerCanRead is the serving binding's
// readability rule, held on the preview binding (Connect Shopify 009). The
// preview guard reads the store as the deployment to learn whether it is a
// development store, which answers for every caller, so it never asked whether
// THIS caller could read it -- and a raw insert() meets no construct's
// capability. A writer sits below v1:shopify:store's developer read floor.
// The writer is a member of the site's organization and holds the store part
// there, so the organization boundary (memql#5598) admits the write and the
// refusal can only be the readability one.
func TestAPreviewBindingNeedsAStoreTheCallerCanRead(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("previewread")
	storeId := seedStore(t, eng, "store-dev-"+suffix, true)
	installSiteOrganizationCapabilities(t, auth.VerbResource{Verb: storeBindingCapability.Verb, Resource: storeBindingCapability.Resource})
	writer := "user-previewread-" + suffix
	accountId := "account-previewread-" + suffix
	writerCtx := siteOrganizationMemberCtx(t, eng, writer, accountId)
	siteId := "site-previewread-" + suffix
	if _, err := createSiteRaw(t, writerCtx, eng, map[string]any{
		"siteId":    siteId,
		"accountId": accountId,
		"hostname":  "previewread-" + suffix + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/" + siteId + "/v1/",
	}); err != nil {
		t.Fatalf("the writer's storefront: %v", err)
	}

	err := rawSiteWrite(writerCtx, eng, siteId, map[string]any{"previewBinding": map[string]any{"storeId": storeId}})
	if err == nil {
		t.Fatal("a writer pointed their storefront's preview at a store they cannot read")
	}
	if !strings.Contains(err.Error(), storeId) || !strings.Contains(err.Error(), "not a store this caller can read") {
		t.Errorf("the refusal is not the readability refusal naming the store: %v", err)
	}

	// Only the CHANGE is judged. Once a cluster owner has pointed the preview,
	// the writer's own later writes inherit it through the read-merge, and
	// re-judging it would lock them out of their own site.
	if err := rawSiteWrite(auth.ContextWithInternalOrigin(systemSiteCtx()), eng, siteId,
		map[string]any{"previewBinding": map[string]any{"storeId": storeId}}); err != nil {
		t.Fatalf("point the preview as the deployment: %v", err)
	}
	if err := rawSiteWrite(writerCtx, eng, siteId, map[string]any{"title": "renamed"}); err != nil {
		t.Fatalf("a rename that keeps the preview binding was refused: %v", err)
	}
	// And clearing it names no store, so the writer who could not have set it
	// can still take it off their own site.
	if err := rawSiteWrite(writerCtx, eng, siteId, map[string]any{"previewBinding": map[string]any{"storeId": ""}}); err != nil {
		t.Fatalf("clearing the preview binding was refused: %v", err)
	}
}

// TestAPreviewBindingChangeHonoursADenyOfTheStorePart holds the store part on
// the preview binding at the write seam, where a raw insert() meets it
// (Connect Shopify 009). The catalog gives developer the part, as the seed
// does (Connect Shopify 003), so the refusal is the per-person DENY and not
// the role: the developer without one, on the same catalog, is the control.
//
// The organization boundary (memql#5598) honours the same deny and refuses
// the raw write before this seam's check is reached, so the check is also
// asked directly: it does not depend on the boundary.
func TestAPreviewBindingChangeHonoursADenyOfTheStorePart(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	storePart := auth.VerbResource{Verb: storeBindingCapability.Verb, Resource: storeBindingCapability.Resource}
	installSiteOrganizationCapabilities(t, storePart)
	eng.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetGrantSource(nil)
		auth.SetMembershipSource(nil)
	})

	suffix := uniqueSuffix("previewdeny")
	storeId := seedStore(t, eng, "store-dev-"+suffix, true)
	previewAs := func(dev string) error {
		seedPrincipal(t, eng, dev, auth.RoleDeveloper)
		ctx := userSiteCtx(dev)
		siteId := "site-" + dev
		if _, err := createSiteRaw(t, ctx, eng, map[string]any{
			"siteId":    siteId,
			"hostname":  dev + "." + siteTestDomain,
			"kind":      "shopify_storefront",
			"bundleRef": "blob://sites/" + siteId + "/v1/",
		}); err != nil {
			t.Fatalf("%s's storefront: %v", dev, err)
		}
		return rawSiteWrite(ctx, eng, siteId, map[string]any{"previewBinding": map[string]any{"storeId": storeId}})
	}

	if err := previewAs("allowed-" + suffix); err != nil {
		t.Fatalf("a developer holding the store part was refused a preview binding: %v", err)
	}
	denied := "denied-" + suffix
	writeGrant(t, eng, auth.SubjectKindUser, denied, storePart.Verb, storePart.Resource, auth.GrantDeny)
	if err := previewAs(denied); err == nil {
		t.Fatal("a raw insert pointed a preview binding for a developer denied the store part")
	} else if !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Errorf("the refusal is not the capability refusal: %v", err)
	}

	seamAs := func(dev string) error {
		return eng.validateSitePreviewBindingChange(userSiteCtx(dev),
			map[string]any{"previewBinding": map[string]any{"storeId": storeId}}, "", dev, eng.canReadStore)
	}
	if err := seamAs("allowed-" + suffix); err != nil {
		t.Fatalf("the seam refused a developer holding the store part: %v", err)
	}
	if err := seamAs(denied); err == nil {
		t.Fatal("the seam admitted a developer denied the store part")
	} else if !strings.Contains(err.Error(), CodeCapabilityNotHeld) {
		t.Errorf("the seam's refusal is not the capability refusal: %v", err)
	}
}

// seedStore registers a v1:shopify:store the way a cluster owner does.
func seedStore(t *testing.T, eng *MemQLEngine, storeId string, development bool) string {
	t.Helper()
	if _, err := runSiteMutation(t, systemSiteCtx(), eng, "createStore", map[string]any{
		"storeId":       storeId,
		"domain":        storeId + ".myshopify.com",
		"isDevelopment": development,
	}); err != nil {
		t.Fatalf("seed store %s: %v", storeId, err)
	}
	return storeId
}

// rawSiteWrite is a raw insert() onto a site id: the path that names no
// construct, so no construct's @requiresCapability is asked.
func rawSiteWrite(ctx context.Context, eng *MemQLEngine, siteId string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = eng.Execute(ctx, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(conceptPlatformSite), langparser.QuoteString(siteId), string(b)))
	return err
}
