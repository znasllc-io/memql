package pipelinesteps

import (
	"context"
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

// liveQueueConfig is the deployed component's configuration, pointed at a disposable
// queue, polling quickly enough for a proof to fit its context.
func liveQueueConfig(t *testing.T, namespace, queue string) Config {
	t.Helper()
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
	cfg.Namespace, cfg.QueueName, cfg.NodePool = namespace, queue, "builds"
	cfg.PollInterval = 250 * time.Millisecond
	return cfg
}

// createLiveQueue makes a flavor, a ClusterQueue and a LocalQueue under one disposable
// name, and removes them when the test ends.
func createLiveQueue(t *testing.T, cluster string, kubectl func([]byte, ...string) []byte, namespace, name, stopPolicy, cpu, memory, disk, pods string) {
	t.Helper()
	type object = map[string]any
	create := func(kind, objectNamespace string, spec object) {
		t.Helper()
		metadata := object{"name": name}
		if objectNamespace != "" {
			metadata["namespace"] = objectNamespace
		}
		body, _ := json.Marshal(object{"apiVersion": "kueue.x-k8s.io/v1beta2", "kind": kind, "metadata": metadata, "spec": spec})
		kubectl(body, "create", "-f", "-")
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			args := []string{"--context", cluster, "delete", kind, name, "--ignore-not-found", "--wait=false"}
			if objectNamespace != "" {
				args = append(args, "-n", objectNamespace)
			}
			if out, err := exec.CommandContext(cleanup, "kubectl", args...).CombinedOutput(); err != nil {
				t.Errorf("cleanup %s: %v: %s", kind, err, out)
			}
		})
	}
	create("ResourceFlavor", "", object{"nodeLabels": object{"memql.io/pipeline-pool": "builds"}, "nodeTaints": []object{{"key": "memql.io/pipeline-pool", "value": "builds", "effect": "NoSchedule"}}})
	create("ClusterQueue", "", object{
		"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": namespace}}, "stopPolicy": stopPolicy,
		"resourceGroups": []object{{"coveredResources": []string{"cpu", "memory", "ephemeral-storage", "pods"}, "flavors": []object{{"name": name, "resources": []object{
			{"name": "cpu", "nominalQuota": cpu}, {"name": "memory", "nominalQuota": memory}, {"name": "ephemeral-storage", "nominalQuota": disk}, {"name": "pods", "nominalQuota": pods},
		}}}}},
	})
	create("LocalQueue", namespace, object{"clusterQueue": name})
}

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
	cfg := liveQueueConfig(t, ns, name)
	cfg.NodeID = "queue-replacement"
	// testing.T cancels its context before cleanup callbacks. Keep the proxy
	// alive until the attempt resources have been acknowledged and removed.
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	t.Cleanup(stopProxy)
	kube := localPipelineKube(t, proxyCtx, cluster, ns)
	library := &rtLibrary{}
	runner := NewRunner(cfg, kube, nil, library, anonymousPlacementClone{})
	createLiveQueue(t, cluster, kubectl, ns, name, "Hold", "1000m", "5Gi", "24Gi", "1")
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

// anonymousQueueClone mints nothing: this proof clones a small public fixture.
type anonymousQueueClone struct{}

func (anonymousQueueClone) CloneToken(context.Context, int64, string, string) (string, error) {
	return "", nil
}

// A controller outage must cost a new step time and nothing else. The real runners
// here take every path that writes a Job while the controller is down: the first
// create and claim on a replica holding a fresh isolation proof, and on one that has to
// run the probe Job first. When the webhooks on Jobs failed closed, the API server
// answered each of those with a 500: the first step failed
// pipeline_execution_uncertain within a second and stayed failed, the second was
// refused pipeline_isolation_unenforced. It scales the controller to zero, so it needs
// a disposable cluster with no Argo CD self-heal on the operator, which it reports
// rather than passing on an outage that did not last.
func TestNewStepsWaitThroughAControllerOutage(t *testing.T) {
	if os.Getenv("MEMQL_CAPACITY_TEST_CONTEXT") != os.Getenv("MEMQL_PIPELINES_ISOLATION_TEST_CONTEXT") || os.Getenv("MEMQL_CAPACITY_TEST_CONTEXT") == "" {
		t.Skip("set both local qualification contexts to the same k3d installation")
	}
	ctx, cluster, name, kubectl := localPipelineControls(t)
	const ns = "memql-pipelines"
	var existing struct{ Items []json.RawMessage }
	if err := json.Unmarshal(kubectl(nil, "-n", ns, "get", "jobs", "-o", "json"), &existing); err != nil || len(existing.Items) != 0 {
		t.Fatal("this proof requires an idle local build namespace")
	}
	createLiveQueue(t, cluster, kubectl, ns, name, "None", "2000m", "10Gi", "48Gi", "2")
	cfg := liveQueueConfig(t, ns, name)
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	t.Cleanup(stopProxy)
	kube := localPipelineKube(t, proxyCtx, cluster, ns)
	replica := func(node string) *Runner {
		c := cfg
		c.NodeID = node
		return NewRunner(c, kube, nil, &rtLibrary{}, anonymousQueueClone{})
	}
	newRun := func(key string) StepRun {
		run := StepRun{Execution: pl.ExecutionContainer, RunID: name + "-" + key, WorkRunID: name, StepKey: "checks/" + key, Attempt: 1, OwnerUserID: "local-queue-rehearsal",
			Repository: pl.Repository{Owner: "octocat", Name: "Hello-World", CloneURL: "https://github.com/octocat/Hello-World.git"}, SHA: "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d", Image: cfg.CloneImage,
			Command: "printf 'ran after the outage\\n'", TimeoutSeconds: 120, RunDeadline: time.Now().Add(7 * time.Minute).UTC().Format(time.RFC3339Nano)}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			if _, err := replica("outage-cleanup").CancelRun(cleanup, CancelRequest{RunID: run.RunID}); err != nil {
				t.Errorf("cleanup attempt: %v", err)
			}
		})
		return run
	}
	jobOf := func(run StepRun) string { return JobName(run.RunID, run.StepKey, run.Attempt) }

	proved, unproved := replica("outage-proved"), replica("outage-unproved")
	if got := proved.Run(ctx, newRun("before")); got.Status != pl.OutcomeSucceeded {
		t.Fatalf("a step with the controller up: %+v; failure: %+v", got, got.Failure)
	}

	scale := func(context context.Context, replicas string) ([]byte, error) {
		return exec.CommandContext(context, "kubectl", "--context", cluster, "-n", "kueue-system", "scale", "deployment/kueue-controller-manager", "--replicas="+replicas).CombinedOutput()
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if out, err := scale(cleanup, "1"); err != nil {
			t.Errorf("restoring the controller: %v: %s", err, out)
		}
		if out, err := exec.CommandContext(cleanup, "kubectl", "--context", cluster, "-n", "kueue-system", "rollout", "status", "deployment/kueue-controller-manager", "--timeout=100s").CombinedOutput(); err != nil {
			t.Errorf("waiting for the controller: %v: %s", err, out)
		}
	})
	if out, err := scale(ctx, "0"); err != nil {
		t.Fatalf("stopping the controller: %v: %s", err, out)
	}
	endpoints := func() string {
		return strings.TrimSpace(string(kubectl(nil, "-n", "kueue-system", "get", "endpoints", "kueue-webhook-service", "-o", "jsonpath={.subsets[*].addresses[*].ip}")))
	}
	for endpoints() != "" {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}

	withProof, withoutProof := newRun("with-proof"), newRun("without-proof")
	type outcome struct {
		label  string
		result pl.StepResult
	}
	ended := make(chan outcome, 2)
	go func() { ended <- outcome{"the replica with a proof", proved.Run(ctx, withProof)} }()
	go func() { ended <- outcome{"the replica without one", unproved.Run(ctx, withoutProof)} }()

	suspended := func(run StepRun) bool {
		job, err := kube.GetJob(ctx, jobOf(run))
		return err == nil && job.Spec.Suspend != nil && *job.Spec.Suspend && job.Status.StartTime.IsZero()
	}
	for !suspended(withProof) || !suspended(withoutProof) {
		select {
		case got := <-ended:
			t.Fatalf("a step on %s ended during the outage: %s; failure: %+v", got.label, got.result.Status, got.result.Failure)
		case <-ctx.Done():
			t.Fatalf("both Jobs never stood suspended: %v", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	// Long past the runner's retry budget of ten polls, so a hook refusing a heartbeat
	// or a claim would have shown by now.
	select {
	case got := <-ended:
		t.Fatalf("a step on %s ended during the outage: %s; failure: %+v", got.label, got.result.Status, got.result.Failure)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(15 * time.Second):
	}
	if !suspended(withProof) || !suspended(withoutProof) {
		t.Fatal("a Job started with no controller to admit it")
	}
	if got := endpoints(); got != "" {
		t.Fatalf("the controller came back during the proof (%s), so it proved nothing: turn off the kueue Application's automated sync", got)
	}

	if out, err := scale(ctx, "1"); err != nil {
		t.Fatalf("restarting the controller: %v: %s", err, out)
	}
	for range 2 {
		select {
		case got := <-ended:
			if got.result.Status != pl.OutcomeSucceeded || got.result.LogTail != "ran after the outage" {
				t.Errorf("a step on %s after the outage: %s %q; failure: %+v", got.label, got.result.Status, got.result.LogTail, got.result.Failure)
			}
		case <-ctx.Done():
			t.Fatalf("waiting for the steps once the controller returned: %v", ctx.Err())
		}
	}
	for _, run := range []StepRun{withProof, withoutProof} {
		var pods struct{ Items []Pod }
		if err := json.Unmarshal(kubectl(nil, "-n", ns, "get", "pods", "-l", "job-name="+jobOf(run), "-o", "json"), &pods); err != nil || len(pods.Items) != 1 {
			t.Errorf("%s did not execute exactly once: %d pods, %v", run.StepKey, len(pods.Items), err)
		}
	}
}
