package dev

// go_cache_single_writer_test.go -- the shared Go cache has exactly one writer
// (memql#5482).
//
// actions/setup-go's own cache is saved by whichever job finishes first. In
// ci.yml that was the path-routing lane, so every Go cache this repository
// held was 22 MB and every other lane restored almost nothing: the module's
// `go build` took 137s on every run and no test result was ever cached. The
// fix is structural -- setup-go's cache off everywhere, one composite action
// restoring one cache, and ONE job (go-checks, on a push to main) writing it --
// and each half of that is a line somebody could "tidy" back:
//
//   - re-enabling setup-go's cache in one job brings the race back, now
//     competing with the real writer for the same storage;
//   - a second save step (say, in a pull-request lane) fills the store with
//     caches nobody else can read and evicts the one they can;
//   - a job that skips the restore silently compiles cold again.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const goCacheAction = "./.github/actions/go-cache"

type cacheStep struct {
	ID   string         `yaml:"id"`
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	If   string         `yaml:"if"`
	With map[string]any `yaml:"with"`
}

func cacheJobs(t *testing.T) map[string][]cacheStep {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []cacheStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(ciYAML(t), &wf); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	out := map[string][]cacheStep{}
	for name, job := range wf.Jobs {
		out[name] = job.Steps
	}
	if len(out) == 0 {
		t.Fatal("parsed no jobs from ci.yml; this guard cannot pass vacuously")
	}
	return out
}

// TestEveryGoJobRestoresTheSharedCache: setup-go's cache off, the shared one
// restored, in every job that sets Go up.
func TestEveryGoJobRestoresTheSharedCache(t *testing.T) {
	goJobs := 0
	for job, steps := range cacheJobs(t) {
		setup := -1
		for i, s := range steps {
			if strings.HasPrefix(s.Uses, "actions/setup-go@") {
				setup = i
			}
		}
		if setup < 0 {
			continue
		}
		goJobs++
		if v, ok := steps[setup].With["cache"]; !ok || v != false {
			t.Errorf("job %q: actions/setup-go must set `cache: false` (got %v). Its own cache is saved "+
				"by whichever job finishes first -- the 22 MB race memql#5482 removed.", job, v)
		}
		restored := false
		for _, s := range steps[setup+1:] {
			if s.Uses == goCacheAction {
				restored = true
				break
			}
		}
		if !restored {
			t.Errorf("job %q sets Go up but never uses %s after it, so it compiles cold every run", job, goCacheAction)
		}
	}
	if goJobs == 0 {
		t.Fatal("no job in ci.yml sets Go up; this guard is reading the wrong file")
	}
}

// TestTheGoCacheHasOneWriter: exactly one save, in go-checks, on a push, of
// what the composite action restored.
func TestTheGoCacheHasOneWriter(t *testing.T) {
	var saves []string
	for job, steps := range cacheJobs(t) {
		for _, s := range steps {
			if strings.HasPrefix(s.Uses, "actions/cache/save@") {
				saves = append(saves, job)
				cond := strings.ReplaceAll(s.If, " ", "")
				if !strings.Contains(cond, "github.event_name=='push'") {
					t.Errorf("job %q saves a cache on `if: %s`; only a push to main may write the shared "+
						"Go cache -- a pull request's cache is visible to that pull request alone", job, s.If)
				}
				path, _ := s.With["path"].(string)
				key, _ := s.With["key"].(string)
				if !strings.Contains(path, "steps.go-cache.outputs.paths") || !strings.Contains(key, "steps.go-cache.outputs.key") {
					t.Errorf("job %q saves path %q under key %q; it must save exactly what %s restored, "+
						"under the key it computed", job, path, key, goCacheAction)
				}
			}
			if strings.HasPrefix(s.Uses, "actions/cache@") {
				if path, _ := s.With["path"].(string); strings.Contains(path, "go-build") || strings.Contains(path, "pkg/mod") {
					t.Errorf("job %q restores AND saves a Go cache path through actions/cache (%q): a second "+
						"writer of the shared cache", job, path)
				}
			}
		}
	}
	if len(saves) != 1 || saves[0] != "go-checks" {
		t.Errorf("the shared Go cache must have exactly one writer, go-checks; saves found in %v", saves)
	}
}

// TestTheGoCacheActionRestoresByPrefix: the composite action restores both
// paths and falls back by prefix, so a go.sum bump restores yesterday's cache
// instead of cold-starting every job.
func TestTheGoCacheActionRestoresByPrefix(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "actions", "go-cache", "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Runs struct {
			Steps []cacheStep `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(raw, &action); err != nil {
		t.Fatal(err)
	}
	var restore *cacheStep
	for i, s := range action.Runs.Steps {
		if strings.HasPrefix(s.Uses, "actions/cache/save@") || (strings.HasPrefix(s.Uses, "actions/cache@")) {
			t.Errorf("the go-cache action uses %s, which SAVES; restoring is its whole job -- go-checks saves", s.Uses)
		}
		if strings.HasPrefix(s.Uses, "actions/cache/restore@") {
			restore = &action.Runs.Steps[i]
		}
	}
	if restore == nil {
		t.Fatal("the go-cache action has no actions/cache/restore step")
	}
	if rk, _ := restore.With["restore-keys"].(string); strings.Count(strings.TrimSpace(rk), "\n") < 1 {
		t.Errorf("restore-keys = %q; want two prefix fallbacks (same sums, then any), so one go.sum bump "+
			"does not cold-start every job", rk)
	}
	if !strings.Contains(string(raw), "GOMODCACHE") || !strings.Contains(string(raw), "GOCACHE") {
		t.Error("the go-cache action must cache both GOMODCACHE and GOCACHE")
	}
}
