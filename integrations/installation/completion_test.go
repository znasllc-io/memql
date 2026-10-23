package installation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Populate live Kubernetes protocol objects, not native proof fields. The real
// image/owner/replica verifier must independently accept every observation.
func protocolCompletionWorkload(t *testing.T, receiver *receiverFixture, manifest string) {
	t.Helper()
	live := workloadObject(t, manifest)
	live["metadata"] = workloadMeta("bff")
	live["status"] = map[string]any{"observedGeneration": json.Number("1"), "replicas": json.Number("1"), "readyReplicas": json.Number("1"), "updatedReplicas": json.Number("1"), "availableReplicas": json.Number("1")}
	receiver.objects["apis/apps/v1/namespaces/memql/deployments/bff"] = live
	podSpec := resourceMap(resourceMap(resourceMap(live, "spec"), "template"), "spec")
	body, err := json.Marshal(podSpec)
	require.NoError(t, err)
	spec := workloadObject(t, string(body))
	spec["nodeName"] = "worker"
	image := resourceText(spec["containers"].([]any)[0].(map[string]any), "image")
	labels := map[string]any{"app": "bff"}
	setMeta := workloadMeta("bff-set")
	setMeta["labels"], setMeta["ownerReferences"] = labels, workloadOwnerFixture("apps/v1", "Deployment", "bff")
	set := map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": setMeta}
	meta := workloadMeta("bff-pod")
	meta["labels"], meta["ownerReferences"] = labels, workloadOwnerFixture("apps/v1", "ReplicaSet", "bff-set")
	pod := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "engine", "ready": true, "imageID": image, "state": map[string]any{"running": map[string]any{}}}}}}
	receiver.objects["api/v1/namespaces/memql/pods?limit=256"] = workloadListFixture("v1", "Pod", []any{pod})
	receiver.objects["apis/apps/v1/namespaces/memql/replicasets?limit=256"] = workloadListFixture("apps/v1", "ReplicaSet", []any{set})
	nodeMeta := workloadMeta("worker")
	delete(nodeMeta, "namespace")
	receiver.objects["api/v1/nodes/worker"] = map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": nodeMeta, "status": map[string]any{"nodeInfo": map[string]any{"operatingSystem": "linux", "architecture": "arm64"}, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
}
