package architecture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

// TestArchitectureModelIsCurrent regenerates the model and compares it with the
// checked-in copy.
//
// Skipped under -short: it shells out to the extractor over the whole
// workspace (~6s) and writes a 42MB temp file. CI runs the suite without
// -short, so the gate is live there.
func TestArchitectureModelIsCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("regenerates the whole architecture model; skipped in -short")
	}

	root := workspaceRoot(t)
	checkedIn := filepath.Join(root, "component", "architecture", "embedded", model.CanonicalFilename)
	if _, err := os.Stat(checkedIn); err != nil {
		t.Fatalf("checked-in model not found at %s: %v", checkedIn, err)
	}

	regenerated := filepath.Join(t.TempDir(), model.CanonicalFilename)
	// Regenerate through `make arch-model` ITSELF, so the flag set exists in
	// exactly one place (memql#2844 review).
	//
	// The first version duplicated the flags here and "pinned" them with a
	// test that substring-grepped the whole Makefile. That guard was satisfied
	// by the COMMENT above the target: deleting --calls and --reproducible
	// from the recipe left it green, and `--cluster memql` matched
	// `--cluster memqlPRODUCTION` besides. A guard narrower than the thing it
	// guards -- which is the exact defect class this gate is for. There is no
	// second copy to keep in step now.
	before := sumFile(t, checkedIn)
	cmd := exec.Command("make", "arch-model", "ARCH_MODEL_OUT="+regenerated)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("regenerating the model failed: %v\n%s", err, out)
	}

	// The target's DEFAULT output is the tracked artifact, so this test sits
	// one variable-substitution away from rewriting 42MB of tracked source.
	// Demonstrated: hardcode --out in the recipe and `go test ./...` leaves the
	// working tree dirty. It still FAILS, so nothing passes vacuously -- but a
	// test that mutates the repo is its own problem.
	if _, err := os.Stat(regenerated); err != nil {
		t.Fatalf("`make arch-model` exited 0 but wrote nothing to %s -- did the target lose its "+
			"recipe, or stop honouring ARCH_MODEL_OUT? (%v)", regenerated, err)
	}
	if sumFile(t, checkedIn) != before {
		t.Fatalf("regenerating wrote into the WORKING TREE (%s). ARCH_MODEL_OUT is not being "+
			"honoured, so this test just modified tracked source.", checkedIn)
	}

	if sumFile(t, checkedIn) == sumFile(t, regenerated) {
		return
	}

	// Differ. Say WHAT differs -- a 42MB dump helps nobody.
	got, want := loadModel(t, regenerated), loadModel(t, checkedIn)
	t.Errorf("component/architecture/embedded/%s is STALE.\n\n"+
		"  checked in: %d nodes, %d edges\n"+
		"  regenerated: %d nodes, %d edges\n%s\n"+
		"Run `make arch-model` and commit the result. The cockpit's Topology tab reads this\n"+
		"file, so while it is stale the topology it renders disagrees with the code.",
		model.CanonicalFilename,
		len(want.Nodes), len(want.Edges), len(got.Nodes), len(got.Edges),
		firstNodeDelta(want, got))
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

// firstNodeDelta names a few nodes present in one model and not the other, so
// the failure points at the change instead of just asserting one exists.
func firstNodeDelta(want, got *model.Model) string {
	inWant := map[model.ID]bool{}
	for _, n := range want.Nodes {
		inWant[n.ID] = true
	}
	inGot := map[model.ID]bool{}
	for _, n := range got.Nodes {
		inGot[n.ID] = true
	}

	var added, removed []string
	for _, n := range got.Nodes {
		if !inWant[n.ID] && len(added) < 5 {
			added = append(added, string(n.ID))
		}
	}
	for _, n := range want.Nodes {
		if !inGot[n.ID] && len(removed) < 5 {
			removed = append(removed, string(n.ID))
		}
	}

	out := ""
	if len(added) > 0 {
		out += "\n  in the code but NOT the model (first few): " + join(added)
	}
	if len(removed) > 0 {
		out += "\n  in the model but NOT the code (first few): " + join(removed)
	}
	if out == "" {
		out = "\n  node sets match; the difference is in edges or node contents"
	}
	return out
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
