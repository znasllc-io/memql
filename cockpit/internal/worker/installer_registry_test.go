package worker

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// The CLI and the shell installer share this file but use different YAML
// indentation. Exercise the real writer and reader across repeated installs.
func TestInstallerPreservesGoWrittenSiblingHome(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")
	disabled := false
	local := Home{ID: "local", ClusterURL: "https://api.memql.localhost", OSURL: "https://os.memql.localhost", Token: "mql_wkr_local_test", Enabled: &disabled}
	original := WorkersFile{Version: 1, WorkerName: "existing-machine", StateDir: filepath.Join(dir, "state"), LogLevel: "debug", Labels: map[string]string{"team": "engineering"}, Concurrency: map[string]uint32{"HEADLESS": 3}, Capabilities: []string{"HEADLESS"}, Homes: []Home{local}}
	if err := SaveWorkers(registry, original); err != nil {
		t.Fatal(err)
	}
	library, err := filepath.Abs("../../scripts/install/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"mql_wkr_first_test", "mql_wkr_rotated_test"} {
		cmd := exec.Command("bash", "-c", `source "$1"; write_worker_yaml "$2" https://api.production.example "$3" replacement-name no HEADLESS`, "installer-test", library, legacy, token)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("installer: %v\n%s", err, out)
		}
		got, err := LoadWorkers(registry, legacy)
		if err != nil {
			t.Fatalf("installer produced an unreadable registry: %v", err)
		}
		if len(got.Homes) != 2 {
			t.Fatalf("homes = %d, want two", len(got.Homes))
		}
		if !reflect.DeepEqual(got.Homes[0], local) {
			t.Fatal("installer changed the disabled sibling home")
		}
		if got.Homes[1].Token != token || !got.Homes[1].IsEnabled() {
			t.Fatal("production home was not upserted")
		}
		if got.WorkerName != original.WorkerName || got.StateDir != original.StateDir || got.LogLevel != original.LogLevel || !reflect.DeepEqual(got.Labels, original.Labels) || !reflect.DeepEqual(got.Concurrency, original.Concurrency) {
			t.Fatal("installer changed shared registry settings")
		}
	}
}
