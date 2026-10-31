package install

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"gopkg.in/yaml.v3"
)

// liveCapacityCluster is what both opt-in proofs run behind: an explicit local
// k3d context whose API server is on this machine, and kubectl against it.
func liveCapacityCluster(t *testing.T) (ctx context.Context, cluster string, command func(input any, args ...string) ([]byte, error), run func(input any, args ...string) []byte) {
	t.Helper()
	cluster = os.Getenv("MEMQL_CAPACITY_TEST_CONTEXT")
	if cluster == "" {
		t.Skip("set MEMQL_CAPACITY_TEST_CONTEXT to an explicit local k3d context")
	}
	if !strings.HasPrefix(cluster, "k3d-") {
		t.Fatal("capacity qualification accepts only a local k3d context")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	command = func(input any, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--context", cluster}, args...)...)
		if input != nil {
			data, err := json.Marshal(input)
			if err != nil {
				return nil, err
			}
			cmd.Stdin = bytes.NewReader(data)
		}
		return cmd.CombinedOutput()
	}
	run = func(input any, args ...string) []byte {
		t.Helper()
		out, err := command(input, args...)
		if err != nil {
			t.Fatalf("kubectl %v: %v: %s", args, err, out)
		}
		return out
	}
	server := strings.TrimSpace(string(run(nil, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}")))
	u, err := url.Parse(server)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" && u.Hostname() != "0.0.0.0") {
		t.Fatalf("refusing non-local API server %q", server)
	}
	return ctx, cluster, command, run
}

// This opt-in proof uses the installed controller and its actual namespace-
// scoped webhooks. It never changes the installed queue, namespace quota or
// serving workloads. The two disposable Jobs fit the existing local ceiling.
func TestLocalQueuePreservesExecutionAllowance(t *testing.T) {
	ctx, cluster, command, run := liveCapacityCluster(t)
	var existing struct{ Items []json.RawMessage }
	if err := json.Unmarshal(run(nil, "-n", "memql-pipelines", "get", "jobs", "-o", "json"), &existing); err != nil || len(existing.Items) != 0 {
		t.Fatal("qualification requires the local pipeline namespace to be idle")
	}
	name := fmt.Sprintf("capacity-proof-%d", time.Now().UnixNano())
	const namespace = "memql-pipelines"
	type object map[string]any
	var created []struct{ kind, name, namespace string }
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 50*time.Second)
		defer stop()
		for i := len(created) - 1; i >= 0; i-- {
			o := created[i]
			args := []string{"--context", cluster, "delete", o.kind, o.name, "--ignore-not-found", "--wait=false"}
			if o.namespace != "" {
				args = append(args, "-n", o.namespace)
			}
			if out, err := exec.CommandContext(cleanup, "kubectl", args...).CombinedOutput(); err != nil {
				t.Errorf("cleanup %s/%s: %v: %s", o.kind, o.name, err, out)
			}
		}
	})
	create := func(kind, objectName, ns string, spec object) {
		t.Helper()
		apiVersion := "kueue.x-k8s.io/v1beta2"
		if kind == "Job" {
			apiVersion = "batch/v1"
		}
		metadata := object{"name": objectName}
		if ns != "" {
			metadata["namespace"] = ns
		}
		if kind == "Job" {
			metadata["labels"] = object{"kueue.x-k8s.io/queue-name": name}
		}
		run(object{"apiVersion": apiVersion, "kind": kind, "metadata": metadata, "spec": spec}, "create", "-f", "-")
		created = append(created, struct{ kind, name, namespace string }{kind, objectName, ns})
	}
	create("ResourceFlavor", name, "", object{"nodeLabels": object{"memql.io/pipeline-pool": "builds"}, "nodeTaints": []object{{"key": "memql.io/pipeline-pool", "value": "builds", "effect": "NoSchedule"}}})
	create("ClusterQueue", name, "", object{
		"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": namespace}},
		"queueingStrategy":  "BestEffortFIFO", "stopPolicy": "Hold",
		"preemption": object{"withinClusterQueue": "Never", "reclaimWithinCohort": "Never", "borrowWithinCohort": object{"policy": "Never"}},
		"resourceGroups": []object{{"coveredResources": []string{"cpu", "memory", "ephemeral-storage", "pods"}, "flavors": []object{{"name": name, "resources": []object{
			{"name": "cpu", "nominalQuota": "500m"}, {"name": "memory", "nominalQuota": "128Mi"}, {"name": "ephemeral-storage", "nominalQuota": "128Mi"}, {"name": "pods", "nominalQuota": "1"},
		}}}}},
	})
	create("LocalQueue", name, namespace, object{"clusterQueue": name})
	// Exercise the shipped independent ceiling policy, under disposable names.
	// The real queue and its operator configuration remain untouched.
	parameter := name + "-capacity"
	run(object{"apiVersion": "capacity.memql.io/v1alpha1", "kind": "BackgroundCapacityPolicy", "metadata": object{"name": parameter}, "spec": object{
		"cpuMilli": 500, "memoryMiB": 128, "diskMiB": 128, "jobs": 1, "nodePool": "builds",
	}}, "create", "-f", "-")
	created = append(created, struct{ kind, name, namespace string }{"BackgroundCapacityPolicy", parameter, ""})
	policy, err := os.ReadFile("../../k8s/components/pipelines-capacity/ceiling-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(strings.ReplaceAll(string(policy), "memql-background", name)))
	for {
		var o object
		if err := decoder.Decode(&o); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		run(o, "create", "-f", "-")
		created = append(created, struct{ kind, name, namespace string }{o["kind"].(string), name + "-ceiling", ""})
	}
	// Policy publication is asynchronous. Only its specific rejection is proof
	// of enforcement; an unrelated API failure never counts as a denied edit.
	denyPatch := `[ {"op":"replace","path":"/spec/resourceGroups/0/flavors/0/resources/0/nominalQuota","value":"501m"} ]`
	for {
		out, err := command(nil, "patch", "clusterqueue", name, "--type=json", "--dry-run=server", "-p", denyPatch)
		if err != nil && strings.Contains(string(out), "protected background capacity ceiling") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("independent ceiling was not enforced: %v: %s", err, out)
		case <-time.After(time.Second):
		}
	}
	for _, patch := range []string{
		`[{"op":"replace","path":"/spec/resourceGroups/0/flavors/0/resources/1/nominalQuota","value":"129Mi"}]`,
		`[{"op":"replace","path":"/spec/resourceGroups/0/flavors/0/resources/2/nominalQuota","value":"129Mi"}]`,
		`[{"op":"replace","path":"/spec/resourceGroups/0/flavors/0/resources/3/nominalQuota","value":"2"}]`,
		`[{"op":"replace","path":"/spec/stopPolicy","value":"HoldAndDrain"}]`,
		`[{"op":"add","path":"/spec/cohortName","value":"customer-services"}]`,
	} {
		out, err := command(nil, "patch", "clusterqueue", name, "--type=json", "--dry-run=server", "-p", patch)
		if err == nil || (!strings.Contains(string(out), "protected background capacity ceiling") && !strings.Contains(string(out), "cannot interrupt admitted work") && !strings.Contains(string(out), "cannot borrow capacity from customer services")) {
			t.Fatalf("unsafe change was not rejected by the shipped policy: %v: %s", err, out)
		}
	}
	run(nil, "patch", "clusterqueue", name, "--type=merge", "--dry-run=server", "-p", `{"spec":{"stopPolicy":"None"}}`)
	job := func(seconds, deadline int) object {
		return object{"suspend": true, "backoffLimit": 0, "activeDeadlineSeconds": deadline, "template": object{"spec": object{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"nodeSelector":    object{"memql.io/pipeline-pool": "builds"},
			"tolerations":     []object{{"key": "memql.io/pipeline-pool", "operator": "Equal", "value": "builds", "effect": "NoSchedule"}},
			"securityContext": object{"runAsNonRoot": true, "runAsUser": 1000, "seccompProfile": object{"type": "RuntimeDefault"}},
			"containers": []object{{"name": "proof", "image": "memql-workbench:local", "imagePullPolicy": "Never", "command": []string{"/bin/sh", "-c", fmt.Sprintf("sleep %d", seconds)},
				"securityContext": object{"allowPrivilegeEscalation": false, "capabilities": object{"drop": []string{"ALL"}}},
				"resources":       object{"requests": object{"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"}, "limits": object{"cpu": "500m", "memory": "64Mi", "ephemeral-storage": "64Mi"}},
			}},
		}}}
	}
	type jobState struct {
		Metadata struct{ CreationTimestamp time.Time }
		Spec     struct{ Suspend bool }
		Status   struct {
			StartTime time.Time
			Succeeded int
			Failed    int
		}
	}
	read := func(n string) jobState {
		t.Helper()
		var j jobState
		if err := json.Unmarshal(run(nil, "-n", namespace, "get", "job", n, "-o", "json"), &j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	wait := func(reason string, ready func() bool) {
		t.Helper()
		for !ready() {
			select {
			case <-ctx.Done():
				t.Fatalf("%s: %v", reason, ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	create("Job", name+"-first", namespace, job(120, 180))
	first := read(name + "-first")
	if !first.Spec.Suspend || !first.Status.StartTime.IsZero() {
		t.Fatal("a held queue started work")
	}
	run(nil, "patch", "clusterqueue", name, "--type=merge", "-p", `{"spec":{"stopPolicy":"None"}}`)
	wait("first job admission", func() bool { return !read(name + "-first").Status.StartTime.IsZero() })
	create("Job", name+"-second", namespace, job(3, 15))
	queuedAt := time.Now()
	for time.Since(queuedAt) < 18*time.Second {
		second := read(name + "-second")
		if !second.Spec.Suspend || !second.Status.StartTime.IsZero() || second.Status.Failed != 0 {
			t.Fatalf("queued job consumed an execution deadline: %+v", second)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	run(nil, "-n", namespace, "delete", "job", name+"-first", "--wait=true", "--timeout=40s")
	wait("second job completes after waiting longer than its execution allowance", func() bool {
		second := read(name + "-second")
		if second.Status.Failed != 0 {
			t.Fatalf("second job failed after admission: %+v", second)
		}
		return second.Status.Succeeded == 1
	})
	second := read(name + "-second")
	if second.Status.StartTime.Sub(second.Metadata.CreationTimestamp) < 15*time.Second {
		t.Fatal("proof did not wait beyond the execution timeout")
	}
	var pods struct {
		Items []struct{ Spec struct{ NodeName string } }
	}
	if err := json.Unmarshal(run(nil, "-n", namespace, "get", "pods", "-l", "job-name="+name+"-second", "-o", "json"), &pods); err != nil || len(pods.Items) != 1 {
		t.Fatal("expected exactly one attempt")
	}
	pool := strings.TrimSpace(string(run(nil, "get", "node", pods.Items[0].Spec.NodeName, "-o", `jsonpath={.metadata.labels.memql\.io/pipeline-pool}`)))
	if pool != "builds" {
		t.Fatal("work escaped the dedicated build pool")
	}
	t.Logf("held job stayed suspended; second job waited %s beyond a 15s execution allowance, completed once on %s", second.Status.StartTime.Sub(second.Metadata.CreationTimestamp), pods.Items[0].Spec.NodeName)
}

// The promoter (`memql pipeline-deadline-priority`) stands on a behaviour of the
// pinned controller rather than on anything it could check for itself: Kueue
// re-prioritizes a workload that has not reserved quota when its Job's
// priority-class label changes, a Job that started with no class included, and a
// strict queue then admits by the new order. This proves that with a real pass, on
// a disposable strict queue holding the reverse of the order the pass produces.
// Run it before moving the Kueue pin, whose checksum test stops a new version
// until someone has. Nothing but its own Jobs and queue is touched.
func TestAPromotedJobIsAdmittedFirst(t *testing.T) {
	ctx, cluster, command, run := liveCapacityCluster(t)
	name := fmt.Sprintf("promotion-proof-%d", time.Now().UnixNano())
	const namespace = "memql-pipelines"
	type object = map[string]any
	type createdObject struct{ kind, name, namespace string }
	var created []createdObject
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 50*time.Second)
		defer stop()
		for i := len(created) - 1; i >= 0; i-- {
			o := created[i]
			args := []string{"--context", cluster, "delete", o.kind, o.name, "--ignore-not-found", "--wait=false"}
			if o.namespace != "" {
				args = append(args, "-n", o.namespace)
			}
			if out, err := exec.CommandContext(cleanup, "kubectl", args...).CombinedOutput(); err != nil {
				t.Errorf("cleanup %s/%s: %v: %s", o.kind, o.name, err, out)
			}
		}
	})
	wait := func(reason string, ready func() bool) {
		t.Helper()
		for !ready() {
			select {
			case <-ctx.Done():
				t.Fatalf("%s: %v", reason, ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	create := func(kind, objectName, ns string, body object) {
		t.Helper()
		metadata := object{"name": objectName}
		if ns != "" {
			metadata["namespace"] = ns
		}
		body["apiVersion"], body["kind"], body["metadata"] = "kueue.x-k8s.io/v1beta2", kind, metadata
		// The other proof's disposable ceiling policy covers every ClusterQueue, and
		// its removal is published asynchronously: run right after it, this can meet
		// the policy's binding after its parameter is gone, which denies everything.
		// Only that passes as a wait; any other refusal is the failure it says.
		wait("creating "+kind, func() bool {
			out, err := command(body, "create", "-f", "-")
			if err != nil && !strings.Contains(string(out), "ValidatingAdmissionPolicy 'capacity-proof-") {
				t.Fatalf("creating %s: %v: %s", kind, err, out)
			}
			return err == nil
		})
		created = append(created, createdObject{kind, objectName, ns})
	}

	// The classes a pass promotes to are the component's, and may already be
	// installed. Only what this test creates is removed again.
	classes, err := os.ReadFile("../../k8s/components/pipelines-capacity/priorities.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(classes))
	for {
		var class object
		if err := decoder.Decode(&class); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		className := class["metadata"].(object)["name"].(string)
		if len(bytes.TrimSpace(run(nil, "get", "workloadpriorityclass", className, "--ignore-not-found", "-o", "name"))) == 0 {
			run(class, "create", "-f", "-")
			created = append(created, createdObject{"WorkloadPriorityClass", className, ""})
		}
	}
	lowClass := name + "-low"
	create("WorkloadPriorityClass", lowClass, "", object{"value": 10})
	create("ResourceFlavor", name, "", object{"spec": object{"nodeLabels": object{"memql.io/pipeline-pool": "builds"}, "nodeTaints": []object{{"key": "memql.io/pipeline-pool", "value": "builds", "effect": "NoSchedule"}}}})
	create("ClusterQueue", name, "", object{"spec": object{
		"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": namespace}},
		"queueingStrategy":  "StrictFIFO", "stopPolicy": "Hold",
		"preemption": object{"withinClusterQueue": "Never", "reclaimWithinCohort": "Never", "borrowWithinCohort": object{"policy": "Never"}},
		"resourceGroups": []object{{"coveredResources": []string{"cpu", "memory", "ephemeral-storage", "pods"}, "flavors": []object{{"name": name, "resources": []object{
			{"name": "cpu", "nominalQuota": "500m"}, {"name": "memory", "nominalQuota": "128Mi"}, {"name": "ephemeral-storage", "nominalQuota": "128Mi"}, {"name": "pods", "nominalQuota": "1"},
		}}}}},
	}})
	create("LocalQueue", name, namespace, object{"spec": object{"clusterQueue": name}})

	// Created oldest first, so that without a pass the order is young, old, urgent:
	// the class it starts in admits young and old before urgent, which has none.
	now := time.Now()
	step := func(jobName, class string, left time.Duration) string {
		t.Helper()
		jobName = name + "-" + jobName
		labels := object{"kueue.x-k8s.io/queue-name": name, pipelinesteps.LabelManagedBy: pipelinesteps.ManagedBy}
		if class != "" {
			labels["kueue.x-k8s.io/priority-class"] = class
		}
		run(object{"apiVersion": "batch/v1", "kind": "Job",
			"metadata": object{"name": jobName, "namespace": namespace, "labels": labels,
				"annotations": object{pipelinesteps.AnnotRunDeadline: now.Add(left).UTC().Format(time.RFC3339Nano)}},
			"spec": object{"suspend": true, "backoffLimit": 0, "activeDeadlineSeconds": 180, "template": object{"spec": object{
				"restartPolicy": "Never", "automountServiceAccountToken": false,
				"nodeSelector":    object{"memql.io/pipeline-pool": "builds"},
				"tolerations":     []object{{"key": "memql.io/pipeline-pool", "operator": "Equal", "value": "builds", "effect": "NoSchedule"}},
				"securityContext": object{"runAsNonRoot": true, "runAsUser": 1000, "seccompProfile": object{"type": "RuntimeDefault"}},
				"containers": []object{{"name": "proof", "image": "memql-workbench:local", "imagePullPolicy": "Never", "command": []string{"/bin/sh", "-c", "sleep 170"},
					"securityContext": object{"allowPrivilegeEscalation": false, "capabilities": object{"drop": []string{"ALL"}}},
					"resources":       object{"requests": object{"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"}, "limits": object{"cpu": "500m", "memory": "64Mi", "ephemeral-storage": "64Mi"}},
				}},
			}}},
		}, "create", "-f", "-")
		created = append(created, createdObject{"Job", jobName, namespace})
		return jobName
	}
	young := step("young", lowClass, 6*time.Hour)
	old := step("old", lowClass, 30*time.Minute)
	urgent := step("urgent", "", 10*time.Minute)

	proxy := exec.CommandContext(ctx, "kubectl", "--context", cluster, "proxy", "--address=127.0.0.1", "--port=0")
	stdout, err := proxy.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Process.Kill(); _ = proxy.Wait() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("kubectl proxy did not start: %v", scanner.Err())
	}
	base := "http://" + strings.TrimPrefix(scanner.Text(), "Starting to serve on ")
	kube := pipelinesteps.NewKube(deploycontrol.NewClusterAPIWith(base, "", &http.Client{Timeout: 20 * time.Second}), namespace)

	settings := pipelinesteps.DeadlinePriority{Queue: name, ExpiringWindow: 15 * time.Minute, AgedWindow: 60 * time.Minute}
	pass, err := settings.Pass(ctx, kube, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pass.Failed) != 0 || len(pass.Promoted) != 2 {
		t.Fatalf("pass: %+v", pass)
	}

	priorities := func() map[string]int {
		t.Helper()
		var list struct {
			Items []struct {
				Metadata struct {
					OwnerReferences []struct{ Name string } `json:"ownerReferences"`
				}
				Spec struct{ Priority int }
			}
		}
		if err := json.Unmarshal(run(nil, "-n", namespace, "get", "workloads", "-o", "json"), &list); err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, item := range list.Items {
			if len(item.Metadata.OwnerReferences) == 1 {
				got[item.Metadata.OwnerReferences[0].Name] = item.Spec.Priority
			}
		}
		return got
	}
	wait("the controller re-prioritizing both workloads", func() bool {
		got := priorities()
		return got[urgent] == 400 && got[old] == 350 && got[young] == 10
	})

	waiting := func(jobName string) bool {
		t.Helper()
		var job struct{ Spec struct{ Suspend bool } }
		if err := json.Unmarshal(run(nil, "-n", namespace, "get", "job", jobName, "-o", "json"), &job); err != nil {
			t.Fatal(err)
		}
		return job.Spec.Suspend
	}
	run(nil, "patch", "clusterqueue", name, "--type=merge", "-p", `{"spec":{"stopPolicy":"None"}}`)
	wait("the expiring Job, created last with no class, admitted first", func() bool { return !waiting(urgent) })
	if !waiting(old) || !waiting(young) {
		t.Fatal("the queue admitted past its one slot")
	}
	run(nil, "-n", namespace, "delete", "job", urgent, "--wait=true", "--timeout=40s")
	wait("the aged Job admitted next", func() bool { return !waiting(old) })
	if !waiting(young) {
		t.Fatal("the young Job went ahead of the aged one, though it is older")
	}
	run(nil, "-n", namespace, "delete", "job", old, "--wait=true", "--timeout=40s")
	wait("the young Job admitted last", func() bool { return !waiting(young) })
}
