package packages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/integrations/workbench"
)

const missingBuildShell = "the build command could not be started: fork/exec /bin/sh: no such file or directory"

func automaticRetryHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, spaOnlyPackage(), autoPackage())
	h.deps.Logger = discardLogger()
	rep, err := Analyze(spaOnlyPackage(), Options{SourceVersion: "sha-old"})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.rows["query packageDeployments"] = []map[string]any{{
		"id": "v1:platform:packageDeployment:earlier", "packageId": "v1:platform:package:abc",
		"status": StatusSucceeded, "report": reportMap(t, rep),
	}}
	h.engine.rows["query sitesForPackage"] = []map[string]any{
		{"id": "v1:platform:site:storefront", "hostname": "shop.example.com", "packageDeployableName": "storefront"},
		{"id": "v1:platform:site:docs", "hostname": "docs.example.com", "packageDeployableName": "docs"},
	}
	return h
}

func failedAutomaticAttempt(h *harness, id string) map[string]any {
	return map[string]any{
		"id": id, "packageId": "v1:platform:package:abc", "automatic": true,
		"status": StatusRefused, "sourceVersion": "sha-abc123",
		"snapshotArtifactId": "blob://packages/snapshots/snap.tar.gz",
		"finishedAt":         h.deps.now().Add(-autoDeploymentRetryDelay).Format(time.RFC3339Nano),
		"error":              map[string]any{"code": CodeDeployableBuildFailed, "message": missingBuildShell},
	}
}

func stubAutomaticAttempt(h *harness, row map[string]any) {
	// Include the closing quote so the base ID cannot match a retry ID.
	key := fmt.Sprintf("query packageDeploymentById(deploymentId: %s)", langparser.QuoteString(rowString(row, "id")))
	h.engine.rows[key] = []map[string]any{row}
}

func TestMissingShellAutomaticFailureRetriesSameSnapshotOnWorkingSurface(t *testing.T) {
	h := automaticRetryHarness(t)
	runner := &recordingRunner{answer: &workbench.BuildResult{
		ErrorCode: workbench.BuildCodeFailed, ErrorMessage: missingBuildShell, NodeId: "identity-without-shell",
	}}
	h.deps.Builder = NewWorkbenchBuilder(runner, discardLogger())
	base := policyDeploymentId(autoPackage(), "sha-abc123")
	if _, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123"); RefusalCode(err) != CodeDeployableBuildFailed {
		t.Fatalf("missing-shell run should fail at the real build adapter: %v", err)
	}
	if len(h.publisher.published) != 0 || !h.engine.sawStatement(missingBuildShell) {
		t.Fatal("failed build published or lost the infrastructure error")
	}
	failed := failedAutomaticAttempt(h, base)
	stubAutomaticAttempt(h, failed)
	runner.answer = nil // The elected workbench now builds successfully.
	if started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123"); err != nil || !started {
		t.Fatalf("later automatic retry did not start: %v %v", started, err)
	}
	if len(h.publisher.published) != 2 || h.fetcher.repoFetches != 1 || len(h.publisher.snapshotReads) != 1 {
		t.Fatalf("retry must publish both apps from the original snapshot: published=%v fetches=%d reads=%v", h.publisher.published, h.fetcher.repoFetches, h.publisher.snapshotReads)
	}
	retry := base + "-retry-2"
	last := runner.requests[len(runner.requests)-1]
	if last.DeploymentId != retry || last.OwnerUserId != rowString(autoPackage(), "ownerUserId") {
		t.Fatalf("retry lost distinct identity or owner: %+v", last)
	}
	if !h.engine.sawStatement(`fromDeploymentId: "`+base+`"`) || !h.engine.sawStatement(`mutation closePackageDeployment(deploymentId: "`+retry+`", status: "succeeded"`) {
		t.Fatal("retry lineage or successful terminal row was not recorded")
	}
	if rowString(failed, "status") != StatusRefused {
		t.Fatal("retry changed the prior failure")
	}
}

func TestAutomaticInfrastructureRetryPreservesStopsAndDelay(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"canceled", func(r map[string]any) { r["status"] = StatusCancelled }},
		{"confirmation needed", func(r map[string]any) { r["status"] = StatusAwaitingConfirm }},
		{"already building", func(r map[string]any) { r["status"] = StatusBuilding }},
		{"succeeded", func(r map[string]any) { r["status"] = StatusSucceeded }},
		{"abandoned unknown stage", func(r map[string]any) { r["status"] = StatusAbandoned }},
		{"not automatic", func(r map[string]any) { r["automatic"] = false }},
		{"different package", func(r map[string]any) { r["packageId"] = "v1:platform:package:other" }},
		{"different source", func(r map[string]any) { r["sourceVersion"] = "sha-other" }},
		{"missing snapshot", func(r map[string]any) { delete(r, "snapshotArtifactId") }},
		{"missing finish time", func(r map[string]any) { delete(r, "finishedAt") }},
		{"cooldown", func(r map[string]any) { r["finishedAt"] = "2026-09-01T11:59:00Z" }},
		{"future timestamp", func(r map[string]any) { r["finishedAt"] = "2026-09-02T00:00:00Z" }},
		{"source failure", func(r map[string]any) {
			r["error"] = map[string]any{"code": CodeSourceUnreadable, "message": missingBuildShell}
		}},
		{"credential revoked", func(r map[string]any) { r["error"] = map[string]any{"code": CodeCredentialRevoked} }},
		{"build timed out", func(r map[string]any) { r["error"] = map[string]any{"code": CodeDeployableBuildTimeout} }},
		{"broken source command", func(r map[string]any) {
			r["error"] = map[string]any{"code": CodeDeployableBuildFailed, "message": "the build command exited with code 1"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := automaticRetryHarness(t)
			row := failedAutomaticAttempt(h, policyDeploymentId(autoPackage(), "sha-abc123"))
			tc.change(row)
			stubAutomaticAttempt(h, row)
			if started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123"); err != nil || started {
				t.Fatalf("stop was retried: started=%v err=%v", started, err)
			}
			if len(h.builder.built) != 0 || h.engine.sawStatement("mutation openPackageDeployment") {
				t.Fatal("stop opened or built a deployment")
			}
		})
	}
}

func TestAutomaticRetryCapAndConcurrentFeedIdentity(t *testing.T) {
	h := automaticRetryHarness(t)
	base := policyDeploymentId(autoPackage(), "sha-abc123")
	stubAutomaticAttempt(h, failedAutomaticAttempt(h, base))
	for observation := 0; observation < 2; observation++ {
		id, from, err := h.deps.nextAutoDeployment(context.Background(), autoPackage(), "sha-abc123")
		if err != nil || id != base+"-retry-2" || from != base {
			t.Fatalf("feeds must converge on the same retry: %q %q %v", id, from, err)
		}
	}
	stubAutomaticAttempt(h, failedAutomaticAttempt(h, base+"-retry-2"))
	id, from, err := h.deps.nextAutoDeployment(context.Background(), autoPackage(), "sha-abc123")
	if err != nil || id != base+"-retry-3" || from != base+"-retry-2" {
		t.Fatalf("wrong final retry: %q %q %v", id, from, err)
	}
	stubAutomaticAttempt(h, failedAutomaticAttempt(h, id))
	for poll := 0; poll < 4; poll++ {
		before := len(h.engine.statements())
		id, _, err := h.deps.nextAutoDeployment(context.Background(), autoPackage(), "sha-abc123")
		if err != nil || id != "" || len(h.engine.statements())-before != 3 {
			t.Fatalf("exhausted retry must stop after three exact reads: %q %v", id, err)
		}
	}
}

func TestAutomaticRetryRespectsLiveRunAndChangedPlan(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			h := automaticRetryHarness(t)
			base := policyDeploymentId(autoPackage(), "sha-abc123")
			stubAutomaticAttempt(h, failedAutomaticAttempt(h, base))
			h.publisher.stored = map[string][]byte{"blob://packages/snapshots/snap.tar.gz": fixtureTarGz(spaOnlyPackage())}
			if live {
				h.engine.rows["query packageDeployments"] = []map[string]any{{"status": StatusBuilding}}
			} else {
				// The last approved plan now differs, so the retry must park.
				h.engine.rows["query packageDeployments"][0]["report"] = reportMap(t, &Report{OK: true})
			}
			started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123")
			if err != nil || started == live || len(h.builder.built) != 0 {
				t.Fatalf("retry ignored live run/plan gate: started=%v err=%v built=%v", started, err, h.builder.built)
			}
			if !live && !h.engine.sawStatement(`"awaiting_confirm"`) {
				t.Fatal("changed plan did not wait for confirmation")
			}
		})
	}
}

func TestAutomaticRetryLookupFailureIsFailClosed(t *testing.T) {
	h := automaticRetryHarness(t)
	h.engine.fail = map[string]error{"query packageDeploymentById": errors.New("read unavailable")}
	if started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123"); started || err == nil || !strings.Contains(err.Error(), "read unavailable") {
		t.Fatalf("lookup failure started a run: %v %v", started, err)
	}
}
