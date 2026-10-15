package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Keep the behavioral script tests in the normal required Go test lane.
func TestRecoverEngineMirrorBehavior(t *testing.T) {
	cmd := exec.Command("python3", "-B", "-m", "unittest", "-v", "recover_engine_image_mirror_test.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recovery behavior: %v\n%s", err, out)
	}
}

func TestRecoverEngineMirrorWorkflowBoundary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "recover-engine-image-mirror.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Concurrency struct {
			Group  string `yaml:"group"`
			Cancel bool   `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Jobs map[string]struct {
			If          string            `yaml:"if"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 || workflow.On["workflow_dispatch"] == nil {
		t.Fatal("recovery must be manual only")
	}
	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
		t.Fatal("workflow credentials must be read-only")
	}
	if workflow.Concurrency.Group != "build-engine-images-${{ inputs.version }}" || workflow.Concurrency.Cancel {
		t.Fatal("recovery must serialize with the release builder")
	}
	if len(workflow.Jobs) != 1 {
		t.Fatal("recovery must have one bounded writer")
	}
	job := workflow.Jobs["recover"]
	if job.If != "github.ref == 'refs/heads/main' && github.repository == 'znasllc-io/memql'" {
		t.Fatal("writer must be restricted to repository main")
	}
	if len(job.Permissions) != 3 || job.Permissions["contents"] != "read" || job.Permissions["id-token"] != "write" || job.Permissions["packages"] != "write" {
		t.Fatal("unexpected writer permissions")
	}
	allowed := map[string]bool{"actions/checkout": true, "actions/setup-go": true, "azure/login": true, "docker/login-action": true}
	for _, step := range job.Steps {
		if step.Uses != "" {
			owner, ref, ok := strings.Cut(step.Uses, "@")
			if !ok || !allowed[owner] || !sha40.MatchString(ref) {
				t.Fatalf("unreviewed action: %s", step.Uses)
			}
		}
		if strings.Contains(step.Run, "${{ inputs.") {
			t.Fatal("dispatch input must reach shell through env, not source interpolation")
		}
	}
}
