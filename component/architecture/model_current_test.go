package architecture

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// Each profile is generated once in this test process, through the canonical
// target. Bytes live only in memory after its temporary output is removed.
// Independent CI jobs have independent source checks; there is no cross-job
// cache or claim that an earlier job qualified another commit.
var generatedModels [2]struct {
	once sync.Once
	raw  []byte
	err  error
}

func generatedBytes(t *testing.T, calls bool) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("whole-source architecture generation is excluded by -short")
	}
	index := 0
	if calls {
		index = 1
	}
	cache := &generatedModels[index]
	cache.once.Do(func() {
		dir, err := os.MkdirTemp("", "memql-architecture-")
		if err != nil {
			cache.err = err
			return
		}
		defer os.RemoveAll(dir)
		out := filepath.Join(dir, model.CanonicalFilename)
		args := []string{"arch-model", "ARCH_MODEL_OUT=" + out}
		if calls {
			args = append(args, "ARCH_MODEL_CALLS=1")
		}
		cmd := exec.Command("make", args...)
		cmd.Dir = workspaceRoot(t)
		if output, err := cmd.CombinedOutput(); err != nil {
			cache.err = fmt.Errorf("canonical generation: %w\n%s", err, output)
			return
		}
		cache.raw, cache.err = os.ReadFile(out)
	})
	if cache.err != nil {
		t.Fatal(cache.err)
	}
	return cache.raw
}

func generatedModel(t *testing.T) *model.Model {
	t.Helper()
	m, err := model.ReadJSON(bytes.NewReader(generatedBytes(t, false)))
	if err != nil {
		t.Fatalf("generated model failed its strict decoder: %v", err)
	}
	return m
}

func TestArchitectureModelBuildsFromSource(t *testing.T) {
	m := generatedModel(t)
	t.Logf("complete structural model: %d nodes, %d edges", len(m.Nodes), len(m.Edges))
	seen := make(map[model.ID]bool)
	for _, n := range m.Nodes {
		if seen[n.ID] {
			t.Errorf("duplicate node ID %s", n.ID)
		}
		seen[n.ID] = true
	}
	// Edges are a multiset. Equal edge records are legal and are not discarded.
	withCalls, why := callGraphOnDemand()
	t.Logf("call graph: %s", why)
	if !withCalls {
		return
	}
	calls, err := model.ReadJSON(bytes.NewReader(generatedBytes(t, true)))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls.Edges) - len(structuralOnly(calls).Edges); n < minCallEdges {
		t.Errorf("call graph has %d edges, floor %d", n, minCallEdges)
	}
	if bad, total := callGraphIsConsistent(calls); total > 0 {
		t.Errorf("call graph disagrees with structural symbols (%d): %s", total, join(bad))
	}
}

// The pure comparison helpers retained below support historical regression
// fixtures. They are not a subset/staleness gate over a committed topology.

// minCallEdges is the floor on the on-demand call graph. Measured at 287,310
// `calls` edges when the call graph left the artifact (memql#5727); the floor is
// a third of that, low enough that no refactor reaches it and high enough that a
// CHA pass which silently built over one package does.
const minCallEdges = 100000

// callGraphOnDemand decides whether this run builds the call graph.
//
// YES wherever the full Go suite runs -- ci.yml's go-tests shards, a push to
// main, a developer's `go test` or `make arch-model-check` -- so the CHA pass is
// exercised on every Go change. NO in go-checks' gate-inputs step, which runs
// this package for a docs-, manifest- or script-only PR (memql#2972): nothing
// such a PR can change reaches the call graph, and building it is the dearest
// part of that lane (~55s of the CI shard's measured time, against ~8s warm on
// a developer machine).
//
// The signal is RUN_GATES, the go-checks job's own env value, which is "true"
// exactly when that step runs and never in a lane that runs the full suite --
// so no workflow edit was needed. If it is ever renamed this silently falls to
// YES, which costs time and loses no coverage.
func callGraphOnDemand() (bool, string) {
	if os.Getenv("RUN_GATES") == "true" {
		return false, "skipped: RUN_GATES=true (ci.yml's docs-only gate-inputs step); the structural model is checked in full"
	}
	return true, "built on demand (ARCH_MODEL_CALLS=1); the structural profile carries none"
}

// structuralOnly projects the structural profile without changing edge multiplicity.
func structuralOnly(m *model.Model) *model.Model {
	out := &model.Model{SchemaVersion: m.SchemaVersion, Nodes: m.Nodes, Edges: make([]model.Edge, 0, len(m.Edges))}
	for _, e := range m.Edges {
		if e.Kind != model.EdgeCalls {
			out.Edges = append(out.Edges, e)
		}
	}
	return out
}

// callGraphIsConsistent reports the call-graph endpoints the structural pass of
// the same model should have a node for and does not: an exported non-closure
// function, or a method whose receiver type the model has as a type or
// interface node. Everything else a call names -- an unexported function, a
// closure (`f$1`), a method of an unexported or non-struct type -- is node-less
// by design (see liveSymbols). Measured at zero when the check was added
// (memql#5727): the two passes agree, and this keeps them agreeing.
func callGraphIsConsistent(m *model.Model) (samples []string, total int) {
	nodes := make(map[model.ID]bool, len(m.Nodes))
	for _, n := range m.Nodes {
		nodes[n.ID] = true
	}
	seen := map[model.ID]bool{}
	var bad []string
	check := func(id model.ID) {
		if nodes[id] || seen[id] || strings.Contains(string(id), "$") {
			return
		}
		seen[id] = true
		s := string(id)
		switch {
		case strings.HasPrefix(s, "func:"):
			name := s[strings.LastIndex(s, ".")+1:]
			if name != "" && strings.ToUpper(name[:1]) == name[:1] && strings.ToLower(name[:1]) != name[:1] {
				bad = append(bad, s)
			}
		case strings.HasPrefix(s, "method:"):
			rest := strings.TrimPrefix(s, "method:")
			open, close := strings.Index(rest, ".("), strings.LastIndex(rest, ").")
			if open < 0 || close < open {
				return
			}
			pkg, recv := rest[:open], rest[open+2:close]
			if nodes[model.TypeID(pkg, recv)] || nodes[model.InterfaceID(pkg, recv)] {
				bad = append(bad, s)
			}
		}
	}
	for _, e := range m.Edges {
		if e.Kind == model.EdgeCalls {
			check(e.From)
			check(e.To)
		}
	}
	sort.Strings(bad)
	if len(bad) > 10 {
		return bad[:10], len(bad)
	}
	return bad, len(bad)
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
// different codebase, and every consumer (the docs generators, the observe
// runtime's FQN join) is reading it as current.
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
			"refreshes, it is a different codebase, and every reader takes it as current. "+
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

func TestNoSourceRefEscapesTheWorkspace(t *testing.T) {
	for _, n := range generatedModel(t).Nodes {
		if n.Source == nil {
			continue
		}
		f := n.Source.File
		if filepath.IsAbs(f) || f == ".." || strings.HasPrefix(f, ".."+string(filepath.Separator)) {
			t.Errorf("node %s has escaping source %q", n.ID, f)
		}
	}
}

func TestGeneratedModelIsReproducible(t *testing.T) {
	m := generatedModel(t)
	if m.GeneratedAt != "" || m.Workspace != "" {
		t.Fatal("canonical output contains a clock or absolute workspace")
	}
	// Strict decode/encode preserves all attributes and edge multiplicities.
	var rewritten bytes.Buffer
	if err := m.WriteJSON(&rewritten); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rewritten.Bytes(), generatedBytes(t, false)) {
		t.Fatal("canonical output does not round-trip byte-for-byte")
	}
}

func TestGeneratedModelClusterIsPinned(t *testing.T) {
	var ids []model.ID
	for _, n := range generatedModel(t).Nodes {
		if n.Kind == model.KindCluster {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) != 1 || ids[0] != "cluster:memql" {
		t.Fatalf("cluster IDs = %v", ids)
	}
}

func TestGeneratedModelIsSorted(t *testing.T) {
	if !generatedModel(t).IsSortedForStableOutput() {
		t.Fatal("generated model is not in canonical order")
	}
}

func TestGeneratedModelHasNoCallGraph(t *testing.T) {
	for _, e := range generatedModel(t).Edges {
		if e.Kind == model.EdgeCalls {
			t.Fatal("structural profile contains call edges")
		}
	}
}

func TestGeneratedModelIsNotGutted(t *testing.T) {
	m := generatedModel(t)
	if len(m.Nodes) < 15000 || len(m.Edges) < 20000 {
		t.Fatalf("incomplete graph: %d nodes, %d edges", len(m.Nodes), len(m.Edges))
	}
}

func TestGeneratedModelLoadsThroughTheRealDecoder(t *testing.T) { _ = generatedModel(t) }

func TestGeneratedModelIsCompact(t *testing.T) {
	if n := bytes.Count(generatedBytes(t, false), []byte("\n")); n != 1 {
		t.Fatalf("got %d newlines, want one", n)
	}
}
