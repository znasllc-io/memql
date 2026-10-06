package main

import (
	"slices"
	"strings"
	"testing"
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
		"go run ./cmd/memql-bench --do=gate --runner=cockpit-linux-arm64 > .memql-proving/envelope.json 2> .memql-proving/gate.txt || status=$?",
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
