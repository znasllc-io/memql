package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAttentionReminderIsScopedAndAdvisory(t *testing.T) {
	bucket := compiledBuckets(t)["osattention"]
	if bucket == nil {
		t.Fatal("missing OS advisory scope")
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"clients/os/src/apps/fleet/FleetApp.tsx", true}, {"clients/os/src/chrome/Shell.tsx", true},
		{"clients/os/src/attention/model.ts", true}, {"clients/os/src/kit/controls.tsx", true},
		{"clients/os/src/system/registry.ts", true}, {"clients/os/test/attention/attention.test.tsx", false},
		{"clients/os/README.md", false}, {"clients/os/src/styles/tokens.css", false},
		{"component/edge/handler.go", false}, {"docs/public/operate/memql-os.md", false},
	} {
		if got := bucket.Match(tc.path); got != tc.want {
			t.Errorf("advisory for %s = %v, want %v", tc.path, got, tc.want)
		}
	}
	raw, err := ReadCIWorkflow()
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name, If, Run string
				Continue      bool `yaml:"continue-on-error"`
			}
		}
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, step := range workflow.Jobs["changes"].Steps {
		if step.Name != "Consider OS change attention (advisory)" {
			continue
		}
		if !step.Continue || !strings.Contains(step.If, "pull_request") || !strings.Contains(step.If, "steps.filter.outputs.osattention == 'true'") {
			t.Fatal("reminder must stay non-blocking and limited to relevant PRs")
		}
		summary := filepath.Join(t.TempDir(), "summary.md")
		cmd := exec.Command("bash", "-c", step.Run)
		cmd.Env = append(os.Environ(), "GITHUB_STEP_SUMMARY="+summary)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("summary step: %v %s", err, out)
		}
		body, err := os.ReadFile(summary)
		if err != nil {
			t.Fatal(err)
		}
		for _, phrase := range []string{"Refactors, minor fixes", "no required decision", "README.md#unseen-changes", "semantic importance"} {
			if !strings.Contains(string(body), phrase) {
				t.Errorf("advisory lacks %q", phrase)
			}
		}
		return
	}
	t.Fatal("missing job-summary reminder; do not replace it with a mandatory impact gate")
}
