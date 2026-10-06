package pipelinesteps

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestFreshRunnersRecoverOneDurablyQueuedAttempt(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "executed once")))
	run.RecoverOnly = true
	secondConfig := h.cfg
	secondConfig.NodeID = rtOther
	second := h.newRunner(secondConfig)
	results := make(chan pl.StepResult, 2)
	for _, runner := range []*Runner{h.r, second} {
		go func() { results <- runner.Run(context.Background(), run) }()
	}
	for range 2 {
		res := h.await(t, results)
		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("recovery: %+v (%+v)", res, res.Failure)
		}
	}
	if len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 1 || len(h.c.requestsFor(http.MethodPost, kubeSecrets)) != 0 || len(h.lib.stored()) != 1 {
		t.Fatal("recovery repeated execution, credential creation or result publication")
	}
}

func TestRecoveryRefusesMissingChangedAndUncertainCreationProof(t *testing.T) {
	for _, fault := range []string{"legacy", "changed command", "creating", "owned"} {
		t.Run(fault, func(t *testing.T) {
			h := newRunnerHarness(t)
			run := rtRun()
			secret := BuildSecret(h.cfg, run, testJobName, rtCloneToken)
			switch fault {
			case "legacy":
				delete(secret.Metadata.Annotations, annotCreationDefinition)
			case "changed command":
				run.Command += "\necho changed"
			case "creating":
				secret.Metadata.Annotations[annotCreation] = "creating prior " + rtT0.Add(-time.Hour).Format(time.RFC3339Nano)
			case "owned":
				secret.Metadata.OwnerReferences = []OwnerReference{{UID: "deleted-job"}}
			}
			h.c.putSecret(secret)
			run.RecoverOnly = true
			res := h.run(t, run)
			wantFailure(t, res, pl.OutcomeFailed, pl.CodeExecutionUncertain)
			if len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 || len(h.tokens.called()) != 0 {
				t.Fatal("uncertain recovery started an effect")
			}
		})
	}
}

func TestCreationClaimRejectsStaleSecretRevisionAndReplacement(t *testing.T) {
	h := newRunnerHarness(t)
	h.c.putSecret(BuildSecret(h.cfg, rtRun(), testJobName, rtCloneToken))
	ctx := context.Background()
	meta, err := h.c.kube.SecretMetadata(ctx, testSecretName)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, state := range []string{"creating first", "creating second"} {
		wg.Go(func() { results <- h.c.kube.SetCreationState(ctx, meta, state) })
	}
	wg.Wait()
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || (first != nil && !deploycontrol.IsConflict(first)) || (second != nil && !deploycontrol.IsConflict(second)) {
		t.Fatalf("exactly one claim must win: %v / %v", first, second)
	}
	fresh, _ := h.c.kube.SecretMetadata(ctx, testSecretName)
	h.c.with(func(c *rtCluster) {
		replacement := c.secrets[testSecretName]
		replacement.Metadata.UID = "replacement-secret"
		c.secrets[testSecretName] = replacement
	})
	if err := h.c.kube.SetCreationState(ctx, fresh, creationQueued); !deploycontrol.IsConflict(err) {
		t.Fatalf("stale owner changed replacement: %v", err)
	}
}

func TestAmbiguousCreateResponseIsNotPermissionToPostAgain(t *testing.T) {
	h := newRunnerHarness(t)
	h.c.with(func(c *rtCluster) { c.createJobAnswers = []kubeAnswer{rtUnavailable} })
	res := h.run(t, rtRun())
	wantFailure(t, res, pl.OutcomeFailed, pl.CodeExecutionUncertain)
	if n := len(h.c.requestsFor(http.MethodPost, kubeJobs)); n != 1 {
		t.Fatalf("ambiguous POST repeated %d times", n)
	}
	secret := h.c.secretNow(t, testSecretName)
	if !strings.HasPrefix(secret.Metadata.Annotations[annotCreation], "creating ") {
		t.Fatal("ambiguous creation lost its durable guard")
	}
}

func TestLostCreateResponseAdoptsTheJobThatActuallyExists(t *testing.T) {
	h := newRunnerHarness(t)
	h.c.with(func(c *rtCluster) { c.loseJobCreates = 1 })
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "executed once")))
	res := h.run(t, rtRun())
	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("lost reply: %+v (%+v)", res, res.Failure)
	}
	if n := len(h.c.requestsFor(http.MethodPost, kubeJobs)); n != 1 {
		t.Fatalf("lost reply repeated POST %d times", n)
	}
}

func TestQueuedDefinitionBindsExecutionAndSecretNamesWithoutValues(t *testing.T) {
	run := rtRun()
	original := creationDefinition(run)
	run.RecoverOnly = true
	run.TimeoutSeconds--
	run.DeadlineCode = pl.CodeRunCeiling
	for name := range run.Secrets {
		run.Secrets[name] = "rotated"
	}
	if original != creationDefinition(run) {
		t.Fatal("recovery metadata or rotated credentials changed execution identity")
	}
	run.Secrets["NEW_SECRET"] = "fixture"
	if original == creationDefinition(run) {
		t.Fatal("different credential names accepted")
	}
}

func TestUnknownCreationCannotBeRequeuedByAnotherAttempt(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	first := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := first.claimCreation(); err != nil || !held {
		t.Fatalf("first claim: %v %v", held, err)
	}
	second := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := second.claimCreation(); err != nil || held {
		t.Fatalf("fresh competing claim: %v %v", held, err)
	}
	if err := second.releaseRejectedCreation(); !errors.Is(err, errCreationUncertain) {
		t.Fatalf("other creator requeued an unknown effect: %v", err)
	}
	h.clock.Advance(time.Minute)
	if held, err := second.claimCreation(); !errors.Is(err, errCreationUncertain) || held {
		t.Fatalf("stale unknown claim replayed: %v %v", held, err)
	}
}

func TestCreationMetadataLostResponsesAreReconciled(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	h.c.with(func(c *rtCluster) { c.loseSecretPatches = 1 })
	if held, err := s.claimCreation(); held || err == nil {
		t.Fatalf("expected lost claim reply: held=%v err=%v", held, err)
	}
	if held, err := s.claimCreation(); !held || err != nil {
		t.Fatalf("own claim was not recovered: held=%v err=%v", held, err)
	}
	h.c.with(func(c *rtCluster) { c.loseSecretPatches = 1 })
	if err := s.releaseRejectedCreation(); err != nil {
		t.Fatalf("lost queued reply was not reconciled: %v", err)
	}
	meta, err := s.creationMetadata()
	if err != nil || meta.Annotations[annotCreation] != creationQueued {
		t.Fatalf("queued proof missing after lost reply: %v", err)
	}
}
