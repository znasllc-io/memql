package packages

import (
	"context"
	"strings"
	"testing"
)

// A run knows what it is: its SCOPE and where it came FROM (memql#4953,
// memql#4954, memql#4955).
//
// The three defects share one cause. A `v1:platform:packageDeployment` row
// recorded nothing about which deployables it was for or which run it
// continued, so every reader had to guess from position -- and the guess was
// "the source's newest run is about whatever you are looking at", which is
// false the moment a run is scoped to one app, which is routine.

// parkedRun is what the store reads back for a run waiting at the gate.
func parkedRun(id, packageId string, extra map[string]any) map[string]any {
	row := map[string]any{
		"id":          id,
		"concept":     packageDeploymentConcept,
		"packageId":   packageId,
		"ownerUserId": "v1:identity:user:someone",
		"status":      StatusAwaitingConfirm,
	}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

// ---------------------------------------------------------------------------
// #4954 -- confirming a parked run advances it
// ---------------------------------------------------------------------------

func TestConfirmingAParkedRunAdvancesItRatherThanMintingANewOne(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	const parked = "v1:platform:packageDeployment:parked"
	h.engine.rows["query packageDeploymentById"] = []map[string]any{
		// No snapshot recorded, so the confirm fetches -- the parked-run
		// fallback. What this test is about is WHICH ROW the confirm runs as.
		parkedRun(parked, "v1:platform:package:abc", nil),
	}

	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:    "v1:platform:package:abc",
		Actor:        clusterOwner(),
		Confirmed:    true,
		DeploymentId: parked,
		Placements:   firstDeployPlacements(),
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	if out.DeploymentId != parked {
		t.Fatalf("the confirmation ran as %s, not as the run it was confirming (%s)",
			out.DeploymentId, parked)
	}
	if h.engine.sawStatement("mutation openPackageDeployment") {
		t.Error("a second row was opened. The source is then left with a run at " +
			"awaiting_confirm nobody will ever answer, the list goes on saying a deploy " +
			"is waiting for a gate that was answered, and the history shows a run that " +
			"never resolved (memql#4954)")
	}
	if !h.engine.sawStatement(`mutation closePackageDeployment(deploymentId: "` + parked + `"`) {
		t.Errorf("the parked run was not closed; statements:\n%s",
			strings.Join(h.engine.queries, "\n"))
	}
}

// THE CONTROL. Two ordinary confirmed deploys with no deployment id are still
// two runs -- which is what TestARetryIsANewRow asserts, and what the resume
// branch must not change.
func TestTwoFreshDeploysAreStillTwoRuns(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	req := DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      clusterOwner(),
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	}
	first, err := Deploy(context.Background(), h.deps, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := Deploy(context.Background(), h.deps, req)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.DeploymentId == second.DeploymentId {
		t.Fatal("two independent deploys shared a row")
	}
}

// A run that is NOT parked is not resumable. Falling through to
// openDeployment is what keeps the append-only guard refusing a second
// pipeline on a row already in flight -- which is the dedup two auto-deploy
// feeds noticing one push rely on.
func TestARunInFlightIsNotResumed(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	const running = "v1:platform:packageDeployment:running"
	h.engine.rows["query packageDeploymentById"] = []map[string]any{
		parkedRun(running, "v1:platform:package:abc", map[string]any{"status": StatusBuilding}),
	}

	_, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:    "v1:platform:package:abc",
		Actor:        clusterOwner(),
		Confirmed:    true,
		DeploymentId: running,
		Placements:   firstDeployPlacements(),
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !h.engine.sawStatement("mutation openPackageDeployment") {
		t.Error("a run at `building` was resumed. Only a parked run is a question " +
			"waiting for an answer; every other non-terminal row is a pipeline already " +
			"writing it")
	}
}

// A deployment id naming somebody else's source is refused before anything is
// opened, rather than resumed or quietly ignored.
func TestAParkedRunOfADifferentPackageIsRefused(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{
		parkedRun("v1:platform:packageDeployment:other", "v1:platform:package:zzz", nil),
	}

	_, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:    "v1:platform:package:abc",
		Actor:        clusterOwner(),
		Confirmed:    true,
		DeploymentId: "v1:platform:packageDeployment:other",
		Placements:   firstDeployPlacements(),
	})
	if err == nil {
		t.Fatal("a run of another source was accepted")
	}
	if !strings.Contains(err.Error(), "different package") {
		t.Errorf("refusal does not say why: %v", err)
	}
}

// ---------------------------------------------------------------------------
// #4955 -- a resumed run deploys its own bytes
// ---------------------------------------------------------------------------

// The promise `packageDeploy(fromDeploymentId:)` makes is that a Retry
// deploys what the lost run was deploying. It held for the ANALYSIS a person
// reads at the gate and broke at the click they read it in order to make,
// because the confirm was a fresh call carrying no fromDeploymentId at all.
func TestConfirmingAResumedRunReadsItsOwnSnapshotRatherThanFetchingAgain(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())

	// An earlier run stored a snapshot, so there is something to resume from.
	// The parked row below names it, which is what a retry's own row records
	// now (memql#4955) -- before this it recorded nothing, so the run that
	// parked with a lost run's bytes could not be resumed on them.
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      clusterOwner(),
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	}); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	if h.publisher.snapshots == 0 {
		t.Fatal("nothing was stored, so there is nothing to resume onto")
	}

	const parked = "v1:platform:packageDeployment:retry"
	h.engine.rows["query packageDeploymentById"] = []map[string]any{
		parkedRun(parked, "v1:platform:package:abc", map[string]any{
			"snapshotArtifactId": "blob://packages/snapshots/snap.tar.gz",
			"sourceVersion":      "sha-abc123",
			"fromDeploymentId":   "v1:platform:packageDeployment:lost",
		}),
	}
	fetchesBefore := h.fetcher.repoFetches

	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:    "v1:platform:package:abc",
		Actor:        clusterOwner(),
		Confirmed:    true,
		DeploymentId: parked,
		Placements:   firstDeployPlacements(),
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}

	if h.fetcher.repoFetches != fetchesBefore {
		t.Errorf("the source was fetched again: %d -> %d. A person reading a report describing "+
			"commit A must not click a button that ships commit B (memql#4955)",
			fetchesBefore, h.fetcher.repoFetches)
	}
	if len(h.publisher.snapshotReads) == 0 {
		t.Error("the run's own stored snapshot was never read")
	}
	if out.Report == nil || out.Report.SourceVersion != "sha-abc123" {
		t.Errorf("the confirmation deployed a different version: %+v", out.Report)
	}
}

// ---------------------------------------------------------------------------
// #4953 -- a run records which deployables it is for
// ---------------------------------------------------------------------------

func TestAScopedRunRecordsWhichDeployablesItIsFor(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())

	// The shape a redeploy from one deployable's page sends: every SIBLING
	// skipped by name.
	placements := firstDeployPlacements()
	placements["docs"] = Placement{Skip: true}

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      clusterOwner(),
		Confirmed:  true,
		Placements: placements,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	open := statementContaining(h.engine.queries, "mutation openPackageDeployment")
	if open == "" {
		t.Fatal("no run was opened")
	}
	if !strings.Contains(open, `"storefront"`) {
		t.Errorf("the run does not record the app it is for:\n%s", open)
	}
	if strings.Contains(open, `"docs"`) {
		t.Errorf("the run records an app it was told to skip:\n%s", open)
	}
}

// A WHOLE-SOURCE RUN RECORDS NOTHING, and that is what makes the field safe to
// add to a live timeline. Every row written before it has no scope, and a
// reader asking "is this run about my app" gets yes -- which is what those
// runs were. One spelling for one meaning.
func TestAWholeSourceRunRecordsNoScope(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      clusterOwner(),
		Confirmed:  true,
		Placements: firstDeployPlacements(),
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	open := statementContaining(h.engine.queries, "mutation openPackageDeployment")
	if !strings.Contains(open, `scopedTo: []`) {
		t.Errorf("a run with nothing skipped should record an empty scope, not a list of "+
			"every declared name:\n%s", open)
	}
}

// The gate is where a person answers WHICH apps they meant: a compose gate
// opens with no placements at all and closes with the skips somebody ticked.
// A scope fixed at open would have the run report progress on apps it was
// told not to build.
func TestConfirmingRestampsTheScope(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	const parked = "v1:platform:packageDeployment:parked"
	h.engine.rows["query packageDeploymentById"] = []map[string]any{
		// No snapshot recorded, so the confirm fetches -- the parked-run
		// fallback. What this test is about is WHICH ROW the confirm runs as.
		parkedRun(parked, "v1:platform:package:abc", nil),
	}
	placements := firstDeployPlacements()
	placements["docs"] = Placement{Skip: true}

	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:    "v1:platform:package:abc",
		Actor:        clusterOwner(),
		Confirmed:    true,
		DeploymentId: parked,
		Placements:   placements,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	scope := statementContaining(h.engine.queries, "mutation recordPackageDeploymentScope")
	if scope == "" {
		t.Fatalf("the confirm recorded no scope; statements:\n%s",
			strings.Join(h.engine.queries, "\n"))
	}
	if !strings.Contains(scope, `"storefront"`) || strings.Contains(scope, `"docs"`) {
		t.Errorf("wrong scope recorded at the gate:\n%s", scope)
	}
}

// scopeFrom is pure, so its edges are worth stating directly.
func TestScopeFromIsTheComplementOfTheSkips(t *testing.T) {
	declared := []string{"storefront", "docs", "web"}

	t.Run("no placements at all is the whole source", func(t *testing.T) {
		if got := scopeFrom(declared, nil); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
	t.Run("placements with no skip is the whole source", func(t *testing.T) {
		got := scopeFrom(declared, map[string]Placement{"storefront": {Hostname: "a"}})
		if got != nil {
			t.Fatalf("got %v, want nil -- a run that skips nothing is a whole-source run, "+
				"and listing every declared name would be a second spelling of that", got)
		}
	})
	t.Run("the complement, in the manifest's own order", func(t *testing.T) {
		got := scopeFrom(declared, map[string]Placement{
			"docs": {Skip: true},
			"web":  {Skip: true},
		})
		if len(got) != 1 || got[0] != "storefront" {
			t.Fatalf("got %v, want [storefront]", got)
		}
	})
	t.Run("the placements' keys are not the answer", func(t *testing.T) {
		// A single-app redeploy names only the SIBLINGS it skips, so reading
		// the map's keys answers with everything the run is NOT for.
		got := scopeFrom(declared, map[string]Placement{"docs": {Skip: true}})
		if len(got) != 2 {
			t.Fatalf("got %v, want storefront and web", got)
		}
	})
	t.Run("no analysis yet falls back to the placements", func(t *testing.T) {
		got := scopeFrom(nil, map[string]Placement{
			"storefront": {Hostname: "a"},
			"docs":       {Skip: true},
		})
		if len(got) != 1 || got[0] != "storefront" {
			t.Fatalf("got %v, want [storefront]", got)
		}
	})
}

func statementContaining(stmts []string, sub string) string {
	for _, q := range stmts {
		if strings.Contains(q, sub) {
			return q
		}
	}
	return ""
}
