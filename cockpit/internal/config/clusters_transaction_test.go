package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/core/filetxn"
	"gopkg.in/yaml.v3"
)

func TestConcurrentClusterUpdatesRetainEveryRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			err := updateClustersAt(t.Context(), path, func(registry *ClustersFile) error {
				registry.Clusters = append(registry.Clusters, ClusterConfig{Name: fmt.Sprint(i), Endpoint: "https://api.example.com"})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var registry ClustersFile
	if err := yaml.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Clusters) != 20 {
		t.Fatalf("lost registration: %d", len(registry.Clusters))
	}
}

func TestEditorCASAndCockpitUpdateShareTheLockContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	original := []byte("editor_preference: browser\nclusters:\n  - name: client\n    endpoint: https://api.client.example\n    future_editor_setting: retain\n")
	if err := filetxn.CompareAndSwap(t.Context(), path, nil, original); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(original)
	digest := hex.EncodeToString(sum[:])
	if err := updateClustersAt(t.Context(), path, func(registry *ClustersFile) error {
		registry.Clusters = append(registry.Clusters, ClusterConfig{Name: "second", Endpoint: "https://api.second.example"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := filetxn.CompareAndSwap(t.Context(), path, &digest, []byte("clusters: []")); !errors.Is(err, filetxn.ErrConflict) {
		t.Fatalf("stale editor write: %v", err)
	}
	data, _ := os.ReadFile(path)
	var registry ClustersFile
	if err := yaml.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Clusters) != 2 || registry.Extra["editor_preference"] != "browser" || registry.Clusters[0].Extra["future_editor_setting"] != "retain" {
		t.Fatalf("lost another client's state: %+v", registry)
	}
}
