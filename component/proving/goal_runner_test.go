package proving

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work/goaleval"
	"github.com/znasllc-io/memql/sdk/go/client"
)

type evaluationClient struct {
	t                             *testing.T
	fix                           bool
	readError                     bool
	status                        string
	created, cancelled, published int
	args                          client.CreateGoalArgs
}

func (f *evaluationClient) ActiveEmbedderBinding(context.Context, client.ActiveEmbedderBindingArgs) (*client.Result, error) {
	return client.ResultFromRows([]client.Row{{"id": "primary", "providerRef": "fleet:test-embedder", "dimensions": 2}}), nil
}

func (f *evaluationClient) CreateGoal(_ context.Context, args client.CreateGoalArgs) (*client.Result, error) {
	f.created++
	f.args = args
	if !args.NoPaidProviders || args.Source != "fleet:qwen3.8:27b" || args.Ceilings["maxModelCalls"] != 80 {
		f.t.Fatalf("unguarded submission: %+v", args)
	}
	dir := args.Input["workspace"].(string)
	if f.fix {
		before, err := os.ReadFile(filepath.Join(dir, "totals.go"))
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "totals.go"), []byte(strings.ReplaceAll(string(before), "len(values)-1", "len(values)")), 0600); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "totals_test.go"), []byte("package totals\nimport \"testing\"\nfunc TestSum(t *testing.T){if Sum([]int{1,9})!=10{t.Fatal(\"last value lost\")}}\n"), 0600); err != nil {
		f.t.Fatal(err)
	}
	return client.ResultFromRows([]client.Row{{"goalId": "goal-eval", "runId": "run-eval"}}), nil
}
func (f *evaluationClient) CancelGoal(context.Context, client.CancelGoalArgs) (*client.Result, error) {
	f.cancelled++
	f.status = "cancelled"
	return client.ResultFromRows(nil), nil
}
func (f *evaluationClient) WorkRunForOwner(context.Context, client.WorkRunForOwnerArgs) (*client.Result, error) {
	if f.readError {
		return nil, errors.New("observer disconnected")
	}
	status := f.status
	if status == "" {
		status = "succeeded"
	}
	return client.ResultFromRows([]client.Row{{"id": "run-eval", "goalId": "goal-eval", "status": status, "routing": map[string]any{"source": "fleet:qwen3.8:27b", "noPaidProviders": true}}}), nil
}
func (f *evaluationClient) WorkModelCallsForOwnerRun(context.Context, client.WorkModelCallsForOwnerRunArgs) (*client.Result, error) {
	return client.ResultFromRows([]client.Row{{"id": "model-eval", "model": "qwen3.8:27b", "provider": "fleet:qwen3.8:27b", "served": "local", "promptRef": "workAgentReply", "decision": map[string]any{"requestedLevel": "reasoning"}}}), nil
}
func (f *evaluationClient) WorkObservationsForOwnerRun(context.Context, client.WorkObservationsForOwnerRunArgs) (*client.Result, error) {
	args, _ := json.Marshal(map[string]any{"action": "exec", "args": map[string]any{"cmd": "go test -count=1 ./...", "cwd": f.args.Input["workspace"]}})
	return client.ResultFromRows([]client.Row{{"id": "check-eval", "kind": "tool_result", "data": map[string]any{"tool": "workerHost", "isError": false, "args": string(args)}}}), nil
}
func (f *evaluationClient) WorkStepsForOwnerRun(context.Context, client.WorkStepsForOwnerRunArgs) (*client.Result, error) {
	return client.ResultFromRows(nil), nil
}
func (f *evaluationClient) WorkRecordGoalEvaluation(context.Context, client.WorkRecordGoalEvaluationArgs) (*client.Result, error) {
	f.published++
	return client.ResultFromRows([]client.Row{{"receiptId": "evaluation-published"}}), nil
}

func testGoalRunner(t *testing.T, f *evaluationClient) (*GoalRunner, goaleval.Catalog, goaleval.Matrix) {
	t.Helper()
	files := os.DirFS("../..")
	catalog, err := goaleval.Load(files, "test/proving/goals/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../test/proving/goals/matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	matrix, err := goaleval.Decode[goaleval.Matrix](raw)
	if err != nil {
		t.Fatal(err)
	}
	matrix.ScenarioIDs = []string{"implement-and-check"}
	return &GoalRunner{Client: f, Files: files, CatalogDir: "test/proving/goals", OutputDir: t.TempDir(), EngineCommit: strings.Repeat("e", 40), HarnessCommit: strings.Repeat("f", 40), PollInterval: time.Millisecond}, catalog, matrix
}

func TestGoalWorkflowRequiresArtifactEvidenceBeyondPlatformSuccess(t *testing.T) {
	for _, fix := range []bool{false, true} {
		t.Run(map[bool]string{false: "false-success", true: "fixed"}[fix], func(t *testing.T) {
			f := &evaluationClient{t: t, fix: fix}
			runner, catalog, matrix := testGoalRunner(t, f)
			report, err := runner.Run(context.Background(), catalog, matrix)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Attempts) != 1 || report.Attempts[0].Result == nil {
				t.Fatalf("missing result: %+v", report)
			}
			want := "failed"
			if fix {
				want = "autonomous"
			}
			if got := report.Attempts[0].Result.Outcome; got != want {
				t.Fatalf("%s, want %s", got, want)
			}
			if f.created != 1 || f.published != 1 || f.cancelled != 0 {
				t.Fatalf("effects: %+v", f)
			}
		})
	}
}

func TestGoalWorkflowPreservesObserverFailureAndStopsOwnedWork(t *testing.T) {
	f := &evaluationClient{t: t, readError: true}
	runner, catalog, matrix := testGoalRunner(t, f)
	report, err := runner.Run(context.Background(), catalog, matrix)
	if err == nil || f.cancelled != 1 || f.published != 0 || len(report.Attempts) != 1 || report.Attempts[0].State != "observer-error" {
		t.Fatalf("report=%+v error=%v client=%+v", report, err, f)
	}
	paths, _ := filepath.Glob(filepath.Join(runner.OutputDir, "memql-evaluation-*", "observer-error.json"))
	if len(paths) != 1 {
		t.Fatal("lost cancellation diagnostic")
	}
}

func TestGoalWorkflowDoesNotRunAnUnimplementedInjection(t *testing.T) {
	f := &evaluationClient{t: t}
	runner, catalog, matrix := testGoalRunner(t, f)
	matrix.ScenarioIDs = []string{"checkpoint-restoration"}
	report, err := runner.Run(context.Background(), catalog, matrix)
	if err != nil {
		t.Fatal(err)
	}
	if f.created != 0 || report.Attempts[0].State != "not-run" || report.Attempts[0].Result != nil {
		t.Fatal("missing injection became measured work")
	}
}

func TestGoalWorkflowWaitsForCancellationBeforeOracles(t *testing.T) {
	f := &evaluationClient{t: t, status: "waiting"}
	runner, catalog, matrix := testGoalRunner(t, f)
	report, err := runner.Run(context.Background(), catalog, matrix)
	if err != nil {
		t.Fatal(err)
	}
	if f.cancelled != 1 || report.Attempts[0].Result.Outcome != "blocked" || f.status != "cancelled" {
		t.Fatalf("blocked work: %+v %+v", report, f)
	}
}
