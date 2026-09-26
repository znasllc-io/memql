package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// Automatic publication borrows the source owner's identity without borrowing
// their cluster rank. Updating bytes must keep an already-authorized store
// binding, while actual attachment still requires reading the destination store.
func TestStorefrontPublicationPreservesAnUnreadableExistingBinding(t *testing.T) {
	installSiteOrganizationCapabilities(t,
		auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/deploy"},
		auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/store"})
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	suffix := uniqueSuffix("boundpub")
	owner, account := "owner-"+suffix, "account-"+suffix
	ownerCtx := siteOrganizationMemberCtx(t, eng, owner, account)
	borrowed := auth.ContextWithUserActor(context.Background(), owner)
	storeA := seedStore(t, eng, "store-a-"+suffix, true)
	storeB := seedStore(t, eng, "store-b-"+suffix, true)
	siteID, err := createSiteRaw(t, ownerCtx, eng, map[string]any{
		"siteId": "site-" + suffix, "accountId": account,
		"hostname": "shop-" + suffix + "." + siteTestDomain,
		"kind":     "shopify_storefront", "bundleRef": "blob://sites/test/old/",
	})
	if err != nil {
		t.Fatalf("create the member's storefront: %v", err)
	}
	seeder := auth.ContextWithInternalOrigin(systemSiteCtx())
	for _, mutation := range []string{"updateSiteStoreBinding", "updateSitePreviewBinding"} {
		if _, err := runSiteMutation(t, seeder, eng, mutation, map[string]any{"siteId": siteID, "storeId": storeA}); err != nil {
			t.Fatalf("authorize the existing %s: %v", mutation, err)
		}
	}
	if readable, err := eng.canReadStore(borrowed, storeA); err != nil || readable {
		t.Fatalf("precondition: the borrowed actor must not read stores: readable=%v err=%v", readable, err)
	}
	stored := func() map[string]any {
		return latestPayload(t, context.Background(), db, conceptPlatformSite, siteID)
	}
	assertBindings := func(t *testing.T) {
		t.Helper()
		row := stored()
		for _, field := range []string{"binding", "previewBinding"} {
			binding, _ := row[field].(map[string]any)
			if got := BareShortId(stringFromAny(binding[bindingStoreIdKey])); got != storeA {
				t.Fatalf("%s changed to %q, want existing store %q", field, got, storeA)
			}
		}
		if !sameRowAuthzOwner(stringFromAny(row["ownerUserId"]), owner) {
			t.Fatal("publication changed the site's owner")
		}
	}
	refusedUnreadable := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "not a store this caller can read") || !strings.Contains(err.Error(), storeB) {
			t.Fatalf("expected the destination store-read refusal, got %v", err)
		}
	}

	t.Run("bundle update inherits both bindings without reading the store", func(t *testing.T) {
		if _, err := runSiteMutation(t, borrowed, eng, "updateSiteBundle", map[string]any{
			"siteId": siteID, "bundleRef": "blob://sites/test/new/",
		}); err != nil {
			t.Fatalf("publishing bytes with unchanged store bindings: %v", err)
		}
		if got := stringFromAny(stored()["bundleRef"]); got != "blob://sites/test/new/" {
			t.Fatalf("published bundle = %q", got)
		}
		assertBindings(t)
	})
	t.Run("changed binding still needs destination store access", func(t *testing.T) {
		refusedUnreadable(t, rawSiteWrite(borrowed, eng, siteID, map[string]any{
			"binding": map[string]any{bindingStoreIdKey: storeB},
		}))
		assertBindings(t)
	})
	t.Run("new binding still needs destination store access", func(t *testing.T) {
		_, err := createSiteRaw(t, borrowed, eng, map[string]any{
			"siteId": "new-" + suffix, "accountId": account,
			"hostname": "new-" + suffix + "." + siteTestDomain,
			"kind":     "shopify_storefront", "bundleRef": "blob://sites/test/new/",
			"binding": map[string]any{bindingStoreIdKey: storeB},
		})
		refusedUnreadable(t, err)
	})
	t.Run("before-write changes are checked against the final row", func(t *testing.T) {
		hookSiteWritesOf(t, eng, owner, func(row map[string]any) {
			row["binding"] = map[string]any{bindingStoreIdKey: storeB}
		})
		_, err := runSiteMutation(t, borrowed, eng, "updateSiteBundle", map[string]any{
			"siteId": siteID, "bundleRef": "blob://sites/test/hooked/",
		})
		refusedUnreadable(t, err)
		assertBindings(t)
		if got := stringFromAny(stored()["bundleRef"]); got == "blob://sites/test/hooked/" {
			t.Fatal("the refused hook still published its bundle")
		}
	})
}
