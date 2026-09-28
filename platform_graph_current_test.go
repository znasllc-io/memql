package main

// platform_graph_current_test.go -- the committed platform graph
// (component/architecture/embedded/platform.graph.json, memql#5727) is not
// wrong, and has not fallen far behind the tree it describes.
//
// THE SHAPE IS THE ARCHITECTURE MODEL'S GATE, COPIED
// (component/architecture/model_current_test.go). Not byte equality: the graph
// describes the whole platform -- roles, deploy base, front door, routing,
// concepts, automations, OS navigation -- so any merge while a PR is open adds
// to it, and a gate that fails on somebody else's merge is unwinnable under a
// merge queue. Instead:
//
//	a node or relationship the committed graph asserts that a regeneration
//	does not have                      -> the graph LIES about the platform. FAIL.
//	one a regeneration has that the
//	committed graph does not           -> the graph is behind. Allowed, up to
//	                                      a drift ceiling.
//
// The ceiling is the model's: 25% of the regenerated graph, on nodes or on
// edges. The graph is two orders of magnitude smaller than the model, so one
// PR moves the percentage further -- a new namespace of concepts is a few
// percent -- but still nowhere near a quarter; what crosses it is a graph
// nobody has regenerated in months.
//
// WHY THE ROOT PACKAGE. The go-checks gate-inputs step (a docs-, manifest- or
// DSL-only PR) and every Go PR the planner runs both reach `./`: the root
// imports app, so any Go change reaches it, and it is in the gate-package list
// for any non-Go change. The graph is read from deploy/, dsl/, the proto
// descriptors, component/node and component/memql/os_navigation.json, so it
// can go stale from any of those, and the root is the one package every such
// PR runs.
//
// Regeneration goes through `make platform-graph`, the one place its flags
// live, exactly as the model's gate goes through `make arch-model`.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/embedded"
	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/node"
)

var committedPlatformGraphPath = filepath.Join("component", "architecture", "embedded", model.PlatformGraphFilename)

// platformMaxBehindPercent is the drift ceiling, the model's (maxBehindPercent
// in component/architecture/model_current_test.go).
const platformMaxBehindPercent = 25

// platformGraphMaxBytes bounds the committed file. Measured at 315,120 bytes
// when it landed; a graph that triples has started recording something it
// should not -- attribute bloat, or a pass that emits per-occurrence nodes --
// and should be looked at before it becomes the next 81 MB artifact.
const platformGraphMaxBytes = 1_000_000

func readPlatformGraph(t *testing.T, path string) *model.PlatformGraph {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	g, err := model.ReadPlatformGraph(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return g
}

type platformTriple struct {
	from, to model.ID
	kind     model.PlatformEdgeKind
}

func platformTriples(g *model.PlatformGraph) map[platformTriple]bool {
	out := make(map[platformTriple]bool, len(g.Edges))
	for _, e := range g.Edges {
		out[platformTriple{e.From, e.To, e.Kind}] = true
	}
	return out
}

func platformNodeIDs(g *model.PlatformGraph) map[model.ID]bool {
	out := make(map[model.ID]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		out[n.ID] = true
	}
	return out
}

// platformStale lists what want asserts and got does not: node ids, then
// (from, kind, to) triples. Capped samples, true totals. Attributes are not
// compared, for the model's reason: an attribute is a detail of a fact that
// still holds, and gating it is byte equality by another name.
func platformStale(want, got *model.PlatformGraph) (nodes []string, nodeTotal int, edges []string, edgeTotal int) {
	live := platformNodeIDs(got)
	for _, n := range want.Nodes {
		if !live[n.ID] {
			nodeTotal++
			if len(nodes) < 10 {
				nodes = append(nodes, string(n.ID))
			}
		}
	}
	liveEdges := platformTriples(got)
	var dead []platformTriple
	for e := range platformTriples(want) {
		if !liveEdges[e] {
			dead = append(dead, e)
		}
	}
	sort.Slice(dead, func(i, j int) bool {
		a, b := dead[i], dead[j]
		if a.from != b.from {
			return a.from < b.from
		}
		if a.to != b.to {
			return a.to < b.to
		}
		return a.kind < b.kind
	})
	for _, d := range dead {
		if len(edges) >= 10 {
			break
		}
		edges = append(edges, fmt.Sprintf("%s -%s-> %s", d.from, d.kind, d.to))
	}
	return nodes, nodeTotal, edges, len(dead)
}

// platformBehind counts what got has that want does not.
func platformBehind(want, got *model.PlatformGraph) (nodes, edges int) {
	committed := platformNodeIDs(want)
	for id := range platformNodeIDs(got) {
		if !committed[id] {
			nodes++
		}
	}
	committedEdges := platformTriples(want)
	for e := range platformTriples(got) {
		if !committedEdges[e] {
			edges++
		}
	}
	return nodes, edges
}

func platformDriftExceedsCeiling(behindNodes, totalNodes, behindEdges, totalEdges int) bool {
	over := func(behind, total int) bool {
		return total > 0 && behind*100 > total*platformMaxBehindPercent
	}
	return over(behindNodes, totalNodes) || over(behindEdges, totalEdges)
}

// TestPlatformGraphIsNotStale regenerates the graph through `make
// platform-graph` and holds the committed one to it. Skipped under -short:
// it runs the generator, a `go list` per role and the DSL load (~6s warm).
func TestPlatformGraphIsNotStale(t *testing.T) {
	if testing.Short() {
		t.Skip("regenerates the platform graph; skipped in -short")
	}
	regenerated := filepath.Join(t.TempDir(), model.PlatformGraphFilename)
	before, err := os.ReadFile(committedPlatformGraphPath)
	if err != nil {
		t.Fatalf("the committed platform graph is missing: %v", err)
	}
	cmd := exec.Command("make", "platform-graph", "PLATFORM_GRAPH_OUT="+regenerated)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("regenerating the platform graph failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(regenerated); err != nil {
		t.Fatalf("`make platform-graph` exited 0 but wrote nothing to %s -- did it stop honouring "+
			"PLATFORM_GRAPH_OUT? (%v)", regenerated, err)
	}
	after, err := os.ReadFile(committedPlatformGraphPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("regenerating wrote into the WORKING TREE (%s); PLATFORM_GRAPH_OUT is not being "+
			"honoured, so this test just modified tracked source", committedPlatformGraphPath)
	}

	want, got := readPlatformGraph(t, committedPlatformGraphPath), readPlatformGraph(t, regenerated)
	behindNodes, behindEdges := platformBehind(want, got)
	t.Logf("platform graph: committed %d nodes / %d edges, regenerated %d nodes / %d edges; "+
		"committed is behind by %d node(s) and %d edge(s) (allowed -- concurrent merges only add -- "+
		"but `make platform-graph` closes it)",
		len(want.Nodes), len(want.Edges), len(got.Nodes), len(got.Edges), behindNodes, behindEdges)

	nodes, nodeTotal, edges, edgeTotal := platformStale(want, got)
	if nodeTotal > 0 {
		t.Errorf("the committed platform graph has %d node(s) the platform no longer has:\n  %s\n\n"+
			"A role, Deployment, host, rule, concept, automation or OS section it names is gone. Run "+
			"`make platform-graph` and commit the result.", nodeTotal, strings.Join(nodes, "\n  "))
	}
	if edgeTotal > 0 {
		t.Errorf("the committed platform graph asserts %d relationship(s) a regeneration does not:\n  %s\n\n"+
			"Both ends usually still exist -- a route moved, a role stopped linking a package, a "+
			"relationship was dropped. Run `make platform-graph` and commit the result.",
			edgeTotal, strings.Join(edges, "\n  "))
	}
	if platformDriftExceedsCeiling(behindNodes, len(got.Nodes), behindEdges, len(platformTriples(got))) {
		t.Errorf("the committed platform graph is %d/%d nodes and %d/%d edges behind a regeneration; "+
			"the ceiling is %d%%. Somewhat behind is deliberate (merges only add); this far behind is a "+
			"different platform, and the diagrams and the Cluster map draw it as current. Run "+
			"`make platform-graph` and commit the result.",
			behindNodes, len(got.Nodes), behindEdges, len(platformTriples(got)), platformMaxBehindPercent)
	}
}

// TestPlatformGraphStalenessCatchesARemovalAndToleratesAnAddition is the
// gate's own fixture: the same helpers, fed a graph that lies and a graph that
// is merely behind.
func TestPlatformGraphStalenessCatchesARemovalAndToleratesAnAddition(t *testing.T) {
	role := model.PlatformNode{ID: model.ServiceID("agent"), Kind: model.PlatformRole, Label: "agent"}
	dep := model.PlatformNode{ID: model.DeploymentID("agent"), Kind: model.PlatformDeployment, Label: "agent"}
	gone := model.PlatformNode{ID: model.DeploymentID("retired"), Kind: model.PlatformDeployment, Label: "retired"}
	runs := model.PlatformEdge{From: role.ID, To: dep.ID, Kind: model.PlatformRunsOn}

	live := &model.PlatformGraph{Nodes: []model.PlatformNode{role, dep}}
	lying := &model.PlatformGraph{Nodes: []model.PlatformNode{role, dep, gone}, Edges: []model.PlatformEdge{runs}}
	if _, n, _, e := platformStale(lying, live); n != 1 || e != 1 {
		t.Fatalf("stale = %d nodes, %d edges; want the retired Deployment and the runs_on edge the regeneration lost", n, e)
	}

	behind := &model.PlatformGraph{Nodes: []model.PlatformNode{role}}
	regenerated := &model.PlatformGraph{Nodes: []model.PlatformNode{role, dep}, Edges: []model.PlatformEdge{runs}}
	if _, n, _, e := platformStale(behind, regenerated); n != 0 || e != 0 {
		t.Fatalf("a committed graph that is merely BEHIND reported %d stale nodes and %d stale edges", n, e)
	}
	if n, e := platformBehind(behind, regenerated); n != 1 || e != 1 {
		t.Fatalf("behind = %d nodes, %d edges; want 1 and 1", n, e)
	}
	if !platformDriftExceedsCeiling(26, 100, 0, 100) || platformDriftExceedsCeiling(25, 100, 25, 100) {
		t.Fatal("the drift ceiling is not 25% exclusive")
	}
}

// TestPlatformGraphIsWellFormed holds the committed file to what every reader
// relies on without regenerating anything, so it also runs under -short and
// can never be tripped by a concurrent merge.
func TestPlatformGraphIsWellFormed(t *testing.T) {
	raw, err := os.ReadFile(committedPlatformGraphPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > platformGraphMaxBytes {
		t.Errorf("the committed platform graph is %d bytes, over the %d ceiling", len(raw), platformGraphMaxBytes)
	}
	g, err := embedded.LoadPlatformGraph()
	if err != nil {
		t.Fatalf("the embedded platform graph does not decode through model.ReadPlatformGraph: %v", err)
	}
	if !bytes.Equal(raw, embedded.PlatformGraphJSON) {
		t.Fatal("the embedded bytes are not the committed file")
	}
	if err := model.ValidatePlatformGraph(g); err != nil {
		t.Fatalf("the committed platform graph is malformed: %v", err)
	}
	if !g.IsSorted() {
		t.Error("the committed platform graph is not in WriteJSON's order; regenerate it, do not edit it")
	}
	var canonical bytes.Buffer
	if err := g.WriteJSON(&canonical); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical.Bytes(), raw) {
		t.Error("the committed platform graph is not in WriteJSON's byte form (one element per line, " +
			"sorted, unescaped); regenerate it with `make platform-graph` rather than reformatting it")
	}
	for _, bad := range []string{"/Users/", "/home/", "C:\\", ".localhost", "example.com"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Errorf("the committed platform graph contains %q: a machine path or an installation's "+
				"domain. The graph describes the platform's shape; hosts are written against <domain>.", bad)
		}
	}

	kinds := map[model.PlatformNodeKind]int{}
	for _, n := range g.Nodes {
		kinds[n.Kind]++
	}
	edgeKinds := map[model.PlatformEdgeKind]int{}
	for _, e := range g.Edges {
		edgeKinds[e.Kind]++
	}
	// Floors, not counts: a pass that silently stopped emitting is the defect
	// the subset gate cannot see (an empty graph is a subset of anything).
	for kind, floor := range map[model.PlatformNodeKind]int{
		model.PlatformPackage: 100, model.PlatformDeployment: 7, model.PlatformK8sService: 7,
		model.PlatformService: 4, model.PlatformRPC: 4, model.PlatformHost: 5, model.PlatformPath: 10,
		model.PlatformRoutingRule: 50, model.PlatformNamespace: 20, model.PlatformConcept: 150,
		model.PlatformAutomation: 40, model.PlatformOSApp: 10, model.PlatformDatabase: 1,
	} {
		if kinds[kind] < floor {
			t.Errorf("the committed platform graph has %d %s node(s), floor %d: a pass stopped emitting", kinds[kind], kind, floor)
		}
	}
	for kind, floor := range map[model.PlatformEdgeKind]int{
		model.PlatformIncludes: 700, model.PlatformRunsOn: 7, model.PlatformServes: 7,
		model.PlatformRoutesTo: 10, model.PlatformRelates: 100, model.PlatformSelects: 7,
		model.PlatformConnectsTo: 1, model.PlatformDials: 1, model.PlatformTriggers: 1,
	} {
		if edgeKinds[kind] < floor {
			t.Errorf("the committed platform graph has %d %s edge(s), floor %d: a pass stopped emitting", edgeKinds[kind], kind, floor)
		}
	}
}

// TestPlatformGraphRolesAreTheRoleTable: the role nodes are exactly
// component/node's roles, by id, description and mesh. Stricter than the
// subset gate on purpose -- the role set is closed, so a role added without
// regenerating is an error rather than a graph that is merely behind.
func TestPlatformGraphRolesAreTheRoleTable(t *testing.T) {
	g := readPlatformGraph(t, committedPlatformGraphPath)
	got := map[model.ID]model.PlatformNode{}
	for _, n := range g.Nodes {
		if n.Kind == model.PlatformRole {
			got[n.ID] = n
		}
	}
	roles := node.Roles()
	if len(got) != len(roles) {
		t.Fatalf("the platform graph has %d roles, component/node %d; run `make platform-graph`", len(got), len(roles))
	}
	for _, r := range roles {
		n, ok := got[model.ServiceID(string(r.Type))]
		if !ok {
			t.Errorf("role %s is missing from the platform graph", r.Type)
			continue
		}
		if n.Attrs["description"] != r.Description || n.Attrs["mesh"] != fmt.Sprintf("%t", r.Mesh) {
			t.Errorf("role %s in the graph = %v, in roles.go = {%q mesh=%t}", r.Type, n.Attrs, r.Description, r.Mesh)
		}
	}
}
