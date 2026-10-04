package pipelines

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The request crosses NodeService as JSON, so its field names ARE the wire
// shared with the substrate's runner (epic memql#5478). This pins them: a
// renamed tag fails here instead of as a step that silently loses a field on
// another node.
func TestStepRequestWireNamesArePinned(t *testing.T) {
	raw, err := json.Marshal(StepRequest{
		Step:    Step{Shard: ShardRef{Index: 1, Count: 2}},
		Secrets: map[string]string{"TOKEN": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"attempt", "compute", "event", "installationId", "mode", "ownerUserId",
		"pipelineId", "repository", "runAttempt", "runId", "runStartedAt",
		"secrets", "sha", "step", "stepKey", "version", "workRunId",
	}
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, want) {
		t.Errorf("StepRequest JSON keys = %v, want %v", keys, want)
	}
}

func TestStepResultWireNamesArePinned(t *testing.T) {
	raw, err := json.Marshal(StepResult{
		Failure:         &Failure{Code: CodeStepTimeout},
		StartedAt:       "s",
		FinishedAt:      "f",
		LogFileID:       "l",
		ArtifactFileIDs: []string{"a"},
		LogTail:         "t",
		LogLines:        1,
		LogCapped:       true,
		Timings:         map[string]float64{"p": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"artifactFileIds", "exitCode", "failure", "finishedAt", "logCapped",
		"logFileId", "logLines", "logTail", "startedAt", "status", "timings", "where",
	}
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, want) {
		t.Errorf("StepResult JSON keys = %v, want %v", keys, want)
	}
}

// The environment contract: what every runner exports, rendered in one place.
func TestEnvironmentRendersTheContractAndPlatformNamesWin(t *testing.T) {
	req := StepRequest{
		RunID:      "run1",
		WorkRunID:  "wrun1",
		StepKey:    "tests/go-tests#2",
		Repository: Repository{Owner: "acme", Name: "app"},
		SHA:        "abc",
		Mode:       ModeAffected,
		Event:      EventPullRequest,
		Version:    "abc",
		Step: Step{
			Packages: []string{"example.test/a", "example.test/b"},
			Shard:    ShardRef{Index: 2, Count: 4},
		},
		Secrets: map[string]string{"NPM_TOKEN": "s3cret", "MEMQL_SHA": "forged"},
	}
	env := req.Environment()
	want := map[string]string{
		"MEMQL_RUN_ID":      "run1",
		"MEMQL_WORK_RUN_ID": "wrun1",
		"MEMQL_STEP":        "tests/go-tests#2",
		"MEMQL_REPOSITORY":  "acme/app",
		"MEMQL_SHA":         "abc",
		"MEMQL_MODE":        "affected",
		"MEMQL_EVENT":       "pull_request",
		"MEMQL_VERSION":     "abc",
		"MEMQL_PACKAGES":    "example.test/a example.test/b",
		"MEMQL_SHARD":       "2/4",
		"NPM_TOKEN":         "s3cret",
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("Environment() =\n%v\nwant\n%v", env, want)
	}
}

func TestEnvironmentOmitsTheShardOfAnUnshardedStep(t *testing.T) {
	env := StepRequest{}.Environment()
	if _, ok := env["MEMQL_SHARD"]; ok {
		t.Errorf("an unsharded step exported MEMQL_SHARD=%q", env["MEMQL_SHARD"])
	}
}

type recordingExecutor struct{ name string }

func (recordingExecutor) Execute(context.Context, StepRequest) (StepResult, error) {
	return StepResult{}, nil
}
func (recordingExecutor) Cancel(context.Context, string) error { return nil }

func TestRegisterExecutorReturnsWhatItReplaced(t *testing.T) {
	first, second := recordingExecutor{"first"}, recordingExecutor{"second"}
	original := RegisterExecutor(first)
	t.Cleanup(func() { RegisterExecutor(original) })

	if prev := RegisterExecutor(second); prev != first {
		t.Errorf("RegisterExecutor returned %v, want the executor it replaced", prev)
	}
	if got := CurrentExecutor(); got != second {
		t.Errorf("CurrentExecutor() = %v, want the last registered", got)
	}
	RegisterExecutor(nil)
	if got := CurrentExecutor(); got != nil {
		t.Errorf("CurrentExecutor() after a nil registration = %v, want nil", got)
	}
}

func TestNeedsIsTheClosedSetSorted(t *testing.T) {
	want := []string{"display", "docker", "gpu", "macos_tooling", "user_files"}
	if got := Needs(); !reflect.DeepEqual(got, want) {
		t.Errorf("Needs() = %v, want %v", got, want)
	}
	if IsNeed("network") {
		t.Error(`IsNeed("network") = true for a need outside the closed set`)
	}
}

// Every code is in the pipeline_ family and has a class. A code with no class
// would render as nothing in the check run's table.
func TestEveryCodeIsAPipelineCodeWithAClass(t *testing.T) {
	codes := Codes()
	if len(codes) == 0 {
		t.Fatal("Codes() is empty")
	}
	for _, code := range codes {
		if !strings.HasPrefix(code, "pipeline_") {
			t.Errorf("code %q is outside the pipeline_ family", code)
		}
		if _, ok := ClassOf(code); !ok {
			t.Errorf("code %q has no class", code)
		}
	}
	if !sort.StringsAreSorted(codes) {
		t.Errorf("Codes() is not sorted: %v", codes)
	}
}

func TestRefusalErrorNamesItsScope(t *testing.T) {
	r := Refuse(CodeNeedUnknown, "tests/os-checks", "needs %q is not one of %v", "network", Needs())
	if got := r.Error(); !strings.HasPrefix(got, "pipeline_need_unknown (tests/os-checks): ") {
		t.Errorf("Error() = %q", got)
	}
	var nilRefusal *Refusal
	if got := nilRefusal.Error(); got != "" {
		t.Errorf("a nil refusal's Error() = %q, want empty", got)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
