package installation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

func TestWorkloadsUseRenderedCardinalityWhenDefaultWasOmitted(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet", "ReplicaSet", "Job"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkloadFixture(t, kind)
			s := f.scope(t)
			key := s.requirements[0].Key
			object := s.objects[key]
			field := "replicas"
			if kind == "Job" {
				field = "completions"
			}
			require.NotContains(t, resourceMap(object.value, "spec"), field)
			live := workloadObject(t, f.api.response[object.apiPath])
			resourceMap(live, "spec")[field] = json.Number("1")
			f.api.response[object.apiPath] = workloadJSON(t, live)
			_, err := s.check(context.Background(), key)
			require.NoError(t, err, "the API's materialized default still matches the admitted render")

			podPath := "api/v1/namespaces/memql/pods?limit=256"
			pods := workloadObject(t, f.api.response[podPath])
			if kind == "Job" {
				// A live completion target may not redefine what the reviewed Job
				// was supposed to run, even when all its extra Pods succeeded.
				resourceMap(live, "spec")[field] = json.Number("2")
				resourceMap(live, "status")["succeeded"] = json.Number("2")
				original := pods["items"].([]any)[0]
				extra := workloadObject(t, workloadJSON(t, original))
				resourceMap(extra, "metadata")["name"] = "extra-success"
				resourceMap(extra, "metadata")["uid"] = "extra-success-uid"
				pods["items"] = append(pods["items"].([]any), extra)
			} else {
				// A human/HPA scale-down is live state, never the admitted
				// default of one. Empty Pod inventories must not make it green.
				resourceMap(live, "spec")[field] = json.Number("0")
				for _, count := range []string{"replicas", "readyReplicas", "availableReplicas", "updatedReplicas", "currentReplicas"} {
					resourceMap(live, "status")[count] = json.Number("0")
				}
				pods["items"] = []any{}
			}
			f.api.response[podPath] = workloadJSON(t, pods)
			f.api.response[object.apiPath] = workloadJSON(t, live)
			_, err = s.check(context.Background(), key)
			require.Error(t, err, "live counts cannot replace omitted defaults in the admitted render")

			// An explicitly reviewed zero-replica target (or two-completion
			// Job) is legitimate. Re-render and obtain fresh native artifact
			// evidence so this positive case has the new admitted intention.
			manifest := workloadObject(t, string(f.artifacts.after.Resources()[0]))
			resourceMap(manifest, "spec")[field] = resourceMap(live, "spec")[field]
			f.artifacts.after = renderInventoryFixture(t, strings.Repeat("b", 40), []string{workloadJSON(t, manifest)})
			admission, err := newArtifactAdmission(context.Background(), f.api, f.artifacts.config, f.artifacts.old, f.artifacts.next, f.artifacts.before, f.artifacts.after)
			require.NoError(t, err)
			workflow, err := loadArtifactWorkflow(strings.Repeat("d", 40))
			require.NoError(t, err)
			f.evidence, err = workflow.run(captureOperator(auth.RoleOwner, "operator"), admission, "operator")
			require.NoError(t, err)
			newScope := f.scope(t)
			_, err = newScope.seal(observeWorkloads(t, newScope))
			require.NoError(t, err, "explicit admitted cardinality still governs legitimate convergence")
		})
	}
}

func TestWorkloadProfilesRefuseUnconvergedControllers(t *testing.T) {
	for _, kind := range []string{"StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob", "Pod", "Cluster"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkloadFixture(t, kind)
			s := f.scope(t)
			var object resourceObject
			for _, o := range s.objects {
				object = o
			}
			live := workloadObject(t, f.api.response[object.apiPath])
			status := resourceMap(live, "status")
			switch kind {
			case "StatefulSet":
				status["updateRevision"] = "unapplied"
			case "DaemonSet":
				status["numberMisscheduled"] = json.Number("1")
			case "ReplicaSet":
				status["availableReplicas"] = json.Number("0")
			case "Job":
				status["active"] = json.Number("1")
			case "CronJob":
				status["active"] = []any{map[string]any{"name": "active"}}
			case "Pod":
				status["phase"] = "Pending"
			case "Cluster":
				status["targetPrimary"] = "not-ready"
			}
			f.api.response[object.apiPath] = workloadJSON(t, live)
			_, err := s.check(context.Background(), s.requirements[0].Key)
			require.Error(t, err)
		})
	}
}

const workloadPodsPath = "api/v1/namespaces/memql/pods?limit=256"

// emptyNodePoolDaemonSet is a one-DaemonSet fixture in the state its controller
// reports once the autoscaler has removed every node the DaemonSet's selector
// admits: the current generation observed, nothing scheduled and no owned Pod
// left. It also returns the Pod that ran there before, for the refusal cases.
func emptyNodePoolDaemonSet(t *testing.T) (*workloadFixture, *workloadAdmission, resourceObject, map[string]any, map[string]any) {
	t.Helper()
	f := newWorkloadFixture(t, "DaemonSet")
	s := f.scope(t)
	object := s.objects[s.requirements[0].Key]
	live := workloadObject(t, f.api.response[object.apiPath])
	status := resourceMap(live, "status")
	for _, field := range []string{"desiredNumberScheduled", "currentNumberScheduled", "numberReady", "numberMisscheduled"} {
		status[field] = json.Number("0")
	}
	// The API omits these two when they are zero.
	delete(status, "updatedNumberScheduled")
	delete(status, "numberAvailable")
	f.api.response[object.apiPath] = workloadJSON(t, live)
	pods := workloadObject(t, f.api.response[workloadPodsPath])
	pod := pods["items"].([]any)[0].(map[string]any)
	pods["items"] = []any{}
	f.api.response[workloadPodsPath] = workloadJSON(t, pods)
	return f, s, object, live, pod
}

// A DaemonSet's cardinality is its controller's placement, not a rendered
// target. Production's build pool scales to zero when no build runs, and its
// buildkit DaemonSet then has no node to run on. On 2026-11-25 that refused the
// 0.31.41 install's completion ("daemon workload has no observed placement")
// until a node joined the pool. Nothing scheduled under the observed current
// template is converged: no Pod can run an unapproved image.
func TestWorkloadsAcceptDaemonSetOnEmptyNodePool(t *testing.T) {
	_, s, _, _, _ := emptyNodePoolDaemonSet(t)
	_, err := s.seal(observeWorkloads(t, s))
	require.NoError(t, err)
}

// Zero is converged only as the controller's settled answer for the current
// template. Unwritten or stale status, and Pods the controller has not removed
// yet, still refuse.
func TestWorkloadsRefuseUnsettledEmptyDaemonSet(t *testing.T) {
	for fault, want := range map[string]string{
		"status unwritten":      "daemon workload has no observed placement",
		"generation unobserved": "daemon workload has no observed placement",
		"pod remains":           "workload ready Pod count differs: got 1, require 0",
		"pod terminating":       "workload still has a terminating Pod",
		"pod misscheduled":      "daemon workload has unavailable or misplaced pods",
	} {
		t.Run(fault, func(t *testing.T) {
			f, s, object, live, pod := emptyNodePoolDaemonSet(t)
			switch fault {
			case "status unwritten":
				live["status"] = map[string]any{}
			case "generation unobserved":
				resourceMap(live, "status")["observedGeneration"] = json.Number("0")
			case "pod remains", "pod terminating":
				if fault == "pod terminating" {
					resourceMap(pod, "metadata")["deletionTimestamp"] = "2026-11-25T23:48:00Z"
				}
				pods := workloadObject(t, f.api.response[workloadPodsPath])
				pods["items"] = []any{pod}
				f.api.response[workloadPodsPath] = workloadJSON(t, pods)
			case "pod misscheduled":
				resourceMap(live, "status")["numberMisscheduled"] = json.Number("1")
			}
			f.api.response[object.apiPath] = workloadJSON(t, live)
			_, err := s.check(context.Background(), s.requirements[0].Key)
			require.ErrorContains(t, err, want)
		})
	}
}

func TestWorkloadsBindIndexAndManifestToTheAssignedNodePlatform(t *testing.T) {
	f := newWorkloadFixture(t, "Deployment")
	publication, err := f.artifacts.next.Release()
	require.NoError(t, err)
	manifest := publication.Components[0].Artifacts[0].ImageDigest
	oldRef := strings.TrimPrefix(f.artifacts.server.URL, "http://") + "/memql/engine@" + manifest
	media := "application/vnd.oci.image.index.v1+json"
	f.artifacts.mu.Lock()
	index := []byte(workloadJSON(t, map[string]any{"schemaVersion": 2, "mediaType": media, "manifests": []any{map[string]any{"mediaType": f.artifacts.media[manifest], "digest": manifest, "size": len(f.artifacts.bodies[manifest]), "platform": map[string]any{"os": "linux", "architecture": "arm64"}}}}))
	root := admissionSHA(index)
	f.artifacts.bodies[root], f.artifacts.media[root] = index, media
	f.artifacts.mu.Unlock()
	newRef := strings.ReplaceAll(oldRef, manifest, root)
	publication.Components[0].Artifacts[0].ImageDigest = root
	f.artifacts.next = signAdmissionRelease(t, publication)
	manifests := []string{}
	for _, m := range f.artifacts.after.Resources() {
		manifests = append(manifests, strings.ReplaceAll(string(m), oldRef, newRef))
	}
	f.artifacts.after = renderInventoryFixture(t, strings.Repeat("b", 40), manifests)
	for path, body := range f.api.response {
		if strings.Contains(path, "deployments/") {
			f.api.response[path] = strings.ReplaceAll(body, oldRef, newRef)
		}
	}
	p := "api/v1/namespaces/memql/pods?limit=256"
	list := workloadObject(t, f.api.response[p])
	pod := list["items"].([]any)[0].(map[string]any)
	resourceMap(pod, "spec")["containers"].([]any)[0].(map[string]any)["image"] = newRef
	f.api.response[p] = workloadJSON(t, list)
	a, err := newArtifactAdmission(context.Background(), f.api, f.artifacts.config, f.artifacts.old, f.artifacts.next, f.artifacts.before, f.artifacts.after)
	require.NoError(t, err)
	w, err := loadArtifactWorkflow(strings.Repeat("d", 40))
	require.NoError(t, err)
	f.evidence, err = w.run(captureOperator(auth.RoleOwner, "operator"), a, "operator")
	require.NoError(t, err)
	require.Equal(t, oldRef, f.evidence.runtimeImages[newRef], "native OCI receipt binds index to selected platform manifest")
	s := f.scope(t)
	_, err = s.seal(observeWorkloads(t, s))
	require.NoError(t, err)
	resourceMap(pod, "status")["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = newRef
	f.api.response[p] = workloadJSON(t, list)
	_, err = s.check(context.Background(), s.requirements[0].Key)
	require.NoError(t, err, "containerd reports the verified root index")
	nodePath := "api/v1/nodes/worker"
	node := workloadObject(t, f.api.response[nodePath])
	resourceMap(resourceMap(node, "status"), "nodeInfo")["architecture"] = "amd64"
	f.api.response[nodePath] = workloadJSON(t, node)
	_, err = s.check(context.Background(), s.requirements[0].Key)
	require.ErrorContains(t, err, "verified platform")
	resourceMap(resourceMap(node, "status"), "nodeInfo")["architecture"] = "arm64"
	f.api.response[nodePath] = workloadJSON(t, node)
	resourceMap(pod, "status")["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = oldImage
	f.api.response[p] = workloadJSON(t, list)
	_, err = s.check(context.Background(), s.requirements[0].Key)
	require.ErrorContains(t, err, "verified image and platform")
}

func TestWorkloadSelectorOwnershipCandidates(t *testing.T) {
	for _, tc := range []struct {
		op     string
		values []any
		labels map[string]any
		match  bool
	}{
		{"In", []any{"yes"}, map[string]any{"key": "yes"}, true},
		{"In", []any{"yes"}, map[string]any{}, false},
		{"NotIn", []any{"yes"}, map[string]any{}, true},
		{"NotIn", []any{"yes"}, map[string]any{"key": "yes"}, false},
		{"Exists", nil, map[string]any{"key": ""}, true},
		{"DoesNotExist", nil, map[string]any{"key": ""}, false},
	} {
		t.Run(tc.op, func(t *testing.T) {
			spec := map[string]any{"selector": map[string]any{"matchExpressions": []any{map[string]any{"key": "key", "operator": tc.op, "values": tc.values}}}}
			match, err := workloadSelector(spec, "Deployment", "name")
			require.NoError(t, err)
			require.Equal(t, tc.match, match(map[string]any{"metadata": map[string]any{"labels": tc.labels}}))
		})
	}
}
