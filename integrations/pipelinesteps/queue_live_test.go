package pipelinesteps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

// Exercise the actual runner and shared Kubernetes state with the installed
// queue controller. All mutations belong to uniquely named disposable objects;
// the installation's queue, quota and serving workloads remain unchanged.
func TestResourceQueueRunnerAgainstLocalKubernetes(t *testing.T) {
	if os.Getenv("MEMQL_CAPACITY_TEST_CONTEXT") != os.Getenv("MEMQL_PIPELINES_ISOLATION_TEST_CONTEXT") || os.Getenv("MEMQL_CAPACITY_TEST_CONTEXT") == "" {
		t.Skip("set both local qualification contexts to the same k3d installation")
	}
	ctx, cluster, name, kubectl := localPipelineControls(t)
	const ns = "memql-pipelines"
	var existing struct{ Items []json.RawMessage }
	if err := json.Unmarshal(kubectl(nil, "-n", ns, "get", "jobs", "-o", "json"), &existing); err != nil || len(existing.Items) != 0 {
		t.Fatal("runner qualification requires an idle local build namespace")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "components", "pipelines", "config.yaml"))
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
	cfg.Namespace, cfg.QueueName, cfg.NodePool, cfg.NodeID = ns, name, "builds", "queue-replacement"
	cfg.PollInterval = 250 * time.Millisecond
	// testing.T cancels its context before cleanup callbacks. Keep the proxy
	// alive until the attempt resources have been acknowledged and removed.
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	t.Cleanup(stopProxy)
	kube := localPipelineKube(t, proxyCtx, cluster, ns)
	library := &rtLibrary{}
	runner := NewRunner(cfg, kube, nil, library, anonymousPlacementClone{})
	type object = map[string]any
	create := func(kind, namespace string, spec object) {
		t.Helper()
		metadata := object{"name": name}
		if namespace != "" {
			metadata["namespace"] = namespace
		}
		body, _ := json.Marshal(object{"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": kind, "metadata": metadata, "spec": spec})
		kubectl(body, "create", "-f", "-")
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			args := []string{"--context", cluster, "delete", kind, name, "--ignore-not-found", "--wait=false"}
			if namespace != "" {
				args = append(args, "-n", namespace)
			}
			if out, err := exec.CommandContext(cleanup, "kubectl", args...).CombinedOutput(); err != nil {
				t.Errorf("cleanup %s: %v: %s", kind, err, out)
			}
		})
	}
	create("ResourceFlavor", "", object{"nodeLabels": object{"memql.io/pipeline-pool": "builds"}, "nodeTaints": []object{{"key": "memql.io/pipeline-pool", "value": "builds", "effect": "NoSchedule"}}})
	create("ClusterQueue", "", object{
		"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": ns}}, "stopPolicy": "Hold",
		"resourceGroups": []object{{"coveredResources": []string{"cpu", "memory", "ephemeral-storage", "pods"}, "flavors": []object{{"name": name, "resources": []object{
			{"name": "cpu", "nominalQuota": "1000m"}, {"name": "memory", "nominalQuota": "5Gi"}, {"name": "ephemeral-storage", "nominalQuota": "24Gi"}, {"name": "pods", "nominalQuota": "1"},
		}}}}},
	})
	create("LocalQueue", ns, object{"clusterQueue": name})
	// A small pinned public fixture keeps Git download time out of the queue proof.
	// Only the fixed printf command below executes; no fixture code is run.
	newRun := func(key string) StepRun {
		run := StepRun{Execution: pl.ExecutionContainer, RunID: name + "-" + key, WorkRunID: name, StepKey: "checks/" + key, Attempt: 1, OwnerUserID: "local-queue-rehearsal",
			Repository: pl.Repository{Owner: "octocat", Name: "Hello-World", CloneURL: "https://github.com/octocat/Hello-World.git"}, SHA: "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d", Image: cfg.CloneImage,
			Command: "printf 'queued command executed once\\n'", TimeoutSeconds: 120, RunDeadline: time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339Nano)}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			jobName := JobName(run.RunID, run.StepKey, run.Attempt)
			for {
				err := runner.Ack(cleanup, AckRequest{JobName: jobName})
				if !deploycontrol.IsConflict(err) {
					if err != nil {
						t.Errorf("cleanup attempt: %v", err)
					}
					break
				}
				if cleanup.Err() != nil {
					t.Errorf("cleanup timed out: %v", cleanup.Err())
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
		return run
	}
	createAbandoned := func(run StepRun) string {
		t.Helper()
		jobName := JobName(run.RunID, run.StepKey, run.Attempt)
		job, err := BuildJob(cfg, run, jobName)
		if err != nil {
			t.Fatal(err)
		}
		job.Metadata.Annotations[AnnotRunner] = rtStamp("departed-queue-runner", time.Now().Add(-time.Hour))
		if _, err := kube.CreateSecret(ctx, BuildSecret(cfg, run, jobName, "")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := kube.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		state := runner.Status(ctx, StatusRequest{JobName: jobName})
		if state.ExecutionStartedAt != "" {
			t.Fatal("a held job started its execution clock")
		}
		return jobName
	}
	t.Run("cancel from another replica", func(t *testing.T) {
		run := newRun("cancel")
		jobName := createAbandoned(run)
		if _, err := runner.CancelRun(ctx, CancelRequest{RunID: run.RunID}); err != nil {
			t.Fatal(err)
		}
		if _, err := kube.GetJob(ctx, jobName); !deploycontrol.IsNotFound(err) {
			t.Fatalf("cancel retained job: %v", err)
		}
		if _, err := kube.SecretMetadata(ctx, SecretName(jobName)); !deploycontrol.IsNotFound(err) {
			t.Fatalf("cancel retained clone credentials: %v", err)
		}
	})
	t.Run("expiry survives replica replacement", func(t *testing.T) {
		run := newRun("expiry")
		run.RunDeadline = time.Now().Add(8 * time.Second).UTC().Format(time.RFC3339Nano)
		jobName := createAbandoned(run)
		run.RecoverOnly = true
		result := runner.Run(ctx, run)
		if result.Status != pl.OutcomeFailed || result.Failure == nil || result.Failure.Code != pl.CodeRunCeiling {
			t.Fatalf("expiry result: %+v", result)
		}
		if _, err := kube.GetJob(ctx, jobName); !deploycontrol.IsNotFound(err) {
			t.Fatalf("expired queued job could start later: %v", err)
		}
		if _, err := kube.SecretMetadata(ctx, jobName+retirementSuffix); err != nil {
			t.Fatalf("expiry lost its durable retirement fence: %v", err)
		}
	})
	t.Run("adopt and execute once", func(t *testing.T) {
		run := newRun("recover")
		jobName := createAbandoned(run)
		run.RecoverOnly = true
		result := make(chan pl.StepResult, 1)
		go func() { result <- runner.Run(ctx, run) }()
		kubectl(nil, "patch", "clusterqueue", name, "--type=merge", "-p", `{"spec":{"stopPolicy":"None"}}`)
		select {
		case got := <-result:
			if got.Status != pl.OutcomeSucceeded || got.LogTail != "queued command executed once" {
				t.Fatalf("recovery: %+v; failure: %+v", got, got.Failure)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		var pods struct{ Items []Pod }
		if err := json.Unmarshal(kubectl(nil, "-n", ns, "get", "pods", "-l", "job-name="+jobName, "-o", "json"), &pods); err != nil || len(pods.Items) != 1 {
			t.Fatal("recovery did not execute exactly once")
		}
		for {
			err := runner.Ack(ctx, AckRequest{JobName: jobName})
			if !deploycontrol.IsConflict(err) {
				if err != nil {
					t.Fatal(err)
				}
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	})
	if !t.Failed() {
		t.Log("real queued attempts cancelled, expired with a retirement fence, and recovered once on another runner")
	}
}
