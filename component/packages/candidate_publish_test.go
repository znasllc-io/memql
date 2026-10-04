package packages

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/edge"
)

// A package deploy can publish a deployable as its CANDIDATE version
// (memql#5601). The choice is a PLACEMENT -- a per-run answer at the confirm
// gate, like skip -- and never a manifest field: the manifest describes the
// software, and whether this run's build goes in front of the public is a
// decision about this run.

// candidatePlacements is a scoped run of validPackage's static `docs` app as
// the candidate, with the storefront left out.
func candidatePlacements() map[string]Placement {
	return map[string]Placement{
		"storefront": {Skip: true},
		"docs":       {Hostname: "docs.example.com", Target: edge.TargetCandidate},
	}
}

func TestACandidatePlacementPublishesTheCandidateAndRecordsItAsOne(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      plainUser(),
		Confirmed:  true,
		Placements: candidatePlacements(),
	})
	if err != nil {
		t.Fatalf("a candidate deploy was refused: %v", err)
	}
	if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetCandidate {
		t.Fatalf("the publish stage published %v, want exactly one candidate publish", got)
	}

	var docs DeployableOutcome
	for _, o := range out.Deployables {
		if o.Name == "docs" {
			docs = o
		}
	}
	// THE OUTCOME NAMES THE CANDIDATE AND NOT THE SERVING VERSION. Rollback
	// re-points every outcome's bundleRef, so a candidate recorded there would
	// make "roll back to this run" serve a version the run never served.
	if docs.CandidateRef != "blob://sites/x/v1/" || docs.BundleRef != "" {
		t.Fatalf("the candidate outcome is %+v, want candidateRef set and bundleRef empty", docs)
	}

	// AND THE SOURCE IS NOT MARKED DEPLOYED. deployedVersion is "the source
	// currently LIVE", and a run that only published candidates changed
	// nothing the public sees -- marking it deployed would also tell the
	// auto-deploy feed there is no update left to ship.
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if statementIndex(h.engine.queries, "recordPackageDeployedVersion") >= 0 {
		t.Fatal("a candidate-only run recorded the package's deployed version")
	}
}

// THE REACHABLE POSITIVE: the same run with no target publishes the serving
// version, records it as one, and marks the source deployed -- so each
// assertion above is the target's doing and not a fixture that never
// publishes or never records.
func TestAPlacementWithNoTargetStillPublishesTheServingVersion(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	placements := candidatePlacements()
	placements["docs"] = Placement{Hostname: "docs.example.com"}
	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      plainUser(),
		Confirmed:  true,
		Placements: placements,
	})
	if err != nil {
		t.Fatalf("a serving deploy was refused: %v", err)
	}
	if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetServing {
		t.Fatalf("the publish stage published %v, want exactly one serving publish", got)
	}
	for _, o := range out.Deployables {
		if o.Name == "docs" && (o.BundleRef != "blob://sites/x/v1/" || o.CandidateRef != "") {
			t.Fatalf("the serving outcome is %+v", o)
		}
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if statementIndex(h.engine.queries, "recordPackageDeployedVersion") < 0 {
		t.Fatal("a serving run did not record the package's deployed version")
	}
}

// A STOREFRONT HAS NO CANDIDATE VERSION. Its Testing destination serves the
// published build against the testing store (storefront-preview.md), so a
// storefront candidate would be served by nothing at all. The run is refused
// before the build, and before any store binding is touched: a re-pointed
// binding reaches the live site the moment it is written, which is precisely
// what somebody asking for a candidate asked not to happen.
func TestAStorefrontCandidateIsRefusedBeforeAnythingIsBuiltOrBound(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	_, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     plainUser(),
		Confirmed: true,
		Placements: map[string]Placement{
			"storefront": {Hostname: "shop.example.com", Target: edge.TargetCandidate},
			"docs":       {Skip: true},
		},
	})
	if err == nil {
		t.Fatal("a storefront was published as a candidate")
	}
	if !strings.Contains(err.Error(), `"storefront"`) || !strings.Contains(err.Error(), "candidate") {
		t.Errorf("the refusal does not say which app and why: %v", err)
	}
	if len(h.builder.built) != 0 || len(h.publisher.published) != 0 || len(h.publisher.bound) != 0 || len(h.publisher.ensured) != 0 {
		t.Fatalf("the refused run still built %v, published %v, bound %v and created %v",
			h.builder.built, h.publisher.published, h.publisher.bound, h.publisher.created)
	}
}

// AN UNKNOWN TARGET IS REFUSED AT THE DOOR, before a run row exists. Reading
// it as the serving version is the direction this must never fail in.
func TestAnUnknownPlacementTargetIsRefusedBeforeARunOpens(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	placements := candidatePlacements()
	placements["docs"] = Placement{Hostname: "docs.example.com", Target: "staging"}
	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      plainUser(),
		Confirmed:  true,
		Placements: placements,
	})
	if err == nil || out != nil {
		t.Fatalf("an unknown target opened a run: %+v, %v", out, err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if statementIndex(h.engine.queries, "openPackageDeployment") >= 0 || len(h.builder.built) != 0 {
		t.Fatal("an unknown target reached the timeline or the build")
	}
}

func TestPlacementsArgCarriesTheTarget(t *testing.T) {
	got := placementsArg(map[string]any{"placements": map[string]any{
		"docs":  map[string]any{"hostname": "docs", "target": "candidate"},
		"other": map[string]any{"hostname": "other"},
	}}, "placements")
	if got["docs"].Target != edge.TargetCandidate {
		t.Errorf("docs target = %q, want candidate", got["docs"].Target)
	}
	if got["other"].Target != "" {
		t.Errorf("an absent target must stay absent until the pipeline reads it as serving, got %q", got["other"].Target)
	}
}
