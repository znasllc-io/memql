package worker

import (
	"testing"
)

func TestFleetRequiresEnabledHome(t *testing.T) {
	t.Parallel()
	_, err := NewFleet(FleetOptions{Workers: emptyWorkers()})
	if err == nil {
		t.Fatal("NewFleet must refuse zero enabled homes")
	}
}

func TestEnabledHomesIsolation(t *testing.T) {
	t.Parallel()
	on, off := true, false
	w := WorkersFile{
		Version:      1,
		WorkerName:   "mac",
		Capabilities: []string{"HEADLESS"},
		Homes: []Home{
			{ID: "a", ClusterURL: "https://a.example", Token: "mql_wkr_aaaaaaaaaaaaaa", Enabled: &on},
			{ID: "b", ClusterURL: "https://b.example", Token: "mql_wkr_bbbbbbbbbbbbbb", Enabled: &off},
			{ID: "c", ClusterURL: "https://c.example", Token: "mql_wkr_cccccccccccccc", Enabled: &on},
		},
	}
	got := w.EnabledHomes()
	if len(got) != 2 {
		t.Fatalf("enabled = %d, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("enabled ids = %v", got)
	}
	ca := w.ConfigForHome(got[0])
	cc := w.ConfigForHome(got[1])
	if ca.Token == cc.Token || ca.ClusterURL == cc.ClusterURL {
		t.Fatal("homes must project distinct credentials")
	}
	if ca.StateDir == cc.StateDir {
		t.Fatal("homes must project distinct StateDir namespaces")
	}
}
