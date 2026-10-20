package ci

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// Read the configuration the installed native runner consumes. Transitional
// comparisons with hosted workflows must not become permanent dependencies.
func nativePipeline(t *testing.T) *pipelines.Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(RepoRoot(), "memql-package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Pipeline *pipelines.Spec `yaml:"pipeline"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if refusal := pipelines.Validate(manifest.Pipeline); refusal != nil {
		t.Fatal(refusal)
	}
	return manifest.Pipeline
}

func nativeRequiredStep(t *testing.T, name string) pipelines.StepSpec {
	t.Helper()
	for _, stage := range nativePipeline(t).Stages {
		for _, step := range stage.Steps {
			if step.Name != name {
				continue
			}
			if len(stage.On) > 0 && !(slices.Contains(stage.On, "pull_request") && slices.Contains(stage.On, "merge_group")) {
				t.Fatalf("%s must gate both PRs and merge candidates, got events %v", name, stage.On)
			}
			if step.When != nil {
				t.Fatalf("%s must not skip on path selection", name)
			}
			if !strings.Contains(step.Run, "set -eu\n") || strings.Contains(step.Run, "||") {
				t.Fatalf("%s can hide a failing command", name)
			}
			return step
		}
	}
	t.Fatalf("native pipeline has no required %s step", name)
	return pipelines.StepSpec{}
}
