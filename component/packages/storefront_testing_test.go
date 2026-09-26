package packages

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/edge"
)

func TestStorefrontTestingManifestSurvivesAnalysisAndReportTransfer(t *testing.T) {
	tree := validPackage()
	tree[ManifestName] = file(strings.Replace(validManifest, "  - name: storefront\n", "  - name: storefront\n    testing:\n      binding:\n        store: sandbox.myshopify.com\n", 1))
	rep, err := Analyze(tree, Options{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	var transferred Report
	if err := json.Unmarshal(raw, &transferred); err != nil {
		t.Fatal(err)
	}
	dep := transferred.Deployables[0]
	if dep.Testing == nil || dep.Testing.Binding.Store != "sandbox.myshopify.com" || dep.Binding.Store != "acme.myshopify.com" {
		t.Fatalf("lost independent bindings: %+v", dep)
	}
	before := PlanFingerprint(&transferred)
	dep.Testing.Binding.Store = "another.myshopify.com"
	if PlanFingerprint(&transferred) == before {
		t.Fatal("testing-store change bypasses deployment review")
	}
}

func TestStorefrontMayDeclareNoStoresForDesignPreview(t *testing.T) {
	for _, binding := range []string{"", "    binding: {}\n", "    testing: {binding: {store: sandbox.myshopify.com}}\n"} {
		tree := validPackage()
		tree[ManifestName] = file("formatVersion: 1\nname: design\ndeployables:\n  - name: storefront\n    path: clients/web\n    kind: shopify_storefront\n" + binding)
		if rep, err := Analyze(tree, Options{}); err != nil || !rep.OK {
			t.Fatalf("design preview refused: %v", err)
		}
	}
	tree := validPackage()
	tree[ManifestName] = file("formatVersion: 1\nname: invalid\ndeployables:\n  - {name: app, path: clients/web, kind: spa, testing: {binding: {store: sandbox.myshopify.com}}}\n")
	if _, err := ReadManifest(tree); RefusalCode(err) != CodeManifestInvalid {
		t.Fatalf("testing accepted on non-storefront: %v", err)
	}
}

func TestPublishingTestingStorePreservesProductionAddressAndBinding(t *testing.T) {
	for _, tc := range []struct {
		name, named, resolved, prior string
		want                         []string
	}{
		{"attach", "sandbox.myshopify.com", "sandbox", "", []string{"testing:existing -> sandbox"}},
		{"same sandbox on both", "live.myshopify.com", "live", "", []string{"testing:existing -> live"}},
		{"unchanged", "sandbox.myshopify.com", "sandbox", "sandbox", nil},
		{"unavailable", "missing.myshopify.com", "", "sandbox", nil},
		{"omitted", "", "", "sandbox", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &recordingEngine{rows: map[string][]map[string]any{"query sitesForPackage": {{"id": "existing", "hostname": "original.example.test", "packageDeployableName": "storefront", "binding": map[string]any{"storeId": "live"}, "previewBinding": map[string]any{"storeId": tc.prior}}}}}
			publisher := &fakePublisher{}
			deps := &Deps{Store: &store{engine: engine}, Publisher: publisher, Logger: discardLogger(), Stores: storeIdFor(map[string]string{"live.myshopify.com": "live", tc.named: tc.resolved})}
			rep := storefrontOnlyReport("live.myshopify.com")
			if tc.named != "" {
				rep.Deployables[0].Testing = &ManifestTesting{Binding: &ManifestBinding{Store: tc.named}}
			}
			out, err := deps.publish(context.Background(), DeployRequest{PackageId: "p"}, ownerPackage(), rep, map[string]edge.Bundle{"storefront": {"index.html": []byte("same design")}})
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 || out[0].Hostname != "original.example.test" || len(publisher.ensured) != 0 || !reflect.DeepEqual(publisher.bound, tc.want) {
				t.Fatalf("unexpected publish: out=%+v bindings=%v", out, publisher.bound)
			}
			if tc.name == "unavailable" && (out[0].Refusal == nil || out[0].Refusal.Fatal) {
				t.Fatal("unavailable testing store must leave an actionable nonfatal note")
			}
		})
	}
}
