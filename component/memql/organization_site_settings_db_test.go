package memql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// organization_site_settings_db_test.go -- the organization boundary
// (validateOrganizationSensitiveChanges) asked a deployable part for status,
// deletion, the serving version, the candidate and the two store bindings, and
// for nothing else. A member of the site's organization who holds Edit on Data
// and no part could therefore rewrite every OTHER field of a live site with a
// plain write: switch its shopper forms off (reviews and signups answer 404),
// replace its runtime settings (a storefront with no API version loads no
// products), change its kind, hostname or resolution tail. memQL OS draws the
// runtime editor only for a publish holder; the engine did not agree with it.
//
// Each case is a member of the site's organization denied `publish`, writing one
// field directly, the way a hand-made request would. The holder case and the
// rename case are the controls: the boundary must still admit a publish holder,
// and a label is not a serving change.
func TestASiteFieldThatChangesWhatVisitorsGetAsksPublish(t *testing.T) {
	installSiteOrganizationCapabilities(t, auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/deploy"})
	eng, db, _ := sharedReadMergeEngine(t)
	t.Setenv(memqlDomainEnv, siteTestDomain)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	suffix := uniqueSuffix("sitepub")
	account := "acct-" + suffix
	holderID, deniedID := "holder-"+suffix, "member-"+suffix
	holder := siteOrganizationMemberCtx(t, eng, holderID, account)
	member := siteOrganizationMemberCtx(t, eng, deniedID, account)
	writeGrant(t, eng, auth.SubjectKindUser, deniedID, auth.VerbExecute, "app:deployables/publish", auth.GrantDeny)

	site := func(t *testing.T, name string) string {
		t.Helper()
		id := "site-" + name + "-" + suffix
		if _, err := createSiteRaw(t, holder, eng, map[string]any{
			"siteId":    id,
			"accountId": account,
			"hostname":  name + "-" + suffix + "." + siteTestDomain,
			"kind":      "spa",
			"bundleRef": "",
			"status":    "draft",
		}); err != nil {
			t.Fatalf("seed site %s: %v", id, err)
		}
		return id
	}
	stored := func(t *testing.T, siteID string) map[string]any {
		return latestPayload(t, holder, db, conceptPlatformSite, conceptPlatformSite+":"+siteID)
	}

	cases := []struct {
		field string
		value any
	}{
		{"shopperForms", true},
		{"settings", map[string]any{"apiBase": "https://example.test"}},
		{"storeSettings", map[string]any{"teststore": map[string]any{"apiVersion": "2025-01"}}},
		{"resolutionTail", "not_found"},
		{"kind", "static"},
		{"apiProxy", true},
		{"extensionRuntimePath", "/editor/"},
		{"hostname", "moved-" + suffix + "." + siteTestDomain},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			siteID := site(t, "f-"+strings.ToLower(c.field))
			before := stored(t, siteID)[c.field]
			err := organizationInsert(t, eng, member, conceptPlatformSite, siteID, map[string]any{c.field: c.value})
			if err == nil {
				t.Fatalf("admitted: a member denied publish changed %s on the site", c.field)
			}
			if !strings.Contains(err.Error(), CodeCapabilityNotHeld) || !strings.Contains(err.Error(), "publish") {
				t.Fatalf("refused, but not by the organization boundary asking publish: %v", err)
			}
			if after := stored(t, siteID)[c.field]; !reflect.DeepEqual(after, before) {
				t.Fatalf("the refused write still changed %s: %v -> %v", c.field, before, after)
			}
		})
	}

	t.Run("control: a publish holder changes the settings", func(t *testing.T) {
		siteID := site(t, "holder")
		want := map[string]any{"apiBase": "https://example.test"}
		if err := organizationInsert(t, eng, holder, conceptPlatformSite, siteID, map[string]any{"settings": want}); err != nil {
			t.Fatalf("a publish holder was refused the settings: %v", err)
		}
		if got := stored(t, siteID)["settings"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("settings = %v, want %v", got, want)
		}
	})
	t.Run("control: a member without publish still renames the site", func(t *testing.T) {
		siteID := site(t, "rename")
		if err := organizationInsert(t, eng, member, conceptPlatformSite, siteID, map[string]any{"title": "renamed", "notes": "a note"}); err != nil {
			t.Fatalf("a label change was refused: %v", err)
		}
	})
}
