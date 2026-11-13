package installation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the real Pod/Node observer with the private root-to-manifest facts
// that artifact admission supplies. This isolates runtime spelling from registry
// transport; the full OCI-to-workload path remains covered by
// TestWorkloadsBindIndexAndManifestToTheAssignedNodePlatform.
func observeRuntimeImageFixture(t *testing.T, runtime, field, architecture string) error {
	t.Helper()
	const repository = "docker.io/rancher/mirrored-library-busybox"
	const rootDigest = "sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5"
	root, err := immutableImage(repository + "@" + rootDigest)
	require.NoError(t, err)
	manifest := "index.docker.io/rancher/mirrored-library-busybox@sha256:" + strings.Repeat("b", 64)
	api := resourceAPIFixture()
	meta := workloadMeta("worker")
	delete(meta, "namespace")
	api.response["api/v1/nodes/worker"] = workloadJSON(t, map[string]any{
		"apiVersion": "v1", "kind": "Node", "metadata": meta,
		"status": map[string]any{
			"nodeInfo":   map[string]any{"operatingSystem": "linux", "architecture": architecture},
			"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		},
	})
	container := map[string]any{"name": "profiles", "image": repository + "@" + rootDigest}
	expected := map[string]any{field: []any{container}}
	spec := workloadObject(t, workloadJSON(t, expected))
	spec["nodeName"] = "worker"
	state := map[string]any{"name": "profiles", "imageID": runtime, "ready": true, "state": map[string]any{"running": map[string]any{}}}
	statusField := "containerStatuses"
	if field == "initContainers" {
		statusField = "initContainerStatuses"
		state["state"] = map[string]any{"terminated": map[string]any{"exitCode": json.Number("0")}}
	}
	pod := map[string]any{
		"metadata": workloadMeta("profiles-pod"), "spec": spec,
		"status": map[string]any{
			"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			statusField: []any{state},
		},
	}
	scope := &workloadAdmission{api: api, platform: "linux/amd64", runtimeImages: map[string]string{root: manifest}}
	return scope.observeWorkloadPod(context.Background(), newReceiverReads(api), pod, expected, false, nil)
}

func TestWorkloadsCanonicalizeDockerHubRuntimeImageIDs(t *testing.T) {
	const repository = "rancher/mirrored-library-busybox"
	for _, field := range []string{"containers", "initContainers"} {
		for _, host := range []string{"docker.io", "index.docker.io"} {
			for _, prefix := range []string{"", "docker-pullable://"} {
				for _, digest := range []string{
					"sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5",
					"sha256:" + strings.Repeat("b", 64),
				} {
					t.Run(field+"/"+prefix+host+"/"+digest, func(t *testing.T) {
						err := observeRuntimeImageFixture(t, prefix+host+"/"+repository+"@"+digest, field, "amd64")
						require.NoError(t, err, "same repository and verified root or selected-platform manifest must survive Docker Hub's hostname alias")
					})
				}
			}
		}
	}
}

func TestWorkloadsRuntimeImageCanonicalizationPreservesDigestAndPlatformFences(t *testing.T) {
	const repository = "docker.io/rancher/mirrored-library-busybox"
	const root = "sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5"
	const configID = "sha256:ff7a7936e9306ce4a789cf5523922da5e585dc1216e400efb3b6872a5137ee6b"
	unknown := "sha256:" + strings.Repeat("c", 64)
	for _, tc := range []struct{ name, runtime, architecture, refusal string }{
		{"config ID", configID, "amd64", "verified image and platform"},
		{"containerd config ID", "containerd://" + configID, "amd64", "verified image and platform"},
		{"docker config ID", "docker://" + configID, "amd64", "verified image and platform"},
		{"qualified config digest", repository + "@" + configID, "amd64", "verified image and platform"},
		{"unknown digest", repository + "@" + unknown, "amd64", "verified image and platform"},
		{"foreign registry", "other.invalid/rancher/mirrored-library-busybox@" + root, "amd64", "verified image and platform"},
		{"foreign repository", "docker.io/other/image@" + root, "amd64", "verified image and platform"},
		{"tag only", repository + ":latest", "amd64", "verified image and platform"},
		{"unqualified repository", "rancher/mirrored-library-busybox@" + root, "amd64", "verified image and platform"},
		{"URL", "https://" + repository + "@" + root, "amd64", "verified image and platform"},
		{"repeated runtime prefix", "docker-pullable://docker-pullable://" + repository + "@" + root, "amd64", "verified image and platform"},
		{"truncated digest", repository + "@sha256:101b4", "amd64", "verified image and platform"},
		{"empty", "", "amd64", "verified image and platform"},
		{"wrong node platform", repository + "@" + root, "arm64", "Node is not ready on the verified platform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := observeRuntimeImageFixture(t, tc.runtime, "containers", tc.architecture)
			require.ErrorContains(t, err, tc.refusal)
		})
	}
}
