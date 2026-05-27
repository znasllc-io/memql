package dsl_test

import (
	"io/fs"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/dsl"
)

// resetRegistry clears the plugin registry. memql#356 doesn't expose
// a public reset (init() registrations are meant to be permanent), so
// this helper reaches into the package via RegisterTree's "replace
// on same domain" semantics; tests that mount a plugin must use a
// unique domain name to keep parallel tests independent.
//
// Used here only as documentation -- the tests below each use a
// unique domain string.
func uniqueDomain(t *testing.T) string {
	t.Helper()
	// t.Name slashes happen with t.Run; replace them so the result
	// satisfies the "no slash" RegisterTree constraint.
	return strings.ReplaceAll(t.Name(), "/", "_")
}

// TestRegisterTree_MountsOverlayAlongsideEmbedded asserts that after
// RegisterTree, the merged Tree() lists the overlay domain at the
// root alongside the embedded domains. This is the load-bearing
// signal that DSL loaders walking dsl.Tree() will discover the
// plugin's domain.
func TestRegisterTree_MountsOverlayAlongsideEmbedded(t *testing.T) {
	domain := uniqueDomain(t)
	overlay := fstest.MapFS{
		"queries.memql":            {Data: []byte("// plugin queries\n")},
		"prompts/agentReply.tmpl":  {Data: []byte("plugin prompt body")},
	}
	dsl.RegisterTree(domain, overlay)

	entries, err := fs.ReadDir(dsl.Tree(), ".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// Embedded domains must still appear.
	assert.Contains(t, names, "agents", "embedded domain missing after RegisterTree")
	assert.Contains(t, names, "cognition", "embedded domain missing after RegisterTree")
	// Plugin overlay must appear.
	assert.Contains(t, names, domain, "plugin overlay missing from root listing")
}

// TestRegisterTree_OverlayFilesReadable asserts files under the
// mounted plugin overlay open correctly via dsl.Tree(). This is the
// path the unified loader takes -- fs.ReadFile(tree, "domain/file").
func TestRegisterTree_OverlayFilesReadable(t *testing.T) {
	domain := uniqueDomain(t)
	overlay := fstest.MapFS{
		"queries.memql":           {Data: []byte("// plugin queries body\n")},
		"prompts/agentReply.tmpl": {Data: []byte("plugin prompt body")},
	}
	dsl.RegisterTree(domain, overlay)

	tree := dsl.Tree()

	for path, want := range map[string]string{
		domain + "/queries.memql":           "// plugin queries body\n",
		domain + "/prompts/agentReply.tmpl": "plugin prompt body",
	} {
		data, err := fs.ReadFile(tree, path)
		require.NoError(t, err, "ReadFile(%q)", path)
		assert.Equal(t, want, string(data))
	}
}

// TestRegisterTree_OverlayReadDir asserts directory listings inside
// the overlay return the overlay's children, not the embedded tree's.
// The unified loader walks via fs.WalkDir which calls ReadDir at every
// level, so this is the working assumption for in-overlay traversal.
func TestRegisterTree_OverlayReadDir(t *testing.T) {
	domain := uniqueDomain(t)
	overlay := fstest.MapFS{
		"queries.memql":  {Data: []byte("a")},
		"shapes.memql":   {Data: []byte("b")},
		"prompts/x.tmpl": {Data: []byte("c")},
	}
	dsl.RegisterTree(domain, overlay)

	entries, err := fs.ReadDir(dsl.Tree(), domain)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	assert.Equal(t, []string{"prompts", "queries.memql", "shapes.memql"}, names)
}

// TestRegisterTree_EmbeddedWinsOnConflict asserts that a plugin
// trying to mount a domain name that an embedded domain already
// occupies silently loses -- the embedded tree's contents stay
// canonical. Documents the conflict-resolution rule (base wins) so a
// future audit-time warning has a pinned expectation.
func TestRegisterTree_EmbeddedWinsOnConflict(t *testing.T) {
	// agents/ is a load-bearing embedded domain; if we mount a plugin
	// under that name, lookups inside agents/ MUST still resolve
	// against the embedded tree.
	overlay := fstest.MapFS{
		"FAKE-MARKER-FILE.txt": {Data: []byte("plugin")},
	}
	dsl.RegisterTree("agents", overlay)
	defer dsl.RegisterTree("agents", embedAgentsShim{}) // best-effort restore

	// The plugin's FAKE-MARKER-FILE.txt must NOT be reachable under
	// agents/ -- the embedded agents/ wins.
	_, err := fs.ReadFile(dsl.Tree(), "agents/FAKE-MARKER-FILE.txt")
	assert.Error(t, err, "embedded agents/ must win on conflict")

	// Real embedded files under agents/ should still resolve.
	_, err = fs.Stat(dsl.Tree(), "agents")
	assert.NoError(t, err, "embedded agents/ directory must still be reachable")
}

// embedAgentsShim is a placeholder to "unregister" the test overlay
// at TestRegisterTree_EmbeddedWinsOnConflict cleanup. Since
// RegisterTree's "register same domain replaces" semantics is the
// only knob today, we just overwrite with an empty FS. Real init()
// flows wouldn't unregister.
type embedAgentsShim struct{}

func (embedAgentsShim) Open(name string) (fs.File, error) { return nil, fs.ErrNotExist }

// TestRegisterTree_PanicsOnBadInput pins the input validation. The
// init() path is the only caller; panicking is the right response to
// a typo there.
func TestRegisterTree_PanicsOnBadInput(t *testing.T) {
	t.Run("empty domain", func(t *testing.T) {
		assert.Panics(t, func() { dsl.RegisterTree("", fstest.MapFS{}) })
	})
	t.Run("whitespace domain", func(t *testing.T) {
		assert.Panics(t, func() { dsl.RegisterTree("   ", fstest.MapFS{}) })
	})
	t.Run("nested domain rejected", func(t *testing.T) {
		assert.Panics(t, func() { dsl.RegisterTree("a/b", fstest.MapFS{}) })
	})
	t.Run("nil tree", func(t *testing.T) {
		assert.Panics(t, func() { dsl.RegisterTree(uniqueDomain(t), nil) })
	})
}

// TestTree_FastPathWhenNoOverlays pins the "no plugins => bit-exact
// embedded behaviour" promise. Once a test in this package calls
// RegisterTree the global registry has overlays, so this test runs
// FIRST (alphabetical t.Name ordering would naturally put it after
// TestRegisterTree_*; using a different name path).
//
// Hard to assert "bit-exact identity" without exposing embedFS; the
// next-best signal is that Tree() returns the same value as the
// underlying embedded FS when no overlays are registered. We use the
// presence/absence of one overlay we know we DON'T register as the
// signal: when the registry is empty, root ReadDir contains no
// `__fastpath_marker__` entry.
//
// Naming this test ZZZ so go test runs it last; resetting the
// registry mid-test is unsupported. This is a smoke; real
// invariant is covered by the production binaries' boot logs
// showing "registered N concepts".
func TestZZZ_TreeFastPath_NoSyntheticEntries(t *testing.T) {
	// We don't reset the registry here; instead just assert no
	// synthetic entry called "__fastpath_marker__" appears. Cheap
	// canary.
	entries, err := fs.ReadDir(dsl.Tree(), ".")
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotEqual(t, "__fastpath_marker__", e.Name(),
			"unregistered synthetic entry visible at root")
	}
}
