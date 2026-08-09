package steps

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// install_allowlist_test.go -- install(11), memql#3368.
//
// THE ALLOWLIST IS BOTH BOUNDARIES AT ONCE.
//
//   - It is the SECURITY boundary: a `shell.script` action can only run a
//     capability script registered in capabilityScriptAllowlist, so an
//     author- or event-derived `script` value can never select an arbitrary
//     path (capability_script.go).
//   - It is equally the REACHABILITY boundary: an install script that exists,
//     is contract-conformant, and works perfectly when a human or the host
//     executor runs it directly will silently DO NOTHING on the in-engine
//     path if its id is missing here -- the runner rejects the id before the
//     script is ever exec'd. Nothing else in the build notices the gap: the
//     script's own tests pass, the contract test passes, the graph loads.
//
// So the assertion below is deliberately structural rather than a hand-kept
// list: it WALKS scripts/install/ and fails on any `cap_init` id that is not
// registered. Adding a new install capability script is therefore a two-file
// change by construction (the script + this map), and forgetting the map half
// is a red test instead of a silently inert capability.

// capInitRe extracts the capability id from the script's cap_init call, e.g.
//
//	cap_init "install.detect" "Inventory ..."
//	cap_init "install.magicLink" \
//	    "Recover ..."
var capInitRe = regexp.MustCompile(`(?m)^\s*cap_init\s+"([^"]+)"`)

// installScriptCapabilities walks scripts/install/ and returns capability id ->
// repo-relative script path for every conformant capability script found.
func installScriptCapabilities(t *testing.T) map[string]string {
	t.Helper()
	root := repoRootFromTest(t)
	dir := filepath.Join(root, "scripts", "install")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read scripts/install: %v", err)
	}
	found := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		abs := filepath.Join(dir, e.Name())
		b, rerr := os.ReadFile(abs)
		if rerr != nil {
			t.Fatalf("read %s: %v", abs, rerr)
		}
		m := capInitRe.FindSubmatch(b)
		if m == nil {
			t.Errorf("scripts/install/%s declares no cap_init id -- every capability script must call cap_init", e.Name())
			continue
		}
		id := string(m[1])
		if prev, dup := found[id]; dup {
			t.Errorf("capability id %q declared by two scripts: %s and scripts/install/%s", id, prev, e.Name())
			continue
		}
		found[id] = "scripts/install/" + e.Name()
	}
	if len(found) == 0 {
		t.Fatal("walked scripts/install/ and found no capability scripts -- the walk is broken")
	}
	return found
}

// TestInstallScriptsAreAllowlisted is the reachability assertion: every
// capability script under scripts/install/ is registered in the allowlist,
// mapped to its own path.
func TestInstallScriptsAreAllowlisted(t *testing.T) {
	found := installScriptCapabilities(t)

	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		want := found[id]
		got, ok := capabilityScriptAllowlist[id]
		if !ok {
			t.Errorf("capability %q (%s) is NOT in capabilityScriptAllowlist -- "+
				"it works from the host executor but is inert on the in-engine path; "+
				"add %q: %q to component/automations/steps/capability_script.go",
				id, want, id, want)
			continue
		}
		if got != want {
			t.Errorf("capability %q maps to %q but its cap_init lives in %q", id, got, want)
		}
	}
}

// TestInstallAllowlistEntriesResolve is the reverse direction: every
// install.* entry in the allowlist points at a file that exists and declares
// exactly that capability id. Catches a stale entry left behind by a rename.
func TestInstallAllowlistEntriesResolve(t *testing.T) {
	root := repoRootFromTest(t)
	found := installScriptCapabilities(t)

	for id, rel := range capabilityScriptAllowlist {
		if !strings.HasPrefix(id, "install.") {
			continue
		}
		abs := filepath.Join(root, rel)
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("allowlist entry %q -> %q does not exist: %v", id, rel, err)
			continue
		}
		if got, ok := found[id]; !ok {
			t.Errorf("allowlist entry %q has no scripts/install/ script declaring that cap_init id", id)
		} else if got != rel {
			t.Errorf("allowlist entry %q -> %q but cap_init %q lives in %q", id, rel, id, got)
		}
	}
}
