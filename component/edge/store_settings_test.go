package edge

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// Settings that belong to ONE STORE travel with the store the edge chooses
// (memql#5602). A storefront's runtime settings carried values that are one
// store's -- the Customer Account API client of one store's Headless channel,
// the wholesale adapter configured for one store -- and the Testing
// destination swapped the store while every destination read the same
// settings, so a Testing session handed the testing store the LIVE store's
// client id, which it refuses.
//
// The fix keeps design D7 literal: nothing here asks "is this a preview". The
// site row keeps per-store settings keyed by store id, and settingsForSite
// merges in the map of whichever store the in-force binding names. Under a
// grant, previewSite has already substituted the preview binding, so the
// testing store's values follow with no code that knows why.

// storeSettingsSite is a storefront bound to "acme" for Production and to
// "acme-dev" for Testing, with values for both stores and site-level values
// that belong to neither.
func storeSettingsSite() *Site {
	site := previewSiteRow(siteStatusLiveValue)
	site.Kind = storefrontKind
	site.Settings = map[string]string{"apiBase": "https://api.example.com", "region": "eu"}
	site.StoreSettings = map[string]map[string]string{
		"acme":     {"customerAccountClientId": "live-client", "wholesaleAdapter": "shopifyB2B"},
		"acme-dev": {"customerAccountClientId": "dev-client", "wholesaleAdapter": "customerTag", "region": "us"},
	}
	return site
}

// servedSettings serves /runtime-config.json for host through h and returns
// the document's settings object.
func servedSettings(t *testing.T, h *Handler, host, token string) map[string]string {
	t.Helper()
	rec := getWithToken(h, &Site{Hostname: host}, runtimeConfigPath, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s on %s = %d: %s", runtimeConfigPath, host, rec.Code, rec.Body.String())
	}
	var doc struct {
		Settings map[string]string `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("runtime-config is not JSON: %v", err)
	}
	return doc.Settings
}

func TestAStoreSettingTravelsWithTheStoreTheEdgeChooses(t *testing.T) {
	original := storeSettingsSite()
	ex := &destinationExec{
		stubExec: &stubExec{rows: map[string]*Site{original.Hostname: original}},
		stores:   map[string]*BoundStore{"acme": original.Store, "acme-dev": original.PreviewStore},
	}
	resolver := NewResolver(ex, time.Hour)
	exec := newStubPreviewExec()
	h := previewHandler(original, exec)
	h.resolver = resolver
	token := exec.issue(t, PreviewGrant{ID: "g-settings", SiteID: original.ID, CandidateRef: original.BundleRef})
	testingHost := frontdoor.StorefrontTestingHost(original.Hostname)

	production := servedSettings(t, h, original.Hostname, "")
	testing := servedSettings(t, h, testingHost, token)

	for key, want := range map[string]string{
		"customerAccountClientId": "live-client",
		"wholesaleAdapter":        "shopifyB2B",
		"apiBase":                 "https://api.example.com",
		"region":                  "eu",
	} {
		if production[key] != want {
			t.Errorf("Production settings[%s] = %q, want %q (the live store's, or the site's own): %v", key, production[key], want, production)
		}
	}
	for key, want := range map[string]string{
		"customerAccountClientId": "dev-client",
		"wholesaleAdapter":        "customerTag",
		"apiBase":                 "https://api.example.com",
		// A store's value overrides the site's for that store, and only there.
		"region": "us",
	} {
		if testing[key] != want {
			t.Errorf("Testing settings[%s] = %q, want %q (the testing store's, or the site's own): %v", key, testing[key], want, testing)
		}
	}

	// A VALID TESTING COOKIE CHANGES NOTHING ON PRODUCTION: the grant is not
	// what chooses the values, the destination's binding is.
	if again := servedSettings(t, h, original.Hostname, token); again["customerAccountClientId"] != "live-client" {
		t.Errorf("a testing cookie moved Production's per-store values: %v", again)
	}
	// AND THE CACHED SITE WAS NOT WRITTEN THROUGH: the merge works on a copy.
	if original.Settings["customerAccountClientId"] != "" || len(original.Settings) != 2 {
		t.Errorf("serving merged per-store values into the cached site's settings: %v", original.Settings)
	}
}

// AN UNBOUND DESTINATION GETS NO STORE'S VALUES. Testing with no store
// attached is design preview, and it must not inherit Production's client
// any more than it inherits Production's store.
func TestAnUnboundDestinationGetsOnlyTheSitesOwnSettings(t *testing.T) {
	site := storeSettingsSite()
	site.StorefrontTesting = true
	site.PreviewBinding, site.PreviewStore = nil, nil
	got := settingsForSite(previewSite(site, &PreviewGrant{ID: "g"}))
	if got["customerAccountClientId"] != "" || got["wholesaleAdapter"] != "" {
		t.Fatalf("an unbound destination inherited a store's values: %v", got)
	}
	if got["apiBase"] != "https://api.example.com" || got["region"] != "eu" {
		t.Fatalf("an unbound destination lost the site's own settings: %v", got)
	}
}

// ONE STORE, ONE SET OF VALUES. Both destinations may select the same store,
// and then they get the same values -- they are that store's.
func TestBothDestinationsOnOneStoreGetThatStoresValues(t *testing.T) {
	site := storeSettingsSite()
	site.PreviewBinding, site.PreviewStore = site.Binding, site.Store
	production := settingsForSite(site)
	site.StorefrontTesting = true
	testing := settingsForSite(previewSite(site, &PreviewGrant{ID: "g"}))
	if production["customerAccountClientId"] != "live-client" || testing["customerAccountClientId"] != "live-client" {
		t.Fatalf("one store served two sets of values: production %v, testing %v", production, testing)
	}
}

// KIND IS THE GATE, as it is for the storefront block: a binding left on a spa
// row resolves no store's values.
func TestStoreSettingsApplyOnlyToAStorefront(t *testing.T) {
	site := storeSettingsSite()
	site.Kind = "spa"
	if got := settingsForSite(site); got["customerAccountClientId"] != "" {
		t.Fatalf("a spa row was served a store's values: %v", got)
	}
}

// THE BINDING'S STORE ID IS MATCHED BARE. A binding naming the store by its
// canonical id still finds the values its bare id keys.
func TestStoreSettingsMatchTheBindingByBareId(t *testing.T) {
	site := storeSettingsSite()
	site.Binding = map[string]any{"storeId": "v1:shopify:store:acme"}
	if got := settingsForSite(site); got["customerAccountClientId"] != "live-client" {
		t.Fatalf("a canonical binding id missed the store's values: %v", got)
	}
}

// THE PROJECTION KEEPS ONLY STRINGS, for rowStringMap's reason: the guard
// admits nothing else, so a non-string is a raw write that bypassed it, and
// it is dropped rather than coerced. A store entry that is not an object is
// dropped whole.
func TestSiteFromRowProjectsOnlyStringStoreSettings(t *testing.T) {
	site := siteFromRow(map[string]any{
		"id": "v1:platform:site:s",
		"storeSettings": map[string]any{
			"acme":    map[string]any{"customerAccountClientId": "live-client", "count": float64(3)},
			"broken":  "not an object",
			"acme-dv": map[string]any{},
		},
	})
	if got := site.StoreSettings["acme"]; len(got) != 1 || got["customerAccountClientId"] != "live-client" {
		t.Errorf("acme = %v, want only its string value", got)
	}
	if _, ok := site.StoreSettings["broken"]; ok {
		t.Error("a non-object store entry survived the projection")
	}
	if site := siteFromRow(map[string]any{"id": "s"}); site.StoreSettings == nil {
		t.Error("an absent storeSettings projected nil; it must be an empty map")
	}
}
