package packages

import (
	"context"
	"strings"
	"testing"
)

func TestDeploymentModeResolvesBareIDBeforeWriting(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	i := NewIntegration(h.engine, discardLogger())
	i.depsOnce.Do(func() { i.deps = h.deps })
	if _, err := i.handleSetAutoDeploy(callerCtx("v1:identity:user:someone"), map[string]any{"packageId": "abc", "autoDeploy": true}, 0); err != nil {
		t.Fatal(err)
	}
	for _, q := range h.engine.statements() {
		if strings.HasPrefix(q, "mutation setPackageAutoDeploy") {
			if !strings.Contains(q, `"v1:platform:package:abc"`) || !strings.Contains(q, "autoDeploy: true") {
				t.Fatalf("wrong mutation: %s", q)
			}
			return
		}
	}
	t.Fatal("mode was not written")
}

func TestWebhookOnlyUpdatesItsTrackedBranch(t *testing.T) {
	main := trackedPackage("old", "old", false)
	other := trackedPackage("old-release", "old-release", false)
	other["id"], other["repoRef"] = "v1:platform:package:release", "release"
	i, engine := feedHarness(t, main, other)
	_, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{"source": "github", "body": pushBody}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range engine.statements() {
		if strings.HasPrefix(q, "mutation recordPackageUpstreamVersion") && strings.Contains(q, "package:release") {
			t.Fatalf("main push changed another branch: %s", q)
		}
	}
	if !engine.sawStatement("mutation recordPackageUpstreamVersion") {
		t.Fatal("default branch was not detected")
	}
}

func TestPollUpdatesOnlyThePackageWhoseHeadWasResolved(t *testing.T) {
	main := trackedPackage("old", "old", false)
	other := trackedPackage("old", "old", false)
	other["id"] = "v1:platform:package:other"
	i, engine := feedHarness(t, main, other)
	if _, err := notePackageUpstream(context.Background(), i.deps, main, "main-sha"); err != nil {
		t.Fatal(err)
	}
	for _, q := range engine.statements() {
		if strings.Contains(q, "package:other") {
			t.Fatalf("poll changed another source: %s", q)
		}
	}
}

func TestUnchangedPendingRevisionReconcilesAfterAutomaticIsEnabled(t *testing.T) {
	pkg := autoPackage()
	pkg["latestKnownVersion"], pkg["updateAvailable"] = "pending-sha", true
	h := newHarness(t, spaOnlyPackage(), pkg)
	n, err := notePackageUpstream(context.Background(), h.deps, pkg, "pending-sha")
	if err != nil || n != 0 {
		t.Fatalf("unchanged detection: n=%d err=%v", n, err)
	}
	if !h.engine.sawStatement("mutation openPackageDeployment") {
		t.Fatal("pending automatic update was never reconciled")
	}
	if h.engine.sawStatement("mutation recordPackageUpstreamVersion") {
		t.Fatal("unchanged detection must not broadcast a heartbeat")
	}
}

func TestAutomaticRunRechecksManualPolicyBeforeEffects(t *testing.T) {
	current := ownerPackage() // Manual on the authoritative re-read.
	h := newHarness(t, spaOnlyPackage(), current)
	_, err := h.deps.startAutoRun(context.Background(), autoPackage(), "new") // stale feed snapshot
	if err == nil {
		t.Fatal("expected cancellation when source switched to Manual")
	}
	if len(h.builder.built) != 0 || len(h.publisher.published) != 0 {
		t.Fatal("manual source was built or published automatically")
	}
	if !h.engine.sawStatement(`"cancelled"`) {
		t.Fatal("policy change must stop the automatic attempt")
	}
}

func TestDeletedBranchDoesNotBecomeAnAvailableVersion(t *testing.T) {
	if _, err := parseGitHubPush(`{"deleted":true,"after":"00000000","ref":"refs/heads/main","repository":{"html_url":"https://github.com/acme/widget"}}`); err == nil {
		t.Fatal("branch deletion is not a new version")
	}
}

func TestPolicyRearmHasANewDeduplicatedAttempt(t *testing.T) {
	pkg := autoPackage()
	legacy := policyDeploymentId(pkg, "same-sha")
	pkg["autoDeployChangedAt"] = "2026-09-16T10:00:00Z"
	first := policyDeploymentId(pkg, "same-sha")
	if first == legacy || first != policyDeploymentId(pkg, "same-sha") {
		t.Fatal("policy epoch must distinguish explicit rearming and deduplicate repeated feeds")
	}
	pkg["autoDeployChangedAt"] = "2026-09-16T10:01:00Z"
	if policyDeploymentId(pkg, "same-sha") == first {
		t.Fatal("rearming must let a stopped pending revision run")
	}
}

type policyChangingBuilder struct {
	base   Builder
	change func()
}

func (b policyChangingBuilder) Build(ctx context.Context, run BuildRun, snapshot *SourceSnapshot, dep DeployableReport) (BuildResult, error) {
	result, err := b.base.Build(ctx, run, snapshot, dep)
	b.change()
	return result, err
}

func TestSwitchToManualDuringBuildPreventsPublishing(t *testing.T) {
	pkg := autoPackage()
	h := newHarness(t, spaOnlyPackage(), pkg)
	h.deps.Builder = policyChangingBuilder{base: h.builder, change: func() { pkg["autoDeploy"] = false }}
	out, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: rowString(pkg, "id"), Automatic: true, Confirmed: true, Placements: firstDeployPlacements(), Actor: Actor{UserId: rowString(pkg, "ownerUserId")}})
	if err == nil || out.Status != StatusCancelled {
		t.Fatalf("expected cancelled automatic run, got %+v %v", out, err)
	}
	if len(h.publisher.published) != 0 {
		t.Fatal("policy disabled during build but new files were published")
	}
}

func TestSuccessfulDeploymentDoesNotAdvertiseAnOlderPolledHead(t *testing.T) {
	for _, changedDuringBuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll lags manual deploy", true: "newer head during build"}[changedDuringBuild], func(t *testing.T) {
			pkg := ownerPackage()
			pkg["latestKnownVersion"], pkg["updateAvailable"] = "older-polled-head", true
			h := newHarness(t, spaOnlyPackage(), pkg) // fetch returns sha-abc123
			if changedDuringBuild {
				h.deps.Builder = policyChangingBuilder{base: h.builder, change: func() { pkg["latestKnownVersion"] = "newer-during-build" }}
			}
			out, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: rowString(pkg, "id"), Confirmed: true, Placements: firstDeployPlacements(), Actor: Actor{UserId: rowString(pkg, "ownerUserId")}})
			if err != nil || out.Status != StatusSucceeded {
				t.Fatalf("deploy: %+v %v", out, err)
			}
			want := "updateAvailable: false"
			if changedDuringBuild {
				want = "updateAvailable: true"
			}
			for _, q := range h.engine.statements() {
				if strings.HasPrefix(q, "mutation recordPackageDeployedVersion") {
					if !strings.Contains(q, want) || !strings.Contains(q, `deployedVersion: "sha-abc123"`) {
						t.Fatalf("wrong completion: %s", q)
					}
					return
				}
			}
			t.Fatal("deployment version was not recorded")
		})
	}
}

func TestRetainedSnapshotKeepsTheAlreadyKnownNewerHeadPending(t *testing.T) {
	for _, newerAfterAnalyze := range []bool{false, true} {
		for _, confirm := range []bool{false, true} {
			t.Run(map[bool]string{false: "retry", true: "confirm"}[confirm], func(t *testing.T) {
				pkg := ownerPackage()
				pkg["latestKnownVersion"] = "older-polled-head"
				h := newHarness(t, spaOnlyPackage(), pkg)
				first, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: rowString(pkg, "id"), Actor: plainUser()})
				if err != nil || first.Status != StatusAwaitingConfirm {
					t.Fatalf("park: %+v %v", first, err)
				}
				if newerAfterAnalyze {
					pkg["latestKnownVersion"], pkg["updateAvailable"] = "newer-polled-head", true
				}
				h.engine.rows[`query packageDeploymentById(deploymentId: "`+first.DeploymentId+`")`] = []map[string]any{{"id": first.DeploymentId, "packageId": rowString(pkg, "id"), "status": StatusAwaitingConfirm, "sourceVersion": "sha-abc123", "snapshotArtifactId": "blob://packages/snapshots/snap.tar.gz", "report": reportMap(t, first.Report)}}
				req := DeployRequest{PackageId: rowString(pkg, "id"), Actor: plainUser(), Confirmed: true, Placements: firstDeployPlacements()}
				if confirm {
					req.DeploymentId = first.DeploymentId
				} else {
					req.FromDeploymentId = first.DeploymentId
				}
				out, err := Deploy(context.Background(), h.deps, req)
				if err != nil || out.Status != StatusSucceeded {
					t.Fatalf("retained deploy: %+v %v", out, err)
				}
				if h.fetcher.repoFetches != 1 {
					t.Fatal("retained snapshot unexpectedly refetched the repository")
				}
				for _, q := range h.engine.statements() {
					if strings.HasPrefix(q, "mutation recordPackageDeployedVersion") {
						want := "updateAvailable: false"
						if newerAfterAnalyze {
							want = "updateAvailable: true"
						}
						if !strings.Contains(q, want) || !strings.Contains(q, `deployedVersion: "sha-abc123"`) {
							t.Fatalf("lost known newer head: %s", q)
						}
						return
					}
				}
				t.Fatal("retained version was not recorded")
			})
		}
	}

}
