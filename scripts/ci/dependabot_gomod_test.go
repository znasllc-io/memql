package ci

// dependabot_gomod_test.go -- exactly one gomod entry, and it is the workspace
// root (memql#3300).
//
// THE MISTAKE THIS PREVENTS, which was made once and looked correct
//
// The tree is 48 Go modules. The obvious reading is that each nested `go.mod`
// needs its own Dependabot entry or it goes UNWATCHED -- no PR at all,
// silently, which is the worse of the two failure modes. So a second entry was
// added listing `/component/**`, `/core`, `/docs`, `/dsl`, `/integrations/**`.
//
// The premise was wrong. Dependabot's Go updater is `go.work`-AWARE: an entry
// at `/` resolves the whole WORKSPACE and updates every module that uses the
// bumped dependency. Measured on the split tree, one root-entry PR
// (`grpc 1.82.1 -> 1.83.0`, memql#3295) touched 27 distinct module directories
// spanning wire, base, engine, platform and the servers.
//
// With both entries live every dependency was processed twice, and the
// duplicates shipped as pairs: #3297/#3294 were the same `docker/cli` bump and
// #3298/#3296 the same `runc` bump, one from each entry. Seven open PRs for
// what was really two shipped-dependency updates.
//
// WHY A TEST RATHER THAN A COMMENT
//
// The comment in dependabot.yml says not to re-add it. But the reasoning that
// produces the mistake -- "48 modules, one entry, surely the nested ones are
// unwatched" -- is the intuitive one, and it will be had again by someone who
// has not read that comment. A green test is a cheaper place to learn it than
// a week of duplicate PRs.
//
// THE PREMISE THIS RESTS ON, and where it is enforced
//
// "One entry at `/` covers everything" is true only while `go.work` lists
// every module on disk. That is not assumed here -- the `module-boundaries`
// lane's first step asserts exactly it, and fails the build otherwise. This
// test and that step are two halves of one claim; if that step is ever
// removed, this test's premise dies with it and a per-directory entry becomes
// necessary again.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

type dependabotConfig struct {
	Updates []struct {
		Ecosystem   string   `yaml:"package-ecosystem"`
		Directory   string   `yaml:"directory"`
		Directories []string `yaml:"directories"`
	} `yaml:"updates"`
}

// repoRootForDependabot ascends to the repository root.
//
// go.work, not go.mod: this package sits in the root module, but the tree is
// ~48 modules and the same walk elsewhere has repeatedly found a nested one
// instead. go.work exists exactly once, at the repository root.
func repoRootForDependabot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work not found walking up from test file")
		}
		dir = parent
	}
}

func TestDependabotHasExactlyOneGomodEntryAtTheWorkspaceRoot(t *testing.T) {
	root := repoRootForDependabot(t)
	raw, err := os.ReadFile(filepath.Join(root, ".github", "dependabot.yml"))
	if err != nil {
		t.Fatalf("read .github/dependabot.yml: %v", err)
	}
	var cfg dependabotConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse .github/dependabot.yml: %v", err)
	}

	var gomod []int
	for i, u := range cfg.Updates {
		if u.Ecosystem == "gomod" {
			gomod = append(gomod, i)
		}
	}

	if len(gomod) == 0 {
		t.Fatal("no gomod entry in .github/dependabot.yml -- Go dependencies would go " +
			"entirely unwatched. If Dependabot was deliberately turned off for Go, delete " +
			"this test in the same commit and say why.")
	}
	if len(gomod) > 1 {
		var dirs []string
		for _, i := range gomod {
			if d := cfg.Updates[i].Directory; d != "" {
				dirs = append(dirs, d)
			}
			dirs = append(dirs, cfg.Updates[i].Directories...)
		}
		t.Fatalf("%d gomod entries in .github/dependabot.yml, want exactly 1: %v\n\n"+
			"Dependabot's Go updater is go.work-aware, so the entry at \"/\" already resolves "+
			"the WHOLE WORKSPACE -- a single root-entry PR was measured touching 27 module "+
			"directories across every tier. A second entry does not cover modules the first "+
			"misses; it covers the SAME modules again, and every dependency then arrives as "+
			"two PRs (memql#3300 -- #3297/#3294 were one docker/cli bump, #3298/#3296 one "+
			"runc bump).\n\n"+
			"If a module genuinely is unwatched, the cause is that it is missing from "+
			"go.work, and the module-boundaries lane fails on that directly. Fix it there.",
			len(gomod), dirs)
	}

	only := cfg.Updates[gomod[0]]
	if len(only.Directories) > 0 {
		t.Errorf("the gomod entry uses `directories: %v`. It must be a single `directory: \"/\"` "+
			"-- a directory LIST is the per-module shape whose duplication memql#3300 removed.",
			only.Directories)
	}
	if only.Directory != "/" {
		t.Errorf("the gomod entry is rooted at %q, want \"/\". Anywhere else and the workspace "+
			"resolution that makes one entry sufficient does not happen, so every module "+
			"outside it silently stops being watched.", only.Directory)
	}
}
