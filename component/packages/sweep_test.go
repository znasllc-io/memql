package packages

import (
	"context"
	"strings"
	"testing"
	"time"
)

// sweep_test.go covers the abandoned-run half (task memql#4902): the sweep's
// judgement, the sentence it writes, and the retry that reuses the snapshot.

// atTime returns a Deps whose clock is fixed, so "older than the threshold" is
// arithmetic a test can state rather than a race with the wall clock.
func atTime(h *harness, now time.Time) {
	h.deps.Now = func() time.Time { return now }
}

func inFlightRow(id string, status string, heartbeat time.Time) map[string]any {
	return map[string]any{
		"id":            id,
		"packageId":     "v1:platform:package:abc",
		"status":        status,
		"sourceVersion": "sha-abc123",
		"nodeId":        "bff-4",
		"heartbeatAt":   heartbeat.UTC().Format(time.RFC3339),
	}
}

func TestASweepClosesTheRunsWhoseNodesStoppedAnswering(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	atTime(h, now)
	h.engine.rows["query packageDeploymentsInFlight"] = []map[string]any{
		// Silent for ten minutes: gone.
		inFlightRow("v1:platform:packageDeployment:stranded", StatusBuilding, now.Add(-10*time.Minute)),
		// Beat five seconds ago: merely slow, and slow is not gone.
		inFlightRow("v1:platform:packageDeployment:working", StatusBuilding, now.Add(-5*time.Second)),
	}

	res, err := SweepAbandoned(context.Background(), h.deps)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Checked != 2 || res.Abandoned != 1 {
		t.Fatalf("want 2 checked and 1 abandoned, got %+v", res)
	}
	if !h.engine.sawStatement("mutation abandonPackageDeployment") {
		t.Fatalf("the stranded run must be closed; statements: %v", h.engine.statements())
	}
	if h.engine.sawStatement("packageDeployment:working") {
		t.Fatal("a run that is merely slow must not be touched")
	}
	// It is NEVER written as failed: nothing failed that this sweep can name.
	for _, q := range h.engine.statements() {
		if strings.Contains(q, "abandonPackageDeployment") && strings.Contains(q, `"failed"`) {
			t.Fatalf("the sweep must not write a failure verdict: %s", q)
		}
	}
}

func TestTheAbandonedSentenceNamesTheNodeAndWhenItWasHeard(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	last := now.Add(-10 * time.Minute)
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	atTime(h, now)
	h.engine.rows["query packageDeploymentsInFlight"] = []map[string]any{
		inFlightRow("v1:platform:packageDeployment:stranded", StatusBuilding, last),
	}

	if _, err := SweepAbandoned(context.Background(), h.deps); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var closed string
	for _, q := range h.engine.statements() {
		if strings.Contains(q, "abandonPackageDeployment") {
			closed = q
		}
	}
	if closed == "" {
		t.Fatal("nothing was closed")
	}
	// The three things a person needs, and nothing the sweep cannot know.
	for _, want := range []string{"bff-4", last.Format(time.RFC3339), "nothing was published", CodeDeploymentAbandoned} {
		if !strings.Contains(closed, want) {
			t.Errorf("the sentence must carry %q: %s", want, closed)
		}
	}
}

func TestARowWithNoTimestampAtAllIsLeftAlone(t *testing.T) {
	// Sweeping on an absent timestamp would close every row written before
	// heartbeats existed the moment this shipped -- including one somebody is
	// looking at.
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	atTime(h, now)
	h.engine.rows["query packageDeploymentsInFlight"] = []map[string]any{
		{"id": "v1:platform:packageDeployment:old", "status": StatusBuilding, "packageId": "v1:platform:package:abc"},
	}

	res, err := SweepAbandoned(context.Background(), h.deps)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Abandoned != 0 {
		t.Fatalf("a row with no evidence of life or death must be left alone, got %+v", res)
	}
}

func TestAnOlderRowIsJudgedByItsStartWhenItNeverBeat(t *testing.T) {
	// The reachable positive for the case above: startedAt IS evidence, so a
	// run that died before its first beat is still swept.
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	atTime(h, now)
	h.engine.rows["query packageDeploymentsInFlight"] = []map[string]any{
		{
			"id":        "v1:platform:packageDeployment:old",
			"status":    StatusBuilding,
			"packageId": "v1:platform:package:abc",
			"startedAt": now.Add(-48 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	res, err := SweepAbandoned(context.Background(), h.deps)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Abandoned != 1 {
		t.Fatalf("a run that started two days ago and never beat must be closed, got %+v", res)
	}
}

func TestTheThresholdCannotBeConfiguredBelowTheCadence(t *testing.T) {
	// An operator who set the threshold under the heartbeat interval would
	// have a cluster that sweeps its own healthy deploys, and the failure
	// would look like a broken build surface rather than like a setting.
	t.Setenv(HeartbeatIntervalEnv, "15")
	t.Setenv(AbandonedAfterEnv, "5")
	if got := AbandonedAfter(); got < 3*HeartbeatInterval() {
		t.Fatalf("the threshold must be clamped to at least three heartbeats, got %s", got)
	}
	// And a sane value is left exactly as it was.
	t.Setenv(AbandonedAfterEnv, "600")
	if got := AbandonedAfter(); got != 10*time.Minute {
		t.Fatalf("a configured threshold above the floor must be kept, got %s", got)
	}
}

func TestARunningDeployWritesItsHeartbeat(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	if _, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      clusterOwner(),
		Confirmed:  true,
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}, "docs": {Hostname: "docs.example.com"}},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !h.engine.sawStatement("mutation heartbeatPackageDeployment") {
		t.Fatalf("a running deploy must say it is alive, or the sweep will close it; statements: %v", h.engine.statements())
	}
}

// ---------------------------------------------------------------------------
// Retry from the stored snapshot
// ---------------------------------------------------------------------------

func TestARetryReadsTheStoredSnapshotInsteadOfFetching(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	// The earlier run: succeeded once, so its snapshot is stored.
	first, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      clusterOwner(),
		Confirmed:  true,
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}, "docs": {Hostname: "docs.example.com"}},
	})
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	if h.publisher.snapshots == 0 {
		t.Fatal("the first run must store a snapshot, or there is nothing to retry from")
	}

	// The retry names it. The prior row answers with the ref the publisher
	// minted, and the fetcher must not be reached at all.
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":                 first.DeploymentId,
		"packageId":          "v1:platform:package:abc",
		"status":             StatusAbandoned,
		"sourceVersion":      "sha-abc123",
		"snapshotArtifactId": "blob://packages/snapshots/snap.tar.gz",
	}}
	fetchesBefore := h.fetcher.repoFetches

	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:        "v1:platform:package:abc",
		Actor:            clusterOwner(),
		Confirmed:        true,
		FromDeploymentId: first.DeploymentId,
		// The harness's recording engine answers no sites for the package, so
		// the publish stage asks for hostnames as it would on a FIRST deploy.
		// Supplied here rather than seeded, because what this test is about is
		// where the BYTES came from.
		Placements: map[string]Placement{"storefront": {Hostname: "shop.example.com"}, "docs": {Hostname: "docs.example.com"}},
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if h.fetcher.repoFetches != fetchesBefore {
		t.Fatalf("a retry must NOT go back to the repository: fetches %d -> %d", fetchesBefore, h.fetcher.repoFetches)
	}
	if len(h.publisher.snapshotReads) != 1 {
		t.Fatalf("the retry must read the stored snapshot exactly once, got %v", h.publisher.snapshotReads)
	}
	if out.Status != StatusSucceeded {
		t.Fatalf("the retry must deploy, got %q", out.Status)
	}
	// It is the SAME version, not a new one: the point of a retry is that it
	// deploys what the lost run was deploying.
	if out.Report == nil || out.Report.SourceVersion != "sha-abc123" {
		t.Fatalf("the retry must carry the earlier run's version, got %+v", out.Report)
	}
}

func TestARetryOfARunThatKeptNoSnapshotSaysSo(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":        "v1:platform:packageDeployment:ancient",
		"packageId": "v1:platform:package:abc",
		"status":    StatusFailed,
		// No snapshotArtifactId: every run from before snapshots were kept.
	}}

	_, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:        "v1:platform:package:abc",
		Actor:            clusterOwner(),
		Confirmed:        true,
		FromDeploymentId: "v1:platform:packageDeployment:ancient",
	})
	if err == nil {
		t.Fatal("a retry with no snapshot must refuse rather than silently fetching fresh")
	}
	if got := RefusalCode(err); got != CodeSnapshotUnavailable {
		t.Fatalf("want %s, got %s (%v)", CodeSnapshotUnavailable, got, err)
	}
	// The sentence has to say what to do instead.
	if !strings.Contains(err.Error(), "Deploy again") {
		t.Errorf("the refusal must name the way forward: %v", err)
	}
}
