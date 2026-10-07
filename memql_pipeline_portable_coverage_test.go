package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// These are independent lanes, not packages already included in go-tests.
// Hold their actual commands to the hosted lanes while both paths exist.
func TestEnginePortableLanesKeepHostedCommands(t *testing.T) {
	spec := engineManifest(t).Pipeline
	workflow := readManifestCIWorkflow(t)
	for _, name := range []string{"sdk-ts-typecheck", "viewkit-checks"} {
		step, ok := engineDeclaredStep(spec, name)
		if !ok {
			t.Fatalf("missing portable lane %s", name)
		}
		var want []string
		for _, command := range workflow.Jobs[name].Steps {
			if strings.TrimSpace(command.Run) != "" {
				want = append(want, strings.TrimSpace(command.Run))
			}
		}
		got := portableCommands(step.Run)
		if len(want) < 3 || !slices.Equal(got, want) {
			t.Errorf("%s no longer runs the hosted checks in order: got %q want %q", name, got, want)
		}
	}
	conformance, ok := engineDeclaredStep(spec, "mcp-conformance")
	if !ok {
		t.Fatal("MCP conformance missing")
	}
	var hosted string
	for _, step := range workflow.Jobs["conformance"].Steps {
		if strings.Contains(step.Run, "./test/conformance/...") {
			hosted = strings.TrimSpace(step.Run)
		}
	}
	if hosted == "" || !slices.Contains(portableCommands(conformance.Run), hosted) {
		t.Fatalf("conformance does not run its real database suite: %q", hosted)
	}
	if value, found := engineStepExports(conformance.Run, "MEMQL_DIFFERENTIAL_REQUIRED"); !found || value != workflow.Jobs["conformance"].Env["MEMQL_DIFFERENTIAL_REQUIRED"] || value != "1" {
		t.Error("conformance can report a language differential as a passing warning")
	}
	for _, name := range []string{"mcp-conformance", "proving"} {
		step, ok := engineDeclaredStep(spec, name)
		if !ok || !slices.Contains(step.Services, "postgres") {
			t.Fatalf("%s lacks its database", name)
		}
		if value, found := engineStepExports(step.Run, "MEMQL_REQUIRE_DB"); !found || value != "1" {
			t.Errorf("%s can silently skip database cases", name)
		}
		hostedJob := name
		if name == "mcp-conformance" {
			hostedJob = "conformance"
		}
		if value, found := engineStepExports(step.Run, "MEMQL_DATABASE_DSN"); !found || value == "" || value != workflow.Jobs[hostedJob].Env["MEMQL_DATABASE_DSN"] {
			t.Errorf("%s does not reach its isolated database", name)
		}
		for _, extension := range []string{"timescaledb CASCADE", `"uuid-ossp"`, `"pgcrypto"`, "vector"} {
			if !slices.Contains(portableCommands(step.Run), "CREATE EXTENSION IF NOT EXISTS "+extension+";") {
				t.Errorf("%s omits required extension %s", name, extension)
			}
		}
	}
	proving, _ := engineDeclaredStep(spec, "proving")
	for _, command := range []string{
		"go run ./cmd/memql-bench --print-spec > .memql-proving/spec.json",
		"go run ./cmd/memql-bench --do=gate --runner=workbench-linux-arm64 > .memql-proving/envelope.json 2> .memql-proving/gate.txt || status=$?",
		`test "$(wc -l < .memql-proving/envelope.json)" -eq 1`,
		`jq -e '.ok == true' .memql-proving/envelope.json > /dev/null`,
		`test "$status" -eq 0`,
		`go run ./cmd/memql-bench --do=scorecard --check > .memql-proving/scorecard.json`,
		`jq -e '.ok == true' .memql-proving/scorecard.json > /dev/null`,
	} {
		if !slices.Contains(portableCommands(proving.Run), command) {
			t.Errorf("proving lacks failure/evidence gate %s", command)
		}
	}
	if !slices.Contains(proving.Artifacts, ".memql-proving") {
		t.Error("proving discards its report")
	}
}

func portableCommands(script string) []string {
	var commands []string
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != "set -eu" && !strings.HasPrefix(line, "#") {
			commands = append(commands, line)
		}
	}
	return commands
}

func TestEnginePortableTagAndIsolationCoverage(t *testing.T) {
	spec := engineManifest(t).Pipeline
	data, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix yaml.Node `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	type tagMatrix struct {
		Tag     []string `yaml:"tag"`
		Include []struct {
			Tag      string `yaml:"tag"`
			Packages string `yaml:"packages"`
		} `yaml:"include"`
	}

	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	assertCommand := func(name, command string) {
		t.Helper()
		step, found := engineDeclaredStep(spec, name)
		if !found || !slices.Contains(portableCommands(step.Run), command) {
			t.Errorf("%s lost required command %q", name, command)
		}
	}
	var build, tests tagMatrix
	buildNode := workflow.Jobs["build-node-tags"].Strategy.Matrix
	testNode := workflow.Jobs["go-tests-tags"].Strategy.Matrix
	if err := buildNode.Decode(&build); err != nil {
		t.Fatal(err)
	}
	if err := testNode.Decode(&tests); err != nil {
		t.Fatal(err)
	}
	tags, suites := build.Tag, tests.Include
	if len(tags) != 5 || len(suites) != 7 {
		t.Fatal("hosted coverage inventory changed; review parity")
	}
	for _, tag := range tags {
		assertCommand("build-"+tag, "go build -tags "+tag+" github.com/znasllc-io/memql/...")
		assertCommand("build-"+tag, "go vet -tags "+tag+" github.com/znasllc-io/memql/...")
	}
	for _, suite := range suites {
		assertCommand("test-"+suite.Tag, "go test -tags "+suite.Tag+" -timeout=300s "+suite.Packages)
	}
	for _, step := range readManifestCIWorkflow(t).Jobs["build-clustere2e"].Steps {
		if step.Run != "" {
			assertCommand("build-clustere2e", step.Run)
		}
	}
	assertCommand("build-clustere2e", "export GOWORK=off")
	assertCommand("module-boundaries", "bash scripts/ci/module-integrity.sh")
	bashStep, found := engineDeclaredStep(spec, "bash32-smoke")
	if !found || !strings.Contains(string(data), strings.TrimPrefix(bashStep.Image, "docker.io/library/")) || !strings.HasPrefix(bashStep.Image, "docker.io/library/bash@sha256:") {
		t.Fatal("Bash compatibility does not use the hosted lane's pinned interpreter")
	}
	assertCommand("bash32-smoke", "bash ./scripts/ci/bash32-smoke.sh")
}

func TestEngineSecurityScansDoNotTurnFindingsIntoSuccess(t *testing.T) {
	spec := engineManifest(t).Pipeline
	graph, _ := engineImportGraph(t)
	for _, event := range []pipelines.Event{pipelines.EventPullRequest, pipelines.EventPush, pipelines.EventMergeGroup, pipelines.EventRelease} {
		plan, refusal := compileEngineOpening(spec, graph, event, []string{"README.md"})
		if refusal != nil {
			t.Fatal(refusal)
		}
		wantStage := "secrets-history"
		wantCommands := []string{"--unshallow --tags", `--head="$MEMQL_SHA"`, "python3 scripts/ci/gitleaks-history.py", "--jobs=4", "--batch-size=1", "--timeout=20700"}
		forbidden := "gitleaks dir ."
		if event == pipelines.EventPullRequest {
			wantStage = "secrets-current"
			wantCommands = []string{"--redact=100", "gitleaks dir ."}
			forbidden = "gitleaks-history.py"
		}
		count := 0
		for _, step := range plan.Steps() {
			if step.Name != "gitleaks" {
				continue
			}
			count++
			if step.Stage != wantStage || !step.RunAfterFailure || step.Skip != nil || len(step.Artifacts) == 0 || strings.Contains(step.Run, "|| true") {
				t.Fatalf("%s selects the wrong scan, skips it, discards reports or ignores failure: %+v", event, step)
			}
			if strings.Contains(step.Run, "MEMQL_EVENT") || strings.Contains(step.Run, forbidden) {
				t.Fatalf("%s hides scan selection inside the command", event)
			}
			for _, need := range append(wantCommands, "@v8.30.1") {
				if !strings.Contains(step.Run, need) {
					t.Errorf("%s scan omits %s", event, need)
				}
			}
			if event != pipelines.EventPullRequest && (step.CPUMilli != 4000 || step.MemoryMiB != 6144) {
				t.Fatal("history scan allocation differs from its bounded parallel rehearsal")
			}
			if event != pipelines.EventPullRequest && step.TimeoutSeconds != 6*60*60 {
				t.Fatal("history scan must declare the measured long-workload budget explicitly")
			}
		}
		if count != 1 {
			t.Fatalf("%s plans %d secret scans, want exactly one", event, count)
		}
	}
	vuln, found := engineDeclaredStep(spec, "go-vulnerabilities")
	if !found || len(vuln.Artifacts) == 0 || strings.Contains(vuln.Run, "|| true") {
		t.Fatal("Go vulnerability check is absent, discards reports or ignores failure")
	}
	// govulncheck's JSON/SARIF modes return zero even with findings. Preserve
	// its text-mode verdict until machine reports have an explicit evaluator.
	if !strings.Contains(vuln.Run, "@v1.7.0") || !strings.Contains(vuln.Run, "govulncheck github.com/znasllc-io/memql/...") || strings.Contains(vuln.Run, "-format") || !strings.Contains(vuln.Run, `test "$status" -eq 0`) {
		t.Fatal("Go vulnerability findings can disappear behind formatting or incomplete module coverage")
	}
}
