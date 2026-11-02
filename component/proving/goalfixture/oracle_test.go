package goalfixture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

// stubCommand puts a fake command first on PATH, for the ways a command ends
// that a healthy toolchain cannot be asked to produce on demand.
func stubCommand(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// setBound shortens one watchdog so a test can watch it fire.
func setBound(t *testing.T, bound *time.Duration, value time.Duration) {
	t.Helper()
	before := *bound
	*bound = value
	t.Cleanup(func() { *bound = before })
}

// workerAuthoredFix leaves the checkout as a worker that did everything right
// would, so a verdict other than pass can only come from the oracle's own run.
func workerAuthoredFix(t *testing.T, w *Workspace) {
	t.Helper()
	tests := []byte("package totals\nimport \"testing\"\nfunc TestFinalElement(t *testing.T){if Sum([]int{2,3})!=5{t.Fatal(\"last value missing\")}}\n")
	if err := os.WriteFile(filepath.Join(w.Checkout, "totals_test.go"), tests, 0600); err != nil {
		t.Fatal(err)
	}
	fixed := strings.ReplaceAll(string(w.Before["totals.go"]), "len(values)-1", "len(values)")
	if err := os.WriteFile(filepath.Join(w.Checkout, "totals.go"), []byte(fixed), 0600); err != nil {
		t.Fatal(err)
	}
}

func verdicts(assertions []goaleval.Assertion) map[string]string {
	out := map[string]string{}
	for _, a := range assertions {
		out[a.CriterionID] = a.Verdict
	}
	return out
}

// Measured on train memql#6406 and in a starved container: a killed `go test`
// answered "fail", so a correct fix scored as a behavioral failure.
func TestOracleThatCouldNotFinishNeverJudgesTheWork(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for name, stop := range map[string]func(*testing.T){
		"killed by its watchdog": func(t *testing.T) {
			setBound(t, &goTestBound, 50*time.Millisecond)
			stubCommand(t, "go", "exec sleep 60")
		},
		"killed from outside": func(t *testing.T) {
			stubCommand(t, "go", "kill -9 $$")
		},
		"could not start": func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := prepared(t, "implement-and-check")
			workerAuthoredFix(t, w)
			stop(t)
			assertions, err := w.Verify(context.Background(), scenarioByID(t, "implement-and-check"))
			if !errors.Is(err, ErrOracleDidNotFinish) || !strings.Contains(err.Error(), "regression-base") {
				t.Fatalf("Verify error %v does not say which oracle did not finish", err)
			}
			got := verdicts(assertions)
			want := map[string]string{"regression": "unverified", "correctness": "unverified", "checks": "unverified", "preservation": "pass"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("verdicts %v, want %v", got, want)
			}
			// The saved evidence must not read as the behavior it could not check.
			raw, err := os.ReadFile(filepath.Join(w.Root, "oracle-regression.json"))
			if err != nil {
				t.Fatal(err)
			}
			var evidence OracleEvidence
			if err := json.Unmarshal(raw, &evidence); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(evidence.Detail, "did not finish") || !strings.Contains(evidence.Output, "go did not finish after") {
				t.Fatalf("evidence does not say the run did not finish: %+v", evidence)
			}
		})
	}
}

// A reported failure is a finding about the work and survives a sibling run
// that told us nothing; an unfinished run must never be read as either answer.
func TestRegressionOracleDoesNotLetAnUnfinishedRunHideAFailure(t *testing.T) {
	const (
		killed = "kill -9 $$"
		passes = "exit 0"
		fails  = "echo '--- FAIL: TestFinalElement (0.00s)'; exit 1"
	)
	for _, tc := range []struct {
		name, base, control, final, want string
	}{
		{"every run concluded", fails, passes, passes, "pass"},
		{"base killed", killed, passes, passes, "unverified"},
		{"control killed", fails, killed, passes, "unverified"},
		{"final killed", fails, passes, killed, "unverified"},
		{"test does not detect the bug, control killed", passes, killed, passes, "fail"},
		{"test fails correct behavior, final killed", fails, fails, killed, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := prepared(t, "implement-and-check")
			workerAuthoredFix(t, w)
			s := scenarioByID(t, "implement-and-check")
			s.Criteria = []goaleval.Criterion{{ID: "regression"}}
			stubCommand(t, "go", `case "$PWD" in
*oracle-regression-base-*) `+tc.base+` ;;
*oracle-regression-control-*) `+tc.control+` ;;
*oracle-regression-final-*) `+tc.final+` ;;
*) exit 64 ;;
esac`)
			assertions, err := w.Verify(context.Background(), s)
			if got := verdicts(assertions)["regression"]; got != tc.want {
				t.Fatalf("regression verdict %q, want %q", got, tc.want)
			}
			concluded := tc.base != killed && tc.control != killed && tc.final != killed
			if concluded != (err == nil) {
				t.Fatalf("Verify error %v with every run concluded=%v", err, concluded)
			}
		})
	}
}

// One unfinished run already means nothing is scored, so another watchdog on the
// same machine buys nothing: at --cpus=0.02 two of them outlasted the package's
// own -timeout=600s, which panicked before the oracle could say why.
func TestOracleStartsNoFurtherRunAfterOneDidNotFinish(t *testing.T) {
	w := prepared(t, "implement-and-check")
	workerAuthoredFix(t, w)
	runs := filepath.Join(t.TempDir(), "runs")
	stubCommand(t, "go", `echo "$PWD" >> '`+runs+`'; kill -9 $$`)
	_, err := w.Verify(context.Background(), scenarioByID(t, "implement-and-check"))
	if !errors.Is(err, ErrOracleDidNotFinish) {
		t.Fatalf("Verify error %v", err)
	}
	started, readErr := os.ReadFile(runs)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if n := strings.Count(string(started), "\n"); n != 1 || !strings.Contains(string(started), "oracle-regression-base-") {
		t.Fatalf("started %d runs after the first did not finish:\n%s", n-1, started)
	}
}

// Work the oracle can judge without running anything keeps its definite answer.
func TestOracleStillFailsWorkItCanJudgeWithoutRunningIt(t *testing.T) {
	t.Run("no model-authored tests", func(t *testing.T) {
		w := prepared(t, "implement-and-check")
		stubCommand(t, "go", "kill -9 $$")
		assertions, err := w.Verify(context.Background(), scenarioByID(t, "implement-and-check"))
		if !errors.Is(err, ErrOracleDidNotFinish) {
			t.Fatalf("Verify error %v", err)
		}
		got := verdicts(assertions)
		if got["regression"] != "fail" || got["checks"] != "fail" || got["correctness"] != "unverified" {
			t.Fatalf("verdicts %v", got)
		}
	})
	t.Run("preserved bytes changed and git status killed", func(t *testing.T) {
		w := prepared(t, "implement-and-check")
		if err := os.WriteFile(filepath.Join(w.Checkout, "NOTES.txt"), []byte("overwritten\n"), 0600); err != nil {
			t.Fatal(err)
		}
		s := scenarioByID(t, "implement-and-check")
		s.Criteria = []goaleval.Criterion{{ID: "preservation"}}
		stubCommand(t, "git", "kill -9 $$")
		assertions, err := w.Verify(context.Background(), s)
		if !errors.Is(err, ErrOracleDidNotFinish) {
			t.Fatalf("Verify error %v", err)
		}
		if got := verdicts(assertions)["preservation"]; got != "fail" {
			t.Fatalf("preservation %q, want fail", got)
		}
	})
	t.Run("preserved bytes intact and git status killed", func(t *testing.T) {
		w := prepared(t, "implement-and-check")
		s := scenarioByID(t, "implement-and-check")
		s.Criteria = []goaleval.Criterion{{ID: "preservation"}}
		stubCommand(t, "git", "kill -9 $$")
		assertions, err := w.Verify(context.Background(), s)
		if !errors.Is(err, ErrOracleDidNotFinish) {
			t.Fatalf("Verify error %v", err)
		}
		if got := verdicts(assertions)["preservation"]; got != "unverified" {
			t.Fatalf("preservation %q, want unverified", got)
		}
	})
	t.Run("git status reports an error of its own", func(t *testing.T) {
		w := prepared(t, "implement-and-check")
		s := scenarioByID(t, "implement-and-check")
		s.Criteria = []goaleval.Criterion{{ID: "preservation"}}
		stubCommand(t, "git", "echo 'fatal: not a git repository'; exit 128")
		assertions, err := w.Verify(context.Background(), s)
		if err != nil {
			t.Fatalf("Verify error %v for a command that reported its own status", err)
		}
		if got := verdicts(assertions)["preservation"]; got != "fail" {
			t.Fatalf("preservation %q, want fail", got)
		}
	})
}

// The bug itself: a slow `go test` was held to the bound meant for a git command.
func TestGoTestIsNotHeldToTheOrdinaryCommandBound(t *testing.T) {
	w := prepared(t, "implement-and-check")
	workerAuthoredFix(t, w)
	s := scenarioByID(t, "implement-and-check")
	s.Criteria = []goaleval.Criterion{{ID: "correctness"}}
	setBound(t, &commandBound, time.Millisecond)
	stubCommand(t, "go", "sleep 1; exit 0")
	assertions, err := w.Verify(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got := verdicts(assertions)["correctness"]; got != "pass" {
		t.Fatalf("correctness %q, want pass", got)
	}
}
