package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// organization_before_write_db_test.go -- memql#5636.
//
// The organization boundary (validateOrganizationSensitiveChanges, memql#5598)
// asks a deployable part at the site's organization for every sensitive
// change. A before-write automation edits the incoming row, and an authored
// one may set status, bundleRef, binding and previewBinding on its author's
// own writes. So the boundary has to judge the row those hooks PRODUCED: a
// person denied a part must not get the change by writing an unrelated field
// and letting their own hook add the guarded one.
//
// Each case pairs the denied person with a HOLDER running the same hook. The
// holder's write must land with the hook's value, which is what shows the
// hook ran and that the refusal is the deny rather than anything else. Where
// a direct write can make the change, the case also checks it is still
// refused with no hook installed.

// hookSiteWritesOf installs a before-write hook on v1:platform:site that
// applies edit to every update the named person makes, and removes it when the
// test ends. It stands in for an authored automation, whose hooks fire only on
// their author's own writes.
func hookSiteWritesOf(t *testing.T, eng *MemQLEngine, userID string, edit func(row map[string]any)) {
	t.Helper()
	source := "authored:test-" + userID
	eng.SetBeforeWriteHookSource(source, map[string][]BeforeWriteHook{conceptPlatformSite: {{
		Name: "hook-" + userID,
		On:   "update",
		Apply: func(ctx context.Context, row map[string]any) error {
			if ac, _ := auth.AccessFromContext(ctx); ac != nil && ac.UserId == userID {
				edit(row)
			}
			return nil
		},
	}}})
	t.Cleanup(func() { eng.SetBeforeWriteHookSource(source, nil) })
}

func TestTheOrganizationBoundaryJudgesTheRowBeforeWriteHooksProduced(t *testing.T) {
	storePart := auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/store"}
	installSiteOrganizationCapabilities(t, storePart)
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	suffix := uniqueSuffix("orgbw")
	acme := "acme-" + suffix
	if err := organizationInsert(t, eng, groupSeedCtx(), conceptAccountsAccount, acme, map[string]any{"name": acme, "status": "active", "domainStatus": "unverified"}); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	store := "store-" + suffix
	if _, err := runSiteMutation(t, systemSiteCtx(), eng, "createStore", map[string]any{
		"storeId":            store,
		"domain":             store + ".myshopify.com",
		"storefrontTokenRef": "SHOPIFY_STOREFRONT_TOKEN",
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// person seeds a principal, denying them each named part with a person
	// grant, which holds in every organization.
	person := func(t *testing.T, name string, role auth.Role, deny ...string) (string, context.Context) {
		t.Helper()
		id := name + "-" + suffix
		seedPrincipal(t, eng, id, role)
		for _, part := range deny {
			writeGrant(t, eng, auth.SubjectKindUser, id, auth.VerbExecute, "app:deployables/"+part, auth.GrantDeny)
		}
		return id, rankActorCtx(id, role)
	}
	// An empty draft, which is ordinary data: a bundleRef would ask deploy.
	site := func(t *testing.T, ctx context.Context, name, kind string) string {
		t.Helper()
		id := "site-" + name + "-" + suffix
		if _, err := createSiteRaw(t, ctx, eng, map[string]any{
			"siteId":    id,
			"accountId": acme,
			"hostname":  name + "-" + suffix + "." + siteTestDomain,
			"kind":      kind,
			"bundleRef": "",
			"status":    "draft",
		}); err != nil {
			t.Fatalf("seed site %s: %v", id, err)
		}
		return id
	}
	rename := func(t *testing.T, ctx context.Context, siteID string) error {
		return organizationInsert(t, eng, ctx, conceptPlatformSite, siteID, map[string]any{"title": "renamed"})
	}
	refusedFor := func(t *testing.T, err error, part string) {
		t.Helper()
		if err == nil {
			t.Fatalf("admitted: a person denied %s made the change it guards (memql#5636)", part)
		}
		if !strings.Contains(err.Error(), CodeCapabilityNotHeld) || !strings.Contains(err.Error(), part) {
			t.Fatalf("refused, but not by the organization boundary asking %s: %v", part, err)
		}
	}
	stored := func(t *testing.T, siteID string) map[string]any {
		return latestPayload(t, context.Background(), db, conceptPlatformSite, conceptPlatformSite+":"+siteID)
	}
	boundStore := func(row map[string]any) string {
		binding, _ := row["binding"].(map[string]any)
		return BareShortId(stringFromAny(binding[bindingStoreIdKey]))
	}

	t.Run("publish: a hook that sets status live", func(t *testing.T) {
		deniedID, denied := person(t, "pub-denied", auth.RoleDeveloper, "publish")
		holderID, holder := person(t, "pub-holder", auth.RoleDeveloper)
		deniedSite, holderSite := site(t, denied, "pub-denied", "spa"), site(t, holder, "pub-holder", "spa")

		if err := rename(t, denied, deniedSite); err != nil {
			t.Fatalf("control: a rename with no hook was refused: %v", err)
		}
		refusedFor(t, organizationInsert(t, eng, denied, conceptPlatformSite, deniedSite, map[string]any{"status": "live"}), "publish")
		goLive := func(row map[string]any) { row["status"] = "live" }
		hookSiteWritesOf(t, eng, deniedID, goLive)
		hookSiteWritesOf(t, eng, holderID, goLive)

		refusedFor(t, rename(t, denied, deniedSite), "publish")
		if got := stringFromAny(stored(t, deniedSite)["status"]); got != "draft" {
			t.Fatalf("the refused write still took the site %q", got)
		}
		if err := rename(t, holder, holderSite); err != nil {
			t.Fatalf("a person holding publish was refused the same hooked write: %v", err)
		}
		if got := stringFromAny(stored(t, holderSite)["status"]); got != "live" {
			t.Fatalf("the holder's hook did not run: status %q, want live", got)
		}
	})

	// Cluster owners, because on this tree v1:shopify:store is read by cluster
	// owners only, and the binding guard refuses a store the caller cannot
	// read before anything asks the store part.
	t.Run("store: a hook that sets the binding", func(t *testing.T) {
		deniedID, denied := person(t, "bind-denied", auth.RoleOwner, "store")
		holderID, holder := person(t, "bind-holder", auth.RoleOwner)
		deniedSite, holderSite := site(t, denied, "bind-denied", "shopify_storefront"), site(t, holder, "bind-holder", "shopify_storefront")

		refusedFor(t, organizationInsert(t, eng, denied, conceptPlatformSite, deniedSite, map[string]any{"binding": map[string]any{bindingStoreIdKey: store}}), "store")
		bind := func(row map[string]any) { row["binding"] = map[string]any{bindingStoreIdKey: store} }
		hookSiteWritesOf(t, eng, deniedID, bind)
		hookSiteWritesOf(t, eng, holderID, bind)

		refusedFor(t, rename(t, denied, deniedSite), "store")
		if got := boundStore(stored(t, deniedSite)); got != "" {
			t.Fatalf("the refused write still bound the site to %q", got)
		}
		if err := rename(t, holder, holderSite); err != nil {
			t.Fatalf("a person holding the store part was refused the same hooked write: %v", err)
		}
		if got := boundStore(stored(t, holderSite)); got != store {
			t.Fatalf("the holder's hook did not run: bound to %q, want %q", got, store)
		}
	})

	// A field write whose value is absent DELETES the field, so on an update
	// the final row lacking a field the stored row carried is a change too.
	t.Run("store: a hook that removes the binding", func(t *testing.T) {
		deniedID, denied := person(t, "unbind-denied", auth.RoleOwner, "store")
		holderID, holder := person(t, "unbind-holder", auth.RoleOwner)
		deniedSite, holderSite := site(t, denied, "unbind-denied", "shopify_storefront"), site(t, holder, "unbind-holder", "shopify_storefront")
		for _, id := range []string{deniedSite, holderSite} {
			if err := organizationInsert(t, eng, systemSiteCtx(), conceptPlatformSite, id, map[string]any{"binding": map[string]any{bindingStoreIdKey: store}}); err != nil {
				t.Fatalf("bind %s: %v", id, err)
			}
		}

		unbind := func(row map[string]any) { delete(row, "binding") }
		hookSiteWritesOf(t, eng, deniedID, unbind)
		hookSiteWritesOf(t, eng, holderID, unbind)

		refusedFor(t, rename(t, denied, deniedSite), "store")
		if got := boundStore(stored(t, deniedSite)); got != store {
			t.Fatalf("the refused write still unbound the site: bound to %q", got)
		}
		if err := rename(t, holder, holderSite); err != nil {
			t.Fatalf("a person holding the store part was refused the same hooked write: %v", err)
		}
		if got := boundStore(stored(t, holderSite)); got != "" {
			t.Fatalf("the holder's hook did not run: still bound to %q", got)
		}
	})
}
