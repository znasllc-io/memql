package dslfs

import (
	"fmt"
	"sort"
)

// ImportGraph is the file-to-file dependency DAG built from the
// `import (...)` blocks of every parsed .memql file in a DSL tree.
// Nodes are paths-relative-to-root; an edge A -> B means "A imports
// B" (A depends on B). Topological order is bottom-up: a file is
// emitted after every file it depends on.
//
// This package is intentionally filesystem-free. The loader walks
// the embedded FS or disk, parses each file, calls ResolveImport on
// each `import` entry, and feeds the resulting (importer, imported)
// pairs into Add. The graph then exposes Topo / DetectCycle for
// the loader to drive compilation order.
type ImportGraph struct {
	// nodes records every file the graph knows about (both
	// importers and imports). Existence in this set means the file
	// was discovered by the walker; the graph does not assume
	// presence of the imported file -- the loader checks existence
	// separately and surfaces a "missing import" error.
	nodes map[string]struct{}

	// edges is the adjacency list of out-edges per node.
	// edges[A] = {B, C} means A imports both B and C.
	edges map[string]map[string]struct{}
}

// NewImportGraph constructs an empty graph.
func NewImportGraph() *ImportGraph {
	return &ImportGraph{
		nodes: make(map[string]struct{}),
		edges: make(map[string]map[string]struct{}),
	}
}

// AddNode registers a file in the graph. Called once per parsed
// file during loader discovery. A file with no imports still
// needs to be a node so Topo emits it.
func (g *ImportGraph) AddNode(file string) {
	if file == "" {
		return
	}
	g.nodes[file] = struct{}{}
	if _, ok := g.edges[file]; !ok {
		g.edges[file] = make(map[string]struct{})
	}
}

// AddEdge records an import: `from` imports `to`. Idempotent --
// repeated AddEdge calls collapse to one edge. Adds the endpoints
// as nodes if they are not yet registered.
func (g *ImportGraph) AddEdge(from, to string) {
	if from == "" || to == "" {
		return
	}
	g.AddNode(from)
	g.AddNode(to)
	g.edges[from][to] = struct{}{}
}

// Nodes returns the set of registered files, sorted for stable
// output. Useful for tests and the linter's whole-tree report.
func (g *ImportGraph) Nodes() []string {
	out := make([]string, 0, len(g.nodes))
	for n := range g.nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// OutEdges returns the sorted list of files that `from` imports.
// Returns nil for unknown files.
func (g *ImportGraph) OutEdges(from string) []string {
	e, ok := g.edges[from]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(e))
	for v := range e {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// DetectCycle runs DFS to find a cycle in the graph. If found,
// returns the cycle path as `[a, b, c, a]` (start node repeated at
// end). Returns nil if the graph is acyclic.
//
// The DFS visits nodes in sorted order so the reported cycle is
// deterministic across runs -- helpful for test stability and for
// the error message landing on the same files when the same cycle
// exists.
func (g *ImportGraph) DetectCycle() []string {
	const (
		white = 0 // unvisited
		gray  = 1 // currently on the DFS stack
		black = 2 // fully processed
	)
	color := make(map[string]int, len(g.nodes))
	parent := make(map[string]string, len(g.nodes))

	starts := g.Nodes()
	for _, root := range starts {
		if color[root] != white {
			continue
		}
		// Iterative DFS so we can recover the cycle path without
		// blowing the call stack on deep import trees.
		stack := []string{root}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			if color[n] == white {
				color[n] = gray
			}
			progressed := false
			for _, child := range g.OutEdges(n) {
				switch color[child] {
				case white:
					parent[child] = n
					stack = append(stack, child)
					progressed = true
				case gray:
					// Back-edge -> cycle. Walk parent[] from `n`
					// back to `child` to reconstruct the path.
					path := []string{child, n}
					cur := n
					for cur != child {
						p, ok := parent[cur]
						if !ok {
							break
						}
						path = append(path, p)
						cur = p
					}
					// reverse: parent[]-walk gave us bottom-up,
					// we want from->to order.
					reverse(path)
					path = append(path, child)
					return path
				}
				if progressed {
					break
				}
			}
			if !progressed {
				color[n] = black
				stack = stack[:len(stack)-1]
			}
		}
	}
	return nil
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// Topo returns the files in topological order: bottom-up, every
// file emitted after every file it depends on. So a leaf (a file
// no other file imports inwards from, or rather: a file with no
// outgoing imports) comes out first; a root (file that imports a
// lot but nothing imports it) comes out last.
//
// Returns a typed error containing the cycle path if one exists.
//
// Stable ordering: within the topological constraints, files are
// emitted in alphabetical order, so the output is reproducible
// across runs and across machines.
//
// Implementation: DFS post-order with iterative stack to avoid
// recursion depth issues on deep import trees. Visit children in
// alphabetical order; emit a node when all its children have been
// emitted. The result is depends-first by construction.
func (g *ImportGraph) Topo() ([]string, error) {
	if cycle := g.DetectCycle(); cycle != nil {
		return nil, &CycleError{Path: cycle}
	}

	visited := make(map[string]bool, len(g.nodes))
	out := make([]string, 0, len(g.nodes))

	roots := g.Nodes()
	for _, r := range roots {
		if visited[r] {
			continue
		}
		// Iterative DFS post-order. Each stack frame is (node,
		// nextChildIndex). When nextChildIndex == len(children),
		// the node is fully expanded and ready to emit.
		type frame struct {
			node     string
			children []string
			next     int
		}
		stack := []*frame{{node: r, children: g.OutEdges(r)}}
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.next < len(top.children) {
				child := top.children[top.next]
				top.next++
				if !visited[child] {
					stack = append(stack, &frame{node: child, children: g.OutEdges(child)})
				}
				continue
			}
			// Children done. Emit this node.
			if !visited[top.node] {
				visited[top.node] = true
				out = append(out, top.node)
			}
			stack = stack[:len(stack)-1]
		}
	}

	if len(out) != len(g.nodes) {
		return nil, fmt.Errorf("topological sort dropped nodes: got %d of %d", len(out), len(g.nodes))
	}
	return out, nil
}

// CycleError is returned by Topo when the graph contains a cycle.
// Path is the cycle as a list of files, with the first node
// repeated at the end so the message reads naturally:
//
//	"import cycle: ./a.memql -> ./b.memql -> ./a.memql"
type CycleError struct {
	Path []string
}

func (e *CycleError) Error() string {
	if len(e.Path) == 0 {
		return "import cycle"
	}
	s := "import cycle: "
	for i, p := range e.Path {
		if i > 0 {
			s += " -> "
		}
		s += p
	}
	return s
}
