package packages

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"
)

// report.dslChanges says, at analysis and before the confirm gate, whether
// deploying this plan would change the cluster's active DSL set -- the answer
// the candidate refusal acts on at confirm. MemQL OS reads it to offer
// "Deploy as candidate" exactly when the engine would accept it: most product
// bundles ship MemQL that is already active, so "the package carries DSL"
// (report.dslDomains) would hide the feature in the common case.
//
// THREE ANSWERS, and absent is one of them: a node that cannot read the
// pointer does not know, and saying false there would let a consumer offer
// what the confirm then refuses.

// analyzeOnly opens a run without confirming it and returns the report it
// recorded, as the confirm gate shows it.
func analyzeOnly(t *testing.T, h *harness) *Report {
	t.Helper()
	out, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId: "v1:platform:package:abc",
		Actor:     mayDeployDsl(),
	})
	if err != nil || out == nil || out.Report == nil {
		t.Fatalf("analysis: %+v, %v", out, err)
	}
	return out.Report
}

// recordedReport is the report statement the run row was written with.
func recordedReport(t *testing.T, h *harness) string {
	t.Helper()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	q := statementContaining(h.engine.queries, "mutation recordPackageDeploymentReport")
	if q == "" {
		t.Fatal("no report was recorded")
	}
	return q
}

func activeSetMatching(t *testing.T, h *harness, tree fs.FS, domain string) map[string]string {
	t.Helper()
	sub, err := fs.Sub(tree, path.Join(DslRoot, domain))
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := h.stager.PrefixFor(domain, sub)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{domain: prefix}
}

func TestTheReportSaysWhetherDeployingWouldChangeTheActiveDsl(t *testing.T) {
	t.Run("DSL equal to the active set", func(t *testing.T) {
		tree := validPackage()
		h := newHarness(t, tree, ownerPackage())
		h.stager.active = activeSetMatching(t, h, tree, "acme")
		rep := analyzeOnly(t, h)
		if rep.DslChanges == nil || *rep.DslChanges {
			t.Fatalf("dslChanges = %v, want false for DSL the cluster already runs", rep.DslChanges)
		}
		if !strings.Contains(recordedReport(t, h), `"dslChanges":false`) {
			t.Fatalf("the recorded report does not carry dslChanges:false:\n%s", recordedReport(t, h))
		}
	})

	t.Run("a changed domain", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		h.stager.active = map[string]string{"acme": "packages/acme/an-older-tree/"}
		rep := analyzeOnly(t, h)
		if rep.DslChanges == nil || !*rep.DslChanges {
			t.Fatalf("dslChanges = %v, want true for a domain that differs from the active set", rep.DslChanges)
		}
		if !strings.Contains(recordedReport(t, h), `"dslChanges":true`) {
			t.Fatalf("the recorded report does not carry dslChanges:true:\n%s", recordedReport(t, h))
		}
	})

	t.Run("a plan with no DSL", func(t *testing.T) {
		h := newHarness(t, spaOnlyPackage(), ownerPackage())
		rep := analyzeOnly(t, h)
		if rep.DslChanges == nil || *rep.DslChanges {
			t.Fatalf("dslChanges = %v, want false for a package that ships no DSL", rep.DslChanges)
		}
	})

	// UNKNOWN IS ABSENT. Neither answer is true of a node that cannot read the
	// pointer, so the key is left out and the consumer reads it as unknown.
	t.Run("an active set that cannot be read", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		h.deps.Stager = unreadableStager{fakeStager: h.stager}
		rep := analyzeOnly(t, h)
		if rep.DslChanges != nil {
			t.Fatalf("dslChanges = %v, want absent when the active set cannot be read", *rep.DslChanges)
		}
		if strings.Contains(recordedReport(t, h), "dslChanges") {
			t.Fatalf("an unknown answer reached the recorded report:\n%s", recordedReport(t, h))
		}
	})
	t.Run("a node with no stager", func(t *testing.T) {
		h := newHarness(t, validPackage(), ownerPackage())
		h.deps.Stager = nil
		if rep := analyzeOnly(t, h); rep.DslChanges != nil {
			t.Fatalf("dslChanges = %v, want absent with no stager to ask", *rep.DslChanges)
		}
	})
}

// unreadableStager answers everything but the pointer.
type unreadableStager struct{ *fakeStager }

func (unreadableStager) ReadActiveSet(context.Context) (map[string]string, error) {
	return nil, errors.New("object storage is unreachable")
}

// THE CONFIRM ASKS AGAIN. dslChanges is what the pointer said when the run was
// analysed; another deploy can move the pointer before somebody presses
// Confirm, so the candidate refusal re-reads it rather than trusting the
// report. Here the gate showed dslChanges:false, the pointer then moved, and
// the confirm of a candidate run is refused before anything is built.
func TestTheCandidateRefusalRereadsThePointerAtConfirm(t *testing.T) {
	tree := validPackage()
	h := newHarness(t, tree, ownerPackage())
	h.stager.active = activeSetMatching(t, h, tree, "acme")
	parked, err := Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:  "v1:platform:package:abc",
		Actor:      mayDeployDsl(),
		Placements: candidatePlacements(),
	})
	if err != nil || parked.Report == nil || parked.Report.DslChanges == nil || *parked.Report.DslChanges {
		t.Fatalf("the gate should show dslChanges:false, got %+v, %v", parked, err)
	}

	// Another source's deploy moves the pointer while this run waits.
	h.stager.active = map[string]string{"acme": "packages/acme/somebody-elses-tree/"}
	h.engine.rows["query packageDeploymentById"] = []map[string]any{parkedRun(parked.DeploymentId, "v1:platform:package:abc", nil)}

	_, err = Deploy(context.Background(), h.deps, DeployRequest{
		PackageId:    "v1:platform:package:abc",
		Actor:        mayDeployDsl(),
		Confirmed:    true,
		DeploymentId: parked.DeploymentId,
		Placements:   candidatePlacements(),
	})
	if err == nil || !strings.Contains(err.Error(), "candidate") {
		t.Fatalf("a candidate confirmed after the pointer moved was not refused: %v", err)
	}
	if len(h.builder.built) != 0 || h.roller.rolls != 0 || len(h.publisher.published) != 0 {
		t.Fatalf("the refused confirm built %v, rolled %d times and published %v", h.builder.built, h.roller.rolls, h.publisher.published)
	}
}
