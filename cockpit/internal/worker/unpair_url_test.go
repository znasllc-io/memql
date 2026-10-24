package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnpairURLPreservesOtherHomesAndPreviewsWithoutWrites(t *testing.T) {
	dir := t.TempDir()
	registry, mirror := filepath.Join(dir, "workers.yaml"), filepath.Join(dir, "worker.yaml")
	w := emptyWorkers()
	off := false
	w.Homes = []Home{
		{ID: "local", ClusterURL: "https://local.example", Token: "mql_wkr_local"},
		{ID: "duplicate", ClusterURL: "https://LOCAL.example:443/", Token: "mql_wkr_duplicate"},
		{ID: "cloud", ClusterURL: "https://cloud.example", Token: "mql_wkr_cloud", Enabled: &off},
	}
	if err := SaveWorkers(registry, w); err != nil {
		t.Fatal(err)
	}
	if err := WriteLegacyWorkerYAML(mirror, w.ConfigForHome(w.Homes[0])); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(registry)
	result, err := RemoveHomesByURL(registry, mirror, "https://local.example/", true)
	if err != nil || result.Removed != 2 || result.Remaining != 1 || result.Changed {
		t.Fatalf("%+v %v", result, err)
	}
	after, _ := os.ReadFile(registry)
	if string(before) != string(after) {
		t.Fatal("preview wrote registry")
	}
	result, err = RemoveHomesByURL(registry, mirror, "https://local.example/", false)
	if err != nil || !result.Changed || result.Remaining != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	got, err := loadWorkersFile(registry)
	if err != nil || len(got.Homes) != 1 || got.Homes[0].Token != "mql_wkr_cloud" || got.Homes[0].IsEnabled() {
		t.Fatalf("sibling changed: %+v %v", got, err)
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatal("removed or disabled token remained in mirror")
	}
	result, err = RemoveHomesByURL(registry, mirror, "https://local.example", false)
	if err != nil || result.Changed || result.Remaining != 1 {
		t.Fatalf("retry: %+v %v", result, err)
	}
}

func TestUnpairURLLegacyLastHomeAndMalformedSafety(t *testing.T) {
	dir := t.TempDir()
	registry, mirror := filepath.Join(dir, "workers.yaml"), filepath.Join(dir, "worker.yaml")
	w := emptyWorkers()
	home := Home{ID: "old", ClusterURL: "https://legacy.example", Token: "mql_wkr_legacy"}
	if err := WriteLegacyWorkerYAML(mirror, w.ConfigForHome(home)); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveHomesByURL(registry, mirror, home.ClusterURL, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(registry); !os.IsNotExist(err) {
		t.Fatal("preview migrated legacy file")
	}
	result, err := RemoveHomesByURL(registry, mirror, home.ClusterURL, false)
	if err != nil || result.Removed != 1 || result.Remaining != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatal("last-home token remained")
	}
	os.WriteFile(registry, []byte("homes: [bad yaml"), 0600)
	before, _ := os.ReadFile(registry)
	if _, err := RemoveHomesByURL(registry, mirror, home.ClusterURL, false); err == nil {
		t.Fatal("accepted malformed registry")
	}
	after, _ := os.ReadFile(registry)
	if string(before) != string(after) {
		t.Fatal("malformed registry changed")
	}
	os.Remove(registry)
	protected := filepath.Join(dir, "protected")
	os.WriteFile(protected, before, 0600)
	os.Symlink(protected, registry)
	if _, err := RemoveHomesByURL(registry, mirror, home.ClusterURL, false); err == nil {
		t.Fatal("accepted aliased registry")
	}
}
