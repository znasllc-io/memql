package dev

// The OS bundles sdk/ts and reads the worker contract. Changes to either must
// run its typecheck, tests and build even when no OS source file changed.
import (
	"strings"
	"testing"
)

func TestOsLaneCoversItsSourceAndDependencies(t *testing.T) {
	_, step := nativeStep(t, "os-checks")
	nativeStepCoversPaths(t, step, "clients/os/package.json", "clients/os/src/main.ts", "sdk/ts/package.json", "integrations/agent/worker/pipeline_purpose.go", "brand/tokens.css", "Dockerfile")
}

func TestOsLaneRunsTypecheckTestAndBuild(t *testing.T) {
	_, step := nativeStep(t, "os-checks")
	lines := commandLines(step.Run)
	if len(lines) == 0 || lines[0] != "set -eu" {
		t.Fatal("OS checks must fail when a command fails")
	}
	for _, line := range lines {
		if nativeControl.MatchString(line) || strings.ContainsAny(line, ";&") || hasPipe(line) || strings.Contains(line, "||") {
			t.Errorf("OS command can be skipped or hidden: %s", line)
		}
	}
	for _, target := range []string{"os-typecheck", "os-test", "os-build"} {
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line, "make ") {
				for _, word := range strings.Fields(line)[1:] {
					if word == target {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("OS checks never execute make %s", target)
		}
	}
}
