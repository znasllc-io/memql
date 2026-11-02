package memql

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/frontdoor"
)

// TestSiteNamesItsOwnBundle is the rule over values. The end-to-end proof, a row
// written through the engine and read back through the edge, is
// app/site_version_scope_db_test.go.
func TestSiteNamesItsOwnBundle(t *testing.T) {
	const canonical = "v1:platform:site:shop-1"
	for _, tc := range []struct {
		name   string
		siteID string
		ref    string
		want   bool
	}{
		// What the publisher writes, for each spelling of the id it may hold,
		// judged from each spelling of the id the caller may hold.
		{"a version under the bare id, from the bare id", "shop-1", "blob://sites/shop-1/v0123456789ab/", true},
		{"a version under the bare id, from the canonical id", canonical, "blob://sites/shop-1/v0123456789ab/", true},
		{"a version under the canonical id, from the canonical id", canonical, "blob://sites/" + canonical + "/v0123456789ab/", true},
		{"a version under the canonical id, from the bare id", "shop-1", "blob://sites/" + canonical + "/v0123456789ab/", true},
		{"a uuid id", "v1:platform:site:6f1c2d3e-0000-4000-8000-000000000001", "blob://sites/v1:platform:site:6f1c2d3e-0000-4000-8000-000000000001/v0123456789ab/", true},
		{"the placeholder a new deployable holds", "shop-1", "blob://sites/shop-1/pending/", true},
		{"a version with no trailing slash, which the opener adds", "shop-1", "blob://sites/shop-1/v1", true},
		{"nothing published yet", "shop-1", "", true},
		{"nothing published yet, on a row with no id yet", "", "", true},

		{"the shell's directory, on the shell", "v1:platform:site:" + frontdoor.OsSite, "file:///app/os", true},
		{"the shell's directory, from its bare id", frontdoor.OsSite, "file:///app/os", true},
		{"the extension page's directory, on the extension page", "v1:platform:site:" + frontdoor.VSCodeSite, "file:///app/vscode-site", true},
		{"the shell publishing like anybody else", frontdoor.OsSite, "blob://sites/os/v0123456789ab/", true},

		{"another site's version", "shop-1", "blob://sites/other/v0123456789ab/", false},
		{"another site's version, canonically", "shop-1", "blob://sites/v1:platform:site:other/v0123456789ab/", false},
		{"an id the site's own is a prefix of", "shop-1", "blob://sites/shop-1x/v1/", false},
		{"an id that is a prefix of the site's own", "shop-1", "blob://sites/shop-/v1/", false},
		{"its id under another concept's prefix", "shop-1", "blob://sites/v1:other:thing:shop-1/v1/", false},
		{"its id in another case", "shop-1", "blob://sites/SHOP-1/v1/", false},
		{"a prefix no site publishes under", "shop-1", "blob://elsewhere/shop-1/v1/", false},
		{"no prefix at all", "shop-1", "blob://", false},
		{"its prefix with no version", "shop-1", "blob://sites/shop-1/", false},
		{"a path below a version", "shop-1", "blob://sites/shop-1/v1/nested/", false},
		{"a version of dots", "shop-1", "blob://sites/shop-1/../", false},
		{"a path that leaves its prefix", "shop-1", "blob://sites/shop-1/../other/v1/", false},
		{"an empty version segment", "shop-1", "blob://sites/shop-1//", false},
		{"an escaped segment", "shop-1", "blob://sites/shop-1/%2e%2e/", false},
		{"a query on the version", "shop-1", "blob://sites/shop-1/v1?x=1", false},

		{"a directory, on an ordinary site", "shop-1", "file:///srv/site", false},
		{"the shell's directory, on an ordinary site", "shop-1", "file:///app/os", false},
		{"another directory, on the shell", frontdoor.OsSite, "file:///srv/site", false},
		{"a directory inside the shell's", frontdoor.OsSite, "file:///app/os/assets", false},
		{"the parent of the shell's directory", frontdoor.OsSite, "file:///app", false},
		{"another platform site's directory", frontdoor.OsSite, "file:///app/vscode-site", false},
		{"a file reference naming nothing", frontdoor.OsSite, "file://", false},

		{"no scheme", "shop-1", "sites/shop-1/v1/", false},
		{"another scheme", "shop-1", "https://example.test/sites/shop-1/v1/", false},
		{"a scheme in another case", "shop-1", "BLOB://sites/shop-1/v1/", false},
		{"padding around a reference", "shop-1", " blob://sites/shop-1/v1/", false},

		{"a row with no id", "", "blob://sites//v1/", false},
		{"an id of two segments", "shop-1/v1", "blob://sites/shop-1/v1/v2/", false},
		{"an id of two segments, with the shorter prefix", "shop-1/v1", "blob://sites/shop-1/v1/", false},
		{"an id of dots", "..", "blob://sites/../v1/", false},
		{"an id of one dot", ".", "blob://sites/./v1/", false},
		{"an id ending in a dot", "shop-1.", "blob://sites/shop-1./v1/", false},
		{"an id beginning with a dot", ".shop-1", "blob://sites/.shop-1/v1/", false},
		{"an id with a backward slash", `shop\1`, `blob://sites/shop\1/v1/`, false},
		{"an id with an escape", "shop%2f1", "blob://sites/shop%2f1/v1/", false},
		{"an id with a space", "shop 1", "blob://sites/shop 1/v1/", false},
		{"a version ending in a dot", "shop-1", "blob://sites/shop-1/v1./", false},
		{"a version with a backward slash", "shop-1", `blob://sites/shop-1/v\1/`, false},
		{"a dot inside an id", "shop.1", "blob://sites/shop.1/v1/", true},
		{"an underscore at the end of an id", "shop_", "blob://sites/shop_/v1/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SiteNamesItsOwnBundle(tc.siteID, tc.ref); got != tc.want {
				t.Fatalf("SiteNamesItsOwnBundle(%q, %q) = %v, want %v", tc.siteID, tc.ref, got, tc.want)
			}
		})
	}
}

func TestValidateSiteBundleRefs(t *testing.T) {
	const id = "v1:platform:site:shop-1"
	const own, older = "blob://sites/shop-1/v2/", "blob://sites/shop-1/v1/"
	const foreign = "blob://sites/other/v1/"

	for _, tc := range []struct {
		name        string
		id          string
		prior, next map[string]any
		wantCode    string
	}{
		{name: "a new site with its own placeholder", id: id, next: map[string]any{"bundleRef": "blob://sites/shop-1/pending/"}},
		{name: "a new site with none", id: id, next: map[string]any{"bundleRef": ""}},
		{name: "a new site naming another's version", id: id, next: map[string]any{"bundleRef": foreign}, wantCode: CodeSiteVersionNotItsOwn},
		{name: "a new site with a candidate of another's", id: id, next: map[string]any{"bundleRef": own, "candidateRef": foreign}, wantCode: CodeSiteVersionNotItsOwn},
		{name: "a new row with no id yet naming a version", next: map[string]any{"bundleRef": own}, wantCode: CodeSiteVersionNotItsOwn},
		{name: "a new site whose id is two segments", id: "v1:platform:site:shop-1/v1", next: map[string]any{"bundleRef": ""}, wantCode: CodeSiteIDNotOneSegment},
		{name: "a new site whose id is a parent segment", id: "..", next: map[string]any{"bundleRef": ""}, wantCode: CodeSiteIDNotOneSegment},
		{name: "a new site whose id ends in a dot", id: "v1:platform:site:shop-1.", next: map[string]any{"bundleRef": ""}, wantCode: CodeSiteIDNotOneSegment},
		{name: "a new site whose id holds a backward slash", id: `shop\1`, next: map[string]any{"bundleRef": ""}, wantCode: CodeSiteIDNotOneSegment},

		{name: "a publish", id: id, prior: map[string]any{"bundleRef": older}, next: map[string]any{"bundleRef": own}},
		{name: "a rollback", id: id, prior: map[string]any{"bundleRef": own}, next: map[string]any{"bundleRef": older}},
		{name: "a candidate beside what serves", id: id, prior: map[string]any{"bundleRef": older}, next: map[string]any{"bundleRef": older, "candidateRef": own}},
		{name: "a promotion", id: id, prior: map[string]any{"bundleRef": older, "candidateRef": own}, next: map[string]any{"bundleRef": own, "candidateRef": ""}},
		{name: "clearing a candidate", id: id, prior: map[string]any{"bundleRef": older, "candidateRef": own}, next: map[string]any{"bundleRef": older, "candidateRef": ""}},
		{name: "pointing at another's version", id: id, prior: map[string]any{"bundleRef": older}, next: map[string]any{"bundleRef": foreign}, wantCode: CodeSiteVersionNotItsOwn},
		{name: "a candidate of another's", id: id, prior: map[string]any{"bundleRef": older}, next: map[string]any{"bundleRef": older, "candidateRef": foreign}, wantCode: CodeSiteVersionNotItsOwn},
		{name: "a directory", id: id, prior: map[string]any{"bundleRef": older}, next: map[string]any{"bundleRef": "file:///srv/site"}, wantCode: CodeSiteVersionNotItsOwn},

		// A row written before the rule stays writable, so it can be repaired.
		{name: "an older reference a write only inherits", id: id, prior: map[string]any{"bundleRef": foreign}, next: map[string]any{"bundleRef": foreign, "title": "renamed"}},
		{name: "an older candidate a write only inherits", id: id, prior: map[string]any{"bundleRef": own, "candidateRef": foreign}, next: map[string]any{"bundleRef": own, "candidateRef": foreign, "status": "disabled"}},
		{name: "repairing an older reference", id: id, prior: map[string]any{"bundleRef": foreign}, next: map[string]any{"bundleRef": own}},
		{name: "moving from one foreign reference to another", id: id, prior: map[string]any{"bundleRef": foreign}, next: map[string]any{"bundleRef": "blob://sites/third/v1/"}, wantCode: CodeSiteVersionNotItsOwn},
		{name: "an id written before the rule, on an update", id: "v1:platform:site:older/id", prior: map[string]any{"bundleRef": ""}, next: map[string]any{"bundleRef": "", "title": "renamed"}},

		{name: "the shell's seed", id: "v1:platform:site:os", next: map[string]any{"bundleRef": "file:///app/os"}},
		{name: "the extension page's seed", id: "v1:platform:site:vscode", next: map[string]any{"bundleRef": "file:///app/vscode-site"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSiteBundleRefs(tc.id, tc.prior, tc.next)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantCode) {
				t.Fatalf("got %v, want a refusal carrying %s", err, tc.wantCode)
			}
		})
	}
}

// The seeds are what the rule's platform half was written from, and they live in
// another language in another tree. Read them rather than restating them: a third
// platform site, or a moved directory, fails here rather than at somebody's boot.
func TestEverySeededSiteNamesItsOwnBundle(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}

	seeded := map[string]string{}
	for _, def := range eng.Seeds().All() {
		if def.UseConcept == siteSeedConcept {
			seeded[def.Body.fields["id"].str] = def.Body.fields["bundleRef"].str
		}
	}
	for _, name := range frontdoor.PlatformSites() {
		ref, ok := seeded[name]
		if !ok {
			t.Errorf("platform site %q has no seed", name)
			continue
		}
		if want := "file://" + frontdoor.PlatformSiteBundleDir(name); ref != want {
			t.Errorf("platform site %q seeds bundleRef %q, want %q", name, ref, want)
		}
	}
	for id, ref := range seeded {
		if !SiteNamesItsOwnBundle(id, ref) {
			t.Errorf("seed %q names %q, which its own write would refuse", id, ref)
		}
	}
}
