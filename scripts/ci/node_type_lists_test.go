// Static guard: the closed set of NODE TYPES agrees everywhere it is spelled
// out (znasllc-io/memql#5057).
//
// # The failure this exists to prevent
//
// A node type is not one declaration. It is a build file, a deny-list, two
// shell lists, a release-matrix entry and a Deployment, in four languages, with
// nothing tying them together. ADDING one and missing a list is loud -- the node
// does not build, or does not deploy. RETIRING one and missing a list is silent,
// and that asymmetry is the whole reason this file exists.
//
// `app/build_default.go` claims every tag combination the named node types do
// not:
//
//	//go:build !agent && !planner && !bff && !identity && !workbench && !mcp && !edge
//
// That is a DENY list of live node types. Deleting `app/build_voice.go` without
// touching it did not make `-tags voice` an error; it made `voice` a spelling of
// the DEFAULT build. `go build -tags voice .` exits 0 and produces a BFF. The
// image builds, imports, and carries the retired name, and every probe passes
// because a BFF is exactly what is running.
//
// That is what happened: memql#5056's failed update built two images and then
// asked for a `voice-runtime` Dockerfile stage retired in the same batch of
// commits. The stage was the only thing that objected. Had it survived, the run
// would have imported a BFF as `memql-voice:local`, restarted a still-present
// `cognition` Deployment onto a BFF as `memql-cognition:local`, and reported
// success.
//
// # Scope
//
// This asserts the LISTS agree. The runtime refusals are elsewhere and are the
// other half: `scripts/lib/engine_build_args.sh` refuses a node type it does not
// build, and the Dockerfile refuses a `BUILD_TAGS` value with no
// `app/build_<type>.go` behind it. Lists catch the retirement that misses a
// file; refusals catch the CALLER that was written against an older set --
// an out-of-tree script, or a hand-typed `docker build`.
package ci

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

// nodeTypeSet is a set of node-type names, carrying where it was read from so a
// failure names the file rather than the variable.
type nodeTypeSet struct {
	source string
	names  map[string]bool
}

func (s nodeTypeSet) sorted() []string {
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func newNodeTypeSet(source string, names []string) nodeTypeSet {
	set := nodeTypeSet{source: source, names: map[string]bool{}}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			set.names[n] = true
		}
	}
	return set
}

func mustReadRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(RepoRoot(), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// --- the six readings ------------------------------------------------------

// appBuildFiles: `app/build_<type>.go` is what makes a Go build tag select a
// node type, so these files ARE the set. `build_default.go` is the fallback,
// not a node type, and `_test.go` files are not build files.
func appBuildFiles(t *testing.T) nodeTypeSet {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(RepoRoot(), "app", "build_*.go"))
	if err != nil {
		t.Fatalf("glob app/build_*.go: %v", err)
	}
	var names []string
	for _, m := range matches {
		base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "build_"), ".go")
		if base == "default" || strings.HasSuffix(base, "_test") {
			continue
		}
		names = append(names, base)
	}
	if len(names) == 0 {
		t.Fatal("no app/build_<type>.go files found -- the glob or the layout changed")
	}
	return newNodeTypeSet("app/build_<type>.go", names)
}

var denyTagRe = regexp.MustCompile(`!([a-z0-9_]+)`)

// defaultDenyList: the negated tags on `app/build_default.go`'s build
// constraint. This is the list whose staleness is silent.
func defaultDenyList(t *testing.T) nodeTypeSet {
	t.Helper()
	src := mustReadRepoFile(t, filepath.Join("app", "build_default.go"))
	var constraint string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "//go:build ") {
			constraint = line
			break
		}
	}
	if constraint == "" {
		t.Fatal("app/build_default.go has no //go:build line")
	}
	var names []string
	for _, m := range denyTagRe.FindAllStringSubmatch(constraint, -1) {
		names = append(names, m[1])
	}
	return newNodeTypeSet("app/build_default.go //go:build deny-list", names)
}

var bashArrayRe = func(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=\(([^)]*)\)`)
}

func bashArray(t *testing.T, rel, varName string) nodeTypeSet {
	t.Helper()
	src := mustReadRepoFile(t, rel)
	m := bashArrayRe(varName).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s: no literal `%s=( ... )` assignment found", rel, varName)
	}
	return newNodeTypeSet(rel+" "+varName, strings.Fields(m[1]))
}

var matrixNodeRe = regexp.MustCompile(`(?m)^\s*-\s+node:\s*([a-z0-9-]+)\s*$`)

// releaseMatrix: the node types the build server cuts images for. The one list
// no pull-request lane exercises, which is why a gate is the only thing that
// reads it.
func releaseMatrix(t *testing.T) nodeTypeSet {
	t.Helper()
	rel := filepath.Join(".github", "workflows", "build-engine-images.yml")
	src := mustReadRepoFile(t, rel)
	var names []string
	for _, m := range matrixNodeRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		t.Fatalf("%s: no `- node: <type>` matrix entries found", rel)
	}
	return newNodeTypeSet(rel+" matrix", names)
}

// An `image:` VALUE only. Matching the bare name anywhere in the file picked up
// prose -- two comments mentioning `memql-bootstrap:` and `memql-db:latest` --
// and a guard that reads comments is a guard that fails on an edit to a comment.
var engineImageRe = regexp.MustCompile(`(?m)^\s*image:\s*\S*?memql-([a-z0-9]+):`)

// nonNodeEngineImages are `memql-<name>` images that are NOT node types and
// never were. Each needs a reason, because the cost of this map is that a new
// entry must be added by hand; the benefit is that forgetting to add one is a
// LOUD test failure rather than a silent orphan, which is the right way round.
//
// It must never grow an entry for a RETIRED node type. That is the case this
// whole file exists to catch, and admitting one here would be turning the guard
// off from the inside.
var nonNodeEngineImages = map[string]string{
	// The generic engine image, referenced by the dsl-packages component's
	// init container (`/app/memql dsl-fetch`). It runs a SUBCOMMAND, not a
	// node, so it names no node type by design.
	"engine": "dsl-packages init container: /app/memql dsl-fetch",
}

// deployImageRefs: every `memql-<type>` image referenced under deploy/k8s,
// minus the documented non-node images. DERIVED from the image name rather than
// from a list of manifest files, so a manifest left behind for a retired node
// type is visible here.
func deployImageRefs(t *testing.T) nodeTypeSet {
	t.Helper()
	root := filepath.Join(RepoRoot(), "deploy", "k8s")
	var names []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The shared skip list, not a local one (memql#3678): a nested
		// worktree under .claude/ carries a full copy of deploy/k8s, and its
		// manifests are not this checkout's.
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range engineImageRe.FindAllStringSubmatch(string(raw), -1) {
			if _, ok := nonNodeEngineImages[m[1]]; ok {
				continue
			}
			names = append(names, m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/k8s: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("deploy/k8s references no memql-<type> images -- the naming changed")
	}
	return newNodeTypeSet("deploy/k8s memql-<type> image refs", names)
}

// --- the assertions --------------------------------------------------------

func assertSameSet(t *testing.T, want, got nodeTypeSet) {
	t.Helper()
	var missing, extra []string
	for n := range want.names {
		if !got.names[n] {
			missing = append(missing, n)
		}
	}
	for n := range got.names {
		if !want.names[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	t.Errorf(
		"node-type set in %s disagrees with %s.\n"+
			"  %s: %v\n"+
			"  %s: %v\n"+
			"  missing from %s: %v\n"+
			"  present only in %s: %v\n\n"+
			"Retiring a node type means editing every one of these. Missing one is\n"+
			"silent: app/build_default.go absorbs the retired tag and the image builds\n"+
			"as a BFF under the retired name (memql#5057).",
		got.source, want.source,
		want.source, want.sorted(),
		got.source, got.sorted(),
		got.source, missing,
		got.source, extra,
	)
}

// TestNodeTypeListsAgree holds the five exhaustive spellings of the node-type
// set against the build files, which are the set by construction.
func TestNodeTypeListsAgree(t *testing.T) {
	canonical := appBuildFiles(t)
	t.Logf("node types from app/build_<type>.go: %v", canonical.sorted())

	for _, got := range []nodeTypeSet{
		defaultDenyList(t),
		bashArray(t, filepath.Join("scripts", "lib", "engine_build_args.sh"), "ENGINE_NODE_TYPES"),
		bashArray(t, filepath.Join("scripts", "k3d", "dev.sh"), "DEFAULT_APP_NODES"),
		releaseMatrix(t),
	} {
		assertSameSet(t, canonical, got)
	}
}

// TestDeployManifestsNameOnlyRealNodeTypes is the deploy half, kept separate
// because its direction is different: a manifest may legitimately reference one
// node type many times (the migrate Job runs the identity image), so the
// assertion is about the SET of names, not their count.
func TestDeployManifestsNameOnlyRealNodeTypes(t *testing.T) {
	canonical := appBuildFiles(t)
	assertSameSet(t, canonical, deployImageRefs(t))
}

// TestDevScriptDerivesValidNodes guards the one list this fix DELETED rather
// than gated. `scripts/k3d/dev.sh` used to restate the node set as a second
// literal; it now derives VALID_NODES from ENGINE_NODE_TYPES. Restoring the
// literal would restore a list that can disagree with the library it sits
// beside, which is how a retired node type stays addressable in the dev script
// after it stops being buildable.
func TestDevScriptDerivesValidNodes(t *testing.T) {
	rel := filepath.Join("scripts", "k3d", "dev.sh")
	src := mustReadRepoFile(t, rel)
	if !strings.Contains(src, `VALID_NODES=("${ENGINE_NODE_TYPES[@]}")`) {
		t.Errorf(
			"%s must derive VALID_NODES from ENGINE_NODE_TYPES:\n"+
				"    VALID_NODES=(\"${ENGINE_NODE_TYPES[@]}\")\n"+
				"A second literal list is one more place a retirement can miss (memql#5057).",
			rel,
		)
	}
}
