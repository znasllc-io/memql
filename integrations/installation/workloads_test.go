package installation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

type workloadFixture struct {
	artifacts *admissionFixture
	api       *inventoryAPI
	evidence  artifactEvidence
}

func workloadJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	return string(b)
}

func workloadObject(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, receiverJSON([]byte(text), &out))
	return out
}

func workloadOwnerFixture(version, kind, name string) []any {
	return []any{map[string]any{"apiVersion": version, "kind": kind, "name": name, "uid": name + "-uid", "controller": true}}
}

func workloadMeta(name string) map[string]any {
	return map[string]any{"name": name, "namespace": "memql", "uid": name + "-uid", "resourceVersion": "1", "generation": json.Number("1")}
}

func workloadListFixture(version, kind string, items []any) map[string]any {
	return map[string]any{"apiVersion": version, "kind": kind + "List", "metadata": map[string]any{"resourceVersion": "1"}, "items": items}
}

// Use real signed releases, the real OCI byte verifier, and the real Argo
// render codec. Kubernetes responses are protocol fixtures, never proof args.
func newWorkloadFixture(t *testing.T, kinds ...string) *workloadFixture {
	t.Helper()
	f := &workloadFixture{artifacts: newAdmissionFixture(t), api: resourceAPIFixture()}
	old, err := f.artifacts.old.Release()
	require.NoError(t, err)
	next, err := f.artifacts.next.Release()
	require.NoError(t, err)
	oldRef := strings.TrimPrefix(f.artifacts.server.URL, "http://") + "/memql/engine@" + old.Components[0].Artifacts[0].ImageDigest
	nextRef := strings.TrimPrefix(f.artifacts.server.URL, "http://") + "/memql/engine@" + next.Components[0].Artifacts[0].ImageDigest
	before, after := []string{}, []string{}
	pods, sets, jobs := []any{}, []any{}, []any{}
	f.artifacts.config.Bindings = nil
	discovery := map[string][]any{}
	for n, kind := range kinds {
		name := fmt.Sprintf("workload-%d", n)
		version, group, plural := "apps/v1", "apps", strings.ToLower(kind)+"s"
		switch kind {
		case "Pod":
			version, group, plural = "v1", "", "pods"
		case "Job", "CronJob":
			version, group = "batch/v1", "batch"
		case "Cluster":
			version, group = "postgresql.cnpg.io/v1", "postgresql.cnpg.io"
		}
		labels := map[string]any{"workload": name}
		selector := map[string]any{"matchLabels": labels}
		containerName := "engine"
		if kind == "Cluster" {
			labels = map[string]any{"cnpg.io/cluster": name}
			containerName = "postgres"
		}
		podSpec := map[string]any{"containers": []any{map[string]any{"name": containerName, "image": nextRef}}}
		spec := map[string]any{"selector": selector, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec}}
		status := map[string]any{"observedGeneration": json.Number("1"), "replicas": json.Number("1"), "readyReplicas": json.Number("1"), "updatedReplicas": json.Number("1"), "availableReplicas": json.Number("1")}
		slot := imageSlot{resourceIdentity{group, kind, "memql", name}, "containers", containerName}
		switch kind {
		case "StatefulSet":
			status["currentReplicas"], status["currentRevision"], status["updateRevision"] = json.Number("1"), "revision", "revision"
		case "DaemonSet":
			for _, k := range []string{"desiredNumberScheduled", "currentNumberScheduled", "updatedNumberScheduled", "numberReady", "numberAvailable"} {
				status[k] = json.Number("1")
			}
		case "Job":
			status = map[string]any{"succeeded": json.Number("1"), "conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
		case "CronJob":
			spec = map[string]any{"schedule": "0 0 * * *", "jobTemplate": map[string]any{"spec": spec}}
			status = map[string]any{}
		case "Pod":
			spec = podSpec
		case "Cluster":
			spec = map[string]any{"imageName": nextRef, "instances": json.Number("1")}
			status = map[string]any{"readyInstances": json.Number("1"), "currentPrimary": name + "-pod", "targetPrimary": name + "-pod", "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": json.Number("1")}}}
			slot.Field, slot.Container = "imageName", ""
		}
		manifest := map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": name, "namespace": "memql"}, "spec": spec}
		candidate := workloadJSON(t, manifest)
		after = append(after, candidate)
		before = append(before, strings.ReplaceAll(candidate, nextRef, oldRef))
		f.artifacts.config.Bindings = append(f.artifacts.config.Bindings, imageBinding{slot, "engine", "bff"})
		live := workloadObject(t, candidate)
		live["metadata"], live["status"] = workloadMeta(name), status
		base, _, err := apiVersionPath(version)
		require.NoError(t, err)
		discovery[version] = append(discovery[version], map[string]any{"name": plural, "kind": kind, "namespaced": true})
		path := base + "/namespaces/memql/" + plural + "/" + name
		pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": workloadMeta(name + "-pod"), "spec": workloadObject(t, workloadJSON(t, podSpec)), "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": containerName, "ready": true, "imageID": nextRef, "state": map[string]any{"running": map[string]any{}}}}}}
		pm := resourceMap(pod, "metadata")
		pm["labels"] = labels
		pm["ownerReferences"] = workloadOwnerFixture(version, kind, name)
		if kind == "Deployment" {
			setName := name + "-set"
			setMeta := workloadMeta(setName)
			setMeta["labels"] = labels
			setMeta["ownerReferences"] = workloadOwnerFixture(version, kind, name)
			sets = append(sets, map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": setMeta})
			pm["ownerReferences"] = workloadOwnerFixture("apps/v1", "ReplicaSet", setName)
		}
		if kind == "Job" {
			ps := resourceMap(pod, "status")
			ps["phase"] = "Succeeded"
			cs := ps["containerStatuses"].([]any)[0].(map[string]any)
			cs["ready"] = false
			cs["state"] = map[string]any{"terminated": map[string]any{"exitCode": json.Number("0")}}
		}
		if kind == "Cluster" {
			resourceMap(pod, "spec")["initContainers"] = []any{map[string]any{"name": "bootstrap-controller", "image": "operator-infrastructure"}}
			resourceMap(pod, "status")["initContainerStatuses"] = []any{map[string]any{"name": "bootstrap-controller", "state": map[string]any{"terminated": map[string]any{"exitCode": json.Number("0")}}}}
		}
		if kind == "Pod" {
			pod["metadata"] = workloadMeta(name)
			live = pod
		}
		if kind != "CronJob" && kind != "Pod" {
			pods = append(pods, pod)
		}
		f.api.response[path] = workloadJSON(t, live)
	}
	for version, resources := range discovery {
		base, _, err := apiVersionPath(version)
		require.NoError(t, err)
		// Several objects can share a kind; discovery describes types once.
		unique := []any{}
		seen := map[string]bool{}
		for _, r := range resources {
			k := r.(map[string]any)["kind"].(string)
			if !seen[k] {
				unique = append(unique, r)
				seen[k] = true
			}
		}
		f.api.response[base] = workloadJSON(t, map[string]any{"groupVersion": version, "resources": unique})
	}
	f.api.response["api/v1/namespaces/memql/pods?limit=256"] = workloadJSON(t, workloadListFixture("v1", "Pod", pods))
	f.api.response["apis/apps/v1/namespaces/memql/replicasets?limit=256"] = workloadJSON(t, workloadListFixture("apps/v1", "ReplicaSet", sets))
	f.api.response["apis/batch/v1/namespaces/memql/jobs?limit=256"] = workloadJSON(t, workloadListFixture("batch/v1", "Job", jobs))
	f.artifacts.before = renderInventoryFixture(t, strings.Repeat("a", 40), before)
	f.artifacts.after = renderInventoryFixture(t, strings.Repeat("b", 40), after)
	scope, err := newArtifactAdmission(context.Background(), f.api, f.artifacts.config, f.artifacts.old, f.artifacts.next, f.artifacts.before, f.artifacts.after)
	require.NoError(t, err)
	w, err := loadArtifactWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	f.evidence, err = w.run(captureOperator(auth.RoleOwner, "operator"), scope, "operator")
	require.NoError(t, err)
	return f
}

func (f *workloadFixture) scope(t *testing.T) *workloadAdmission {
	t.Helper()
	s, err := newWorkloadAdmission(context.Background(), f.api, f.artifacts.after, f.evidence)
	require.NoError(t, err)
	return s
}

func observeWorkloads(t *testing.T, s *workloadAdmission) []workloadObservation {
	t.Helper()
	obs := []workloadObservation{}
	for _, r := range s.requirements {
		o, err := s.check(context.Background(), r.Key)
		require.NoError(t, err)
		obs = append(obs, o)
	}
	return obs
}

func TestWorkloadsObserveEverySupportedController(t *testing.T) {
	f := newWorkloadFixture(t, "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob", "Pod", "Cluster")
	s := f.scope(t)
	require.Len(t, s.requirements, 8)
	original := observeWorkloads(t, s)
	e, err := s.seal(original)
	require.NoError(t, err)
	require.Equal(t, f.artifacts.after.Digest(), e.render)
	require.Equal(t, f.evidence.digest, e.artifacts)
	require.WithinDuration(t, time.Now().Add(time.Minute), e.expires, 5*time.Second)
	for _, v := range []any{s, e, observeWorkloads(t, s)[0]} {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		require.JSONEq(t, `{}`, string(b))
		require.NotContains(t, fmt.Sprintf("%+v %#v", v, v), "workload-0")
	}
	second := f.scope(t)
	_, err = second.seal(original)
	require.Error(t, err, "new admission cannot inherit old observations")
	_, err = second.seal(observeWorkloads(t, second))
	require.NoError(t, err)
}

func TestWorkloadsRefuseChangedOrIncompleteRuntime(t *testing.T) {
	for _, fault := range []string{"generation", "spec", "controller command", "unavailable", "old image", "wrong manifest", "missing pod", "extra pod", "pod owner", "pod owner omitted", "set owner omitted", "new sidecar", "command", "pod unready", "duplicate status", "deleting", "pagination", "collection duplicate", "missing uid", "malformed status"} {
		t.Run(fault, func(t *testing.T) {
			f := newWorkloadFixture(t, "Deployment")
			s := f.scope(t)
			cp := "apis/apps/v1/namespaces/memql/deployments/workload-0"
			pp := "api/v1/namespaces/memql/pods?limit=256"
			rp := "apis/apps/v1/namespaces/memql/replicasets?limit=256"
			c, p, r := workloadObject(t, f.api.response[cp]), workloadObject(t, f.api.response[pp]), workloadObject(t, f.api.response[rp])
			pod := p["items"].([]any)[0].(map[string]any)
			cs := resourceMap(pod, "status")["containerStatuses"].([]any)[0].(map[string]any)
			switch fault {
			case "generation":
				resourceMap(c, "status")["observedGeneration"] = json.Number("0")
			case "spec":
				resourceMap(c, "spec")["template"] = map[string]any{}
			case "controller command":
				workloadPodSpec(resourceMap(c, "spec"), "Deployment")["containers"].([]any)[0].(map[string]any)["command"] = []any{"unexpected"}
			case "unavailable":
				resourceMap(c, "status")["unavailableReplicas"] = json.Number("1")
			case "old image":
				resourceMap(pod, "spec")["containers"].([]any)[0].(map[string]any)["image"] = oldImage
			case "wrong manifest":
				cs["imageID"] = oldImage
			case "missing pod":
				p["items"] = []any{}
			case "extra pod":
				other := workloadObject(t, workloadJSON(t, pod))
				resourceMap(other, "metadata")["name"] = "extra"
				p["items"] = append(p["items"].([]any), other)
			case "pod owner":
				resourceMap(pod, "metadata")["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "different"
			case "pod owner omitted":
				delete(resourceMap(pod, "metadata"), "ownerReferences")
			case "set owner omitted":
				delete(resourceMap(r["items"].([]any)[0].(map[string]any), "metadata"), "ownerReferences")
			case "new sidecar":
				resourceMap(pod, "spec")["containers"] = append(resourceMap(pod, "spec")["containers"].([]any), map[string]any{"name": "extra", "image": oldImage})
			case "command":
				resourceMap(pod, "spec")["containers"].([]any)[0].(map[string]any)["command"] = []any{"unexpected"}
			case "pod unready":
				cs["ready"] = false
			case "duplicate status":
				resourceMap(pod, "status")["containerStatuses"] = append(resourceMap(pod, "status")["containerStatuses"].([]any), cs)
			case "deleting":
				resourceMap(pod, "metadata")["deletionTimestamp"] = "2026-10-07T00:00:00Z"
			case "pagination":
				resourceMap(p, "metadata")["continue"] = "more"
			case "collection duplicate":
				p["items"] = append(p["items"].([]any), pod)
			case "missing uid":
				delete(resourceMap(pod, "metadata"), "uid")
			case "malformed status":
				resourceMap(pod, "status")["initContainerStatuses"] = "wrong"
			}
			f.api.response[cp], f.api.response[pp], f.api.response[rp] = workloadJSON(t, c), workloadJSON(t, p), workloadJSON(t, r)
			o, err := s.check(context.Background(), s.requirements[0].Key)
			require.Error(t, err)
			require.Empty(t, o.reads)
		})
	}
}

type changingWorkloadAPI struct {
	*inventoryAPI
	path  string
	count int
}

func (a *changingWorkloadAPI) Do(ctx context.Context, method, path, ct string, body []byte) ([]byte, error) {
	if path == a.path {
		a.count++
		if a.count == 2 {
			a.response[path] = strings.ReplaceAll(a.response[path], `"resourceVersion":"1"`, `"resourceVersion":"2"`)
		}
	}
	return a.inventoryAPI.Do(ctx, method, path, ct, body)
}

func TestWorkloadsReobserveControllerAndCollectionBeforeSealing(t *testing.T) {
	for _, path := range []string{"apis/apps/v1/namespaces/memql/deployments/workload-0", "apis/apps/v1/namespaces/memql/replicasets?limit=256", "api/v1/namespaces/memql/pods?limit=256"} {
		t.Run(path, func(t *testing.T) {
			f := newWorkloadFixture(t, "Deployment")
			s := f.scope(t)
			s.api = &changingWorkloadAPI{inventoryAPI: f.api, path: path}
			_, err := s.check(context.Background(), s.requirements[0].Key)
			require.Error(t, err)
		})
	}
}

func TestWorkloadsSealRefusesOmissionSubstitutionAndStaleness(t *testing.T) {
	f := newWorkloadFixture(t, "Deployment", "Deployment")
	s := f.scope(t)
	original := observeWorkloads(t, s)
	for _, fault := range []string{"omitted", "duplicate", "other scope", "other key", "future", "old", "unfinished", "forged reads", "expired"} {
		t.Run(fault, func(t *testing.T) {
			obs := append([]workloadObservation(nil), original...)
			scope := *s
			switch fault {
			case "omitted":
				obs = obs[:1]
			case "duplicate":
				obs[1] = obs[0]
			case "other scope":
				obs[0].scope = "wrong"
			case "other key":
				obs[0].key = "wrong"
			case "future":
				obs[0].completed = time.Now().Add(time.Minute)
			case "old":
				scope.created = time.Now().Add(-2 * time.Minute)
				obs[0].started = time.Now().Add(-time.Minute - time.Second)
			case "unfinished":
				obs[0].completed = obs[0].started.Add(-time.Second)
			case "forged reads":
				obs[0].reads = "claimed"
			case "expired":
				scope.expires = time.Now().Add(-time.Second)
			}
			e, err := scope.seal(obs)
			require.Error(t, err)
			require.Empty(t, e.digest)
		})
	}
	for _, fault := range []string{"render", "workflow", "operator", "images", "expiry", "future"} {
		t.Run("admission/"+fault, func(t *testing.T) {
			e := f.evidence
			switch fault {
			case "render":
				e.before, e.after = "wrong", "wrong"
			case "workflow":
				e.workflow = ""
			case "operator":
				e.operator = ""
			case "images":
				e.runtimeImages = nil
			case "expiry":
				e.expires = time.Now().Add(-time.Second)
			case "future":
				e.observed = time.Now().Add(time.Minute)
			}
			_, err := newWorkloadAdmission(context.Background(), f.api, f.artifacts.after, e)
			require.Error(t, err)
		})
	}
}
