package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHomeIDFromURL(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://api.example.test":        "api.example.test",
		"https://api.example.test/":       "api.example.test",
		"https://API.Example.COM:443/path": "api.example.com",
		"http://localhost:8080":            "localhost",
		"https://api.memql.localhost":      "api.memql.localhost",
		"":                                 "default",
	}
	for in, want := range cases {
		if got := HomeIDFromURL(in); got != want {
			t.Errorf("HomeIDFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadWorkersMigratesLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "worker.yaml")
	workers := filepath.Join(dir, "workers.yaml")
	body := []byte(`cluster_url: https://api.example.test
token: mql_wkr_testtoken1234567890
name: znas-mac
capabilities:
  - HEADLESS
`)
	if err := os.WriteFile(legacy, body, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWorkers(workers, legacy)
	if err != nil {
		t.Fatalf("LoadWorkers: %v", err)
	}
	if _, err := os.Stat(workers); err != nil {
		t.Fatalf("expected workers.yaml to be written: %v", err)
	}
	// Legacy must remain (never wipe tokens).
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy worker.yaml must remain after migrate: %v", err)
	}
	if len(w.Homes) != 1 {
		t.Fatalf("homes = %d, want 1", len(w.Homes))
	}
	if w.Homes[0].ID != "api.example.test" {
		t.Errorf("migrated id = %q, want api.example.test", w.Homes[0].ID)
	}
	if w.Homes[0].Token != "mql_wkr_testtoken1234567890" {
		t.Errorf("token not preserved")
	}
	if w.WorkerName != "znas-mac" {
		t.Errorf("worker_name = %q", w.WorkerName)
	}
	// Second load reads workers.yaml, does not re-migrate.
	w2, err := LoadWorkers(workers, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(w2.Homes) != 1 || w2.Homes[0].ID != w.Homes[0].ID {
		t.Errorf("second load drifted: %+v", w2.Homes)
	}
}

func TestUpsertHomeAdditive(t *testing.T) {
	dir := t.TempDir()
	workers := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")

	w, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers,
		LegacyPath:  legacy,
		ID:          "production",
		ClusterURL:  "https://api.example.test",
		Token:       "mql_wkr_prod_aaaaaaaaaaaa",
		Name:        "znas-mac",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 1 {
		t.Fatalf("homes = %d", len(w.Homes))
	}

	w, err = UpsertHome(UpsertHomeOptions{
		WorkersPath: workers,
		LegacyPath:  legacy,
		ID:          "local",
		ClusterURL:  "https://api.memql.localhost",
		Token:       "mql_wkr_local_bbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 2 {
		t.Fatalf("expected 2 homes after additive upsert, got %d", len(w.Homes))
	}
	ids := map[string]bool{}
	for _, h := range w.Homes {
		ids[h.ID] = true
	}
	if !ids["production"] || !ids["local"] {
		t.Fatalf("missing sibling: %+v", w.Homes)
	}
	// Force replaces THAT home only.
	w, err = UpsertHome(UpsertHomeOptions{
		WorkersPath: workers,
		LegacyPath:  legacy,
		ID:          "local",
		ClusterURL:  "https://api.memql.localhost",
		Token:       "mql_wkr_local_cccccccccccc",
		Force:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 2 {
		t.Fatalf("force must not drop siblings, got %d", len(w.Homes))
	}
	for _, h := range w.Homes {
		if h.ID == "local" && h.Token != "mql_wkr_local_cccccccccccc" {
			t.Errorf("local token not replaced")
		}
		if h.ID == "production" && h.Token != "mql_wkr_prod_aaaaaaaaaaaa" {
			t.Errorf("production token clobbered")
		}
	}
}

func TestRemoveHome(t *testing.T) {
	dir := t.TempDir()
	workers := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")
	if _, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "a", ClusterURL: "https://a.example", Token: "mql_wkr_aaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "b", ClusterURL: "https://b.example", Token: "mql_wkr_bbbbbbbbbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := RemoveHome(workers, legacy, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	w := res.Workers
	if len(w.Homes) != 1 || w.Homes[0].ID != "b" {
		t.Fatalf("after remove: %+v", w.Homes)
	}
	res, err = RemoveHome(workers, legacy, "b", true)
	if err != nil {
		t.Fatal(err)
	}
	w = res.Workers
	if len(w.Homes) != 1 || w.Homes[0].IsEnabled() {
		t.Fatalf("disableOnly left enabled: %+v", w.Homes[0])
	}
	if err := w.ValidateRun(); err != nil {
		t.Fatal("all-paused supervisor must remain available for local resume", err)
	}
}

func TestWorkersFileValidate(t *testing.T) {
	t.Parallel()
	enabled := true
	w := WorkersFile{
		Version:      1,
		WorkerName:   "mac",
		Capabilities: []string{"HEADLESS"},
		Homes: []Home{{
			ID: "x", ClusterURL: "https://x", Token: "not-a-worker-token", Enabled: &enabled,
		}},
	}
	if err := w.Validate(); err == nil || !strings.Contains(err.Error(), "mql_wkr_") {
		t.Fatalf("want token prefix error, got %v", err)
	}
}

func TestConfigForHome(t *testing.T) {
	t.Parallel()
	enabled := true
	w := WorkersFile{
		Version:      1,
		WorkerName:   "znas-mac",
		LogLevel:     "debug",
		StateDir:     "/tmp/state",
		Capabilities: []string{"HEADLESS"},
		Concurrency:  map[string]uint32{"HEADLESS": 4},
		Homes: []Home{{
			ID: "production", ClusterURL: "https://api.example", Token: "mql_wkr_zzzzzzzzzzzzzz", Enabled: &enabled,
		}},
	}
	cfg := w.ConfigForHome(w.Homes[0])
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "znas-mac" || cfg.ClusterURL != "https://api.example" {
		t.Fatalf("projection: %+v", cfg)
	}
	wantState := filepath.Join("/tmp/state", "homes", "production")
	if cfg.StateDir != wantState {
		t.Fatalf("StateDir = %q, want namespaced %q", cfg.StateDir, wantState)
	}
}

func TestConfigForHomeNamespacesDistinctHomes(t *testing.T) {
	t.Parallel()
	on := true
	w := WorkersFile{
		Version: 1, WorkerName: "mac", StateDir: "/var/memql/state",
		Capabilities: []string{"HEADLESS"},
		Homes: []Home{
			{ID: "a.example", ClusterURL: "https://a.example", Token: "mql_wkr_aaaaaaaaaaaaaa", Enabled: &on},
			{ID: "b.example", ClusterURL: "https://b.example", Token: "mql_wkr_bbbbbbbbbbbbbb", Enabled: &on},
		},
	}
	ca := w.ConfigForHome(w.Homes[0])
	cb := w.ConfigForHome(w.Homes[1])
	if ca.StateDir == cb.StateDir {
		t.Fatalf("homes share StateDir %q; want distinct namespaces", ca.StateDir)
	}
	if !strings.HasSuffix(ca.StateDir, filepath.Join("homes", "a.example")) {
		t.Fatalf("home a StateDir = %q", ca.StateDir)
	}
	if !strings.HasSuffix(cb.StateDir, filepath.Join("homes", "b.example")) {
		t.Fatalf("home b StateDir = %q", cb.StateDir)
	}
}

func TestSanitizeHomeID(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                "default",
		"api.example.com": "api.example.com",
		"prod/west":       "prod_west",
		"..":              "default",
		"a b":             "a_b",
	}
	for in, want := range cases {
		if got := sanitizeHomeID(in); got != want {
			t.Errorf("sanitizeHomeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveWorkersMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workers.yaml")
	enabled := true
	w := WorkersFile{
		Version: 1, WorkerName: "n", Capabilities: []string{"HEADLESS"},
		Homes: []Home{{ID: "h", ClusterURL: "https://h", Token: "mql_wkr_hhhhhhhhhhhhhh", Enabled: &enabled}},
	}
	if err := SaveWorkers(path, w); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %04o; want 0600", info.Mode().Perm())
	}
}

func TestUpsertHomeSameURLDifferentIDNoForce(t *testing.T) {
	dir := t.TempDir()
	workers := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")

	// Pair path: explicit --home-id local for api.memql.localhost.
	if _, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "local", ClusterURL: "https://api.memql.localhost",
		Token: "mql_wkr_local_bbbbbbbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}

	// Install path: empty ID → HomeIDFromURL = api.memql.localhost.
	// Same cluster_url must refresh WITHOUT --force and keep id "local".
	w, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ClusterURL: "https://api.memql.localhost",
		Token:      "mql_wkr_local_cccccccccccc",
	})
	if err != nil {
		t.Fatalf("same URL refresh without force: %v", err)
	}
	if len(w.Homes) != 1 {
		t.Fatalf("homes = %d, want 1 (no duplicate)", len(w.Homes))
	}
	if w.Homes[0].ID != "local" {
		t.Fatalf("id = %q, want preserved local", w.Homes[0].ID)
	}
	if w.Homes[0].Token != "mql_wkr_local_cccccccccccc" {
		t.Fatalf("token not refreshed")
	}
}

func TestUpsertHomeForceRequiredForIDRemap(t *testing.T) {
	dir := t.TempDir()
	workers := filepath.Join(dir, "workers.yaml")
	legacy := filepath.Join(dir, "worker.yaml")
	if _, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "local", ClusterURL: "https://api.memql.localhost",
		Token: "mql_wkr_local_bbbbbbbbbbbb",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "local", ClusterURL: "https://api.other.example",
		Token: "mql_wkr_other_dddddddddddd",
	})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want --force required for id remap, got %v", err)
	}
	w, err := UpsertHome(UpsertHomeOptions{
		WorkersPath: workers, LegacyPath: legacy,
		ID: "local", ClusterURL: "https://api.other.example",
		Token: "mql_wkr_other_dddddddddddd", Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Homes) != 1 || w.Homes[0].ClusterURL != "https://api.other.example" {
		t.Fatalf("force remap failed: %+v", w.Homes)
	}
}

// pairTwo enrolls homes a and b through UpsertHome, which also writes
// the legacy mirror -- naming b, the one upserted last.
func pairTwo(t *testing.T) (workers, legacy string) {
	t.Helper()
	dir := t.TempDir()
	workers = filepath.Join(dir, "workers.yaml")
	legacy = filepath.Join(dir, "worker.yaml")
	for _, h := range []struct{ id, url, tok string }{
		{"a", "https://a.example", "mql_wkr_aaaaaaaaaaaaaa"},
		{"b", "https://b.example", "mql_wkr_bbbbbbbbbbbbbb"},
	} {
		if _, err := UpsertHome(UpsertHomeOptions{WorkersPath: workers, LegacyPath: legacy, ID: h.id, ClusterURL: h.url, Token: h.tok}); err != nil {
			t.Fatal(err)
		}
	}
	return workers, legacy
}

func mirrorToken(t *testing.T, legacy string) string {
	t.Helper()
	cfg, err := LoadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Token
}

// TestUnpairingTheMirroredHomeRewritesTheMirror (memql-cockpit#429). The
// mirror named b; removing b must leave no copy of b's token behind, and
// point the mirror at a home that is still enabled.
func TestUnpairingTheMirroredHomeRewritesTheMirror(t *testing.T) {
	workers, legacy := pairTwo(t)
	if got := mirrorToken(t, legacy); got != "mql_wkr_bbbbbbbbbbbbbb" {
		t.Fatalf("precondition: the mirror names %q, want b's token", got)
	}
	res, err := RemoveHome(workers, legacy, "b", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mirror != MirrorRewritten || res.MirrorHome != "a" {
		t.Fatalf("mirror outcome = %v (%q), want rewritten to a", res.Mirror, res.MirrorHome)
	}
	if got := mirrorToken(t, legacy); got != "mql_wkr_aaaaaaaaaaaaaa" {
		t.Fatalf("the mirror still holds %q after b was unpaired", got)
	}
}

// Removing the last home deletes the mirror: there is nothing left for it
// to name, and anything it kept would be a token the person unpaired.
func TestUnpairingTheLastHomeDeletesTheMirror(t *testing.T) {
	workers, legacy := pairTwo(t)
	if _, err := RemoveHome(workers, legacy, "a", false); err != nil {
		t.Fatal(err)
	}
	res, err := RemoveHome(workers, legacy, "b", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mirror != MirrorDeleted {
		t.Fatalf("mirror outcome = %v, want deleted", res.Mirror)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker.yaml must be gone after the last home is unpaired: %v", err)
	}
}

// A mirror naming some other, still enabled, home is left exactly as it
// was.
func TestUnpairingAnotherHomeLeavesTheMirrorAlone(t *testing.T) {
	workers, legacy := pairTwo(t)
	before, _ := os.ReadFile(legacy)
	res, err := RemoveHome(workers, legacy, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mirror != MirrorUntouched {
		t.Fatalf("mirror outcome = %v, want untouched", res.Mirror)
	}
	after, _ := os.ReadFile(legacy)
	if string(before) != string(after) {
		t.Fatal("a mirror naming a surviving home must not be rewritten")
	}
}

// Disabling counts as going: a disabled home must not be connectable
// from a file nobody is looking at.
func TestDisablingTheMirroredHomeCountsAsGoing(t *testing.T) {
	workers, legacy := pairTwo(t)
	res, err := RemoveHome(workers, legacy, "b", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mirror != MirrorRewritten || mirrorToken(t, legacy) != "mql_wkr_aaaaaaaaaaaaaa" {
		t.Fatalf("disabling b left the mirror %v naming %q", res.Mirror, mirrorToken(t, legacy))
	}
	if _, err := RemoveHome(workers, legacy, "a", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("with every home disabled the mirror must be gone")
	}
}

// TestDecideRunModeNeverResurrectsAnUnpairedCluster is the #429 defect
// as a table: workers.yaml exists with nothing enabled -- the state
// `worker unpair` leaves -- and a valid worker.yaml sits beside it. The
// worker used to run that worker.yaml; now it waits, connected to
// nothing.
func TestDecideRunModeNeverResurrectsAnUnpairedCluster(t *testing.T) {
	on, off := true, false
	legacy := Config{ClusterURL: "https://prod.example", Token: "mql_wkr_prod_aaaaaaaaaaaa", Name: "m", Capabilities: []string{"HEADLESS"}}
	base := WorkersFile{Version: 1, WorkerName: "m", Capabilities: []string{"HEADLESS"}}
	withHomes := func(homes ...Home) WorkersFile { w := base; w.Homes = homes; return w }
	prod := Home{ID: "prod", ClusterURL: "https://prod.example", Token: "mql_wkr_prod_aaaaaaaaaaaa", Enabled: &on}
	prodOff := prod
	prodOff.Enabled = &off
	bad := Home{ID: "bad", ClusterURL: "https://bad.example", Token: "not-a-worker-token", Enabled: &on}

	cases := []struct {
		name          string
		forceSingle   bool
		workersExists bool
		workers       WorkersFile
		legacy        Config
		want          runMode
		wantErr       bool
	}{
		{"an enabled home runs the fleet", false, true, withHomes(prod), legacy, runFleet, false},
		{"unpaired: no homes left, mirror beside it", false, true, withHomes(), legacy, runNoHomes, false},
		{"disabled: every home off, mirror beside it", false, true, withHomes(prodOff), legacy, runFleet, false},
		{"never had a workers.yaml: its worker.yaml runs", false, false, withHomes(), legacy, runSingleHome, false},
		{"never had either file", false, false, withHomes(), Config{Name: "m", Capabilities: []string{"HEADLESS"}}, runNoHomes, false},
		{"a malformed registry is an error, not a fallback", false, true, withHomes(bad), legacy, runFleet, true},
		{"--cluster/--token runs one home", true, true, withHomes(prod), legacy, runSingleHome, false},
		{"--cluster with no token is an error", true, true, withHomes(prod), Config{ClusterURL: "https://x", Name: "m", Capabilities: []string{"HEADLESS"}}, runSingleHome, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decideRunMode(tc.forceSingle, tc.workersExists, tc.workers, tc.legacy)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("mode = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTwoHomesOnOneClusterOpenOneStream (memql-cockpit#433). Two streams
// from one machine to one cluster register it twice and flap its row. The
// LATER entry connects -- enrollments are appended, so it holds the
// newest token -- and the other is reported, not dialled. And the file is
// still a valid registry: refusing it would refuse `worker pair` and
// `worker unpair`, the two commands that fix it.
func TestTwoHomesOnOneClusterOpenOneStream(t *testing.T) {
	on, off := true, false
	w := WorkersFile{Version: 1, WorkerName: "m", Capabilities: []string{"HEADLESS"}, Homes: []Home{
		{ID: "local", ClusterURL: "https://api.example.com", Token: "mql_wkr_old_aaaaaaaaaaaa", Enabled: &on},
		{ID: "other", ClusterURL: "https://api.other.example", Token: "mql_wkr_oth_aaaaaaaaaaaa", Enabled: &on},
		{ID: "api.example.com", ClusterURL: "https://api.example.com:443", Token: "mql_wkr_new_aaaaaaaaaaaa", Enabled: &on},
	}}
	if err := w.Validate(); err != nil {
		t.Fatalf("a registry with a duplicate must stay valid, so pair and unpair still work on it: %v", err)
	}
	run, skipped := w.runnableHomes()
	var ids []string
	for _, h := range run {
		ids = append(ids, h.ID)
	}
	if got := strings.Join(ids, ","); got != "other,api.example.com" {
		t.Fatalf("connects %q, want other and the LATER of the two api.example.com entries", got)
	}
	if len(skipped) != 1 || skipped[0].Home.ID != "local" || skipped[0].Connects != "api.example.com" {
		t.Fatalf("skipped = %+v, want local, superseded by api.example.com", skipped)
	}
	if got, want := duplicateHomeSentence(skipped[0]),
		`homes "local" and "api.example.com" both point at https://api.example.com; one machine holds one stream per cluster, so only "api.example.com" connects -- remove "local" from workers.yaml, or set enabled: false on it`; got != want {
		t.Fatalf("sentence:\n got %s\nwant %s", got, want)
	}

	// A disabled duplicate opens no stream and costs nothing.
	w.Homes[2].Enabled = &off
	if _, skipped := w.runnableHomes(); len(skipped) != 0 {
		t.Fatalf("a disabled duplicate must not be reported: %+v", skipped)
	}
}

// A token an earlier build's unpair left in worker.yaml -- naming a home
// that is no longer in workers.yaml -- is cleared the next time the
// mirror is synced, rather than staying on disk forever.
func TestSyncLegacyMirrorClearsAStaleToken(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "worker.yaml")
	stale := Config{ClusterURL: "https://gone.example", Token: "mql_wkr_gone_aaaaaaaaaaaa", Name: "m", Capabilities: []string{"HEADLESS"}}
	if err := WriteLegacyWorkerYAML(legacy, stale); err != nil {
		t.Fatal(err)
	}
	on := true
	w := WorkersFile{Version: 1, WorkerName: "m", StateDir: dir, Capabilities: []string{"HEADLESS"},
		Homes: []Home{{ID: "live", ClusterURL: "https://live.example", Token: "mql_wkr_live_aaaaaaaaaaaa", Enabled: &on}}}
	outcome, home, err := syncLegacyMirror(legacy, w)
	if err != nil || outcome != MirrorRewritten || home != "live" {
		t.Fatalf("sync = (%v, %q, %v), want rewritten to live", outcome, home, err)
	}
	if got := mirrorToken(t, legacy); got != "mql_wkr_live_aaaaaaaaaaaa" {
		t.Fatalf("mirror token = %q, want live's", got)
	}
	if outcome, _, _ := syncLegacyMirror(legacy, w); outcome != MirrorUntouched {
		t.Fatalf("a mirror that already names an enabled home must be left alone, got %v", outcome)
	}
	if outcome, _, _ := syncLegacyMirror(legacy, WorkersFile{Version: 1, WorkerName: "m", Capabilities: []string{"HEADLESS"}}); outcome != MirrorDeleted {
		t.Fatalf("with nothing enabled the mirror must go, got %v", outcome)
	}
	if outcome, _, _ := syncLegacyMirror(legacy, w); outcome != MirrorAbsent {
		t.Fatalf("an absent mirror stays absent, got %v", outcome)
	}
}
