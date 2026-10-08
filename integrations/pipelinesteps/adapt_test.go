package pipelinesteps

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// adapt_test.go -- a pl.StepRequest becomes the StepRun both paths hand a
// step (epic memql#5478). The StepRun is the runner's whole view of the step,
// so a field the adapter drops is a field no runner sees.

// testRequest is a step request as the seam's driver builds one.
func testRequest() pl.StepRequest {
	return pl.StepRequest{
		RunID:          "run-7f3a",
		WorkRunID:      "work-91c2",
		StepKey:        "tests.go-tests#2",
		Attempt:        2,
		RunAttempt:     1,
		RunStartedAt:   "2026-10-04T10:00:00Z",
		PipelineID:     "pipe-1c0d",
		OwnerUserID:    "user-5d1e",
		Repository:     pl.Repository{Owner: "acme", Name: "widget", CloneURL: "https://github.com/acme/widget.git"},
		SHA:            testSHA,
		Mode:           pl.ModeAffected,
		Event:          pl.EventPullRequest,
		Version:        testSHA,
		InstallationID: 42,
		Compute:        pl.ComputeCluster,
		Step: pl.Step{
			Execution: pl.ExecutionContainer,
			Platform:  "linux/amd64",
			Key:       "tests.go-tests#2",
			Stage:     "tests",
			Name:      "go-tests",
			Kind:      pl.StepCommand,
			Run:       "go test $MEMQL_PACKAGES",
			Image:     "registry.example.com/acme/toolchain:1.4",
			Services: map[string]pl.Service{
				"postgres": {Image: "postgres:16", Env: map[string]string{"POSTGRES_USER": "memql"}, Ready: "pg_isready"},
			},
			Caches:         []string{"go"},
			TimeoutSeconds: 900,
			Artifacts:      []string{"coverage.out"},
			Secrets:        []string{"NPM_TOKEN", "DEPLOY_KEY"},
			Packages:       []string{"github.com/acme/widget/a", "github.com/acme/widget/b"},
			Shard:          pl.ShardRef{Index: 2, Count: 3},
		},
		Secrets: map[string]string{"NPM_TOKEN": plantedNPM, "DEPLOY_KEY": plantedDeploy},
	}
}

// TestStepRequestBecomesAStepRun: the contract environment travels as plain
// values, the secrets travel separately and only as secrets, and every other
// field the runner needs is the request's.
func TestStepRequestBecomesAStepRun(t *testing.T) {
	req := testRequest()
	run := stepRunFor(req, 600, pl.CodeRunCeiling)

	// THE ENVIRONMENT IS THE SEAM'S, minus the secrets. Rendered by
	// StepRequest.Environment() and nowhere else, so the cluster and the fleet
	// export the same contract.
	whole := req.Environment()
	for name := range req.Secrets {
		if _, ok := run.Env[name]; ok {
			t.Errorf("Env carries the secret %s as a plain value: a Job would put it in the pod spec and the "+
				"fleet would put it in the dispatch's env, where neither masks it", name)
		}
	}
	if !maps.Equal(run.Secrets, req.Secrets) {
		t.Errorf("Secrets = %v, want the request's resolved values %v", run.Secrets, req.Secrets)
	}
	union := maps.Clone(run.Env)
	maps.Copy(union, run.Secrets)
	if !maps.Equal(union, whole) {
		t.Errorf("Env + Secrets = %v\nwant Environment() = %v", union, whole)
	}
	for _, name := range []string{"MEMQL_RUN_ID", "MEMQL_STEP", "MEMQL_SHA", "MEMQL_PACKAGES", "MEMQL_SHARD", "MEMQL_MODE", "MEMQL_EVENT", "MEMQL_VERSION"} {
		if run.Env[name] == "" {
			t.Errorf("Env lacks the contract variable %s", name)
		}
	}

	want := StepRun{
		RunID:          req.RunID,
		WorkRunID:      req.WorkRunID,
		StepKey:        req.StepKey,
		Attempt:        2,
		OwnerUserID:    req.OwnerUserID,
		Repository:     req.Repository,
		SHA:            req.SHA,
		InstallationID: 42,
		Execution:      req.Step.Execution,
		Needs:          req.Step.Needs,
		Platform:       req.Step.Platform,
		Image:          req.Step.Image,
		Command:        req.Step.Run,
		Env:            run.Env,
		Secrets:        run.Secrets,
		Services:       req.Step.Services,
		Caches:         []string{"go"},
		Artifacts:      []string{"coverage.out"},
		TimeoutSeconds: 600,
		DeadlineCode:   pl.CodeRunCeiling,
		GoTimings:      true,
	}
	if !reflect.DeepEqual(run, want) {
		t.Errorf("StepRun =\n%v\nwant\n%v", run, want)
	}

	// The StepRun owns its slices and maps: the driver's request is not the
	// runner's to edit, and the other way around.
	run.Caches[0], run.Artifacts[0] = "npm", "elsewhere"
	run.Services["postgres"] = pl.Service{Image: "edited"}
	if req.Step.Caches[0] != "go" || req.Step.Artifacts[0] != "coverage.out" || req.Step.Services["postgres"].Image != "postgres:16" {
		t.Error("editing the StepRun edited the request it was made from")
	}

	t.Run("a secret named like a contract variable stays the platform's", func(t *testing.T) {
		// The compiler refuses a MEMQL_* secret; Environment() is the second
		// wall and keeps the platform's value. The StepRun keeps that answer:
		// the secret is not carried at all, so no runner can let it win.
		req := testRequest()
		req.Secrets["MEMQL_SHA"] = "forged-sha-value"
		run := stepRunFor(req, 600, pl.CodeStepTimeout)
		if run.Env["MEMQL_SHA"] != req.SHA {
			t.Errorf("MEMQL_SHA = %q, want the platform's %q", run.Env["MEMQL_SHA"], req.SHA)
		}
		if _, ok := run.Secrets["MEMQL_SHA"]; ok {
			t.Error("the colliding secret is carried, so a runner could export it over the platform's value")
		}
	})

	t.Run("a step with no secrets carries none", func(t *testing.T) {
		req := testRequest()
		req.Secrets, req.Step.Secrets = nil, nil
		if run := stepRunFor(req, 600, pl.CodeStepTimeout); len(run.Secrets) != 0 {
			t.Errorf("Secrets = %v, want none", run.Secrets)
		}
	})

	t.Run("Go timings are asked for of a Go test step only", func(t *testing.T) {
		for _, c := range []struct {
			run      string
			packages []string
			want     bool
		}{
			{"go test ./...", nil, true},
			{"make test", []string{"github.com/acme/widget/a"}, true},
			{"npm test", nil, false},
		} {
			req := testRequest()
			req.Step.Run, req.Step.Packages = c.run, c.packages
			if got := stepRunFor(req, 600, pl.CodeStepTimeout).GoTimings; got != c.want {
				t.Errorf("run %q packages %v: GoTimings = %v, want %v", c.run, c.packages, got, c.want)
			}
		}
	})

	t.Run("an attempt below one is the first", func(t *testing.T) {
		req := testRequest()
		req.Attempt = 0
		if got := stepRunFor(req, 600, pl.CodeStepTimeout).Attempt; got != 1 {
			t.Errorf("Attempt = %d, want 1: the Job's name is derived from it, and the driver's first attempt is 1", got)
		}
	})
}

func TestImageBuildReceivesPinnedCommitAndReleaseMetadata(t *testing.T) {
	req := testRequest()
	req.SHA = testSHA
	req.Event, req.Version = pl.EventRelease, "v1.25.0"
	req.Step.ImageBuild = &pl.ImageBuild{Context: ".", Dockerfile: "Dockerfile", Args: map[string]string{"BUILD_TAGS": "edge"}}
	run := stepRunFor(req, 600, pl.CodeStepTimeout)
	if got := run.ImageBuild.Args["MEMQL_COMMIT"]; got != testSHA {
		t.Fatalf("MEMQL_COMMIT = %q, want pinned SHA %q", got, testSHA)
	}
	if got := run.ImageBuild.Args["MEMQL_RELEASE"]; got != "1.25.0" {
		t.Fatalf("MEMQL_RELEASE = %q, want bare release version", got)
	}
	if _, ok := req.Step.ImageBuild.Args["MEMQL_COMMIT"]; ok {
		t.Fatal("adding build provenance mutated the source step definition")
	}

	req.Event, req.Version = pl.EventPush, testSHA
	run = stepRunFor(req, 600, pl.CodeStepTimeout)
	if got := run.ImageBuild.Args["MEMQL_RELEASE"]; got != "" {
		t.Fatalf("non-release MEMQL_RELEASE = %q, want empty", got)
	}
}

// TestSecretValuesAreEveryValueToMask: the capture and the fleet's result mask
// the resolved secrets and the clone token, sorted, blanks dropped.
func TestSecretValuesAreEveryValueToMask(t *testing.T) {
	run := StepRun{Secrets: map[string]string{"B": "bbbb", "A": "aaaa", "EMPTY": ""}}
	if got, want := secretValues(run, "tok-123456"), []string{"aaaa", "bbbb", "tok-123456"}; !slices.Equal(got, want) {
		t.Errorf("secretValues = %v, want %v", got, want)
	}
}
