package worker

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectBackupHomeSingle(t *testing.T) {
	t.Parallel()
	on := true
	w := WorkersFile{
		Version: 1, WorkerName: "mac", StateDir: "/tmp/state",
		Capabilities: []string{"HEADLESS"},
		Homes: []Home{
			{ID: "only", ClusterURL: "https://only.example", Token: "mql_wkr_oooooooooooooo", Enabled: &on},
		},
	}
	h, cfg, err := selectBackupHome(w, "")
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != "only" {
		t.Fatalf("home = %q", h.ID)
	}
	want := filepath.Join("/tmp/state", "homes", "only")
	if cfg.StateDir != want {
		t.Fatalf("StateDir = %q, want %q", cfg.StateDir, want)
	}
}

func TestSelectBackupHomeRequiresFlagWhenMultiple(t *testing.T) {
	t.Parallel()
	on := true
	w := WorkersFile{
		Version: 1, StateDir: "/tmp/state", Capabilities: []string{"HEADLESS"},
		Homes: []Home{
			{ID: "a", ClusterURL: "https://a.example", Token: "mql_wkr_aaaaaaaaaaaaaa", Enabled: &on},
			{ID: "b", ClusterURL: "https://b.example", Token: "mql_wkr_bbbbbbbbbbbbbb", Enabled: &on},
		},
	}
	if _, _, err := selectBackupHome(w, ""); err == nil || !strings.Contains(err.Error(), "--home") {
		t.Fatalf("want --home required, got %v", err)
	}
	h, cfg, err := selectBackupHome(w, "b")
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != "b" {
		t.Fatalf("home = %q", h.ID)
	}
	if !strings.HasSuffix(cfg.StateDir, filepath.Join("homes", "b")) {
		t.Fatalf("StateDir = %q", cfg.StateDir)
	}
}

func TestSelectBackupHomeUnknown(t *testing.T) {
	t.Parallel()
	on := true
	w := WorkersFile{
		Version: 1, Capabilities: []string{"HEADLESS"},
		Homes: []Home{{ID: "a", ClusterURL: "https://a.example", Token: "mql_wkr_aaaaaaaaaaaaaa", Enabled: &on}},
	}
	if _, _, err := selectBackupHome(w, "missing"); err == nil {
		t.Fatal("expected error for unknown home")
	}
}
