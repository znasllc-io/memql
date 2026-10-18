package pipelinesteps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestFullHistoryCheckoutKeepsPinnedSourceAndCredentialsPrivate(t *testing.T) {
	requireTools(t, "git")
	source := t.TempDir()
	git(t, source, "init", "-q", ".")
	writeFiles(t, source, map[string]string{"one.txt": "one\n"})
	git(t, source, "add", "one.txt")
	git(t, source, "commit", "-q", "-m", "one")
	first := git(t, source, "rev-parse", "HEAD")
	git(t, source, "tag", "old-tag")
	writeFiles(t, source, map[string]string{"two.txt": "two\n"})
	git(t, source, "add", "two.txt")
	git(t, source, "commit", "-q", "-m", "two")
	pinned := git(t, source, "rev-parse", "HEAD")
	git(t, source, "checkout", "--orphan", "comparison")
	writeFiles(t, source, map[string]string{"base.txt": "comparison\n"})
	git(t, source, "add", "base.txt")
	git(t, source, "commit", "-q", "-m", "comparison")
	base := git(t, source, "rev-parse", "HEAD")
	const token = "fixture-clone-token-must-not-persist"
	for _, mode := range []string{"", "full"} {
		t.Run("history="+mode, func(t *testing.T) {
			workspace := t.TempDir()
			env := append(gitEnv(t), "CLONE_URL=file://"+source, "SHA="+pinned, "BASE_SHA="+base, "CLONE_HISTORY="+mode, "GIT_TOKEN="+token)
			result := runScript(t, cloneScript, workspace, env)
			if result.code != 0 {
				t.Fatalf("checkout: %d %s", result.code, result.stderr)
			}
			if git(t, workspace, "rev-parse", "HEAD") != pinned {
				t.Fatal("checkout moved off the requested commit")
			}
			if _, err := os.Stat(filepath.Join(workspace, "base.txt")); !os.IsNotExist(err) {
				t.Fatal("comparison checkout leaked into pinned tree")
			}
			wantCount, wantShallow := "1", "true"
			if mode == "full" {
				wantCount, wantShallow = "2", "false"
				if git(t, workspace, "rev-parse", "old-tag") != first {
					t.Fatal("tag missing")
				}
				if git(t, workspace, "cat-file", "-t", base) != "commit" {
					t.Fatal("non-ancestor comparison base missing")
				}
			}
			if got := git(t, workspace, "rev-list", "--count", "HEAD"); got != wantCount {
				t.Fatalf("history count %s, want %s", got, wantCount)
			}
			if got := git(t, workspace, "rev-parse", "--is-shallow-repository"); got != wantShallow {
				t.Fatalf("shallow=%s", got)
			}
			config, err := os.ReadFile(filepath.Join(workspace, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(result.stdout+result.stderr+string(config), token) || strings.Contains(string(config), "extraheader") {
				t.Fatal("clone credential escaped")
			}
		})
	}
}

func TestCheckoutHistorySurvivesRemoteRunnerWithoutLocalState(t *testing.T) {
	req := testRequest()
	req.Step.CheckoutHistory = "full"
	sent := stepRunFor(req, 600, pl.CodeRunCeiling)
	sent.Env["MEMQL_BASE_SHA"] = strings.Repeat("b", 40)
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var received StepRun
	if err = json.Unmarshal(raw, &received); err != nil {
		t.Fatal(err)
	}
	job := mustBuild(t, testConfig(), received)
	clone, _, ok := initNamed(job, "clone")
	if !ok {
		t.Fatal("clone absent")
	}
	found := map[string]string{}
	for _, env := range clone.Env {
		found[env.Name] = env.Value
	}
	if found["CLONE_HISTORY"] != "full" || found["BASE_SHA"] != sent.Env["MEMQL_BASE_SHA"] {
		t.Fatal("history scope lost crossing replicas")
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == gitTokenKey || env.Name == "CLONE_HISTORY" {
				t.Fatal("checkout-only input reached command")
			}
		}
	}
	received.CheckoutHistory = "unsupported"
	if _, refusal := BuildJob(testConfig(), received, JobName(received.RunID, received.StepKey, received.Attempt)); refusal == nil {
		t.Fatal("unknown history mode reached Job")
	}
	received.CheckoutHistory = "full"
	received.Env["MEMQL_BASE_SHA"] = "--upload-pack=bad"
	if _, refusal := BuildJob(testConfig(), received, JobName(received.RunID, received.StepKey, received.Attempt)); refusal == nil {
		t.Fatal("non-SHA comparison base reached Job")
	}
}
