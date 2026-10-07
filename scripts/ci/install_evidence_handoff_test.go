package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInstallClusterEvidenceHandoffIsNarrowAndBounded(t *testing.T) {
	type workflowStep struct {
		Name string            `yaml:"name"`
		If   string            `yaml:"if"`
		Run  string            `yaml:"run"`
		With map[string]string `yaml:"with"`
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}

	path := filepath.Join(gateRepoRoot(t), ".github", "workflows", "install-cluster-e2e.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var handoff, upload *workflowStep
	for _, job := range workflow.Jobs {
		for i := range job.Steps {
			switch job.Steps[i].Name {
			case "Hand the evidence back to the runner":
				handoff = &job.Steps[i]
			case "Upload the receipt, the reports and the snapshots":
				upload = &job.Steps[i]
			}
		}
	}
	if handoff == nil || upload == nil {
		t.Fatal("workflow must retain both the evidence ownership handoff and artifact upload steps")
	}
	if handoff.If != "always()" {
		t.Fatalf("evidence handoff condition = %q, want always() so failed installs retain diagnostics", handoff.If)
	}
	if !strings.Contains(handoff.Run, "timeout --signal=TERM --kill-after=5s 30s") {
		t.Fatal("evidence ownership repair must have one finite 30-second timeout")
	}
	if strings.Contains(handoff.Run, "chown -R") || strings.Contains(handoff.Run, `"$RUNNER_TEMP"`) {
		t.Fatal("evidence handoff must not recursively change ownership across RUNNER_TEMP")
	}

	const runnerTempPrefix = "${{ runner.temp }}/"
	for _, artifact := range strings.Split(upload.With["path"], "\n") {
		artifact = strings.TrimSpace(artifact)
		if !strings.HasPrefix(artifact, runnerTempPrefix) {
			continue
		}
		name := strings.TrimPrefix(artifact, runnerTempPrefix)
		if !strings.Contains(handoff.Run, `"$RUNNER_TEMP/`+name+`"`) {
			t.Errorf("uploaded evidence %q is not in the bounded ownership handoff", name)
		}
	}
}
