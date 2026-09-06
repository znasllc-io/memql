package memql

import (
	"strings"
	"testing"
)

// platform_site_settings_db_test.go -- epic memql#4906, the half of runtime
// settings that has to run against a real engine.
//
// The unit tests beside this file drive validateSiteSettings directly. That is
// necessary and not sufficient, for the reason the custom-domain suite states:
// every one of them would keep passing if executeWrite stopped calling the
// guard, and the failure would be silent -- a settings object with a secret's
// name in it landing on a row and being served to every visitor, with the suite
// green. So the properties that matter are proven HERE, through `eng.Execute`
// on the real `updateSiteSettings` mutation.
//
// It also proves the two things only the write path can say: that the mutation
// REPLACES rather than merges (the read-merge would otherwise make clearing a
// setting inexpressible), and that `settings` reaches a reader at all -- a
// concept field is not a readable field, and `siteFull` projecting it is what
// puts it in front of the edge and the OS.
//
// Postgres-gated like its neighbours. CI's db-tests lane runs this package with
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

// seedSettingsSite creates a deployable owned by the given caller, at a
// hostname under the test domain the hostname policy admits.
func seedSettingsSite(t *testing.T, eng *MemQLEngine, suffix, owner string) string {
	t.Helper()
	id := "site-set-" + suffix
	if _, err := createSiteRaw(t, userSiteCtx(owner), eng, map[string]any{
		"siteId":    id,
		"hostname":  "set" + suffix + "." + siteTestDomain,
		"bundleRef": "blob://sites/" + id + "/v1/",
		"status":    "draft",
	}); err != nil {
		t.Fatalf("could not seed the deployable: %v", err)
	}
	return id
}

// settingsOf reads the row back through the SHAPE a client reads it through --
// `siteById` projecting `siteFull` -- rather than through the raw row. That is
// the assertion worth making: a concept field nothing projects reaches nobody,
// and the edge and the OS both read this through the shape.
func settingsOf(t *testing.T, eng *MemQLEngine, ctxUser, siteId string) map[string]any {
	t.Helper()
	res, err := eng.Execute(userSiteCtx(ctxUser), `query siteById(siteId: "`+siteId+`")`)
	if err != nil {
		t.Fatalf("read the deployable back: %v", err)
	}
	rows := MaterializeRows(res)
	if len(rows) == 0 {
		t.Fatalf("the deployable %q did not come back for %q", siteId, ctxUser)
	}
	out, _ := rows[0]["settings"].(map[string]any)
	return out
}

// The whole feature, end to end on the write path: an owner writes settings,
// the row carries them, and a reader gets them through the shape the edge and
// the OS read.
func TestSiteSettingsRoundTripThroughTheMutationAndTheShape(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("settings-roundtrip")
	owner := "user-set-" + suffix
	id := seedSettingsSite(t, eng, suffix, owner)

	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteSettings", map[string]any{
		"siteId":   id,
		"settings": map[string]any{"apiBase": "https://api.eu.example", "region": "eu"},
	}); err != nil {
		t.Fatalf("an owner writing their own deployable's settings must be admitted: %v", err)
	}

	got := settingsOf(t, eng, owner, id)
	if got["apiBase"] != "https://api.eu.example" || got["region"] != "eu" {
		t.Fatalf("settings = %v, want the values written", got)
	}
}

// A REPLACE, NOT A MERGE. `update{}` is a read-merge, so a settings object
// that merged would make removing a key impossible: every "delete this
// setting" would silently re-save the value already there. The editor sends
// the map it shows, and this is what makes that true.
func TestSiteSettingsWriteReplacesRatherThanMerges(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("settings-replace")
	owner := "user-set-" + suffix
	id := seedSettingsSite(t, eng, suffix, owner)

	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteSettings", map[string]any{
		"siteId":   id,
		"settings": map[string]any{"apiBase": "https://api.example", "region": "eu", "flag": "on"},
	}); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteSettings", map[string]any{
		"siteId":   id,
		"settings": map[string]any{"apiBase": "https://api.example"},
	}); err != nil {
		t.Fatalf("the second write: %v", err)
	}

	got := settingsOf(t, eng, owner, id)
	if len(got) != 1 || got["apiBase"] != "https://api.example" {
		t.Errorf("settings = %v, want exactly the one key the second write named -- a merge would have kept region and flag", got)
	}

	// And clearing every setting is expressible, which is the same property
	// read from the other end.
	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteSettings", map[string]any{
		"siteId":   id,
		"settings": map[string]any{},
	}); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got := settingsOf(t, eng, owner, id); len(got) != 0 {
		t.Errorf("settings = %v, want empty after a write of {}", got)
	}
}

// A publish, a rename or a status flip inherits the stored settings through
// the read-merge and never has to restate them. This is the case that would
// break every ordinary write if the guard read the merged payload.
func TestSiteSettingsSurviveAnUnrelatedWrite(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("settings-inherit")
	owner := "user-set-" + suffix
	id := seedSettingsSite(t, eng, suffix, owner)

	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteSettings", map[string]any{
		"siteId":   id,
		"settings": map[string]any{"apiBase": "https://api.example"},
	}); err != nil {
		t.Fatalf("write the settings: %v", err)
	}
	if _, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteBundle", map[string]any{
		"siteId":    id,
		"bundleRef": "blob://sites/" + id + "/v2/",
	}); err != nil {
		t.Fatalf("publish a new bundle: %v", err)
	}

	if got := settingsOf(t, eng, owner, id); got["apiBase"] != "https://api.example" {
		t.Errorf("settings = %v, want the stored value to survive an unrelated write", got)
	}
}

// The guard is REACHED from the mutation path -- the property the unit tests
// cannot see. Each refusal is the one the unit suite pins, asserted here
// through `eng.Execute` so an executeWrite that stopped calling the guard
// fails rather than going quiet.
func TestSiteSettingsGuardIsReachedFromTheMutationPath(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("settings-refusals")
	owner := "user-set-" + suffix
	id := seedSettingsSite(t, eng, suffix, owner)

	for name, tc := range map[string]struct {
		settings map[string]any
		says     string
	}{
		"a malformed key":     {map[string]any{"api-base": "x"}, "api-base"},
		"a key ending in Ref": {map[string]any{"apiTokenRef": "some-secret"}, "binding"},
		"a non-string value":  {map[string]any{"retries": 3}, "retries"},
		"an over-long value":  {map[string]any{"apiBase": strings.Repeat("v", defaultSiteSettingsMaxValueLength+1)}, "MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runSiteMutation(t, userSiteCtx(owner), eng, "updateSiteSettings", map[string]any{
				"siteId": id, "settings": tc.settings,
			})
			if err == nil {
				t.Fatalf("%s must be refused on the real mutation path", name)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal must say %q; got %q", tc.says, err.Error())
			}
		})
	}

	// Nothing was written by any of them.
	if got := settingsOf(t, eng, owner, id); len(got) != 0 {
		t.Errorf("settings = %v, want nothing written by a refused call", got)
	}
}

// A systemOwned row refuses the write, and refuses it for a CLUSTER OWNER too:
// the seeded portal and OS rows are re-written at every boot, so a value set
// here would be reverted and look like it had worked until then.
func TestSiteSettingsRefusedOnASystemOwnedRow(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("settings-systemowned")
	id := "site-set-sys-" + suffix
	// Seeded as the system actor, which is the only actor that can create a
	// systemOwned row at a hostname outside the slug rule.
	if _, err := createSiteRaw(t, systemSiteCtx(), eng, map[string]any{
		"siteId":      id,
		"hostname":    "portal-set-" + suffix + "." + siteTestDomain,
		"bundleRef":   "file:///app/os",
		"status":      "live",
		"systemOwned": true,
	}); err != nil {
		t.Fatalf("seed the system-owned row: %v", err)
	}

	if _, err := runSiteMutation(t, ownerCustomDomainCtx(), eng, "updateSiteSettings", map[string]any{
		"siteId": id, "settings": map[string]any{"apiBase": "https://api.example"},
	}); err == nil {
		t.Fatal("a cluster owner must not write settings onto a system-owned row")
	}

	// The seed itself still can, which is what keeps a cluster able to
	// re-seed its own surfaces.
	if _, err := runSiteMutation(t, systemSiteCtx(), eng, "updateSiteSettings", map[string]any{
		"siteId": id, "settings": map[string]any{"apiBase": "https://api.example"},
	}); err != nil {
		t.Fatalf("the system actor must still be able to write: %v", err)
	}
}

// ROW AUTHORIZATION IS UNCHANGED BY ANY OF THIS: a second user cannot write
// another person's deployable's settings. The guard says what a settings
// object may contain; guardRowAuthzWrite says whose row it may land on, and
// this is the assertion that the new mutation did not open a door beside it.
func TestSiteSettingsCannotBeWrittenAcrossUsers(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)

	suffix := uniqueSuffix("settings-crossuser")
	owner := "user-set-" + suffix
	stranger := "user-other-" + suffix
	id := seedSettingsSite(t, eng, suffix, owner)

	if _, err := runSiteMutation(t, userSiteCtx(stranger), eng, "updateSiteSettings", map[string]any{
		"siteId": id, "settings": map[string]any{"apiBase": "https://api.attacker.example"},
	}); err == nil {
		t.Fatal("a stranger must not write another person's deployable settings")
	}
	if got := settingsOf(t, eng, owner, id); len(got) != 0 {
		t.Errorf("settings = %v, want nothing written by a refused cross-user call", got)
	}
}
