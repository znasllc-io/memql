package packages

import (
	"context"
	"io/fs"
	"path"
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

// A TARGET OF THE WRONG TYPE REFUSES THE CALL (memql#5601). stringArg answers
// "" for a non-string, and "" is the serving version -- so a client that sent
// `target: true` meaning "yes, the candidate" would have published to every
// visitor. The call is refused before a run row exists. JSON null is the one
// exception: it is the platform's unset value, the same as leaving the key out.
func TestAWrongTypedTargetRefusesTheCallBeforeARunOpens(t *testing.T) {
	for name, target := range map[string]any{"a boolean": true, "a number": float64(1), "an object": map[string]any{"kind": "candidate"}, "a list": []any{"candidate"}} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, spaOnlyPackage(), ownerPackage())
			i := NewIntegration(h.engine, discardLogger())
			i.depsOnce.Do(func() { i.deps = h.deps })
			_, err := i.handleDeploy(callerCtx("v1:identity:user:someone"), map[string]any{
				"packageId": "v1:platform:package:abc",
				"confirm":   true,
				"placements": map[string]any{
					"storefront": map[string]any{"skip": true},
					"docs":       map[string]any{"hostname": "docs.example.com", "target": target},
				},
			}, 0)
			if err == nil || !strings.Contains(err.Error(), "target") || !strings.Contains(err.Error(), `"docs"`) {
				t.Fatalf("%s as the target was not refused by name: %v", name, err)
			}
			if hasCall(h.engine.statements(), "mutation openPackageDeployment(") || len(h.publisher.published) != 0 {
				t.Fatal("a wrong-typed target opened a run or published")
			}
		})
	}

	t.Run("null is unset and publishes the serving version", func(t *testing.T) {
		h := newHarness(t, spaOnlyPackage(), ownerPackage())
		i := NewIntegration(h.engine, discardLogger())
		i.depsOnce.Do(func() { i.deps = h.deps })
		if _, err := i.handleDeploy(callerCtx("v1:identity:user:someone"), map[string]any{
			"packageId": "v1:platform:package:abc",
			"confirm":   true,
			"placements": map[string]any{
				"storefront": map[string]any{"skip": true},
				"docs":       map[string]any{"hostname": "docs.example.com", "target": nil},
			},
		}, 0); err != nil {
			t.Fatalf("a null target was refused: %v", err)
		}
		if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetServing {
			t.Fatalf("a null target published %v, want the serving version", got)
		}
	})
}

// A CANDIDATE RUN DOES NOT ROLL THE CLUSTER ONTO NEW DSL (memql#5601). Staging
// a changed MemQL domain flips the active-set pointer and restarts every node
// that reads DSL, and a candidate run leaves every visitor on the serving
// build -- so the public would keep app N running against DSL N+1, which is a
// deploy of half a version that nobody chose. Refused after the analysis
// (which is what knows the domains) and before the build, the stage and the
// roll.
func TestACandidateRunThatWouldChangeTheDslIsRefusedBeforeAnythingRuns(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	_, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      mayDeployDsl(),
		Confirmed:  true,
		Placements: candidatePlacements(),
	})
	if err == nil {
		t.Fatal("a candidate run carrying a DSL change was allowed")
	}
	for _, says := range []string{`"docs"`, "acme", "serving version", "every app skipped"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	if len(h.builder.built) != 0 || len(h.stager.staged) != 0 || h.stager.written != 0 || h.roller.rolls != 0 || len(h.publisher.published) != 0 {
		t.Fatalf("the refused run built %v, staged %v, wrote the pointer %d times, rolled %d times and published %v",
			h.builder.built, h.stager.staged, h.stager.written, h.roller.rolls, h.publisher.published)
	}
}

// THE REACHABLE POSITIVE: the same candidate run against a cluster already
// running this exact DSL changes no pointer, so it publishes its candidate
// and rolls nothing.
func TestACandidateRunWhoseDslIsUnchangedPublishesWithoutARoll(t *testing.T) {
	tree := validPackage()
	h := newHarness(t, tree, ownerPackage())
	sub, err := fs.Sub(tree, path.Join(DslRoot, "acme"))
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := h.stager.PrefixFor("acme", sub)
	if err != nil {
		t.Fatal(err)
	}
	h.stager.active = map[string]string{"acme": prefix}

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      mayDeployDsl(),
		Confirmed:  true,
		Placements: candidatePlacements(),
	}); err != nil {
		t.Fatalf("a candidate run with unchanged DSL was refused: %v", err)
	}
	if h.roller.rolls != 0 || h.stager.written != 0 {
		t.Fatalf("unchanged DSL rolled %d times and wrote the pointer %d times", h.roller.rolls, h.stager.written)
	}
	if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetCandidate {
		t.Fatalf("published %v, want one candidate", got)
	}
}

// ---------------------------------------------------------------------------
// The target survives the confirm gate (memql#5601)
// ---------------------------------------------------------------------------
//
// A run is opened and later confirmed by a SECOND call, and placements used to
// be read from the confirming call alone -- so a run opened as a candidate and
// confirmed by a call that left the target out published to every visitor.
// The contract: the run records its candidates when it opens, the way it
// records its scope; at the gate an EXPLICIT target on the confirming call
// wins, because that is where a person decides with the plan in front of them,
// and an OMITTED one keeps what was recorded. A run can therefore never fall
// toward the serving version because a second call was less specific.

const parkedCandidateRun = "v1:platform:packageDeployment:parked-candidate"

func confirmParked(t *testing.T, recorded []any, placements map[string]Placement) *harness {
	t.Helper()
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	extra := map[string]any{}
	if recorded != nil {
		extra["candidates"] = recorded
	}
	h.engine.rows["query packageDeploymentById"] = []map[string]any{parkedRun(parkedCandidateRun, "v1:platform:package:abc", extra)}
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:    "v1:platform:package:abc",
		Actor:        plainUser(),
		Confirmed:    true,
		DeploymentId: parkedCandidateRun,
		Placements:   placements,
	}); err != nil {
		t.Fatalf("confirming the parked run: %v", err)
	}
	return h
}

func TestARunRecordsItsCandidatesWhenItOpens(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      plainUser(),
		Placements: candidatePlacements(),
	}); err != nil {
		t.Fatalf("opening the run: %v", err)
	}
	open := statementContaining(h.engine.queries, "mutation openPackageDeployment")
	if !strings.Contains(open, `candidates: ["docs"]`) {
		t.Fatalf("the opened run does not record docs as a candidate:\n%s", open)
	}
}

func TestAConfirmThatOmitsTheTargetKeepsTheRecordedCandidate(t *testing.T) {
	h := confirmParked(t, []any{"docs"}, map[string]Placement{
		"storefront": {Skip: true},
		"docs":       {Hostname: "docs.example.com"},
	})
	if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetCandidate {
		t.Fatalf("a run opened as a candidate and confirmed without a target published %v, want the candidate", got)
	}
}

func TestAnExplicitTargetAtTheConfirmWins(t *testing.T) {
	h := confirmParked(t, []any{"docs"}, map[string]Placement{
		"storefront": {Skip: true},
		"docs":       {Hostname: "docs.example.com", Target: edge.TargetServing},
	})
	if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetServing {
		t.Fatalf("an explicit serving target at the gate published %v, want the serving version", got)
	}
	restamp := statementContaining(h.engine.queries, "mutation recordPackageDeploymentScope")
	if !strings.Contains(restamp, `candidates: []`) {
		t.Fatalf("the gate's answer was not recorded on the run:\n%s", restamp)
	}
}

// The OS flow: open the run to see the plan, then choose at the gate.
func TestARunOpenedWithNoTargetCanBeConfirmedAsACandidate(t *testing.T) {
	h := confirmParked(t, nil, map[string]Placement{
		"storefront": {Skip: true},
		"docs":       {Hostname: "docs.example.com", Target: edge.TargetCandidate},
	})
	if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetCandidate {
		t.Fatalf("a candidate chosen at the gate published %v", got)
	}
	restamp := statementContaining(h.engine.queries, "mutation recordPackageDeploymentScope")
	if !strings.Contains(restamp, `candidates: ["docs"]`) {
		t.Fatalf("the candidate chosen at the gate was not recorded on the run:\n%s", restamp)
	}
}

// A RETRY RESTARTS THE RUN THAT WAS LOST, and that run was a candidate run: a
// retry whose placements leave the target out publishes the candidate again
// rather than putting the build in front of the public.
func TestARetryKeepsTheLostRunsCandidates(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	first, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      plainUser(),
		Confirmed:  true,
		Placements: candidatePlacements(),
	})
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	h.engine.rows[`query packageDeploymentById(deploymentId: "`+first.DeploymentId+`")`] = []map[string]any{{
		"id":                 first.DeploymentId,
		"packageId":          "v1:platform:package:abc",
		"status":             StatusAbandoned,
		"sourceVersion":      "sha-abc123",
		"snapshotArtifactId": "blob://packages/snapshots/snap.tar.gz",
		"candidates":         []any{"docs"},
	}}
	h.publisher.targets = nil
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:        "v1:platform:package:abc",
		Actor:            plainUser(),
		Confirmed:        true,
		FromDeploymentId: first.DeploymentId,
		Placements: map[string]Placement{
			"storefront": {Skip: true},
			"docs":       {Hostname: "docs.example.com"},
		},
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := h.publisher.targets; len(got) != 1 || got[0] != edge.TargetCandidate {
		t.Fatalf("the retry of a candidate run published %v, want the candidate", got)
	}
}
