package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This opt-in proof uses the installed controller and its actual namespace-
// scoped webhooks. It never changes the installed queue, namespace quota or
// serving workloads. The two disposable Jobs fit the existing local ceiling.
func TestLocalQueuePreservesExecutionAllowance(t *testing.T) {
	cluster := os.Getenv("MEMQL_CAPACITY_TEST_CONTEXT")
	if cluster == "" {
		t.Skip("set MEMQL_CAPACITY_TEST_CONTEXT to an explicit local k3d context")
	}
	if !strings.HasPrefix(cluster, "k3d-") {
		t.Fatal("capacity qualification accepts only a local k3d context")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	command := func(input any, args ...string) ([]byte, error) {
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
	run := func(input any, args ...string) []byte {
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
