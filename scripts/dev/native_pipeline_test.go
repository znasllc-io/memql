package dev

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
)

func nativePipeline(t *testing.T) *pipelines.Spec {
	t.Helper()
	m, err := packages.ReadManifest(os.DirFS(repoRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	if refusal := pipelines.Validate(m.Pipeline); refusal != nil {
		t.Fatal(refusal)
	}
	return m.Pipeline
}

func nativeStep(t *testing.T, name string) (pipelines.StageSpec, pipelines.StepSpec) {
	t.Helper()
	for _, stage := range nativePipeline(t).Stages {
		for _, step := range stage.Steps {
			if step.Name == name {
				if len(stage.On) > 0 && !slices.Contains(stage.On, "pull_request") {
					t.Fatalf("%s is not a PR check", name)
				}
				return stage, step
			}
		}
	}
	t.Fatalf("native pipeline has no %s", name)
	return pipelines.StageSpec{}, pipelines.StepSpec{}
}

func nativeStepCoversPaths(t *testing.T, step pipelines.StepSpec, paths ...string) {
	t.Helper()
	if step.When == nil {
		return
	}
	spec := nativePipeline(t)
	for _, path := range paths {
		matched, err := pipelines.AnyPathMatches(spec.Select.Buckets[step.When.Bucket], []string{path})
		if err != nil || !matched {
			t.Errorf("%s does not run for %s: %v", step.Name, path, err)
		}
	}
}

var nativeNoTests = regexp.MustCompile(`(^|[\s=])--?(test\.)?((run|skip)[\s=]|count([=\s]+)0([\s]|$))`)
var nativeControl = regexp.MustCompile(`^(if|then|else|elif|fi|for|while|until|do|done|case|esac|function|exit|return|eval|source|exec|alias|cd|\.)\b|^[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\)|^[({]`)

// Only direct, uncached, unfiltered go-test commands contribute coverage.
// This deliberately refuses control flow instead of guessing whether it runs.
func nativeSuitePatterns(run string) ([]string, error) {
	lines := commandLines(run)
	if len(lines) == 0 || lines[0] != "set -eu" {
		return nil, fmt.Errorf("suite lacks fail-fast shell")
	}
	var patterns []string
	for _, line := range lines {
		if nativeControl.MatchString(line) || strings.ContainsAny(line, ";&") || hasPipe(line) || strings.Contains(line, "||") || nativeNoTests.MatchString(line) {
			return nil, fmt.Errorf("suite may hide, skip or narrow tests: %s", line)
		}
		if !strings.HasPrefix(line, "go test ") {
			continue
		}
		if !strings.Contains(line, "-count=1") {
			return nil, fmt.Errorf("suite can reuse test results: %s", line)
		}
		for _, word := range strings.Fields(line) {
			if strings.HasPrefix(word, "./") {
				patterns = append(patterns, word)
			}
		}
	}
	if len(patterns) == 0 {
		return nil, fmt.Errorf("suite runs no explicit test packages")
	}
	return patterns, nil
}

func nativeSuite(t *testing.T, name string) []string {
	t.Helper()
	_, step := nativeStep(t, name)
	patterns, err := nativeSuitePatterns(step.Run)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return patterns
}

func TestNativeSuiteCoverageRejectsHiddenOrFilteredTests(t *testing.T) {
	for _, run := range []string{
		"set -eu\n# go test -count=1 ./cmd/memql-lsp/...\ntrue",
		"set -eu\nif false; then\ngo test -count=1 ./...\nfi",
		"set -eu\ngo test -count=1 ./... || true",
		"set -eu\ngo test -count=1 ./... | tee log",
		"set -eu\ngo test -count=0 ./...",
		"set -eu\nexport GOFLAGS=-run=Nothing\ngo test -count=1 ./...",
		"set -eu\nexit 0\ngo test -count=1 ./...",
	} {
		if _, err := nativeSuitePatterns(run); err == nil {
			t.Errorf("accepted %q", run)
		}
	}
	got, err := nativeSuitePatterns("set -eu\ngo test -count=1 ./cmd/memql-lsp/...")
	if err != nil || !slices.Equal(got, []string{"./cmd/memql-lsp/..."}) {
		t.Fatalf("plain suite: %v %v", got, err)
	}
}
