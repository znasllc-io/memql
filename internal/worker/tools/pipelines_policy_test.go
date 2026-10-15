package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pipelines -- this machine's standing consent to run CI pipeline steps
// (memql#5494).
//
// The default-deny half is the one that matters most: a machine upgrading into
// this feature must run nothing. A laptop is never a default place for
// somebody's CI.

func TestPipelinesAreDefaultDeny(t *testing.T) {
	for name, p := range map[string]*Policy{
		"built-in defaults":      DefaultPolicy(),
		"a file with no block":   policyWith(t, "shell:\n  allow: [ls]\n"),
		"allow written as false": policyWith(t, "pipelines:\n  allow: false\n"),
		"a nil policy":           nil,
	} {
		t.Run(name, func(t *testing.T) {
			if p.PipelinesAllowed() {
				t.Fatal("pipelines are allowed without the owner saying so -- default-deny is gone")
			}
			err := p.Pipelines().Check("o/r")
			if err == nil {
				t.Fatal("a machine that allows no pipelines admitted a step")
			}
			// The refusal is what the owner reads in the run's log; it has
			// to name the setting that changes it.
			if !strings.Contains(err.Error(), "pipelines.allow") {
				t.Errorf("the refusal does not name the setting: %v", err)
			}
		})
	}
}

func TestPipelinesAllowWithNoReposAdmitsEveryRepository(t *testing.T) {
	p := policyWith(t, "pipelines:\n  allow: true\n")
	if !p.PipelinesAllowed() {
		t.Fatal("pipelines.allow: true was not read")
	}
	for _, repo := range []string{"o/r", "acme/widgets", ""} {
		if err := p.Pipelines().Check(repo); err != nil {
			t.Errorf("with no repos list, %q was refused: %v", repo, err)
		}
	}
}

func TestPipelinesReposNarrowsWhichRepositoriesRunHere(t *testing.T) {
	p := policyWith(t, "pipelines:\n  allow: true\n  repos:\n    - Acme/Widgets\n    - acme/tools.git\n")
	pp := p.Pipelines()
	// Repository names are compared the way GitHub compares them: without
	// regard to case, and with or without the .git a clone URL carries.
	for _, repo := range []string{"acme/widgets", "ACME/WIDGETS", " acme/widgets ", "acme/widgets.git", "acme/tools"} {
		if err := pp.Check(repo); err != nil {
			t.Errorf("%q is listed but was refused: %v", repo, err)
		}
	}
	err := pp.Check("acme/gadgets")
	if err == nil {
		t.Fatal("a repository the list does not name was admitted")
	}
	if !strings.Contains(err.Error(), "pipelines.repos") || !strings.Contains(err.Error(), "acme/gadgets") {
		t.Errorf("the refusal must name the repository and the setting: %v", err)
	}
	// A step that names no repository cannot be matched against a list, so
	// a list refuses it rather than waving it through.
	if err := pp.Check(""); err == nil {
		t.Error("a step naming no repository was admitted by a repos list")
	}
}

func TestPipelinesPolicyDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	pp := policyWith(t, "pipelines:\n  allow: true\n").Pipelines()
	if pp.MaxTimeoutSec != DefaultPipelineMaxTimeoutSec || DefaultPipelineMaxTimeoutSec != 3600 {
		t.Errorf("max_timeout_sec defaults to %d (constant %d), want 3600", pp.MaxTimeoutSec, DefaultPipelineMaxTimeoutSec)
	}
	if pp.ContainerMemoryMiB != 2048 || (*Policy)(nil).Pipelines().ContainerMemoryMiB != 2048 {
		t.Fatal("the default command memory cap changed")
	}
	if want := filepath.Join(home, ".memql", "pipelines"); pp.WorkspaceRoot != want {
		t.Errorf("with no workspace root anywhere, steps run under %q, want %q", pp.WorkspaceRoot, want)
	}

	// The shell's workspace root, when set, is where the pipelines one hangs.
	shellRoot := t.TempDir()
	pp = policyWith(t, "fs:\n  workspace_root: "+shellRoot+"\npipelines:\n  allow: true\n").Pipelines()
	if want := filepath.Join(shellRoot, "pipelines"); pp.WorkspaceRoot != want {
		t.Errorf("under fs.workspace_root, steps run under %q, want %q", pp.WorkspaceRoot, want)
	}

	// And pipelines.workspace_root wins over both, with ~ expanded.
	pp = policyWith(t, "fs:\n  workspace_root: "+shellRoot+"\npipelines:\n  allow: true\n  workspace_root: ~/ci-steps\n  max_timeout_sec: 90\n").Pipelines()
	if want := filepath.Join(home, "ci-steps"); pp.WorkspaceRoot != want {
		t.Errorf("pipelines.workspace_root resolved to %q, want %q", pp.WorkspaceRoot, want)
	}
	if pp.MaxTimeoutSec != 90 {
		t.Errorf("max_timeout_sec = %d, want 90", pp.MaxTimeoutSec)
	}
}

func TestPipelinesPolicyReplacesOnReloadSoTheConsentCanBeWithdrawn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pipelines:\n  allow: true\n  repos: [o/r]\n  container_memory_mib: 4096\n")
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Pipelines().Check("o/r"); err != nil {
		t.Fatalf("the first file's grant was not honoured: %v", err)
	}
	previous := p.Pipelines()
	if previous.ContainerMemoryMiB != 4096 {
		t.Fatal("the configured memory cap was not loaded")
	}

	// REPLACE, not merge -- the opposite of the allow lists beside it, and
	// because this is a consent, and one
	// that a SIGHUP could not take back would be a grant the file no longer
	// states.
	write("shell:\n  allow: [ls]\n")
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	if p.PipelinesAllowed() {
		t.Fatal("removing the block and reloading left pipelines allowed")
	}

	write("pipelines:\n  allow: true\n  repos: [o/other]\n")
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := p.Pipelines().Check("o/r"); err == nil {
		t.Fatal("a reload that narrowed repos kept admitting the repository it dropped")
	}
	if err := p.Pipelines().Check("o/other"); err != nil {
		t.Fatalf("the reloaded list is not in force: %v", err)
	}
	if p.Pipelines().ContainerMemoryMiB != 2048 || previous.ContainerMemoryMiB != 4096 {
		t.Fatal("reload must restore the default for new steps without changing a running step's snapshot")
	}
}

func TestPipelinesReposAreCopiedOut(t *testing.T) {
	p := policyWith(t, "pipelines:\n  allow: true\n  repos: [o/r]\n")
	pp := p.Pipelines()
	pp.Repos[0] = "o/mutated"
	if again := p.Pipelines(); again.Repos[0] != "o/r" {
		t.Errorf("Pipelines handed out the live repos slice: %v", again.Repos)
	}
}
