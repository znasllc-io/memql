package goalfixture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work/goaleval"
)

func scenarioByID(t *testing.T, id string) goaleval.Scenario {
	t.Helper()
	for _, s := range catalog(t).Scenarios {
		if s.ID == id {
			return s
		}
	}
	t.Fatal(id)
	return goaleval.Scenario{}
}

func prepared(t *testing.T, id string) *Workspace {
	t.Helper()
	s := scenarioByID(t, id)
	f, err := goaleval.ReadFixture(os.DirFS("../../.."), "test/proving/goals", s.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	w, err := Prepare(context.Background(), t.TempDir(), s, f)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestOraclesRejectClaimsAndVerifyRealRedGreenEvidence(t *testing.T) {
	w := prepared(t, "implement-and-check")
	s := scenarioByID(t, "implement-and-check")
	verify := func() map[string]string {
		t.Helper()
		a, err := w.Verify(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range a {
			out[r.CriterionID] = r.Verdict
		}
		return out
	}
	got := verify()
	if got["correctness"] != "fail" || got["regression"] != "fail" || got["checks"] != "fail" || got["preservation"] != "pass" {
		t.Fatalf("untouched bug: %v", got)
	}
	// A test fixture author supplies the simulated worker output here. This is
	// an oracle regression test, never an entry in the live results catalog.
	tests := []byte("package totals\nimport \"testing\"\nfunc TestFinalElement(t *testing.T){if Sum([]int{2,3})!=5{t.Fatal(\"last value missing\")}}\n")
	if err := os.WriteFile(filepath.Join(w.Checkout, "totals_test.go"), tests, 0600); err != nil {
		t.Fatal(err)
	}
	fixed := strings.ReplaceAll(string(w.Before["totals.go"]), "len(values)-1", "len(values)")
	if err := os.WriteFile(filepath.Join(w.Checkout, "totals.go"), []byte(fixed), 0600); err != nil {
		t.Fatal(err)
	}
	got = verify()
	for id, v := range got {
		if v != "pass" {
			t.Fatalf("%s: %s", id, v)
		}
	}
	// Erasing the useful regression does not get a green from the hidden
	// oracle alone, even though the implementation still works.
	if err := os.WriteFile(filepath.Join(w.Checkout, "totals_test.go"), []byte("package totals\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got = verify(); got["regression"] != "fail" {
		t.Fatal(got)
	}
}

func TestOracleRefusesBuildFailuresLinksAndPreservationChanges(t *testing.T) {
	w := prepared(t, "regression-before-fix")
	s := scenarioByID(t, "regression-before-fix")
	if err := os.WriteFile(filepath.Join(w.Checkout, "totals_test.go"), []byte("package totals\nfunc broken(\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := w.Verify(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range a {
		if v.CriterionID == "red" && v.Verdict == "pass" {
			t.Fatal("build error passed as a regression")
		}
	}
	if err := os.Symlink("/tmp", filepath.Join(w.Checkout, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Snapshot(); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Remove(filepath.Join(w.Checkout, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Command(context.Background(), w.Checkout, "git", "add", "--", "operator-untracked.txt"); err != nil {
		t.Fatal(err)
	}
	a, err = w.Verify(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range a {
		if v.CriterionID == "preservation" && v.Verdict != "fail" {
			t.Fatal("staged unrelated work accepted")
		}
	}
}

func TestOraclesRejectAnUnconditionalFailingReproduction(t *testing.T) {
	w := prepared(t, "regression-before-fix")
	if err := os.WriteFile(filepath.Join(w.Checkout, "totals_test.go"), []byte("package totals\nimport \"testing\"\nfunc TestPretendRegression(t *testing.T){t.Fatal(\"last value missing\")}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	checks, err := w.Verify(context.Background(), scenarioByID(t, "regression-before-fix"))
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if check.CriterionID == "red" && check.Verdict != "fail" {
			t.Fatal("unconditional failure passed as a useful regression")
		}
	}
}

func TestMatrixRefusesSpendAndUndeclaredTargets(t *testing.T) {
	b, err := os.ReadFile("../../../test/proving/goals/matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := goaleval.Decode[goaleval.Matrix](b)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(catalog(t)); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*goaleval.Matrix){
		"paid budget": func(m *goaleval.Matrix) { m.PaidProviderBudgetUSD = 1 },
		"paid route": func(m *goaleval.Matrix) {
			m.Setups = append([]goaleval.SetupRequest{}, m.Setups...)
			m.Setups[0].ReasoningModel = "policy:localFirst"
		},
		"unbounded calls":       func(m *goaleval.Matrix) { m.MaxModelCalls = 0 },
		"production repository": func(m *goaleval.Matrix) { m.GitHubRepositories = []string{"znasllc-io/memql"} },
		"schedule enabled":      func(m *goaleval.Matrix) { m.Schedule.Enabled = true },
	} {
		t.Run(name, func(t *testing.T) {
			v := m
			change(&v)
			if v.Validate(catalog(t)) == nil {
				t.Fatal("unsafe matrix admitted")
			}
		})
	}
	if goaleval.Availability(scenarioByID(t, "checkpoint-restoration")) == "" {
		t.Fatal("missing checkpoint adapter silently skipped injection")
	}
}

func catalog(t *testing.T) goaleval.Catalog {
	t.Helper()
	c, err := goaleval.Load(os.DirFS("../../.."), "test/proving/goals/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRefactorRequiresModelAuthoredTestsBeyondHiddenOracles(t *testing.T) {
	w := prepared(t, "refactor-preserve-behavior")
	s := scenarioByID(t, "refactor-preserve-behavior")
	assertions, err := w.Verify(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]string{}
	for _, a := range assertions {
		verdicts[a.CriterionID] = a.Verdict
	}
	if verdicts["correctness"] != "pass" || verdicts["checks"] != "fail" {
		t.Fatalf("hidden behavior checks cannot substitute for model-authored tests: %v", verdicts)
	}
}
