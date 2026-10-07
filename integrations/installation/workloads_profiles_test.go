package installation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

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

func TestWorkloadsRequireSelectedPlatformManifestRatherThanIndex(t *testing.T) {
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
	require.ErrorContains(t, err, "platform manifest")
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
