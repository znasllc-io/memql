package pipelinesteps

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// A real quota rejection is persisted before a fresh Runner recovers the
// attempt. This uses actual API compare-and-swap, scheduling and cleanup;
// the process-loss/forward hop itself is covered by the deterministic tests.
func TestQueuedRecoveryAgainstLocalKubernetes(t *testing.T) {
	pool := os.Getenv("MEMQL_PIPELINES_TEST_NODE_POOL")
	if pool == "" {
		t.Skip("set MEMQL_PIPELINES_TEST_NODE_POOL to an existing local build pool")
	}
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	substrate := filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines")
	for _, file := range []string{"step-serviceaccount.yaml", "networkpolicy.yaml", "probe-networkpolicy.yaml", "ceiling.yaml"} {
		data, err := os.ReadFile(filepath.Join(substrate, file))
		if err != nil {
			t.Fatal(err)
		}
		kubectl(bytes.ReplaceAll(data, []byte("namespace: memql-pipelines"), []byte("namespace: "+ns)), "apply", "-f", "-")
	}
	kubectl(nil, "-n", ns, "patch", "resourcequota", "memql-pipelines-ceiling", "--type=merge", "-p", `{"spec":{"hard":{"count/jobs.batch":"1"}}}`)
	data, err := os.ReadFile(filepath.Join(substrate, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFromEnv(func(key string) string { return settings.Data[key] })
	cfg.Namespace, cfg.NodePool, cfg.NodeID = ns, pool, "queue-runner-a"
	cfg.PollInterval = 250 * time.Millisecond
	sha, err := exec.CommandContext(ctx, "git", "rev-parse", "origin/main").Output()
	if err != nil {
		t.Fatal(err)
	}
	run := StepRun{
		Execution: pl.ExecutionContainer, RunID: ns, WorkRunID: ns, StepKey: "checks/queued-recovery", Attempt: 1, OwnerUserID: "local-rehearsal",
		Repository: pl.Repository{Owner: "znasllc-io", Name: "memql", CloneURL: "https://github.com/znasllc-io/memql.git"},
		SHA:        strings.TrimSpace(string(sha)), Image: cfg.CloneImage, Command: "printf 'queued attempt executed once\\n'",
		TimeoutSeconds: 240, RunDeadline: time.Now().Add(6 * time.Minute).UTC().Format(time.RFC3339Nano),
	}
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	job, err := BuildJob(cfg, run, name)
	if err != nil {
		t.Fatal(err)
	}
	// A suspended Job holds the quota slot without starting a pod.
	blocker, _ := json.Marshal(map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]string{"name": "capacity-blocker", "namespace": ns}, "spec": map[string]any{"suspend": true, "template": map[string]any{"spec": map[string]any{"restartPolicy": "Never", "containers": []map[string]any{{"name": "unused", "image": cfg.CloneImage}}}}}})
	kubectl(blocker, "apply", "-f", "-")
	kube := localPipelineKube(t, ctx, cluster, ns)
	library := &rtLibrary{}
	first := NewRunner(cfg, kube, nil, library, anonymousPlacementClone{})
	if _, err := kube.CreateSecret(ctx, BuildSecret(cfg, run, name, "")); err != nil {
		t.Fatal(err)
	}
	prior := &step{r: first, run: run, jobName: name, ctx: ctx}
	if held, err := prior.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	if _, _, err := kube.CreateJob(ctx, job); !deploycontrol.IsForbiddenQuota(err) {
		t.Fatalf("expected real quota refusal: %v", err)
	}
	if err := prior.releaseRejectedCreation(); err != nil {
		t.Fatal(err)
	}
	meta, err := kube.SecretMetadata(ctx, SecretName(name))
	if err != nil || meta.Annotations[annotCreation] != creationQueued {
		t.Fatalf("queued proof missing: %v", err)
	}
	kubectl(nil, "-n", ns, "delete", "job", "capacity-blocker", "--wait=true", "--timeout=30s")
	cfg.NodeID = "queue-runner-b"
	second := NewRunner(cfg, kube, nil, library, anonymousPlacementClone{})
	run.RecoverOnly = true
	result := second.Run(ctx, run)
	if result.Status != pl.OutcomeSucceeded {
		t.Fatalf("queued recovery failed: %+v (%+v)", result, result.Failure)
	}
	if len(library.stored()) != 1 || result.LogTail != "queued attempt executed once" {
		t.Fatalf("unexpected result evidence: %+v", result)
	}
	for {
		err = second.Ack(ctx, AckRequest{JobName: name})
		if !deploycontrol.IsConflict(err) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if absent, err := second.receiptResourcesAbsent(ctx, name); err != nil || !absent {
		t.Fatalf("cleanup: absent=%v err=%v", absent, err)
	}
	t.Log("quota refusal persisted; fresh runner executed once and confirmed Job, pods and Secret absent")
}
