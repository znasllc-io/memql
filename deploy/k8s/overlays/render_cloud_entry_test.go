// Render gates on the cloud-entry overlay (memql#4203).
//
// overlays/cloud stays top + mesh 2. This overlay is the keep-it / client
// instance: entry CNPG, mesh 1, voice-off as replicas 0. A second
// Application next to deploy/argocd/apps/memql.yaml would be the staging
// split epic memql#3943 removed -- ZNAS Argo lives in its own cluster and
// points at this path.
package overlays

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	entryOverlay  = "cloud-entry"
	entryReplicas = 1
)

var entryMeshOn = []string{
	"identity", "bff", "cognition", "agent", "planner", "workbench", "edge",
}

var entryVoiceOff = []string{
	"voice", "voice-agent", "livekit", "livekit-sip", "mcp", "livekit-redis",
}

func TestCloudEntryLandsWhollyInOneNamespace(t *testing.T) {
	var sawNamespaceObject bool
	for _, r := range parse(t, render(t, entryOverlay)) {
		if r.Kind == "Namespace" {
			sawNamespaceObject = true
			if r.Metadata.Name != cloudNamespace {
				t.Errorf("the Namespace object is named %q, want %q", r.Metadata.Name, cloudNamespace)
			}
			continue
		}
		if r.Metadata.Namespace == "" {
			continue
		}
		if r.Metadata.Namespace != cloudNamespace {
			t.Errorf("%s/%s lands in namespace %q, want %q",
				r.Kind, r.Metadata.Name, r.Metadata.Namespace, cloudNamespace)
		}
	}
	if !sawNamespaceObject {
		t.Error("no Namespace object rendered")
	}
}

func TestCloudEntryMeshReplicasAreOne(t *testing.T) {
	byName := deploymentsByName(t, render(t, entryOverlay))
	for _, node := range entryMeshOn {
		r, ok := byName[node]
		if !ok {
			t.Errorf("%s does not render", node)
			continue
		}
		if r.Spec.Replicas == nil {
			t.Errorf("%s declares no replica count", node)
			continue
		}
		if *r.Spec.Replicas != entryReplicas {
			t.Errorf("%s has %d replicas, want %d", node, *r.Spec.Replicas, entryReplicas)
		}
	}
}

func TestCloudEntryVoiceOffIsReplicasZero(t *testing.T) {
	byName := deploymentsByName(t, render(t, entryOverlay))
	for _, node := range entryVoiceOff {
		r, ok := byName[node]
		if !ok {
			t.Errorf("%s does not render; voice-off is replicas 0, not a missing Deployment", node)
			continue
		}
		if r.Spec.Replicas == nil {
			t.Errorf("%s declares no replica count", node)
			continue
		}
		if *r.Spec.Replicas != 0 {
			t.Errorf("%s has %d replicas, want 0 (voice-off)", node, *r.Spec.Replicas)
		}
	}
}

func TestCloudEntryUsesTheEntryPreset(t *testing.T) {
	rendered := render(t, entryOverlay)
	var saw bool
	for _, r := range parse(t, rendered) {
		if r.Kind != "Cluster" || r.Metadata.Name != "memql-db" {
			continue
		}
		saw = true
	}
	if !saw {
		t.Fatal("memql-db Cluster did not render")
	}
	// Sizes and instance count are the entry preset. Text-level so we do
	// not have to model the whole CNPG CRD here.
	if !strings.Contains(rendered, "instances: 1") {
		t.Error("memql-db is not 1 instance; cloud-entry must compose cnpg-db/presets/entry")
	}
	if !strings.Contains(rendered, "32Gi") {
		t.Error("memql-db data volume is not 32Gi")
	}
	if !strings.Contains(rendered, "16Gi") {
		t.Error("memql-db WAL volume is not 16Gi")
	}
	if strings.Contains(rendered, "256Gi") {
		t.Error("memql-db still carries the top preset 256Gi data volume")
	}
}

func TestCloudEntryHasNoFailOpenVoicePins(t *testing.T) {
	rendered := render(t, entryOverlay)
	if strings.Contains(rendered, "0000000000000000000000000000000000000000000000000000000000000000") {
		t.Error("cloud-entry ships an all-zeros digest; voice-off is replicas 0, not a fake pin")
	}
	// NODE_IP=0.0.0.0 is the cloud overlay's fail-closed LiveKit advertise
	// address. bind_addresses: ["0.0.0.0"] in livekit-config is a listen
	// bind and is fine.
	for _, line := range strings.Split(rendered, "\n") {
		trim := strings.TrimSpace(line)
		if strings.Contains(trim, "name: NODE_IP") {
			t.Errorf("cloud-entry still sets NODE_IP (%s); voice-off is replicas 0", trim)
		}
	}
}

func TestCloudStaysOnTopAndTheInClusterAppIsUnchanged(t *testing.T) {
	// overlays/cloud is not this PR. The in-cluster Application must keep
	// pointing at it -- ZNAS Argo for rg-znas-memql is a different cluster.
	app := readApplication(t, cloudApp)
	if want := "deploy/k8s/overlays/" + cloudOverlay; app.Spec.Source.Path != want {
		t.Errorf("in-cluster Application path is %q, want %q -- do not retarget it at cloud-entry", app.Spec.Source.Path, want)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "argocd", "apps", "memql-entry.yaml")); err == nil {
		t.Error("deploy/argocd/apps/memql-entry.yaml exists; do not add a second Application in the same cluster")
	}
}

func deploymentsByName(t *testing.T, rendered string) map[string]resource {
	t.Helper()
	byName := map[string]resource{}
	for _, r := range parse(t, rendered) {
		if r.Kind == "Deployment" {
			byName[r.Metadata.Name] = r
		}
	}
	return byName
}
