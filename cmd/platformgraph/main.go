// platformgraph writes the platform graph: the system the engine's code
// becomes, as one deterministic JSON file (memql#5727, design record D6).
//
// Usage:
//
//	go run ./cmd/platformgraph --root . --out component/architecture/embedded/platform.graph.json
//
// Run it through `make platform-graph`, which is the one place the flags live;
// TestPlatformGraphIsNotStale regenerates through the same target.
//
// It lives in the ROOT module because what it reads needs the platform tier
// and above -- component/node's role and routing tables, the generated proto
// descriptors, the concept registry and the automation loader -- and the graph
// it writes lives in component/architecture, which is base tier and may take
// that data but never import it.
//
// Eight passes, each a function of the tree:
//
//	nodes        the roles (component/node/roles.go), mesh or not, and the
//	             packages each role's binary links (`go list -deps -tags <type>`,
//	             pinned to linux/amd64 with cgo off, the image's build)
//	deploy       deploy/k8s/base plus components/engine-bff: Deployments,
//	             Services, ports, runs_on, selects, dials, connects_to
//	services     the proto descriptors: gRPC services, their RPCs, and serves
//	             edges from each role to the servers its build constructs
//	frontdoor    the generated cloud front door: hosts and paths, routes_to
//	routing      the mesh event-routing table (node.RoutingRules)
//	concepts     every concept by namespace and its @relationship edges
//	automations  the static automation graph, through the one projection the
//	             automationGraph builtin and cmd/memql-arch share
//	os           the MemQL OS navigation contract
//
// The output carries no timestamp, no absolute path and no domain (hosts are
// written against the placeholder <domain>), and is byte-identical across
// runs and machines.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/znasllc-io/memql/component/architecture/model"
)

func main() {
	root := flag.String("root", ".", "repository root (the directory holding go.work)")
	out := flag.String("out", "", "output path (default: <root>/component/architecture/embedded/"+model.PlatformGraphFilename+")")
	flag.Parse()

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fail("resolve root: %v", err)
	}
	g, err := build(absRoot)
	if err != nil {
		fail("%v", err)
	}
	var buf bytes.Buffer
	if err := g.WriteJSON(&buf); err != nil {
		fail("encode: %v", err)
	}
	path := *out
	if path == "" {
		path = filepath.Join(absRoot, "component", "architecture", "embedded", model.PlatformGraphFilename)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		fail("write: %v", err)
	}
	fmt.Printf("platformgraph: wrote %d nodes, %d edges, %d bytes -> %s\n", len(g.Nodes), len(g.Edges), buf.Len(), path)
}

// build runs every pass over the tree at root and returns the validated graph.
func build(root string) (*model.PlatformGraph, error) {
	b := newBuilder()
	roles, err := nodesPass(b, root)
	if err != nil {
		return nil, fmt.Errorf("nodes: %w", err)
	}
	passes := []struct {
		name string
		run  func() error
	}{
		{"deploy", func() error { return deployPass(b, root) }},
		{"services", func() error { return servicesPass(b, roles) }},
		{"frontdoor", func() error { return frontdoorPass(b, root) }},
		{"routing", func() error { return routingPass(b) }},
		{"concepts", func() error { return conceptsPass(b) }},
		{"automations", func() error { return automationsPass(b) }},
		{"os", func() error { return osPass(b, root) }},
	}
	for _, p := range passes {
		if err := p.run(); err != nil {
			return nil, fmt.Errorf("%s: %w", p.name, err)
		}
	}
	return b.graph()
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "platformgraph: "+format+"\n", args...)
	os.Exit(1)
}
