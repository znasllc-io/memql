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
		Domain:  "example.test",
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
		"attempt", "compute", "domain", "event", "installationId", "mode", "ownerUserId",
		"pipelineId", "repository", "runAttempt", "runId", "runStartedAt",
		"secrets", "sha", "step", "stepKey", "version", "workRunId",
	}
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, want) {
		t.Errorf("StepRequest JSON keys = %v, want %v", keys, want)
	}
}

func TestStepResultWireNamesArePinned(t *testing.T) {
	raw, err := json.Marshal(StepResult{
		Failure:           &Failure{Code: CodeStepTimeout},
		StartedAt:         "s",
		FinishedAt:        "f",
		LogFileID:         "l",
		ArtifactFileIDs:   []string{"a"},
		ArtifactIntentIDs: []string{strings.Repeat("a", 64)},
		LogTail:           "t",
		LogLines:          1,
		LogCapped:         true,
		Timings:           map[string]float64{"p": 1},
		Notes:             []Failure{{Code: CodeArtifactMissing}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"artifactFileIds", "artifactIntentIds", "exitCode", "failure", "finishedAt", "logCapped",
		"logFileId", "logLines", "logTail", "notes", "startedAt", "status", "timings", "where",
	}
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, want) {
		t.Errorf("StepResult JSON keys = %v, want %v", keys, want)
	}
}

// A compiled step crosses NodeService inside the request and is kept on the
// run's journal, so a notify step's links are on the wire too. Their names are
// pinned, and a step that carries none says nothing about them.
func TestStepLinksWireNamesArePinned(t *testing.T) {
	raw, err := json.Marshal(Step{Kind: StepNotify, Links: []Link{{Label: "Docs", URL: "https://memql.io/docs/"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	links, ok := got["links"].([]any)
	if !ok || len(links) != 1 {
		t.Fatalf("a step's links on the wire = %v, want an array of one in %s", got["links"], raw)
	}
	if keys := sortedKeys(links[0].(map[string]any)); !reflect.DeepEqual(keys, []string{"label", "url"}) {
		t.Errorf("a link's JSON keys = %v, want [label url]", keys)
	}

	raw, err = json.Marshal(Step{Kind: StepNotify})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "links") {
		t.Errorf("a step with no links carries the key anyway: %s", raw)
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

// A step reaches its cluster's public hosts -- api.<domain>, identity.<domain>,
// os.<domain> -- from the domain it is handed, so the domain is a platform
// name like the rest: a secret of the same name never replaces it, and a
// cluster with none configured exports nothing rather than an empty value a
// step would build "https://api." from.
func TestEnvironmentCarriesTheClusterDomain(t *testing.T) {
	req := StepRequest{Domain: "example.test", Secrets: map[string]string{"MEMQL_DOMAIN": "evil.test"}}
	if got := req.Environment()["MEMQL_DOMAIN"]; got != "example.test" {
		t.Fatalf("MEMQL_DOMAIN = %q, want example.test", got)
	}
	if _, ok := (StepRequest{}).Environment()["MEMQL_DOMAIN"]; ok {
		t.Fatal("an empty domain must leave MEMQL_DOMAIN unset, not empty")
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

// The work spine reads a "/" in a step key as a NESTED step (its loaders,
// the head rules and the Nexus drawing), so a pipeline step's key must never
// carry one. This pins the format and, over the record's own example in its
// widest run (every stage, shards included), that no compiled key does.
func TestStepKeysAreNeverNested(t *testing.T) {
	if got := StepKey("tests", "go-tests"); got != "tests.go-tests" {
		t.Errorf(`StepKey("tests", "go-tests") = %q, want "tests.go-tests"`, got)
	}
	plan, refusal := Compile(d7ExampleSpec(), CompileInput{
		Mode: ModeFull, Event: EventPush, Compute: ComputeClusterAndFleet,
		AllowedSecrets: []string{"DEPLOY_TOKEN"},
		Selector:       d7Selector(Selection{Full: true}), Timings: d7Timings,
		PackagePolicies: map[string]PackagePolicy{
			"tests.go-tests": {Coverage: PackageCoverageAll, Filter: PackageFilterAll},
			"tests.db-tests": {Coverage: PackageCoverageAll, Filter: PackageFilterDBGated},
		},
		BucketSelection: &BucketSelection{Included: []string{"os"}},
		StageSelection:  &StageSelection{Included: []string{"checks", "tests", "deploy", "notify"}},
	})
	if refusal != nil {
		t.Fatalf("Compile refused the record's example: %v", refusal)
	}
	for _, step := range plan.Steps() {
		if strings.Contains(step.Key, "/") {
			t.Errorf("step key %q carries a /, which the work spine reads as a nested step", step.Key)
		}
		for _, dep := range step.DependsOn {
			if strings.Contains(dep, "/") {
				t.Errorf("step %q depends on %q, which carries a /", step.Key, dep)
			}
		}
	}
}
