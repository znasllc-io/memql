package memql

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
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
	userCtx := userSiteCtx("user-site-shop-" + uniqueSuffix("site"))
	if _, err := createSiteRaw(t, userCtx, eng, map[string]any{
		"siteId":    "site-unreadable-" + uniqueSuffix("site"),
		"hostname":  "unreadable-" + uniqueSuffix("site") + "." + siteTestDomain,
		"kind":      "shopify_storefront",
		"bundleRef": "blob://sites/unreadable/v1/",
		"binding":   map[string]any{"storeId": storeId},
	}); err == nil {
		t.Fatal("a non-owner bound a storefront to a cluster-owner-tier store")
	} else if !strings.Contains(err.Error(), storeId) {
		t.Errorf("the refusal does not name the store it refused: %v", err)
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
