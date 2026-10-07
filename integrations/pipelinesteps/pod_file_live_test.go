package pipelinesteps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// Exercise the actual Kubernetes exec framing and cancellation boundary, not
// a local cat or a log stream. Every resource is scoped to a disposable local
// namespace. The fixture receives no engine, storage or registry credentials.
func TestPodFileStreamAgainstLocalKubernetes(t *testing.T) {
	ctx, cluster, ns, kubectl := localPipelineControls(t)
	// kubectl proxy rejects exec by default. This fixture permits only its
	// exact disposable pod's exec URL, on an ephemeral loopback listener.
	kube := localPipelineKubeProxy(t, ctx, cluster, ns, []string{
		"--reject-paths=^$", "--accept-paths=^/api/v1/namespaces/" + ns + "/pods/file-stream/exec$",
	})
	const image = "docker.io/rancher/mirrored-library-busybox@sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5"
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "file-stream"},
		"spec": map[string]any{
			"restartPolicy": "Never", "activeDeadlineSeconds": 300, "automountServiceAccountToken": false,
			"containers": []any{map[string]any{
				"name": "collector", "image": image,
				"command":         []string{"sh", "-ec", "dd if=/dev/zero of=/tmp/proof bs=1048576 count=128 2>/dev/null; tar -cf /tmp/proof.tar -C /tmp proof; touch /tmp/ready; sleep 290"},
				"readinessProbe":  map[string]any{"exec": map[string]any{"command": []string{"test", "-f", "/tmp/ready"}}, "periodSeconds": 1},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"resources":       map[string]any{"limits": map[string]string{"memory": "128Mi", "cpu": "1", "ephemeral-storage": "384Mi"}},
			}},
		},
	}
	body, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	kubectl(body, "-n", ns, "create", "-f", "-")
	kubectl(nil, "-n", ns, "wait", "pod/file-stream", "--for=condition=Ready", "--timeout=90s")
	const size = 128 << 20
	hash := sha256.New()
	n, err := kube.api.ReadPodFile(ctx, ns, "file-stream", "collector", "/tmp/proof", hash, size)
	if err != nil || n != size {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	want := sha256.New()
	zero := make([]byte, 64<<10)
	for n := 0; n < size; n += len(zero) {
		_, _ = want.Write(zero)
	}
	if !bytes.Equal(hash.Sum(nil), want.Sum(nil)) {
		t.Fatal("streamed file digest mismatch")
	}
	if _, err := kube.api.ReadPodFile(ctx, ns, "file-stream", "collector", "/tmp/proof", io.Discard, 1024); err == nil {
		t.Fatal("oversized transfer succeeded")
	}
	if _, err := kube.api.ReadPodFile(ctx, ns, "file-stream", "collector", "/tmp/absent", io.Discard, 1024); err == nil {
		t.Fatal("missing file succeeded")
	}
	t.Logf("read %d bytes over the authenticated Kubernetes stream with matching digest; overflow and missing-file refusal passed", n)
	read, write := io.Pipe()
	transferred := make(chan error, 1)
	go func() {
		_, err := kube.api.ReadPodFile(ctx, ns, "file-stream", "collector", "/tmp/proof.tar", write, size+(1<<20))
		_ = write.CloseWithError(err)
		transferred <- err
	}()
	snapshot, err := SnapshotArtifacts(ctx, read, []string{"proof"}, size+(1<<20))
	_ = read.CloseWithError(err)
	transportErr := <-transferred
	if err != nil || transportErr != nil {
		t.Fatal("snapshot transport", err, transportErr)
	}
	defer snapshot.Close()
	if len(snapshot.Files) != 1 || snapshot.Files[0].Size != size {
		t.Fatal("wrong snapshot")
	}
	t.Run("immutable object storage", func(t *testing.T) {
		connection := os.Getenv("MEMQL_AZURITE_TEST_CONNECTION_STRING")
		if connection == "" {
			t.Skip("set MEMQL_AZURITE_TEST_CONNECTION_STRING for the combined local storage proof")
		}
		client, err := azblob.NewClientFromConnectionString(connection, nil)
		if err != nil {
			t.Fatal("invalid emulator connection")
		}
		endpoint, err := url.Parse(client.URL())
		if err != nil || endpoint.Scheme != "http" || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost") {
			t.Fatal("refusing storage that is not an explicit loopback emulator")
		}
		if _, err := client.CreateContainer(ctx, ns, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := client.DeleteContainer(cleanup, ns, nil); err != nil {
				t.Error("fixture storage cleanup", err)
			}
		})
		t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", connection)
		uploader, err := azureblob.New(ctx)
		if err != nil {
			t.Fatal("create local storage adapter")
		}
		file := snapshot.Files[0]
		input, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		stored, err := uploader.CreateVerifiedStream(ctx, ns, "proof", input, file.Size, file.SHA256, "application/octet-stream")
		if err != nil {
			t.Fatal(err)
		}
		recovered, err := uploader.VerifyStream(ctx, ns, "proof", file.Size, file.SHA256)
		if err != nil || stored != recovered {
			t.Fatal("object recovery", err)
		}
		t.Logf("Kubernetes file -> private snapshot -> create-only blob -> independent readback verified: %d bytes, sha256=%s", file.Size, file.SHA256)
	})
}
