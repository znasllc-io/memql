package architecture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/embedded"
	"github.com/znasllc-io/memql/component/architecture/model"
)

// model_current_test.go -- memql#2844.
//
// component/architecture/embedded/topology.model.json is a generated artifact
// with NO gate. It drifted silently: #2840 removed the userId argument from
// toggleComputerUseEnabled, the Go SDK regenerated correctly and
// `make sdk-gen-check` reported no drift, but the architecture model kept
// ToggleComputerUseEnabledArgs.UserId in 13 places. The cockpit's Topology tab
// reads this file, so what it renders could disagree with the code
// indefinitely.
//
// WHY THIS IS A GO TEST AND NOT A CI STEP. It gates inside the ordinary
// `go test ./...` lane, so it needs no workflow edit -- which matters because
// the same session that found this could not modify .github/workflows (see
// #2903 for the build-tag lane with the same constraint). `make
// arch-model-check` just runs this test.
//
// WHY IT COULD NOT HAVE EXISTED BEFORE. FOUR things made the artifact
// unreproducible, and all four had to be fixed first:
//
//  1. generated_at was a wall clock and workspace was the generating machine's
//     absolute path -- the checked-in file carried "/Users/znas/..." from a
//     worktree that no longer exists. --reproducible blanks both.
//  2. the cluster node's name came from the workspace FOLDER name, so the file
//     recorded "cluster:wt-2724". --cluster pins it.
//  3. edge order was Go map order: two runs over an identical tree on the same
//     machine emitted the same 121,601 edges in a different sequence.
//     Model.WriteJSON sorts now.
//  4. 27 SourceRefs pointed OUTSIDE the workspace -- GOROOT and the module
//     cache -- as `../../..` chains whose LENGTH encoded how deep the checkout
//     sat on that disk. Two checkouts of the same commit on the same machine
//     produced different files, and on CI the GOROOT path additionally baked
//     in the Go PATCH VERSION. extract.workspaceRelative drops them.
//
// (4) was found only by regenerating from a second checkout at a different
// depth, which is why this test's sibling below does exactly that.
//
// Until those landed, "regenerate and diff" produced a ~900k-line diff on an
// unchanged tree, which is why the artifact was refreshed by hand -- and hand
// editing is how it fell out of sync.

// TestArchitectureModelIsNotStale asserts the committed model is not WRONG.
//
// WHY NOT BYTE-FOR-BYTE. That was the first design, and it cannot work in this
// repo. The model describes the WHOLE tree, so any PR that merges while yours
// is open makes yours stale -- and with a merge queue it is worse than
// friction, it is unwinnable: the queue tests a merge commit against a moving
// main, so any PR behind another is stale by construction. Measured: this
// branch regenerated to 20182 nodes locally and CI's merge commit produced
// 20191, purely because #2905 and #2907 landed in between. A gate that fails on
// somebody else's merge blocks unrelated work, which is worse than the staleness
// it was meant to catch.
//
// WHAT IT CHECKS INSTEAD. Every node in the COMMITTED model must still exist in
// the regenerated one. That is exactly the defect #2844 reported --
// ToggleComputerUseEnabledArgs.UserId survived in the model in 13 places after
// #2840 removed the argument -- and it is immune to concurrent merges, which
// only ADD nodes.
//
// The asymmetry is deliberate and is the whole point:
//
//	node removed from the code, still in the model  -> the model LIES. FAIL.
//	node added to the code, not yet in the model    -> the model is behind. OK.
//
// A missing node makes the cockpit render something that no longer exists. An
// absent one makes it render slightly less than exists, which is what any
// committed snapshot does between refreshes. Run `make arch-model` to catch up.
//
// Skipped under -short: it shells out to the extractor over the whole
// workspace (~6s). CI runs the suite without -short, so the gate is live there.
func TestArchitectureModelIsNotStale(t *testing.T) {
	if testing.Short() {
		t.Skip("regenerates the whole architecture model; skipped in -short")
	}

	root := workspaceRoot(t)
	checkedIn := filepath.Join(root, "component", "architecture", "embedded", model.CanonicalFilename)
	if _, err := os.Stat(checkedIn); err != nil {
		t.Fatalf("checked-in model not found at %s: %v", checkedIn, err)
	}

	regenerated := filepath.Join(t.TempDir(), model.CanonicalFilename)
	// Through `make arch-model` ITSELF, so the flag set exists in exactly one
	// place. A previous version duplicated the flags here and "pinned" them
	// with a test that substring-grepped the Makefile -- which its own comment
	// block satisfied.
	before := sumFile(t, checkedIn)
	cmd := exec.Command("make", "arch-model", "ARCH_MODEL_OUT="+regenerated)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("regenerating the model failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(regenerated); err != nil {
		t.Fatalf("`make arch-model` exited 0 but wrote nothing to %s -- did the target lose its "+
			"recipe, or stop honouring ARCH_MODEL_OUT? (%v)", regenerated, err)
	}
	if sumFile(t, checkedIn) != before {
		t.Fatalf("regenerating wrote into the WORKING TREE (%s). ARCH_MODEL_OUT is not being "+
			"honoured, so this test just modified tracked source.", checkedIn)
	}

	want, got := loadModel(t, checkedIn), loadModel(t, regenerated)
	live := make(map[model.ID]bool, len(got.Nodes))
	for _, n := range got.Nodes {
		live[n.ID] = true
	}

	var gone []string
	for _, n := range want.Nodes {
		if !live[n.ID] {
			if len(gone) < 10 {
				gone = append(gone, string(n.ID))
			}
		}
	}
	if len(gone) > 0 {
		t.Errorf("the committed architecture model references %d symbol(s) that NO LONGER EXIST "+
			"in the code:\n  %s\n\nThe cockpit's Topology tab reads this file, so it is rendering "+
			"things that are gone. Run `make arch-model` and commit the result (memql#2844).",
			len(gone), join(gone))
	}
}

// workspaceRoot walks up from the test's directory to the module root.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

func sumFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func loadModel(t *testing.T, path string) *model.Model {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m model.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return &m
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// TestNoSourceRefEscapesTheWorkspace is the cheap, permanent stand-in for the
// two-worktree check.
//
// That check -- regenerate from a copy at a different directory DEPTH and
// compare -- is the only thing that catches an escaping path, and it is how the
// #2910 review found 27 of them. It is far too heavy to run here. But its
// INVARIANT is a millisecond scan of the committed artifact: no source path may
// be absolute or start with "..", because such a path encodes how deep the
// checkout sits on the generating machine's disk and makes the whole gate
// unsatisfiable everywhere else.
//
// Requires no regeneration, so it also runs under -short.
func TestNoSourceRefEscapesTheWorkspace(t *testing.T) {
	root := workspaceRoot(t)
	m := loadModel(t, filepath.Join(root, "component", "architecture", "embedded", model.CanonicalFilename))

	bad := 0
	for _, n := range m.Nodes {
		if n.Source == nil {
			continue
		}
		f := n.Source.File
		if filepath.IsAbs(f) || f == ".." || strings.HasPrefix(f, ".."+string(filepath.Separator)) {
			if bad < 5 {
				t.Errorf("node %s has a source path outside the workspace: %q", n.ID, f)
			}
			bad++
		}
	}
	if bad > 0 {
		t.Errorf("%d source path(s) escape the workspace.\n\nSuch a path is a `../../..` chain "+
			"whose LENGTH encodes the generating machine's directory depth, so the artifact "+
			"differs between two checkouts of the same commit and the drift gate goes red "+
			"everywhere but the machine that regenerated (memql#2844).", bad)
	}
}

// The four assertions below cover what BYTE-FOR-BYTE was silently enforcing and
// the subset gate is not (memql#2844 follow-up).
//
// Replacing the byte-for-byte comparison was necessary -- it cannot hold under a
// merge queue -- but it quietly dropped four guarantees, and the review found
// every one by doctoring the committed artifact and watching the suite stay
// green:
//
//	generated_at + a developer's home path restored  -> PASSED
//	nodes and edges shuffled                          -> PASSED
//	8,562 signatures corrupted, every source.file     -> PASSED
//	Makefile drops --calls (~100k edges lost)         -> PASSED
//
// The first is the defect this issue OPENS by describing. The second means the
// sort -- the single load-bearing change -- was unverified by anything.
//
// All four are properties of the COMMITTED FILE ALONE. That is what makes them
// safe: they need no regeneration, so a concurrent merge can never trip them,
// which is the exact problem that forced the subset design. They also run under
// -short.

// committedModel loads the checked-in artifact.
func committedModel(t *testing.T) *model.Model {
	t.Helper()
	return loadModel(t, filepath.Join(workspaceRoot(t), "component", "architecture", "embedded",
		model.CanonicalFilename))
}

// TestCommittedModelIsReproducible pins --reproducible.
func TestCommittedModelIsReproducible(t *testing.T) {
	m := committedModel(t)
	if m.GeneratedAt != "" {
		t.Errorf("generated_at is %q, want empty.\n\nThe committed artifact must be generated with "+
			"--reproducible (via `make arch-model`). A wall clock makes it differ on every run, "+
			"which is half of why this file could not be gated at all.", m.GeneratedAt)
	}
	if m.Workspace != "" {
		t.Errorf("workspace is %q, want empty.\n\nThat is the generating machine's ABSOLUTE PATH -- "+
			"the artifact used to carry /Users/znas/... from a worktree that no longer existed. "+
			"It commits a developer's home directory and makes the file machine-specific.",
			m.Workspace)
	}
}

// TestCommittedModelClusterIsPinned pins --cluster. Without it the cluster
// node's name comes from the checkout's FOLDER, so the artifact recorded
// "cluster:wt-2724".
func TestCommittedModelClusterIsPinned(t *testing.T) {
	var ids []model.ID
	for _, n := range committedModel(t).Nodes {
		if n.Kind == "cluster" {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) != 1 || ids[0] != "cluster:memql" {
		t.Errorf("cluster nodes = %v, want exactly [cluster:memql].\n\nWithout --cluster the name "+
			"is taken from the workspace folder, so whoever regenerated it stamps their directory "+
			"into the artifact.", ids)
	}
}

// TestCommittedModelIsSorted pins the sort -- the one change everything else
// rests on, and the one the subset gate is blindest to.
func TestCommittedModelIsSorted(t *testing.T) {
	if !committedModel(t).IsSortedForStableOutput() {
		t.Error("the committed model is not in WriteJSON's order.\n\nThe sort is what makes the " +
			"artifact reproducible at all: before it, two runs over an identical tree on the same " +
			"machine emitted the same 121,601 edges in a different sequence. A shuffled model " +
			"passes the drift gate, which compares node IDs and cannot see order.")
	}
}

// TestCommittedModelIsNotGutted stops the subset gate being satisfied by
// deletion.
//
// Subset is trivially true for an empty model: measured, a committed artifact
// truncated to 3 nodes -- and one with "nodes": [] -- both PASS. Nothing would
// stop silent rot, or a bad regeneration that dropped most of the tree, from
// staying green forever.
//
// An absolute floor rather than a ratio, so it needs no regeneration and is
// merge-immune by construction. Set well below the current size: a 4-day-old
// model across many merges was only 0.76% behind, so this is years of slack
// while still firing instantly on a gutted file.
func TestCommittedModelIsNotGutted(t *testing.T) {
	const minNodes, minEdges = 15000, 90000
	m := committedModel(t)
	if len(m.Nodes) < minNodes {
		t.Errorf("the committed model has %d nodes, floor is %d. Either the tree shrank "+
			"dramatically or a regeneration dropped most of it -- the drift gate cannot tell, "+
			"because a subset check is trivially satisfied by deletion.", len(m.Nodes), minNodes)
	}
	// Edges carry the call graph, so this also pins --calls: dropping it takes
	// the model from ~121k edges to ~21k while leaving the node set identical,
	// which the subset gate passes.
	if len(m.Edges) < minEdges {
		t.Errorf("the committed model has %d edges, floor is %d. The most likely cause is "+
			"regeneration WITHOUT --calls, which drops the CHA call graph (~121k edges to ~21k) "+
			"and leaves the node set unchanged, so the drift gate stays green.",
			len(m.Edges), minEdges)
	}
}

// TestCommittedModelLoadsThroughTheRealDecoder is the FIFTH thing byte-for-byte
// was enforcing, and the one with the worst failure mode.
//
// embedded.Load() is the artifact's only consumer path, and it goes through
// model.ReadJSON, which does two things plain json.Unmarshal does not:
// DisallowUnknownFields and a SchemaVersion check. Every other test here --
// including the drift gate -- uses plain Unmarshal, and NOTHING in the repo
// called embedded.Load() at all; the embedded package had no tests.
//
// Byte-for-byte covered this transitively: the committed file equalled the
// generator's output, and that output is decodable by construction. Measured
// once that guarantee went away:
//
//	schema_version: "0.9"   -> every committed-file test PASSES,
//	                           embedded.Load() -> "schema version mismatch"
//	one extra top-level key -> every committed-file test PASSES,
//	                           embedded.Load() -> "unknown field"
//
// That is worse than staleness: the cockpit loads NOTHING and CI is green. It
// is also the exact case ReadJSON's own doc anticipates -- "in practice that
// only happens if someone hand-edits the generated file" -- which is how #2844
// says the artifact drifted in the first place.
func TestCommittedModelLoadsThroughTheRealDecoder(t *testing.T) {
	if _, err := embedded.Load(); err != nil {
		t.Fatalf("the committed artifact does not decode through model.ReadJSON, the strict "+
			"decoder embedded.Load() uses: %v\n\nIt has DisallowUnknownFields and a schema-version "+
			"check that plain json.Unmarshal does not, so every other test here can pass while "+
			"the cockpit loads nothing at all (memql#2844).", err)
	}
}

// TestCommittedModelIsCompact is the SIXTH, and it re-arms a CI failure that
// already cost this change a full cycle.
//
// Pretty-printed, the artifact is ~1.26M lines, and GitHub cannot generate a
// diff for a change that touches all of them:
//
//	Server Error: Sorry, this diff is taking too long to generate.
//	{"resource":"PullRequest","field":"diff","code":"not_available"}
//
// dorny/paths-filter reads that diff, so `changes` fails and `ci-required`
// fails and the PR is unmergeable -- measured on 55fae413. Byte-for-byte was
// enforcing single-line output. Without this, re-adding SetIndent, or anyone
// running the file through jq or a formatter, silently re-arms it and the whole
// suite stays green; you rediscover it as an unmergeable PR rather than a test
// failure.
func TestCommittedModelIsCompact(t *testing.T) {
	root := workspaceRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "component", "architecture", "embedded",
		model.CanonicalFilename))
	if err != nil {
		t.Fatalf("read the committed model: %v", err)
	}
	// json.Encoder.Encode appends exactly one trailing newline.
	if n := bytes.Count(raw, []byte("\n")); n > 1 {
		t.Errorf("the committed artifact spans %d lines; it must be compact (one line).\n\n"+
			"Pretty-printed, GitHub cannot generate its diff and the `changes` job fails, which "+
			"fails ci-required and makes the PR unmergeable -- measured on 55fae413. Regenerate "+
			"with `make arch-model`; do not run the file through a formatter (memql#2844).", n+1)
	}
}
