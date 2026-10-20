package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// These are independent lanes, not packages already included in go-tests.
// Hold their actual commands to the supported suites and failure contracts.
func TestEnginePortableLanesKeepRequiredCommands(t *testing.T) {
	spec := engineManifest(t).Pipeline
	for name, want := range map[string][]string{
		"sdk-ts-typecheck": {"scripts/ci/retry.sh -- make sdk-ts-install", "make sdk-ts-typecheck", "make sdk-ts-test"},
		"viewkit-checks":   {"scripts/ci/retry.sh -- make viewkit-install", "make viewkit-typecheck", "make viewkit-test"},
	} {
		step, ok := engineDeclaredStep(spec, name)
		if !ok || !slices.Equal(portableCommands(step.Run), want) {
			t.Errorf("%s must install, typecheck and test in order", name)
		}
	}
	conformance, ok := engineDeclaredStep(spec, "mcp-conformance")
	if !ok || !slices.Contains(portableCommands(conformance.Run), "go test -count=1 -timeout=600s -v ./test/conformance/...") {
		t.Fatal("conformance no longer runs its real database suite")
	}
	if value, found := engineStepExports(conformance.Run, "MEMQL_DIFFERENTIAL_REQUIRED"); !found || value != "1" {
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
		if value, found := engineStepExports(step.Run, "MEMQL_DATABASE_DSN"); !found || value != "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable" {
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
		"go run ./cmd/memql-bench --do=gate --runner=workbench-linux-amd64 > .memql-proving/envelope.json 2> .memql-proving/gate.txt || status=$?",
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
	assertCommand := func(name, command string) {
		t.Helper()
		step, found := engineDeclaredStep(spec, name)
		if !found || !slices.Contains(portableCommands(step.Run), command) {
			t.Errorf("%s lost required command %q", name, command)
		}
	}
	for _, tag := range []string{"identity", "mcp", "agent", "planner", "workbench"} {
		assertCommand("build-"+tag, "go build -tags "+tag+" github.com/znasllc-io/memql/...")
		assertCommand("build-"+tag, "go vet -tags "+tag+" github.com/znasllc-io/memql/...")
	}
	// scripts/citags additionally walks every tagged test file and checks that
	// these commands cover its real package and build constraint.
	for tag, packages := range map[string]string{
		"agent":     "./app/ ./integrations/agent/ ./integrations/agent/worker/ ./integrations/pipelinesteps/ ./component/node/",
		"bff":       "./app/ ./component/server/ ./component/node/",
		"edge":      "./app/ ./component/server/ ./component/node/",
		"identity":  "./app/ ./component/node/",
		"mcp":       "./app/ ./component/mcp/ ./component/node/",
		"planner":   "./app/ ./component/node/",
		"workbench": "./app/ ./component/node/",
	} {
		assertCommand("test-"+tag, "go test -tags "+tag+" -timeout=300s "+packages)
	}
	for _, tag := range []string{"clustere2e", "clustere2e,agent"} {
		assertCommand("build-clustere2e", "go vet -tags "+tag+" ./test/clustere2e/")
		assertCommand("build-clustere2e", "go test -tags "+tag+" -c -o /dev/null ./test/clustere2e/")
	}
	assertCommand("build-clustere2e", "export GOWORK=off")
	assertCommand("module-boundaries", "bash scripts/ci/module-integrity.sh")
	bashStep, found := engineDeclaredStep(spec, "bash32-smoke")
	if !found || !strings.HasPrefix(bashStep.Image, "docker.io/library/bash@sha256:") {
		t.Fatal("Bash compatibility does not use a pinned interpreter")
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
		if event == pipelines.EventRelease {
			for _, step := range plan.Steps() {
				if step.Name == "gitleaks" {
					t.Fatal("release qualification repeated the deep scan; the same commit is covered by its push scan and full history runs weekly")
				}
			}
			continue
		}
		wantStage := "secrets-current"
		wantCommands := []string{`--fetch-remote="https://github.com/${MEMQL_REPOSITORY}.git"`, `--head="$MEMQL_SHA"`, "python3 scripts/ci/gitleaks-history.py", "--jobs=3", "--batch-size=1"}
		wantTimeout := 30 * 60
		wantCommands = append(wantCommands, `--base="$MEMQL_BASE_SHA"`, "--timeout=1500")
		count := 0
		for _, step := range plan.Steps() {
			if step.Name != "gitleaks" {
				continue
			}
			count++
			if step.Stage != wantStage || !step.RunAfterFailure || step.Skip != nil || len(step.Artifacts) == 0 || strings.Contains(step.Run, "|| true") {
				t.Fatalf("%s selects the wrong scan, skips it, discards reports or ignores failure: %+v", event, step)
			}
			if strings.Contains(step.Run, "MEMQL_EVENT") || strings.Contains(step.Run, "gitleaks dir .") {
				t.Fatalf("%s hides scan selection inside the command", event)
			}
			for _, need := range append(wantCommands, "@v8.30.1") {
				if !strings.Contains(step.Run, need) {
					t.Errorf("%s scan omits %s", event, need)
				}
			}
			if step.CPUMilli != 3000 || step.MemoryMiB != 6144 {
				t.Fatal("history scan allocation differs from its bounded parallel rehearsal")
			}
			if step.TimeoutSeconds != wantTimeout {
				t.Fatal("history scan must declare the measured long-workload budget explicitly")
			}
		}
		if count != 1 {
			t.Fatalf("%s plans %d secret scans, want exactly one", event, count)
		}
	}
	weekly, refusal := compileEngineCadence(spec, graph, pipelines.EventSchedule, nil, "weekly")
	if refusal != nil {
		t.Fatal(refusal)
	}
	deepScans := 0
	for _, step := range weekly.Steps() {
		if step.Name != "gitleaks" {
			continue
		}
		deepScans++
		if step.Stage != "analysis" || step.TimeoutSeconds != 6*60*60 || !strings.Contains(step.Run, "--timeout=20700") || !strings.Contains(step.Run, "--batch-size=32") || strings.Contains(step.Run, "--base=") {
			t.Fatalf("weekly full-history scan has the wrong scope or budget: %+v", step)
		}
	}
	if deepScans != 1 {
		t.Fatalf("weekly audit plans %d full-history secret scans, want 1", deepScans)
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
	// govulncheck over every module holds the whole program in memory: at the
	// namespace default (4 GiB) the kernel killed it at 4.2 GB resident (run
	// 45f5502f, 2026-10-11), so the budget is declared, not inherited.
	if vuln.MemoryMiB < 8192 {
		t.Fatalf("Go vulnerability check reserves %d MiB; it was OOM-killed at the 4 GiB default", vuln.MemoryMiB)
	}
}
