package pipelinesteps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestRepositoryReadGrantCrossesReplicasAndOnlyReachesOptedInCommand(t *testing.T) {
	req := testRequest()
	req.Step.GitHubReadToken = true
	sent := stepRunFor(req, 600, pl.CodeRunCeiling)
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var received StepRun
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatal(err)
	}
	if !received.GitHubReadToken {
		t.Fatal("grant lost crossing replicas")
	}
	job := mustBuild(t, testConfig(), received)
	var holders []string
	for _, c := range allContainers(job) {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Key == gitTokenKey {
				holders = append(holders, c.Name)
				if c.Name == ContainerStep && (e.Name != repositoryReadTokenVar || e.Value != "" || (e.ValueFrom.SecretKeyRef.Optional != nil && *e.ValueFrom.SecretKeyRef.Optional)) {
					t.Fatal("read credential must be a required secret reference")
				}
			}
		}
	}
	if strings.Join(holders, ",") != "clone,"+ContainerStep {
		t.Fatalf("unexpected token recipients: %v", holders)
	}
	for _, change := range []func(*StepRun){
		func(r *StepRun) { r.InstallationID = 0 },
		func(r *StepRun) { r.ImageBuild = &pl.ImageBuild{} },
		func(r *StepRun) { r.Env[repositoryReadTokenVar] = "injected" },
		func(r *StepRun) { r.Secrets[repositoryReadTokenVar] = "injected" },
	} {
		r := testRun()
		r.GitHubReadToken = true
		change(&r)
		if _, refusal := BuildJob(testConfig(), r, testJobName); refusal == nil {
			t.Fatalf("invalid credential request reached Job: %+v", r)
		}
	}

}

type readGrantTokens struct{ clone, read rtTokens }

func (m *readGrantTokens) CloneToken(ctx context.Context, installation int64, owner, name string) (string, error) {
	return m.clone.CloneToken(ctx, installation, owner, name)
}
func (m *readGrantTokens) RepositoryReadToken(ctx context.Context, installation int64, owner, name string) (string, error) {
	return m.read.CloneToken(ctx, installation, owner, name)
}

func TestRepositoryReadGrantRefreshesWhileQueuedAndMasksBothTokens(t *testing.T) {
	const first, second = "fixture-first-read-token", "fixture-second-read-token"
	h := newRunnerHarness(t)
	minter := &readGrantTokens{read: rtTokens{seq: []string{first, second}}}
	h.r.tokens = minter
	h.c.with(func(c *rtCluster) {
		c.createJobAnswers = []kubeAnswer{rtQuotaRefusal(testJobName)}
		c.onJobCreate = func(_ *rtCluster, n int) {
			if n == 1 {
				h.clock.Advance(tokenRefreshAge + time.Minute)
			}
		}
	})
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "read "+first+" "+second)))
	run := rtRun()
	run.GitHubReadToken = true
	res := h.run(t, run)
	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("read command: %+v / %+v", res, res.Failure)
	}
	if len(minter.clone.called()) != 0 || len(minter.read.called()) != 2 {
		t.Fatal("refresh changed credential profile or failed to mint again")
	}
	if got := string(h.c.secretNow(t, testSecretName).Data[gitTokenKey]); got != second {
		t.Fatal("queued job did not receive refreshed grant")
	}
	if log := string(h.file(t, "tests-go-tests-2.log").Bytes); strings.Contains(log, first) || strings.Contains(log, second) || !strings.Contains(log, "read *** ***") {
		t.Fatalf("read credentials not masked: %s", log)
	}
}

func TestRepositoryReadGrantNeverFallsBackToCloneOrAnonymousToken(t *testing.T) {
	for _, emptyGrant := range []bool{false, true} {
		h := newRunnerHarness(t)
		if emptyGrant {
			h.r.tokens = &readGrantTokens{}
		}
		run := rtRun()
		run.GitHubReadToken = true
		res := h.run(t, run)
		if res.Status == pl.OutcomeSucceeded || res.Failure == nil || res.Failure.Code != pl.CodeCloneFailed {
			t.Fatalf("unsupported grant accepted: %+v", res)
		}
		if len(h.tokens.called()) != 0 {
			t.Fatal("read grant downgraded to clone")
		}
	}
}

func TestImageBuildReadGrantCrossesReplicasAsOneRequiredSecretReference(t *testing.T) {
	cfg, source := imageBuildRun()
	source.GitHubReadToken = true
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var received StepRun
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatal(err)
	}
	job, err := BuildJob(cfg, received, testJobName)
	if err != nil {
		t.Fatal(err)
	}
	var holders []string
	for _, c := range allContainers(job) {
		for _, e := range c.Env {
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				continue
			}
			holders = append(holders, c.Name)
			ref := e.ValueFrom.SecretKeyRef
			if ref.Key != gitTokenKey || ref.Name != SecretName(testJobName) ||
				(c.Name == ContainerStep && (e.Name != repositoryReadTokenVar || e.Value != "" || (ref.Optional != nil && *ref.Optional))) {
				t.Fatalf("builder received an unintended or optional credential: %+v", e)
			}
		}
	}
	if strings.Join(holders, ",") != "clone,"+ContainerStep {
		t.Fatalf("credential reached preparation or artifact collection: %v", holders)
	}
	program := ""
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == stepCommandVar {
			program = e.Value
		}
	}
	if strings.Count(program, "'--secret'") != 1 || !strings.Contains(program, "'id=github_read_token,env="+repositoryReadTokenVar+"'") {
		t.Fatalf("build lacks its fixed secret session: %s", program)
	}
	if strings.Contains(program, "build-arg:"+repositoryReadTokenVar) {
		t.Fatal("read credential sent as an image build argument")
	}
	received.GitHubReadToken = false
	ordinary, err := BuildJob(cfg, received, testJobName)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ordinary.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom != nil || (e.Name == stepCommandVar && strings.Contains(e.Value, "'--secret'")) {
			t.Fatal("ordinary image builder received the optional grant")
		}
	}
	received.GitHubReadToken, received.InstallationID = true, 0
	if _, err := BuildJob(cfg, received, testJobName); err == nil {
		t.Fatal("anonymous repository received an image read grant")
	}
}
