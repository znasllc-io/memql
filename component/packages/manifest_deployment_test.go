package packages

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/edge"
)

func TestManifestPlacementSurvivesReportTransferAndPreservesIdentity(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.test")
	tree := spaOnlyPackage()
	tree[ManifestName] = file(strings.Replace(validManifest, "  - name: storefront\n", "  - name: storefront\n    displayName: Fylo Storefront\n    deployment:\n      slug: quiet-cedar\n      domains: [shop.example.com, www.example.com]\n", 1))
	report, err := Analyze(tree, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Another replica only receives the persisted report, not the analyzer's FS.
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var transferred Report
	if err := json.Unmarshal(raw, &transferred); err != nil {
		t.Fatal(err)
	}
	if transferred.Manifest == nil || declaredFrom(&transferred)[0].DisplayName != "Fylo Storefront" {
		t.Fatal("manifest/label lost across transfer")
	}
	h := newHarness(t, tree, ownerPackage())
	out, err := h.deps.publish(context.Background(), DeployRequest{PackageId: "p", Placements: map[string]Placement{"docs": {Skip: true}}}, ownerPackage(), &transferred, map[string]edge.Bundle{"storefront": {"index.html": []byte("ok")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.publisher.ensured) != 1 || h.publisher.ensured[0].DeployableName != "storefront" || h.publisher.ensured[0].Hostname != "quiet-cedar.example.test" {
		t.Fatalf("identity/default address lost: %+v", h.publisher.ensured)
	}
	if out[0].OwnDomain != "shop.example.com, www.example.com" {
		t.Fatalf("domain results lost: %+v", out)
	}
	for _, domain := range []string{"shop.example.com", "www.example.com"} {
		if !h.engine.sawStatement(`hostname: "` + domain + `"`) {
			t.Fatalf("missing domain %s: %+v", domain, out)
		}
	}
}

func TestManifestPlacementOverridesAndOmission(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.test")
	d := &ManifestDeployment{Slug: "quiet-cedar", Domains: []string{"shop.example.com"}}
	p := manifestPlacement(d, Placement{Hostname: "chosen.example.test", Domains: []string{}})
	if p.Hostname != "chosen.example.test" || len(placementDomainNames(p)) != 0 {
		t.Fatalf("explicit selection overwritten: %+v", p)
	}
	p = manifestPlacement(nil, Placement{Hostname: "existing.example.test", OwnDomain: "existing.example.com"})
	if p.Hostname != "existing.example.test" || !reflect.DeepEqual(placementDomainNames(p), []string{"existing.example.com"}) {
		t.Fatal(p)
	}
	p = manifestPlacement(d, Placement{Skip: true})
	if p.Hostname != "" || len(p.Domains) != 0 {
		t.Fatalf("skipped placement applied: %+v", p)
	}
}

func TestManifestRejectsInvalidPlacementBeforeDeploy(t *testing.T) {
	for _, d := range []*ManifestDeployment{
		{Slug: "Capital"}, {Slug: "two.labels"}, {Domains: []string{"https://shop.example.com"}},
		{Domains: []string{"*.example.com"}}, {Domains: []string{"shop.example.com", "SHOP.EXAMPLE.COM"}},
	} {
		if validateDeployment(d) == nil {
			t.Fatalf("accepted %+v", d)
		}
	}
}

func TestManifestConfigurationChangeRequiresReview(t *testing.T) {
	d := &ManifestDeployment{Slug: "quiet-cedar", Domains: []string{"shop.example.com", "www.example.com"}}
	rep := &Report{OK: true, Deployables: []DeployableReport{{Name: "storefront", Deployment: d}}}
	before := PlanFingerprint(rep)
	d.Domains[0], d.Domains[1] = d.Domains[1], d.Domains[0]
	if PlanFingerprint(rep) != before {
		t.Fatal("domain order alone changed plan")
	}
	d.Slug = "other-name"
	if PlanFingerprint(rep) == before {
		t.Fatal("changed placement bypassed review")
	}
}

func TestManifestRedeployReusesDomainWithoutAddingAgain(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.engine.rows["query customDomainsForSite"] = []map[string]any{{"hostname": "shop.example.com", "status": "live"}}
	if err := h.deps.Store.addCustomDomain(context.Background(), "site", "shop.example.com"); err != nil {
		t.Fatal(err)
	}
	if h.engine.sawStatement("builtin customDomainAdd") {
		t.Fatal("existing binding added twice")
	}
}

func TestManifestRedeployKeepsAddressAndAddsDeclaredDomain(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.engine.rows["query sitesForPackage"] = []map[string]any{{"id": "existing-site", "hostname": "original.example.test", "packageDeployableName": "storefront"}}
	report := &Report{OK: true, Deployables: []DeployableReport{{Name: "storefront", Kind: KindSPA, Deployment: &ManifestDeployment{Slug: "changed-default", Domains: []string{"new.example.com"}}}}}
	out, err := h.deps.publish(context.Background(), DeployRequest{PackageId: "p"}, ownerPackage(), report, map[string]edge.Bundle{"storefront": {"index.html": []byte("ok")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Hostname != "original.example.test" || out[0].SiteId != "existing-site" || out[0].Created || len(h.publisher.ensured) != 0 {
		t.Fatalf("redeploy relocated site: %+v", out)
	}
	if !h.engine.sawStatement(`hostname: "new.example.com"`) {
		t.Fatal("new manifest domain not applied on redeploy")
	}
}
