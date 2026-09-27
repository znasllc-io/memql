// Static guard: the closed set of NODE TYPES agrees everywhere it is spelled
// out (znasllc-io/memql#5057).
//
// # The failure this exists to prevent
//
// A node type is not one declaration. It is TWO build files, TWO deny-lists,
// two shell lists, a release-matrix entry and a Deployment, in four languages,
// with nothing tying them together. ADDING one and missing a list is loud --
// the node does not build, or does not deploy. RETIRING one and missing a list
// is silent, and that asymmetry is the whole reason this file exists.
//
// `component/node/compiled_<type>.go` joined the gate in memql#5115, and it
// arrived proving the point from the other direction: `identity` and `edge`
// were never ADDED to it. Both compiled as the untagged bff default for their
// whole life, so an identity pod whose Deployment lost MEMQL_NODE_TYPE would
// have reported bff, passed app/cluster.go's `Type == NodeTypeBFF` gate and
// started the worker-mesh dialer -- tokenless, on the one node that has no node
// token. Nothing was red; the missing files were invisible because the env var
// happened to say the same thing.
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
//
// It does NOT assert that the tagged binaries BEHAVE. `go test -tags <x>
// ./component/node/` is what does that, and it runs because ci.yml's tagged
// lanes name the package -- they did not until memql#5115, which is how four
// tags failed that package's tests for their whole life.
package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

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

// denyList: the negated tags on a `*_default.go`'s build constraint. These are
// the lists whose staleness is silent -- a retired tag they forget to name
// stops being an error and becomes a spelling of the default file.
//
// There are two of them and they answer different questions, which is why both
// are read rather than one standing in for the other: app/build_default.go
// decides what an untagged binary WIRES UP, component/node/compiled_default.go
// decides what it CALLS ITSELF.
func denyList(t *testing.T, rel string) nodeTypeSet {
	t.Helper()
	src := mustReadRepoFile(t, rel)
	var constraint string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "//go:build ") {
			constraint = line
			break
		}
	}
	if constraint == "" {
		t.Fatalf("%s has no //go:build line", rel)
	}
	var names []string
	for _, m := range denyTagRe.FindAllStringSubmatch(constraint, -1) {
		names = append(names, m[1])
	}
	return newNodeTypeSet(rel+" //go:build deny-list", names)
}

// compiledNodeTypeFiles: `component/node/compiled_<type>.go` is what makes a
// build tag select the node type the binary REPORTS -- node.CompiledNodeType(),
// which beats MEMQL_NODE_TYPE and decides every `Type == node.NodeTypeX` gate
// in app/cluster.go. A node type with no file here is not an error: it compiles
// as the bff default and reports bff (memql#5115).
func compiledNodeTypeFiles(t *testing.T) nodeTypeSet {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(RepoRoot(), "component", "node", "compiled_*.go"))
	if err != nil {
		t.Fatalf("glob component/node/compiled_*.go: %v", err)
	}
	var names []string
	for _, m := range matches {
		base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "compiled_"), ".go")
		if base == "default" || strings.HasSuffix(base, "_test") {
			continue
		}
		names = append(names, base)
	}
	if len(names) == 0 {
		t.Fatal("no component/node/compiled_<type>.go files found -- the glob or the layout changed")
	}
	return newNodeTypeSet("component/node/compiled_<type>.go", names)
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
// tagPassMatrix reads the node tags ci.yml's go-tests-tags job tests under
// (memql#5483): one matrix entry per node type. A node type added without an
// entry has its tagged suites run nowhere; one retired but left here runs a
// pass under a tag that now means the default build.
func tagPassMatrix(t *testing.T) nodeTypeSet {
	t.Helper()
	rel := filepath.Join(".github", "workflows", "ci.yml")
	// Other jobs' matrices are computed expressions, so decode the one job's
	// matrix on its own rather than one struct for all of them.
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix yaml.Node `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(mustReadRepoFile(t, rel)), &wf); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	job, ok := wf.Jobs["go-tests-tags"]
	if !ok {
		t.Fatalf("%s has no go-tests-tags job: the node-tag passes run nowhere", rel)
	}
	var matrix struct {
		Include []struct {
			Tag string `yaml:"tag"`
		} `yaml:"include"`
	}
	if err := job.Strategy.Matrix.Decode(&matrix); err != nil {
		t.Fatalf("%s: go-tests-tags' matrix is not a static include list: %v", rel, err)
	}
	var names []string
	for _, e := range matrix.Include {
		names = append(names, e.Tag)
	}
	if len(names) == 0 {
		t.Fatalf("%s: go-tests-tags has no `include` entries", rel)
	}
	return newNodeTypeSet(rel+" go-tests-tags matrix", names)
}

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

// TestNodeTypeListsAgree holds the six exhaustive spellings of the node-type
// set against the build files, which are the set by construction.
func TestNodeTypeListsAgree(t *testing.T) {
	canonical := appBuildFiles(t)
	t.Logf("node types from app/build_<type>.go: %v", canonical.sorted())

	for _, got := range []nodeTypeSet{
		denyList(t, filepath.Join("app", "build_default.go")),
		compiledNodeTypeFiles(t),
		denyList(t, filepath.Join("component", "node", "compiled_default.go")),
		bashArray(t, filepath.Join("scripts", "lib", "engine_build_args.sh"), "ENGINE_NODE_TYPES"),
		bashArray(t, filepath.Join("scripts", "k3d", "dev.sh"), "DEFAULT_APP_NODES"),
		releaseMatrix(t),
		tagPassMatrix(t),
	} {
		assertSameSet(t, canonical, got)
	}
}

// TestCompiledNodeTypeFilesDeclareTheirTaggedness is the half a name-only
// comparison cannot see. `compiled_<type>.go` carries TWO facts -- the type,
// and that it came from a tag rather than from the default -- and only the
// second decides precedence over MEMQL_NODE_TYPE. A file that names its type
// and forgets the flag is present in every list above and still behaves like an
// untagged build, which is memql#5115 with the file added and nothing fixed.
func TestCompiledNodeTypeFilesDeclareTheirTaggedness(t *testing.T) {
	dir := filepath.Join(RepoRoot(), "component", "node")
	matches, err := filepath.Glob(filepath.Join(dir, "compiled_*.go"))
	if err != nil {
		t.Fatalf("glob component/node/compiled_*.go: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no component/node/compiled_*.go files found -- the layout changed")
	}
	for _, m := range matches {
		rel := filepath.Join("component", "node", filepath.Base(m))
		src := mustReadRepoFile(t, rel)
		want := "compiledNodeTypeTagged = true"
		if strings.HasSuffix(m, "compiled_default.go") {
			want = "compiledNodeTypeTagged = false"
		}
		if !strings.Contains(src, want) {
			t.Errorf("%s must declare `%s`.\n"+
				"The type alone cannot carry this: an untagged build also compiles as bff,\n"+
				"so `CompiledNodeType() == NodeTypeBFF` is true both for a build that CHOSE\n"+
				"bff and one that merely defaulted to it. Only this flag separates them, and\n"+
				"only it lets the tag beat MEMQL_NODE_TYPE (memql#5115).", rel, want)
		}
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
