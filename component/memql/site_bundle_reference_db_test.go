package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The rule judges the row a before-write hook produced, not the caller's delta.
// An authored hook can set a field its author's write never named, so a person
// who may deploy must not get a reference past the rule by renaming a site and
// letting their own hook add it. The holder's own reference is the control: the
// same hook landing a version of the site shows the hook ran and that the refusal
// is about the reference.
func TestABeforeWriteHookIsHeldToTheSiteVersionRule(t *testing.T) {
	installSiteOrganizationCapabilities(t, auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/deploy"})
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	suffix := uniqueSuffix("siteref")
	account := "acme-" + suffix
	if err := organizationInsert(t, eng, groupSeedCtx(), conceptAccountsAccount, account, map[string]any{"name": account, "status": "active", "domainStatus": "unverified"}); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	user := "hook-author-" + suffix
	seedPrincipal(t, eng, user, auth.RoleDeveloper)
	ctx := rankActorCtx(user, auth.RoleDeveloper)
	siteID := "site-hook-" + suffix
	if _, err := createSiteRaw(t, ctx, eng, map[string]any{
		"siteId": siteID, "accountId": account, "hostname": "hook-" + suffix + "." + siteTestDomain, "kind": "spa", "bundleRef": "", "status": "draft",
	}); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	hookRef := ""
	hookSiteWritesOf(t, eng, user, func(row map[string]any) { row["bundleRef"] = hookRef })
	rename := func() error {
		return organizationInsert(t, eng, ctx, conceptPlatformSite, siteID, map[string]any{"title": "renamed"})
	}

	hookRef = "blob://sites/somebody-else/v1/"
	if err := rename(); err == nil || !strings.Contains(err.Error(), CodeSiteVersionNotItsOwn) {
		t.Fatalf("got %v, want a refusal carrying %s", err, CodeSiteVersionNotItsOwn)
	}

	hookRef = "blob://sites/" + siteID + "/v1/"
	if err := rename(); err != nil {
		t.Fatalf("the hook naming the site's own version was refused: %v", err)
	}
	row := latestPayload(t, t.Context(), db, conceptPlatformSite, conceptPlatformSite+":"+siteID)
	if row["bundleRef"] != hookRef {
		t.Fatalf("bundleRef is %v, want the hook's %q", row["bundleRef"], hookRef)
	}
}
