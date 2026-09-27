package dsl

import (
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// TestMountOverlayDomainsOverTheRepositoryTree was the memql#4190 regression:
// memqllint points MountOverlayDomains at the repo's own dsl/ directory, and
// that directory used to contain a domain (harness) an IN-TREE PACK had
// already registered from this package's own init(). Before the in-tree-pack
// skip, the disk walk hit RegisterTree's duplicate-domain panic -- in CI's
// dsl-lint step and only there, because no in-process test ran the mount over
// the real tree with the pack init loaded.
//
// THE IN-TREE PACK IS GONE (work spine A1): the harness pack retired, what
// survived of its tree is the ordinary embedded `memory` domain, and every
// top-level directory under dsl/ is core again. So the half of this test that
// asserted the pack-registration skip has NO SUBJECT and is removed rather
// than rewritten to pass -- a test that cannot fail is worse than no test.
//
// What is kept is the half that still has one: mounting the engine's own dsl/
// tree must mount NOTHING (every domain is core and skips), and unmount must
// not unregister anything the mount did not itself register. That is the
// property memqllint depends on, and it fails if a future domain stops being
// recognised as core.
//
// If an in-tree pack is ever reintroduced, restore the registration assertions
// with it; the skip logic they covered is still in MountOverlayDomains.
func TestMountOverlayDomainsOverTheRepositoryTree(t *testing.T) {
	before := registeredPluginDomainCount()

	var mounted, skippedCore []string
	var unmount func()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("MountOverlayDomains over the repository dsl/ tree panicked: %v", r)
			}
		}()
		mounted, skippedCore, unmount = MountOverlayDomains(nil, os.DirFS("."))
	}()
	defer unmount()

	if len(mounted) != 0 {
		t.Errorf("mounting the engine's own dsl/ tree mounted %v; want nothing (every top-level domain is core)", mounted)
	}
	if len(skippedCore) == 0 {
		t.Error("no domain was reported through skippedCore, so this walk found nothing at all -- " +
			"the mount is not looking at the tree it is supposed to be looking at")
	}

	unmount()
	if after := registeredPluginDomainCount(); after != before {
		t.Fatalf("unmount changed the plug-in registry from %d to %d domains; it must only remove what the mount itself registered", before, after)
	}
}

// registeredPluginDomainCount reads the plug-in registry size, which is what
// "unmount removed only its own registrations" is measured against.
func registeredPluginDomainCount() int {
	pluginTreesMu.RLock()
	defer pluginTreesMu.RUnlock()
	return len(pluginTrees)
}

// TestMountOverlayDomainsMountsWhatBootMounts (memql#5426): boot mounts every
// non-hidden, non-core directory of a MEMQL_DSL_PATH root
// (MountRuntimeDomainsFromEnv), and the overlay mount memqllint and the
// package analysis run used to mount only a directory with a .memql file
// DIRECTLY in it -- so a domain holding nothing but sub-namespace directories
// was mounted at boot, never checked by the lint's parity pass, and never
// staged by a package deploy. It is mounted now, at any depth; a directory
// with no .memql file the walker reads (a sidecar, a soft-disabled tree) is
// still skipped, as a file and a hidden entry are.
func TestMountOverlayDomainsMountsWhatBootMounts(t *testing.T) {
	root := fstest.MapFS{
		"acmesub/memql.toml":              {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"acmesub/billing/concepts.memql":  {Data: []byte("concept invoice {\n  total int\n}\n")},
		"acmedeep/memql.toml":             {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"acmedeep/a/b/concepts.memql":     {Data: []byte("concept widget {\n  name string\n}\n")},
		"acmeflat/memql.toml":             {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"acmeflat/concepts.memql":         {Data: []byte("concept gadget {\n  name string\n}\n")},
		"sidecars/prompts/reply.tmpl":     {Data: []byte("{{.x}}\n")},
		"softoff/_retired/concepts.memql": {Data: []byte("concept gone {\n  name string\n}\n")},
		"_hidden/concepts.memql":          {Data: []byte("concept hidden {\n  name string\n}\n")},
		"README.md":                       {Data: []byte("not a domain\n")},
	}
	mounted, skippedCore, unmount := MountOverlayDomains(nil, root)
	defer unmount()
	sort.Strings(mounted)
	if want := []string{"acmedeep", "acmeflat", "acmesub"}; strings.Join(mounted, ",") != strings.Join(want, ",") {
		t.Fatalf("mounted %v, want %v", mounted, want)
	}
	if len(skippedCore) != 0 {
		t.Fatalf("no domain here collides with a core one, got %v", skippedCore)
	}
	// The mounted domain is readable through the unified tree, nested file
	// and all -- which is what the parity pass and the stager read.
	if _, err := fs.ReadFile(Tree(), "acmesub/billing/concepts.memql"); err != nil {
		t.Fatalf("the nested namespace is not in the merged tree: %v", err)
	}
	// A root language line beside such a domain is reported as unread, as
	// it is beside a domain with a direct .memql file.
	withRootLine := fstest.MapFS{
		"memql.toml":                     {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"acmesub/billing/concepts.memql": {Data: []byte("concept invoice {\n  total int\n}\n")},
	}
	if _, unread := UnreadRootManifest(withRootLine); !unread {
		t.Error("a root memql.toml beside a sub-namespace-only domain was not reported")
	}
}
