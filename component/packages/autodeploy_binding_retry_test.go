package packages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Mirrors the persisted failure after workbench built successfully but the
// former guard rejected updateSiteBundle's inherited serving binding.
func inheritedBindingRetryHarness(t *testing.T) (*harness, map[string]any, map[string]any) {
	t.Helper()
	h := automaticRetryHarness(t)
	base := policyDeploymentId(autoPackage(), "sha-abc123")
	stubAutomaticAttempt(h, failedAutomaticAttempt(h, base)) // distroless first run
	prior := failedAutomaticAttempt(h, base+"-retry-2")
	owner := rowString(autoPackage(), "ownerUserId")
	siteID := "v1:platform:site:storefront"
	message := fmt.Sprintf("edge: pointing %s at blob://sites/%s/vadb660f16678/: v1:platform:site: %q may not bind this storefront to v1:shopify:store %q -- it is not a store this caller can read. Stores are read by developers and cluster owners; binding a storefront to one you cannot read would publish that store's Storefront token under this storefront's hostname.", siteID, siteID, owner, "acme")
	prior["status"], prior["ownerUserId"], prior["requestedBy"] = StatusFailed, owner, owner
	prior["error"] = map[string]any{"code": "deploy_failed", "fatal": true, "message": message}
	prior["deployables"] = []any{map[string]any{"name": "storefront", "siteId": siteID, "refusal": map[string]any{"code": "deployable_publish_failed", "fatal": true, "scope": "storefront", "message": message}}}
	site := map[string]any{"id": siteID, "ownerUserId": owner, "packageId": rowString(autoPackage(), "id"), "packageDeployableName": "storefront", "kind": "shopify_storefront", "binding": map[string]any{"storeId": "v1:shopify:store:acme"}}
	h.engine.rows["query siteById"] = []map[string]any{site}
	stubAutomaticAttempt(h, prior)
	h.publisher.stored = map[string][]byte{"blob://packages/snapshots/snap.tar.gz": fixtureTarGz(spaOnlyPackage())}
	return h, prior, site
}

func TestInheritedBindingPublicationFailureUsesRemainingSnapshotRetry(t *testing.T) {
	h, prior, _ := inheritedBindingRetryHarness(t)
	base := policyDeploymentId(autoPackage(), "sha-abc123")
	started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123")
	if err != nil || !started {
		t.Fatalf("corrected publication must retry: started=%v err=%v", started, err)
	}
	if len(h.publisher.published) != 2 || h.fetcher.repoFetches != 0 || len(h.publisher.snapshotReads) != 1 {
		t.Fatalf("retry must reuse the snapshot: published=%v fetches=%d reads=%v", h.publisher.published, h.fetcher.repoFetches, h.publisher.snapshotReads)
	}
	if !h.engine.sawStatement(`fromDeploymentId: "`+base+`-retry-2"`) || !h.engine.sawStatement(`mutation closePackageDeployment(deploymentId: "`+base+`-retry-3", status: "succeeded"`) {
		t.Fatal("did not use the remaining deterministic slot/lineage")
	}
	if rowString(prior, "status") != StatusFailed {
		t.Fatal("retry erased the original failure")
	}
	// If the final slot fails too, later polls must stay stopped.
	last := make(map[string]any, len(prior))
	for k, v := range prior {
		last[k] = v
	}
	last["id"] = base + "-retry-3"
	stubAutomaticAttempt(h, last)
	if id, _, err := h.deps.nextAutoDeployment(context.Background(), autoPackage(), "sha-abc123"); err != nil || id != "" {
		t.Fatalf("exhausted budget restarted: %q %v", id, err)
	}
}

func TestInheritedBindingRetryRefusesChangedAuthorityAndOtherFailures(t *testing.T) {
	cases := []struct {
		name   string
		change func(*harness, map[string]any, map[string]any)
	}{
		{"store changed", func(_ *harness, _ map[string]any, s map[string]any) {
			s["binding"] = map[string]any{"storeId": "other"}
		}},
		{"store detached", func(_ *harness, _ map[string]any, s map[string]any) { delete(s, "binding") }},
		{"site owner changed", func(_ *harness, _ map[string]any, s map[string]any) { s["ownerUserId"] = "other" }},
		{"site source changed", func(_ *harness, _ map[string]any, s map[string]any) { s["packageId"] = "other" }},
		{"site deployable changed", func(_ *harness, _ map[string]any, s map[string]any) { s["packageDeployableName"] = "other" }},
		{"site kind changed", func(_ *harness, _ map[string]any, s map[string]any) { s["kind"] = "static" }},
		{"site not readable", func(h *harness, _ map[string]any, _ map[string]any) { h.engine.rows["query siteById"] = nil }},
		{"run owner changed", func(_ *harness, r map[string]any, _ map[string]any) { r["ownerUserId"] = "other" }},
		{"requestor changed", func(_ *harness, r map[string]any, _ map[string]any) { r["requestedBy"] = "other" }},
		{"canceled", func(_ *harness, r map[string]any, _ map[string]any) { r["status"] = StatusCancelled }},
		{"cancellation requested", func(_ *harness, r map[string]any, _ map[string]any) { r["cancelRequested"] = true }},
		{"confirmation", func(_ *harness, r map[string]any, _ map[string]any) { r["status"] = StatusAwaitingConfirm }},
		{"credential revoked", func(_ *harness, r map[string]any, _ map[string]any) {
			r["error"].(map[string]any)["code"] = CodeCredentialRevoked
		}},
		{"manual", func(_ *harness, r map[string]any, _ map[string]any) { r["automatic"] = false }},
		{"source moved", func(_ *harness, r map[string]any, _ map[string]any) { r["sourceVersion"] = "other" }},
		{"snapshot missing", func(_ *harness, r map[string]any, _ map[string]any) { delete(r, "snapshotArtifactId") }},
		{"cooldown", func(_ *harness, r map[string]any, _ map[string]any) { r["finishedAt"] = "2026-09-01T11:59:00Z" }},
		{"outcome missing", func(_ *harness, r map[string]any, _ map[string]any) { delete(r, "deployables") }},
		{"other authorization failure", func(_ *harness, r map[string]any, _ map[string]any) {
			r["error"].(map[string]any)["message"] = "edge: publishing refused by organization capability"
		}},
		{"different rejected store", func(_ *harness, r map[string]any, _ map[string]any) {
			replaceBindingFailureMessage(r, `store "acme"`, `store "other"`)
		}},
		{"different failing actor", func(_ *harness, r map[string]any, _ map[string]any) {
			replaceBindingFailureMessage(r, `"v1:identity:user:someone"`, `"other"`)
		}},
		{"different site pointer", func(_ *harness, r map[string]any, _ map[string]any) {
			replaceBindingFailureMessage(r, "edge: pointing v1:platform:site:storefront", "edge: pointing v1:platform:site:other")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, prior, site := inheritedBindingRetryHarness(t)
			tc.change(h, prior, site)
			started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123")
			if err != nil || started || h.engine.sawStatement("mutation openPackageDeployment") || len(h.builder.built) != 0 {
				t.Fatalf("unsafe retry: started=%v err=%v built=%v", started, err, h.builder.built)
			}
		})
	}
}

func replaceBindingFailureMessage(row map[string]any, from, to string) {
	problem := row["error"].(map[string]any)
	problem["message"] = strings.ReplaceAll(rowString(problem, "message"), from, to)
	row["deployables"].([]any)[0].(map[string]any)["refusal"].(map[string]any)["message"] = problem["message"]
}

func TestInheritedBindingRetryKeepsLiveRunAndConfirmationGates(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			h, _, _ := inheritedBindingRetryHarness(t)
			if live {
				h.engine.rows["query packageDeployments"] = []map[string]any{{"status": StatusBuilding}}
			} else {
				h.engine.rows["query packageDeployments"][0]["report"] = reportMap(t, &Report{OK: true})
			}
			started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123")
			if err != nil || started == live || len(h.builder.built) != 0 {
				t.Fatalf("ignored live run/confirmation: %v %v", started, err)
			}
			if !live && !h.engine.sawStatement(`"awaiting_confirm"`) {
				t.Fatal("changed plan did not park")
			}
		})
	}
}

func TestInheritedBindingRetrySiteReadFailureIsFailClosed(t *testing.T) {
	h, _, _ := inheritedBindingRetryHarness(t)
	h.engine.fail = map[string]error{"query siteById": errors.New("site read unavailable")}
	started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-abc123")
	if started || err == nil || !strings.Contains(err.Error(), "site read unavailable") || h.engine.sawStatement("mutation openPackageDeployment") {
		t.Fatalf("read failure allowed retry: %v %v", started, err)
	}
}
