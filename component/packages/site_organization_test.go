package packages

import (
	"context"
	"errors"
	"testing"
)

func TestPackageSiteOrganizationIsChosenBeforeCreation(t *testing.T) {
	pkg := ownerPackage()
	pkg["accountId"] = "operator-org"
	h := newHarness(t, spaOnlyPackage(), pkg)
	placements := firstDeployPlacements()
	placement := placements["storefront"]
	placement.AccountId = "client-org"
	placements["storefront"] = placement
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: "v1:platform:package:abc", Actor: mayDeployDsl(), Confirmed: true, Placements: placements}); err != nil {
		t.Fatal(err)
	}
	for _, req := range h.publisher.ensured {
		want := "operator-org"
		if req.DeployableName == "storefront" {
			want = "client-org"
		}
		if req.AccountId != want {
			t.Fatalf("%s created for %q want %q", req.DeployableName, req.AccountId, want)
		}
	}
	if h.engine.sawStatement("updateSiteAccount") {
		t.Fatal("organization applied after creation")
	}
}

func TestRefusedSiteOrganizationStopsPublishing(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.publisher.ensureErr = errors.New("organization_forbidden")
	out, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: "v1:platform:package:abc", Actor: mayDeployDsl(), Confirmed: true, Placements: firstDeployPlacements()})
	if err == nil && out.Status == StatusSucceeded {
		t.Fatal("refused organization reported successful deploy")
	}
	if len(h.publisher.published) > 0 {
		t.Fatal("site published after organization refusal")
	}
}
