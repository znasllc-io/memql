package packages

import (
	"context"
	"strings"
	"testing"
)

// TestRollbackExecutesTheReversedOrder is the D6 law's other half, and the one
// that is easy to get symmetrically wrong: rolling back in the FORWARD order
// would leave new application code talking to old DSL for the width of the
// rollout, which is the exact window the ordering exists to close.
func TestRollbackExecutesTheReversedOrder(t *testing.T) {
	prior := map[string]any{
		"id":         "v1:platform:packageDeployment:old",
		"packageId":  "v1:platform:package:abc",
		"status":     StatusSucceeded,
		"dslVersion": "packages/acme/0123456789abcdef/",
		"deployables": []any{
			map[string]any{"name": "storefront", "siteId": "v1:platform:site:s1", "bundleRef": "blob://sites/s1/v1/"},
			map[string]any{"name": "docs", "siteId": "v1:platform:site:s2", "bundleRef": "blob://sites/s2/v1/"},
		},
	}
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{prior}
	// The cluster is currently on a NEWER prefix, so the pointer has to move.
	h.stager.active = map[string]string{"acme": "packages/acme/ffffffffffffffff/"}

	// Order is observed through a single log the fakes share.
	var order []string
	h.publisher.onRepoint = func() { order = append(order, "publish") }
	h.stager.onWrite = func() { order = append(order, "pointer") }
	h.roller.onRoll = func() { order = append(order, "roll") }

	res, err := Rollback(context.Background(), h.deps, RollbackRequest{
		PackageId:    "v1:platform:package:abc",
		DeploymentId: "v1:platform:packageDeployment:old",
		Actor:        clusterOwner(),
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}

	want := []string{"publish", "publish", "pointer", "roll"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("rollback must publish back FIRST, then move the pointer, then roll.\n got %v\nwant %v", order, want)
	}
	if !res.Rolled || res.DslVersion != "packages/acme/0123456789abcdef/" {
		t.Fatalf("restored state wrong: %+v", res)
	}
	if len(h.publisher.repointed) != 2 {
		t.Fatalf("both sites must be repointed: %v", h.publisher.repointed)
	}
	if got := h.stager.active["acme"]; got != "packages/acme/0123456789abcdef/" {
		t.Fatalf("the pointer must be back on the prior prefix, got %q", got)
	}
}

func TestRollbackRefusesADeploymentThatWasNeverLive(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":        "v1:platform:packageDeployment:bad",
		"packageId": "v1:platform:package:abc",
		"status":    StatusRefused,
	}}
	if _, err := Rollback(context.Background(), h.deps, RollbackRequest{
		PackageId:    "v1:platform:package:abc",
		DeploymentId: "v1:platform:packageDeployment:bad",
		Actor:        clusterOwner(),
	}); err == nil {
		t.Fatal("a refused deployment carries no state to restore")
	}
}

func TestRollbackToADslVersionNeedsClusterOwner(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":         "v1:platform:packageDeployment:old",
		"packageId":  "v1:platform:package:abc",
		"status":     StatusSucceeded,
		"dslVersion": "packages/acme/0123456789abcdef/",
	}}
	_, err := Rollback(context.Background(), h.deps, RollbackRequest{
		PackageId:    "v1:platform:package:abc",
		DeploymentId: "v1:platform:packageDeployment:old",
		Actor:        plainUser(),
	})
	if RefusalCode(err) != CodeDslRequiresClusterOwner {
		t.Fatalf("putting a DSL version back changes what the cluster can do; want %s, got %v",
			CodeDslRequiresClusterOwner, err)
	}
	// Nothing may have moved.
	if len(h.publisher.repointed) != 0 || h.stager.written != 0 || h.roller.rolls != 0 {
		t.Fatalf("the gate must fire before anything moves")
	}
}
