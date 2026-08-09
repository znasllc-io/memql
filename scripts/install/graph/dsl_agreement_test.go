// dsl_agreement_test.go -- install(14), memql#3371.
//
// THE ASSERTION THIS FILE EXISTS FOR: there is exactly one install, whoever
// runs it.
//
// An install can be driven two ways. A HOST executor -- an editor extension, a
// shell driver, a person -- invokes scripts/install/*.sh directly. The IN-ENGINE
// path dispatches a `shell.script` action, which reaches a script only if a DSL
// action exists to name it. Those two surfaces describe the same install, and
// nothing in the build compares them, so they drift the way two copies of
// anything drift: quietly, and in the direction nobody is looking.
//
// Both failure directions are silent by construction:
//
//   - a graph step whose script has NO action is dead on the in-engine path
//     while working perfectly from a host executor, so it is discovered by
//     whoever first tries to install from inside the product;
//   - an action for a script NO graph runs is a capability the engine can be
//     asked to perform that is not part of any install anybody reviewed.
//
// So the correspondence is asserted IN BOTH DIRECTIONS, plus the argument
// surface: every key an action passes must be a flag the script really
// declares, because a conformant capability script exits 2 on an undeclared
// flag rather than quietly falling back to a default.
package graph

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/actions"
)

// installActionsTreePath is the path as the ENGINE sees it: actions are loaded
// by walking the embedded DSL tree, whose root is dsl/, so an action's Origin is
// "install/actions.memql:<name>". That spelling is load-bearing -- the
// cockpit/runner policy default matches on the origin's "install/" prefix -- so
// the test must use the engine's path, not the repo-relative one.
const (
	installActionsTreePath = "install/actions.memql"
	installActionsRepoPath = "dsl/" + installActionsTreePath
)

// loadInstallActions parses + validates dsl/install/actions.memql through the
// real action loader, so a malformed action fails HERE and not at engine init.
func loadInstallActions(t *testing.T) []*actions.Action {
	t.Helper()
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, installActionsRepoPath))
	if err != nil {
		t.Fatalf("read %s: %v", installActionsRepoPath, err)
	}
	acts, err := actions.LoadSource(string(b), installActionsTreePath)
	if err != nil {
		t.Fatalf("%s failed to load/validate: %v", installActionsRepoPath, err)
	}
	if len(acts) == 0 {
		t.Fatalf("%s declared no actions", installActionsRepoPath)
	}
	return acts
}

// actionScript returns the capability-script id an action selects (its literal
// `script:` call argument).
func actionScript(t *testing.T, a *actions.Action) string {
	t.Helper()
	for _, arg := range a.CallArgs {
		if arg.Key != "script" {
			continue
		}
		s, ok := arg.Literal.(string)
		if !ok {
			t.Errorf("action %q: the script argument must be a string literal, got %#v", a.Name, arg.Literal)
			return ""
		}
		return s
	}
	t.Errorf("action %q names no capability script", a.Name)
	return ""
}

// graphScripts returns every capability-script id named by either shipped graph.
func graphScripts(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, g := range []*Graph{mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)} {
		for i := range g.Steps {
			s := &g.Steps[i]
			out[s.Script] = append(out[s.Script], g.Name+"/"+s.ID)
		}
	}
	return out
}

// --------------------------------------------------------------------------
// direction 1: every script a graph runs is reachable from the engine
// --------------------------------------------------------------------------

func TestEveryGraphScriptHasADslAction(t *testing.T) {
	acts := loadInstallActions(t)
	byScript := map[string]string{}
	for _, a := range acts {
		if s := actionScript(t, a); s != "" {
			byScript[s] = a.Name
		}
	}

	for script, users := range graphScripts(t) {
		if _, ok := byScript[script]; !ok {
			t.Errorf("graph steps %s run capability script %q, which has NO action in %s -- "+
				"the step works from a host executor and does nothing on the in-engine path, "+
				"and nothing else in the build notices",
				strings.Join(users, ", "), script, installActionsRepoPath)
		}
	}
}

// --------------------------------------------------------------------------
// direction 2: every action the engine can dispatch is part of a real install
// --------------------------------------------------------------------------

func TestEveryDslActionIsUsedByAGraph(t *testing.T) {
	used := graphScripts(t)
	for _, a := range loadInstallActions(t) {
		script := actionScript(t, a)
		if script == "" {
			continue
		}
		if _, ok := used[script]; !ok {
			t.Errorf("action %q runs capability script %q, which NO graph step uses -- "+
				"that is a capability the engine can be asked to perform that is not part of any "+
				"install anybody reviewed; either add the graph step or drop the action",
				a.Name, script)
		}
	}
}

// One action per capability: two actions selecting the same script means the
// in-engine path has two names for one operation, and a caller has to guess.
func TestOneActionPerCapabilityScript(t *testing.T) {
	seen := map[string]string{}
	for _, a := range loadInstallActions(t) {
		script := actionScript(t, a)
		if script == "" {
			continue
		}
		if prev, dup := seen[script]; dup {
			t.Errorf("capability script %q is selected by two actions: %q and %q", script, prev, a.Name)
			continue
		}
		seen[script] = a.Name
	}
}

// --------------------------------------------------------------------------
// the argument surface
// --------------------------------------------------------------------------

// A conformant capability script REJECTS an undeclared flag (exit 2) rather
// than falling back to a default, so an action passing `keyFile:` to a script
// that declares `--key-file` does not quietly lose the value -- it fails the
// whole step. That is the better failure, but it should not be discovered on an
// operator's machine.
func TestActionArgsAreDeclaredScriptFlags(t *testing.T) {
	root := repoRoot(t)
	allow := allowlistPath(t, root)
	cache := map[string]map[string]bool{}

	flags := func(script string) map[string]bool {
		if f, ok := cache[script]; ok {
			return f
		}
		rel, ok := allow[script]
		if !ok {
			t.Fatalf("script %q is not allowlisted", script)
		}
		// Executed directly via its shebang, exactly as the runner does.
		out, err := exec.Command(filepath.Join(root, rel), "--print-spec").Output() //nolint:gosec // constant flag, allowlisted path
		if err != nil {
			t.Fatalf("%s --print-spec: %v", rel, err)
		}
		var spec struct {
			Params []struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(out, &spec); err != nil {
			t.Fatalf("%s --print-spec is not JSON: %v", rel, err)
		}
		set := map[string]bool{}
		for _, p := range spec.Params {
			set[p.Name] = true
		}
		cache[script] = set
		return set
	}

	for _, a := range loadInstallActions(t) {
		script := actionScript(t, a)
		if script == "" {
			continue
		}
		declared := flags(script)
		for _, arg := range a.CallArgs {
			if arg.Key == "script" {
				continue // the reserved selector, consumed by the runner
			}
			if !declared[arg.Key] {
				t.Errorf("action %q passes %q to %s, which declares no such flag (has: %s) -- "+
					"the script would exit 2 on the operator's machine",
					a.Name, "--"+arg.Key, script, strings.Join(sortedSet(declared), ", "))
			}
		}
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --------------------------------------------------------------------------
// placement
// --------------------------------------------------------------------------

// An install action has no cluster to run inside. It places k3d on the
// operator's laptop, edits /etc/hosts and installs a trust-store CA, all before
// anything memQL exists to host it -- so cockpit/runner is not a policy
// preference that could reasonably go the other way, it is the only placement
// that can execute at all.
func TestInstallActionsResolveToTheRunnerSurface(t *testing.T) {
	for _, a := range loadInstallActions(t) {
		if got := actions.PolicyDefaultSurface(a); got != actions.CockpitRunnerSurface {
			t.Errorf("action %q (origin %q) defaults to surface %q, want %q",
				a.Name, a.Origin, got, actions.CockpitRunnerSurface)
		}
	}
}

// Every install action is a shell.script call and nothing else: an action that
// reached for fs.* or http.* would be doing the install's work in the engine
// instead of in a reviewed, digest-pinned, exit-code-honest capability script.
func TestInstallActionsUseOnlyTheCapabilityScriptSurface(t *testing.T) {
	for _, a := range loadInstallActions(t) {
		if a.Capability != "shell.script" {
			t.Errorf("action %q calls capability %q -- every install action goes through shell.script "+
				"so the work happens in a contract-conformant capability script", a.Name, a.Capability)
		}
	}
}

// --------------------------------------------------------------------------
// the concept
// --------------------------------------------------------------------------

// The receipts field is the entire reason the run is persisted: it is the only
// evidence uninstall has that an artifact is ours to remove. A concept without
// it would record a run nobody can safely undo.
func TestInstallRunConceptCarriesTheReceipts(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "dsl", "install", "concepts.memql"))
	if err != nil {
		t.Fatalf("read dsl/install/concepts.memql: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "concept installRun") {
		t.Fatal("dsl/install/concepts.memql declares no installRun concept")
	}
	for _, field := range []string{"runId", "graph", "status", "receipts", "completedSteps"} {
		if !strings.Contains(src, field) {
			t.Errorf("installRun carries no %q field", field)
		}
	}
}
