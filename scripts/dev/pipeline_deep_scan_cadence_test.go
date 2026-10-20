package dev

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeepSecurityScansRunWeeklyOrOnDemand(t *testing.T) {
	var root struct {
		Pipeline struct {
			Stages []struct {
				Name     string   `yaml:"name"`
				Schedule string   `yaml:"schedule"`
				On       []string `yaml:"on"`
			} `yaml:"stages"`
		} `yaml:"pipeline"`
	}
	if err := yaml.Unmarshal([]byte(repoFile(t, "memql-package.yaml")), &root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"analysis", "security-reports"} {
		t.Run(name, func(t *testing.T) {
			for _, stage := range root.Pipeline.Stages {
				if stage.Name != name {
					continue
				}
				if stage.Schedule != "weekly" {
					t.Fatalf("%s schedule = %q, want weekly", name, stage.Schedule)
				}
				if !slices.Equal(stage.On, []string{"schedule", "manual"}) {
					t.Fatalf("%s events = %v, want only scheduled and on-demand runs", name, stage.On)
				}
				return
			}
			t.Fatalf("stage %q is missing", name)
		})
	}
}

func repoFile(t *testing.T, rel ...string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/dev/ -> repo root is two directories up.
	parts := append([]string{filepath.Dir(thisFile), "..", ".."}, rel...)
	raw, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(rel...), err)
	}
	return string(raw)
}
