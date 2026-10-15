package worker

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

func TestEnsureMachineIDPersists(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureMachineID(dir)
	if err != nil || first == "" {
		t.Fatalf("first: %q %v", first, err)
	}
	second, err := EnsureMachineID(dir)
	if err != nil || second != first {
		t.Fatalf("second = %q, want %q (%v)", second, first, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, machineIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != first+"\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestBuildRegisterCarriesMachineId(t *testing.T) {
	dir := t.TempDir()
	register := buildRegister(Config{
		Name:         "mbp",
		Capabilities: []string{"HEADLESS"},
		StateDir:     dir,
		Labels:       map[string]string{"os": "darwin"},
		Concurrency:  map[string]uint32{"HEADLESS": 1},
	}, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})
	if register.Labels[LabelMachineId] == "" {
		t.Fatal("Register must carry machineId label")
	}
	again := buildRegister(Config{
		Name:         "mbp",
		Capabilities: []string{"HEADLESS"},
		StateDir:     dir,
		Concurrency:  map[string]uint32{"HEADLESS": 1},
	}, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})
	if again.Labels[LabelMachineId] != register.Labels[LabelMachineId] {
		t.Fatalf("machineId must be stable across Registers: %q vs %q", again.Labels[LabelMachineId], register.Labels[LabelMachineId])
	}
}

// TestMachineStateRoot is the one function that finds a machine's root
// from any state dir the three run paths hand it (memql-cockpit#430).
func TestMachineStateRoot(t *testing.T) {
	root := filepath.Join("/", "var", "memql", "state")
	cases := map[string]string{
		root:                                   root,
		root + "/":                             root,
		filepath.Join(root, "homes", "prod"):   root,
		filepath.Join(root, "homes", "a.b.c"):  root,
		filepath.Join(root, "homes"):           filepath.Join(root, "homes"),
		filepath.Join(root, "other", "prod"):   filepath.Join(root, "other", "prod"),
		filepath.Join(root, "homes", "p", "x"): filepath.Join(root, "homes", "p", "x"),
	}
	for in, want := range cases {
		if got := machineStateRoot(in); got != want {
			t.Errorf("machineStateRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOneMachineIDWhicheverWayTheWorkerStarted. The fleet, the
// single-home path and the legacy mirror each hand buildRegister a
// different state dir; all three must register this machine under the
// same id, or switching between them mints the duplicate row the id
// exists to prevent.
func TestOneMachineIDWhicheverWayTheWorkerStarted(t *testing.T) {
	root := t.TempDir()
	on := true
	w := WorkersFile{Version: 1, WorkerName: "mbp", StateDir: root, Capabilities: []string{"HEADLESS"},
		Homes: []Home{{ID: "prod", ClusterURL: "https://api.prod.example", Token: "mql_wkr_prod_aaaaaaaaaaaa", Enabled: &on}}}

	fleet := w.ConfigForHome(w.Homes[0])
	single := Config{Name: "mbp", Capabilities: []string{"HEADLESS"}, StateDir: root}
	mirror := Config{Name: "mbp", Capabilities: []string{"HEADLESS"}, StateDir: filepath.Join(root, "homes", "prod")}

	var ids []string
	for _, cfg := range []Config{fleet, single, mirror} {
		reg := buildRegister(cfg, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})
		ids = append(ids, reg.Labels[LabelMachineId])
	}
	if ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("machine ids fleet/single/mirror = %q; want one id for all three", ids)
	}
	raw, err := os.ReadFile(filepath.Join(root, machineIDFile))
	if err != nil || strings.TrimSpace(string(raw)) != ids[0] {
		t.Fatalf("the id must live at the machine root: %q, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(root, "homes", "prod", machineIDFile)); err == nil {
		t.Fatal("no per-home id file may be written any more")
	}
}

// A build before #430 kept the id per home. The first one (by name) is
// ADOPTED, so the cluster that already holds it keeps its reclaim key.
func TestResolveMachineIDAdoptsAnEarlierPerHomeID(t *testing.T) {
	root := t.TempDir()
	for home, id := range map[string]string{"zeta": "zzzz", "alpha": "aaaa"} {
		dir := filepath.Join(root, "homes", home)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, machineIDFile), []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := peekMachineID(root); got != "aaaa" {
		t.Fatalf("peek = %q, want the first home's id", got)
	}
	if _, err := os.Stat(filepath.Join(root, machineIDFile)); err == nil {
		t.Fatal("peeking must not write the machine-level file")
	}
	got, err := ResolveMachineID(root)
	if err != nil || got != "aaaa" {
		t.Fatalf("ResolveMachineID = %q, %v; want the adopted aaaa", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, machineIDFile))
	if strings.TrimSpace(string(raw)) != "aaaa" {
		t.Fatalf("the adopted id must be persisted at the root, got %q", raw)
	}
	// And from then on the root wins, whatever the homes say.
	if err := os.WriteFile(filepath.Join(root, "homes", "alpha", machineIDFile), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := ResolveMachineID(root); got != "aaaa" {
		t.Fatalf("after adoption the root's id must win, got %q", got)
	}
}

// A machine with nothing to adopt mints once, and peeking says so
// without minting.
func TestResolveMachineIDMintsOnce(t *testing.T) {
	root := t.TempDir()
	if got := peekMachineID(root); got != "" {
		t.Fatalf("peek on a fresh machine = %q, want nothing", got)
	}
	first, err := ResolveMachineID(root)
	if err != nil || len(first) != 32 {
		t.Fatalf("minted %q, %v; want 32 hex digits", first, err)
	}
	if again, _ := ResolveMachineID(root); again != first {
		t.Fatalf("second resolve = %q, want %q", again, first)
	}
}

// A Config that carries the id resolved for the whole machine uses it,
// whatever its state dir says -- the fleet resolves once so two homes
// cannot race each other into minting two.
func TestMachineIDForPrefersTheResolvedID(t *testing.T) {
	cfg := Config{MachineID: "resolved-once", StateDir: t.TempDir()}
	if got, err := machineIDFor(cfg); err != nil || got != "resolved-once" {
		t.Fatalf("machineIDFor = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, machineIDFile)); err == nil {
		t.Fatal("a resolved id must not be re-derived onto disk")
	}
}

// Two homes reaching Register at the same moment on a machine with no id
// yet resolve ONE id between them, not one each.
func TestResolveMachineIDIsOneIDUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	const n = 16
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := ResolveMachineID(root)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("concurrent resolution minted %q and %q; want one id", first, id)
		}
	}
}
