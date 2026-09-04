package packages

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCloseDeploymentRendersNoNullForAbsentFields pins the two shapes a close
// takes that the render used to get wrong, and that the row's schema refuses:
//
//   - the SUCCESS path closes with no error, and `error: null` is not an
//     object;
//   - a run that stops before publish closes with no outcomes, and
//     `deployables: null` is not an array.
//
// jsonLiteral's nil check sees an interface holding a typed nil (*Problem,
// []DeployableOutcome) as non-nil, marshals it, and writes the word null.
// Every affected run then stayed non-terminal with its heartbeat gone, and
// the sweep closed it "abandoned" -- so on aks-memql (2026-09-04) a package
// deploy that had built, rolled and published the storefront reported "this
// cluster lost the node that was running it" while the site served.
func TestCloseDeploymentRendersNoNullForAbsentFields(t *testing.T) {
	rec := &recordingEngine{}
	s := &store{engine: rec}
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 6, 49, 29, 0, time.UTC)

	// The success shape: outcomes, no error.
	if err := s.closeDeployment(ctx, deploymentClose{
		DeploymentId: "v1:platform:packageDeployment:ok",
		Status:       StatusSucceeded,
		Deployables:  []DeployableOutcome{{Name: "storefront", SiteId: "v1:platform:site:s", Hostname: "storefront.example.com"}},
		DslVersion:   "packages/fylo/abc/",
		FinishedAt:   at,
	}); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The early-stop shape: an error, no outcomes yet.
	if err := s.closeDeployment(ctx, deploymentClose{
		DeploymentId: "v1:platform:packageDeployment:early",
		Status:       StatusFailed,
		Error:        &Problem{Code: "deploy_failed", Message: "the store went away", Fatal: true},
		FinishedAt:   at,
	}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(rec.queries) != 2 {
		t.Fatalf("expected two statements, got %d", len(rec.queries))
	}
	for _, q := range rec.queries {
		if strings.Contains(q, "null") {
			t.Errorf("a close statement carries the word null, which the row's schema refuses for both `deployables` and `error`: %s", q)
		}
	}
	if !strings.Contains(rec.queries[0], "deployables: [{") {
		t.Errorf("the success close must carry its outcomes as an array: %s", rec.queries[0])
	}
	if strings.Contains(rec.queries[0], "error:") {
		t.Errorf("a close with no error must not name the error argument at all: %s", rec.queries[0])
	}
	if !strings.Contains(rec.queries[1], "deployables: []") {
		t.Errorf("a close before publish must carry an EMPTY array, not nothing and not null: %s", rec.queries[1])
	}
	if !strings.Contains(rec.queries[1], `error: {"code":"deploy_failed"`) {
		t.Errorf("a close with an error must carry it as an object: %s", rec.queries[1])
	}
}
