package architecture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// WHAT IT CHECKS INSTEAD. Every symbol the COMMITTED model REFERENCES must still
// exist in the regenerated one -- as a node id, and as an edge endpoint. That is
// exactly the defect #2844 reported -- ToggleComputerUseEnabledArgs.UserId
// survived in the model in 13 places after #2840 removed the argument -- and it
// is immune to concurrent merges, which only ADD symbols.
//
// The asymmetry is deliberate and is the whole point:
//
//	symbol removed from the code, still in the model  -> the model LIES. FAIL.
//	symbol added to the code, not yet in the model    -> the model is behind. OK.
//
// A missing symbol makes the cockpit render something that no longer exists. An
// absent one makes it render slightly less than exists, which is what any
// committed snapshot does between refreshes. Run `make arch-model` to catch up.
//
// EDGES ARE CHECKED TOO, AND ONLY SINCE memql#3050. Until then the check built
// its live set from `got.Nodes` alone and compared only `want.Nodes`, so it could
// not see a deleted UNEXPORTED function: the extractor emits no node for one, so
// deleting it removes no node and "committed nodes are a subset of live nodes" is
// satisfied vacuously while the edge list rots. Hit for real -- PR #3033
// (memql#2951) deleted `memqlTypeToJSONType` and left 33 `calls` edges pointing
// at it, green through every CI run, found only by an adversarial review. The
// live set now includes edge endpoints; see liveSymbols for why it must.
//
// THE EDGE SET ITSELF IS CHECKED ONLY SINCE memql#3288. #3050 checked edge
// ENDPOINTS, which is a check about symbols, not about relationships: delete a
// call site between two functions that both still exist and every endpoint stays
// live, so the gate saw nothing. Measured on `main` at f9a2d046, with the gate
// green: 15 committed `calls` edges named a call the code no longer makes, all 15
// with both endpoints alive. staleEdges compares the (From, To, Kind) triples.
//
// WHAT THIS GATE STILL CANNOT SEE, deliberately, and why -- the half of memql#3288
// that was missing from this comment block rather than from the code:
//
//   - ANYTHING ADDED. Every check walks `want` and asks whether `got` agrees;
//     nothing iterates `got`. That is the add-only asymmetry above, and it is
//     load-bearing, not an oversight.
//
//   - Therefore also A FACT SURGICALLY REMOVED from the committed file. Deleting
//     one edge from the artifact leaves it a strict subset, which is the "behind"
//     direction, which passes. This is not fixable while the asymmetry holds:
//     "the artifact is missing edge A->B" and "a concurrent merge added a call
//     A->B" are THE SAME OBSERVATION from here, so no rule can fail the first and
//     pass the second. A count-equality gate "fixes" the repro precisely by giving
//     up merge-tolerance -- measured on this same commit, the committed artifact
//     was 552 nodes and 7,761 edge triples behind a regeneration purely from
//     merges, so equality would have been red on an untouched `main`. What bounds
//     this instead is the drift ceiling below: unbounded staleness fails, one
//     merge's worth does not.
//
//   - NODE ATTRIBUTES on a node that still exists -- signature, doc, attrs, and
//     source.line. Tempting, and measured before rejecting: of 1,353 shared node
//     ids whose JSON differed from a regeneration, 1,343 differed ONLY in
//     source.line, because editing any file shifts the line of every declaration
//     below it. Gating that is byte-equality wearing a hat: it would go red on
//     essentially every PR that touches Go, which is the design this file exists
//     to replace.
//
//   - EDGE ATTRIBUTES, for the same reason at smaller scale. Keying the edge
//     comparison on Attrs as well as (From, To, Kind) turned the same 15 findings
//     into 49; the extra 34 were `call_sites` counts changing on calls that still
//     happen. The relationship is the fact worth gating; how many times it occurs
//     in a function body is not.
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
	live := liveSymbols(got)

	// Always reported, green or red: the numbers are the cheap signal the gate
	// itself is too permissive to assert on, and memql#3288 asked for them
	// specifically. A reader of a passing run can see how far behind the artifact
	// has drifted without regenerating anything themselves.
	behindNodes, behindEdges := reportCounts(t, want, got)

	if gone, total := staleNodes(want, live); total > 0 {
		t.Errorf("the committed architecture model references %d node(s) that NO LONGER EXIST "+
			"in the code:\n  %s\n\nThe cockpit's Topology tab reads this file, so it is rendering "+
			"things that are gone. Run `make arch-model` and commit the result (memql#2844).",
			total, join(gone))
	}

	if gone, total := staleEdgeEndpoints(want, live); total > 0 {
		t.Errorf("the committed architecture model has edges pointing at %d symbol(s) the "+
			"regenerated model NO LONGER MENTIONS:\n  %s\n\nThese have no NODE in the model, "+
			"so the node check above cannot see them -- and a dangling edge renders a call "+
			"into a symbol the code no longer has.\n\nA node-less symbol leaves the live set "+
			"when the extractor stops emitting an edge for it, which happens if it was "+
			"deleted, RENAMED, or simply lost its LAST CALL SITE -- there is no "+
			"declaration-level edge for an unexported function, only per-call-site ones. All "+
			"three want the same thing: run `make arch-model` and commit the result "+
			"(memql#3050).",
			total, join(gone))
	}

	if gone, total := staleEdges(want, got); total > 0 {
		t.Errorf("the committed architecture model asserts %d RELATIONSHIP(s) the regenerated "+
			"model does not have:\n  %s\n\nTypically both endpoints still exist, which is why "+
			"neither check above can see them: they are edges, not symbols. The model is claiming "+
			"a call, import, containment or implements that the code no longer contains, and the "+
			"cockpit draws it. The usual cause is a call site being deleted or moved while both "+
			"functions survive. Run `make arch-model` and commit the result (memql#3288).",
			total, join(gone))
	}

	assertDriftIsBounded(t, behindNodes, len(got.Nodes), behindEdges, len(edgeTriples(got)))
}

// edgeTriple is an edge's IDENTITY for drift purposes: who, to whom, how.
//
// Attrs are deliberately excluded -- see the blind-spot list on the gate. A
// `calls` edge carries call_sites, which moves when a function body gains a
// second call to something it already called; that is not a relationship
// changing. Measured: including Attrs turned 15 findings into 49, and all 34
// extra were call_sites counts on calls that still happen.
//
// Duplicate triples are collapsed. model.Edge documents (From, To, Kind) as a
// multi-set key, so this is a real narrowing -- but the artifact contains zero
// duplicate triples today (measured on both the committed and regenerated
// models), and a duplicate is a fact stated twice, not a different fact.
type edgeTriple struct {
	from model.ID
	to   model.ID
	kind model.EdgeKind
}

func edgeTriples(m *model.Model) map[edgeTriple]bool {
	set := make(map[edgeTriple]bool, len(m.Edges))
	for _, e := range m.Edges {
		set[edgeTriple{from: e.From, to: e.To, kind: e.Kind}] = true
	}
	return set
}

// staleEdges is the memql#3288 check: every RELATIONSHIP the committed model
// asserts must still be asserted by a freshly regenerated one.
//
// This is the same subset shape as staleNodes and staleEdgeEndpoints, one level
// up: those ask whether a SYMBOL still exists, this asks whether a FACT ABOUT TWO
// SYMBOLS still holds. It closes the gap between them -- delete the only call
// from A to B and both symbols stay live (they have other callers and callees),
// so the endpoint check is satisfied while the committed model keeps drawing an
// arrow that the code does not contain.
//
// WHY THE ADD-ONLY ASYMMETRY SURVIVES. It iterates `want.Edges` only. A
// relationship the code has and the artifact does not cannot be reported, so a
// concurrent merge -- which only adds -- can never turn this red. Same structural
// guarantee as the two checks above, and the reason this could be added at all:
// the alternative shape, comparing counts or sets both ways, fails on other
// people's merges.
//
// Capped at 10 samples with the true total reported, matching its siblings.
func staleEdges(want, got *model.Model) (samples []string, total int) {
	live := edgeTriples(got)

	dead := make([]edgeTriple, 0, 16)
	seen := make(map[edgeTriple]bool)
	for _, e := range want.Edges {
		t := edgeTriple{from: e.From, to: e.To, kind: e.Kind}
		if t.from == "" || t.to == "" || live[t] || seen[t] {
			continue
		}
		seen[t] = true
		dead = append(dead, t)
	}
	sort.Slice(dead, func(i, j int) bool {
		if dead[i].from != dead[j].from {
			return dead[i].from < dead[j].from
		}
		if dead[i].to != dead[j].to {
			return dead[i].to < dead[j].to
		}
		return dead[i].kind < dead[j].kind
	})

	for _, d := range dead {
		if len(samples) >= 10 {
			break
		}
		samples = append(samples, fmt.Sprintf("%s -%s-> %s", d.from, d.kind, d.to))
	}
	return samples, len(dead)
}

// reportCounts logs both models' sizes and how far behind the committed one is,
// and returns the two behind-counts for the ceiling below.
//
// memql#3288 asked for the counts to be PRINTED, and that half is unconditional:
// on a green run this is the only place a reader learns the artifact is 7,000
// edges old. It is a t.Logf and not an assertion because count EQUALITY is the
// merge-fragile design this file exists to avoid -- see the blind-spot list.
func reportCounts(t *testing.T, want, got *model.Model) (behindNodes, behindEdges int) {
	t.Helper()

	liveNodes := make(map[model.ID]bool, len(got.Nodes))
	for _, n := range got.Nodes {
		liveNodes[n.ID] = true
	}
	committedNodes := make(map[model.ID]bool, len(want.Nodes))
	for _, n := range want.Nodes {
		committedNodes[n.ID] = true
	}
	for id := range liveNodes {
		if !committedNodes[id] {
			behindNodes++
		}
	}

	committedEdges := edgeTriples(want)
	for e := range edgeTriples(got) {
		if !committedEdges[e] {
			behindEdges++
		}
	}

	t.Logf("architecture model: committed %d nodes / %d edges, regenerated %d nodes / %d edges; "+
		"committed is behind by %d node(s) and %d edge(s) (which is allowed -- concurrent merges "+
		"only add -- but `make arch-model` closes it)",
		len(want.Nodes), len(want.Edges), len(got.Nodes), len(got.Edges), behindNodes, behindEdges)

	return behindNodes, behindEdges
}

// assertDriftIsBounded puts a CEILING on the one thing the add-only asymmetry
// leaves unbounded: how far behind the artifact is allowed to get.
//
// The asymmetry is right -- failing on someone else's merge is worse than being
// slightly behind -- but "slightly" was enforced by nothing at all. memql#3288's
// own words: "nothing forces that refresh; behind is bounded only by whoever
// remembers." A model 40% behind is not a snapshot between refreshes, it is a
// different codebase, and every consumer (the cockpit's Topology tab, the
// observe runtime's FQN join) is reading it as current.
//
// A PERCENTAGE of the regenerated model, not an absolute, so it scales with the
// tree -- and generous, because the failure it must not produce is a red gate on
// an innocent PR. Calibration: on f9a2d046, three weeks and many merges after the
// last refresh, the artifact was 2.5% behind on nodes and 5.8% behind on edges.
// 25% is therefore several months of neglect, and no single merge approaches it.
// It is a backstop against abandonment, not a freshness policy; the freshness
// policy is CLAUDE.md's "regenerate LAST in any change that touches Go".
// maxBehindPercent is the ceiling; see assertDriftIsBounded for the calibration.
const maxBehindPercent = 25

// driftExceedsCeiling is the pure predicate, so a fixture test can pin both ends
// of the threshold without fabricating a *testing.T.
func driftExceedsCeiling(behindNodes, totalNodes, behindEdges, totalEdges int) bool {
	over := func(behind, total int) bool {
		return total > 0 && behind*100 > total*maxBehindPercent
	}
	return over(behindNodes, totalNodes) || over(behindEdges, totalEdges)
}

func assertDriftIsBounded(t *testing.T, behindNodes, totalNodes, behindEdges, totalEdges int) {
	t.Helper()

	if driftExceedsCeiling(behindNodes, totalNodes, behindEdges, totalEdges) {
		t.Errorf("the committed architecture model is %d/%d nodes (%.1f%%) and %d/%d edges "+
			"(%.1f%%) behind a regeneration; the ceiling is %d%%.\n\nBeing SOMEWHAT behind is "+
			"deliberate -- concurrent merges only add symbols and a gate that fails on someone "+
			"else's merge is unwinnable. Being this far behind is not a snapshot between "+
			"refreshes, it is a different codebase, and the cockpit renders it as current. "+
			"Run `make arch-model` and commit the result (memql#3288).",
			behindNodes, totalNodes, pct(behindNodes, totalNodes),
			behindEdges, totalEdges, pct(behindEdges, totalEdges), maxBehindPercent)
	}
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}

// liveSymbols is every ID the regenerated model can vouch for: node IDs PLUS
// every edge endpoint.
//
// THE ENDPOINTS ARE THE LOAD-BEARING HALF, and they are what memql#3050 turned
// on. Measured on the artifact THIS COMMIT SHIPS: of 25,295 distinct endpoints,
// 5,041 have no node at all -- 4,190 funcs, 816 methods, 33 interfaces, 2 types.
// (The pre-regeneration figures were 25,194 / 5,006 / 4,155; the shape is what
// matters, and it does not move. Expect the exact counts to drift with the tree
// -- they are here to show the ORDER of the problem, not as a value to assert.)
// Unexported functions and generated closures like `main$1` appear only as call
// graph endpoints, because the types pass does not emit nodes for them.
//
// So validating endpoints against the NODE set would report all 5,006 as
// dangling on a perfectly current model. What actually proves a symbol still
// exists is that a FRESHLY REGENERATED model still mentions it somewhere: the
// extractor rebuilds the call graph from the current source, so it cannot emit
// an edge to a function that is no longer there.
func liveSymbols(m *model.Model) map[model.ID]bool {
	live := make(map[model.ID]bool, len(m.Nodes)+len(m.Edges))
	for _, n := range m.Nodes {
		live[n.ID] = true
	}
	for _, e := range m.Edges {
		live[e.From] = true
		live[e.To] = true
	}
	return live
}

// staleNodes is the original #2844 check: every node in the committed model
// must still exist. Returns up to 10 IDs for the message plus the TRUE total,
// which the capped slice used to under-report.
func staleNodes(want *model.Model, live map[model.ID]bool) (samples []string, total int) {
	for _, n := range want.Nodes {
		if !live[n.ID] {
			total++
			if len(samples) < 10 {
				samples = append(samples, string(n.ID))
			}
		}
	}
	return samples, total
}

// staleEdgeEndpoints is the memql#3050 check: every endpoint of every committed
// edge must still exist in the live symbol set.
//
// WHY THE ADD-ONLY ASYMMETRY SURVIVES. It iterates the COMMITTED edges only, so
// a symbol that exists in the code but is merely newer than the artifact appears
// in `live` and in no committed edge -- it cannot be reported. That is the
// concurrent-merge case the subset design exists to protect, and it is protected
// here for the same structural reason: nothing ever fails for being present.
//
// Reported per missing symbol rather than per edge, with the edge count, because
// one deleted function takes a whole fan of edges with it -- the incident was 33
// `calls` edges to a single deleted `memqlTypeToJSONType`. Sorted by edge count
// then ID so the message is deterministic.
func staleEdgeEndpoints(want *model.Model, live map[model.ID]bool) (samples []string, total int) {
	edges := make(map[model.ID]int)
	kinds := make(map[model.ID]map[model.EdgeKind]bool)

	note := func(id model.ID, kind model.EdgeKind) {
		if id == "" || live[id] {
			return
		}
		edges[id]++
		if kinds[id] == nil {
			kinds[id] = make(map[model.EdgeKind]bool)
		}
		kinds[id][kind] = true
	}
	for _, e := range want.Edges {
		note(e.From, e.Kind)
		note(e.To, e.Kind)
	}

	dead := make([]model.ID, 0, len(edges))
	for id := range edges {
		dead = append(dead, id)
	}
	sort.Slice(dead, func(i, j int) bool {
		if edges[dead[i]] != edges[dead[j]] {
			return edges[dead[i]] > edges[dead[j]]
		}
		return dead[i] < dead[j]
	})

	for _, id := range dead {
		if len(samples) >= 10 {
			break
		}
		ks := make([]string, 0, len(kinds[id]))
		for k := range kinds[id] {
			ks = append(ks, string(k))
		}
		sort.Strings(ks)
		samples = append(samples, fmt.Sprintf("%s (%d %s edge(s))", id, edges[id],
			strings.Join(ks, "+")))
	}
	return samples, len(dead)
}

// workspaceRoot walks up from the test's directory to the REPOSITORY root.
//
// It looks for go.work, not go.mod. Since memql#3228 the tree is many modules,
// and this package is one of them -- a go.mod walk stopped at
// component/architecture/, so every model test resolved the wrong root
// (memql#3241). go.work exists once, at the repository root, which is what
// "workspace root" has always meant here.
func workspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.work above %s", dir)
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
