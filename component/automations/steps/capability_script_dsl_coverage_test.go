package steps

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// scriptArgPattern matches the `script: "<id>"` argument of a `capability
// script(...)` call in an authored action body.
var scriptArgPattern = regexp.MustCompile(`script:\s*"([A-Za-z0-9_.]+)"`)

// TestEveryDSLScriptIdIsAllowlisted closes a silent-inertness gap (memql#4485,
// memql#4486).
//
// THE FAILURE IT CATCHES, demonstrated before it was written: change an
// action's `script: "release.engine"` to `script: "release.engineTYPO"` and
// EVERY existing check still passes. `go run ./cmd/memqllint dsl/` reports "286
// files, no diagnostics"; the action loads; the action-rules validator is
// happy, because a capability arg is a string and that string is well-formed.
//
// The value only resolves at DISPATCH, against capabilityScriptAllowlist --
// which is the security boundary and therefore correctly refuses anything it
// does not know. So the capability is INERT on the in-engine path while running
// perfectly from a human shell, which is the worst possible split: the author
// tests it by hand, it works, and the automation that was the entire point
// quietly does nothing.
//
// An equivalent gate exists for scripts/install/ (install_allowlist_test.go)
// and stops there. scripts/deploy/ and scripts/release/ had no walker at all,
// which is how seven install-phase capabilities could have shipped unreachable.
func TestEveryDSLScriptIdIsAllowlisted(t *testing.T) {
	root := repoRootFromTest(t)
	dslDir := filepath.Join(root, "dsl")

	type usage struct{ file, id string }
	var uses []usage

	err := filepath.WalkDir(dslDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			// The repo-wide skip list first (memql#3678). Every tree-walking
			// test shares it, and the reason is not tidiness: a git worktree
			// under .claude/ is a FULL COPY of the repository, so a walk that
			// descends into one counts the same dsl/ tree twice and fails only
			// on the machine that happens to have a worktree open.
			if repowalk.SkipDir(name) {
				return fs.SkipDir
			}
			// Then _-prefixed and .-prefixed directories, exactly as the DSL
			// walker does (soft-disable / hidden -- root CLAUDE.md,
			// MEMQL_DSL_PATH semantics). dsl/_reference/ holds authoring
			// SKELETONS whose bodies are deliberately placeholders
			// (`script: "x"`), and several are don't-do-this examples of
			// retired forms. Holding them to the live corpus's standard would
			// either fail forever or push somebody to "fix" the skeletons into
			// looking like real actions, which destroys what they are for.
			if path != dslDir && (strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range scriptArgPattern.FindAllStringSubmatch(string(b), -1) {
			uses = append(uses, usage{file: rel, id: m[1]})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dslDir, err)
	}

	// A gate that examines nothing reports success about nothing. The corpus is
	// well over a dozen shell-backed actions; a count near zero means the regex
	// or the walk stopped matching, not that the tree became clean.
	if len(uses) < 10 {
		t.Fatalf("found only %d `script: \"...\"` references under dsl/, which is too few to be real. "+
			"This gate's regex or its walk has stopped matching the authored form; fix the scan rather than "+
			"trusting the pass.", len(uses))
	}

	var bad []string
	for _, u := range uses {
		if _, ok := capabilityScriptAllowlist[u.id]; !ok {
			bad = append(bad, u.file+" names script id "+u.id)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("%d authored action(s) name a capability script id that is not in capabilityScriptAllowlist:\n  %s\n\n"+
			"Such an action LOADS AND LINTS CLEAN and is inert at dispatch, because the allowlist is the security "+
			"boundary and refuses ids it does not know. Either the id is a typo, or the script was added without "+
			"registering it. Registration is three files, not one: the script itself, capabilityScriptAllowlist in "+
			"component/automations/steps/capability_script.go, and CAPABILITY_SCRIPTS in "+
			"editors/vscode/src/install/runner.ts (which the extension's own test deep-equals against the Go map).",
			len(bad), strings.Join(bad, "\n  "))
	}

	t.Logf("checked %d script-id references across dsl/ against %d allowlist entries", len(uses), len(capabilityScriptAllowlist))
}

// TestEveryAllowlistedScriptExists is the other direction: an allowlist entry
// pointing at a path that is not there.
//
// It fails at dispatch with a message about a missing file rather than about a
// rename -- and it is invisible until something actually runs the capability,
// which for a deploy verb can be months. Renaming or deleting a script is the
// ordinary way in.
func TestEveryAllowlistedScriptExists(t *testing.T) {
	root := repoRootFromTest(t)

	var missing []string
	for id, rel := range capabilityScriptAllowlist {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil {
			missing = append(missing, id+" -> "+rel+" (not found)")
			continue
		}
		if info.IsDir() {
			missing = append(missing, id+" -> "+rel+" (is a directory)")
			continue
		}
		// A capability script that is not executable runs anyway through the
		// runner, but fails for a human who invokes it directly -- and the
		// contract is that both paths behave identically.
		if info.Mode().Perm()&0o111 == 0 {
			missing = append(missing, id+" -> "+rel+" (not executable; chmod +x)")
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Fatalf("%d allowlist entr(ies) do not resolve to an executable script:\n  %s\n\n"+
			"capabilityScriptAllowlist is the map a `shell.script` action resolves through, so an entry naming a "+
			"path that is not there turns into a dispatch-time failure about a missing file, months after the "+
			"rename that caused it.", len(missing), strings.Join(missing, "\n  "))
	}

	if len(capabilityScriptAllowlist) == 0 {
		t.Fatal("capabilityScriptAllowlist is empty; this gate examined nothing")
	}
}
