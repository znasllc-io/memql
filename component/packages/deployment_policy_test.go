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
	out, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: rowString(pkg, "id"), Automatic: true, Confirmed: true, Actor: Actor{UserId: rowString(pkg, "ownerUserId")}})
	if err == nil || out.Status != StatusCancelled {
		t.Fatalf("expected cancelled automatic run, got %+v %v", out, err)
	}
	if len(h.publisher.published) != 0 {
		t.Fatal("policy disabled during build but new files were published")
	}
}
