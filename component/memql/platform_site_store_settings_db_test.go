package memql

import (
	"strings"
	"testing"
)

// platform_site_store_settings_db_test.go -- memql#5602, the half of per-store
// runtime settings that has to run against a real engine.
//
// TestSiteStoreSettingsGuard drives the guard directly, which would keep
// passing if executeWrite stopped calling it; this file proves it is reached
// through the real updateSiteStoreSettings mutation. It also proves the two
// facts only the write and read paths can state: that the write REPLACES, and
// that `storeSettings` reaches a reader at all -- through siteById, which the
// OS reads, and through siteByHostname under the edge's own synthetic actor,
// which is the read component/edge projects with siteFromRow. A concept field
// nothing projects reaches nobody.
//
// Postgres-gated like its neighbours; MEMQL_REQUIRE_DB=1 turns the skip into a
// failure.

// storeSettingsOf reads storeSettings back through siteFull, as the OS does.
func storeSettingsOf(t *testing.T, eng *MemQLEngine, ctxUser, siteId string) map[string]any {
	t.Helper()
	res, err := eng.Execute(userSiteCtx(ctxUser), `query siteById(siteId: "`+siteId+`")`)
	if err != nil {
		t.Fatalf("read the deployable back: %v", err)
	}
	rows := MaterializeRows(res)
	if len(rows) == 0 {
		t.Fatalf("the deployable %q did not come back for %q", siteId, ctxUser)
	}
	out, _ := rows[0]["storeSettings"].(map[string]any)
	return out
}

func TestSiteStoreSettingsRoundTripAndReachTheEdgesRead(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("store-settings")
	owner := "user-store-set-" + suffix
	id := seedSettingsSite(t, eng, suffix, owner)
	hostname := "set" + suffix + "." + siteTestDomain

	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteStoreSettings", map[string]any{
		"siteId": id,
		"storeSettings": map[string]any{
			"acme":     map[string]any{"customerAccountClientId": "live-client", "wholesaleAdapter": "shopifyB2B"},
			"acme-dev": map[string]any{"customerAccountClientId": "dev-client"},
		},
	}); err != nil {
		t.Fatalf("an owner writing their own deployable's per-store settings must be admitted: %v", err)
	}

	got := storeSettingsOf(t, eng, owner, id)
	acme, _ := got["acme"].(map[string]any)
	dev, _ := got["acme-dev"].(map[string]any)
	if acme["customerAccountClientId"] != "live-client" || acme["wholesaleAdapter"] != "shopifyB2B" || dev["customerAccountClientId"] != "dev-client" {
		t.Fatalf("storeSettings through siteFull = %v, want both stores' values", got)
	}

	// THE EDGE'S READ: siteByHostname under a synthetic cluster owner, the
	// same query and the same shape component/edge's SiteByHostname issues.
	res, err := eng.Execute(systemSiteCtx(), `query siteByHostname(hostname: "`+hostname+`")`)
	if err != nil {
		t.Fatalf("siteByHostname: %v", err)
	}
	rows := MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("siteByHostname returned %d rows for a draft... want 1 (the edge resolves drafts too)", len(rows))
	}
	edgeView, _ := rows[0]["storeSettings"].(map[string]any)
	if fromEdge, _ := edgeView["acme-dev"].(map[string]any); fromEdge["customerAccountClientId"] != "dev-client" {
		t.Fatalf("the edge's read does not carry storeSettings: %v", rows[0])
	}

	// AN UNRELATED WRITE INHERITS THEM, and so does a settings edit: the two
	// objects are written by two mutations and neither clobbers the other.
	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteSettings", map[string]any{
		"siteId": id, "settings": map[string]any{"apiBase": "https://api.example"},
	}); err != nil {
		t.Fatalf("write the site's own settings: %v", err)
	}
	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteBundle", map[string]any{
		"siteId": id, "bundleRef": "blob://sites/" + id + "/v2/",
	}); err != nil {
		t.Fatalf("publish a new bundle: %v", err)
	}
	if got := storeSettingsOf(t, eng, owner, id); len(got) != 2 {
		t.Fatalf("storeSettings = %v after unrelated writes, want both stores kept", got)
	}

	// A REPLACE: a write naming one store drops the other, and {} clears.
	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteStoreSettings", map[string]any{
		"siteId": id, "storeSettings": map[string]any{"acme": map[string]any{"customerAccountClientId": "live-client-2"}},
	}); err != nil {
		t.Fatalf("the second write: %v", err)
	}
	got = storeSettingsOf(t, eng, owner, id)
	if acme, _ := got["acme"].(map[string]any); len(got) != 1 || acme["customerAccountClientId"] != "live-client-2" {
		t.Fatalf("storeSettings = %v, want exactly the store the second write named -- a merge would have kept acme-dev", got)
	}
	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteStoreSettings", map[string]any{
		"siteId": id, "storeSettings": map[string]any{},
	}); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got := storeSettingsOf(t, eng, owner, id); len(got) != 0 {
		t.Fatalf("storeSettings = %v, want empty after a write of {}", got)
	}
}

// The guard is REACHED from the mutation path, and a refused write leaves
// nothing behind.
func TestSiteStoreSettingsGuardIsReachedFromTheMutationPath(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("store-settings-refusals")
	owner := "user-store-set-" + suffix
	id := seedSettingsSite(t, eng, suffix, owner)

	for name, tc := range map[string]struct {
		storeSettings map[string]any
		says          string
	}{
		"a canonical store id":  {map[string]any{"v1:shopify:store:acme": map[string]any{"a": "b"}}, "bare id"},
		"a key ending in Ref":   {map[string]any{"acme": map[string]any{"clientSecretRef": "x"}}, "clientSecretRef"},
		"a non-string value":    {map[string]any{"acme": map[string]any{"retries": 3}}, "retries"},
		"a store that is a str": {map[string]any{"acme": "live-client"}, "acme"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteStoreSettings", map[string]any{
				"siteId": id, "storeSettings": tc.storeSettings,
			})
			if err == nil {
				t.Fatalf("%s must be refused on the real mutation path", name)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal must say %q; got %q", tc.says, err.Error())
			}
		})
	}
	if got := storeSettingsOf(t, eng, owner, id); len(got) != 0 {
		t.Errorf("storeSettings = %v, want nothing written by a refused call", got)
	}
}

// An ordinary member of another organization cannot write them, even holding
// the same app grants as the deployable's creator -- the site's own row tier
// decides, exactly as it does for settings (TestSiteSettingsCannotBeWrittenAcrossUsers).
func TestSiteStoreSettingsCannotBeWrittenAcrossOrganizations(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("store-settings-crossorg")
	owner := "user-store-set-" + suffix
	stranger := "user-store-other-" + suffix
	installSiteOrganizationCapabilities(t)
	accountID := "store-settings-account-" + suffix
	ownerCtx := siteOrganizationMemberCtx(t, eng, owner, accountID)
	strangerCtx := siteOrganizationMemberCtx(t, eng, stranger, "store-settings-stranger-account-"+suffix)
	id := "site-store-set-" + suffix
	if _, err := createSiteRaw(t, ownerCtx, eng, map[string]any{
		"siteId": id, "accountId": accountID, "hostname": "sset" + suffix + "." + siteTestDomain,
		"bundleRef": "blob://sites/" + id + "/v1/", "status": "draft",
	}); err != nil {
		t.Fatalf("seed organization deployable: %v", err)
	}

	if _, err := runSiteMutation(t, strangerCtx, eng, "updateSiteStoreSettings", map[string]any{
		"siteId": id, "storeSettings": map[string]any{"acme": map[string]any{"customerAccountClientId": "attacker"}},
	}); err == nil {
		t.Fatal("a member of another organization wrote this deployable's per-store settings")
	}
	// The reachable positive: the creator writes them through the same call.
	if _, err := runSiteMutation(t, ownerCtx, eng, "updateSiteStoreSettings", map[string]any{
		"siteId": id, "storeSettings": map[string]any{"acme": map[string]any{"customerAccountClientId": "live-client"}},
	}); err != nil {
		t.Fatalf("the deployable's own organization member was refused: %v", err)
	}
}
