package model

// platform.go -- the platform graph (memql#5727, design record D6).
//
// The architecture model (model.go) is the CODE: packages, types, methods. The
// platform graph is the SYSTEM that code becomes: the node roles and what each
// binary links, the Deployments and Services that run them, the gRPC services
// they serve, the front door's hosts and paths, the mesh routing table, the
// concepts and their relationships, the automation trigger graph and the OS's
// navigation. It is written by cmd/platformgraph (root module, because reading
// those facts needs component/node, the proto descriptors and the concept
// registry) and committed at component/architecture/embedded/platform.graph.json.
//
// Only the SCHEMA lives here. This package is base tier and imports nothing
// above it, so it can describe the graph and read it back, never build it.
//
// Ids come from ids.go. A role is a Service (ServiceID), a package PackageID,
// an automation AutomationID: the same strings the architecture model,
// component/observe and v1:cluster:nodeType.codeReference use, which is what
// lets the two graphs and the live cluster be joined without a mapping table.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// PlatformSchemaVersion identifies the platform graph's on-disk format.
const PlatformSchemaVersion = "1"

// PlatformGraphFilename is the committed artifact's name, beside
// topology.model.json in component/architecture/embedded.
const PlatformGraphFilename = "platform.graph.json"

// PlatformNodeKind names what a platform-graph node is. Closed: a reader may
// switch on it exhaustively, and ValidatePlatformGraph refuses any other.
type PlatformNodeKind string

const (
	PlatformRole        PlatformNodeKind = "role"        // a node role (ServiceID)
	PlatformPackage     PlatformNodeKind = "package"     // a Go package some role links (PackageID)
	PlatformDeployment  PlatformNodeKind = "deployment"  // a Kubernetes Deployment
	PlatformK8sService  PlatformNodeKind = "k8sService"  // a Kubernetes Service
	PlatformDatabase    PlatformNodeKind = "database"    // the database the nodes connect to
	PlatformService     PlatformNodeKind = "service"     // a gRPC service from the proto descriptors
	PlatformRPC         PlatformNodeKind = "rpc"         // one method of a gRPC service
	PlatformHost        PlatformNodeKind = "host"        // a front-door host, domain as a placeholder
	PlatformPath        PlatformNodeKind = "path"        // a routed path under a host
	PlatformRoutingRule PlatformNodeKind = "routingRule" // a mesh event-routing rule
	PlatformNamespace   PlatformNodeKind = "namespace"   // a DSL namespace
	PlatformConcept     PlatformNodeKind = "concept"     // a DSL concept
	PlatformAutomation  PlatformNodeKind = "automation"  // a DSL automation (AutomationID)
	PlatformOSApp       PlatformNodeKind = "osApp"       // a MemQL OS app
	PlatformOSSection   PlatformNodeKind = "osSection"   // a section of an OS app
)

// PlatformEdgeKind names a platform-graph relationship. Direction is From -> To.
type PlatformEdgeKind string

const (
	PlatformContains   PlatformEdgeKind = "contains"    // parent -> child: service -> rpc, host -> path, namespace -> concept, osApp -> osSection
	PlatformIncludes   PlatformEdgeKind = "includes"    // role -> package its binary links
	PlatformRunsOn     PlatformEdgeKind = "runs_on"     // role -> the Deployment that runs it
	PlatformSelects    PlatformEdgeKind = "selects"     // k8sService -> the Deployment its selector matches
	PlatformDials      PlatformEdgeKind = "dials"       // deployment -> a k8sService its env addresses
	PlatformConnectsTo PlatformEdgeKind = "connects_to" // deployment -> database
	PlatformServes     PlatformEdgeKind = "serves"      // role -> gRPC service its build constructs the server of
	PlatformRoutesTo   PlatformEdgeKind = "routes_to"   // path -> the k8sService the front door sends it to
	PlatformForwardsTo PlatformEdgeKind = "forwards_to" // routingRule -> the role a targeted forward rule addresses
	PlatformRelates    PlatformEdgeKind = "relates"     // concept -> concept, one @relationship
	PlatformTriggers   PlatformEdgeKind = "triggers"    // automation -> automation its writes can fire
	PlatformWrites     PlatformEdgeKind = "writes"      // automation -> concept it writes
)

var platformNodeKinds = map[PlatformNodeKind]bool{
	PlatformRole: true, PlatformPackage: true, PlatformDeployment: true, PlatformK8sService: true,
	PlatformDatabase: true, PlatformService: true, PlatformRPC: true, PlatformHost: true,
	PlatformPath: true, PlatformRoutingRule: true, PlatformNamespace: true, PlatformConcept: true,
	PlatformAutomation: true, PlatformOSApp: true, PlatformOSSection: true,
}

var platformEdgeKinds = map[PlatformEdgeKind]bool{
	PlatformContains: true, PlatformIncludes: true, PlatformRunsOn: true, PlatformSelects: true,
	PlatformDials: true, PlatformConnectsTo: true, PlatformServes: true, PlatformRoutesTo: true,
	PlatformForwardsTo: true, PlatformRelates: true, PlatformTriggers: true, PlatformWrites: true,
}

// PlatformNode is one entity. Label is what a diagram prints; Attrs carries the
// kind-specific facts, all strings so the file has one value type.
type PlatformNode struct {
	ID    ID                `json:"id"`
	Kind  PlatformNodeKind  `json:"kind"`
	Label string            `json:"label"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// PlatformEdge is one directed relationship. (From, To, Kind) may repeat when
// Attrs differ -- two @relationship fields between the same two concepts are
// two facts.
type PlatformEdge struct {
	From  ID                `json:"from"`
	To    ID                `json:"to"`
	Kind  PlatformEdgeKind  `json:"kind"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// PlatformGraph is the whole graph. It carries no timestamp and no path
// outside the repository: it is a function of the tree, byte for byte.
type PlatformGraph struct {
	SchemaVersion string         `json:"schema_version"`
	Nodes         []PlatformNode `json:"nodes"`
	Edges         []PlatformEdge `json:"edges"`
}

// WriteJSON writes g in its one canonical form: nodes sorted by id, edges by
// (from, to, kind, attrs), one element per line. One line per element rather
// than one line for the file (topology.model.json's form, forced by its size)
// because this file is small enough for a diff to be READ, and a reviewer of a
// change to the platform should see which facts moved. It does not mutate g.
func (g *PlatformGraph) WriteJSON(w io.Writer) error {
	nodes := append([]PlatformNode(nil), g.Nodes...)
	edges := append([]PlatformEdge(nil), g.Edges...)
	sort.Slice(nodes, func(i, j int) bool { return platformNodeLess(nodes[i], nodes[j]) })
	sort.Slice(edges, func(i, j int) bool { return platformEdgeLess(edges[i], edges[j]) })

	var b bytes.Buffer
	version, err := compactJSON(g.SchemaVersion)
	if err != nil {
		return err
	}
	b.WriteString(`{"schema_version":`)
	b.Write(version)
	b.WriteString(`,"nodes":[`)
	for i, n := range nodes {
		line, err := compactJSON(n)
		if err != nil {
			return err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("\n")
		b.Write(line)
	}
	b.WriteString("\n],\"edges\":[")
	for i, e := range edges {
		line, err := compactJSON(e)
		if err != nil {
			return err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("\n")
		b.Write(line)
	}
	b.WriteString("\n]}\n")
	_, err = w.Write(b.Bytes())
	return err
}

// compactJSON encodes v without HTML escaping -- host ids carry "<domain>",
// which json.Marshal would spell <domain> -- and without the
// encoder's trailing newline.
func compactJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// IsSorted reports whether g is already in WriteJSON's order, so a test can
// hold the committed artifact to it without a second comparator.
func (g *PlatformGraph) IsSorted() bool {
	return sort.SliceIsSorted(g.Nodes, func(i, j int) bool { return platformNodeLess(g.Nodes[i], g.Nodes[j]) }) &&
		sort.SliceIsSorted(g.Edges, func(i, j int) bool { return platformEdgeLess(g.Edges[i], g.Edges[j]) })
}

func platformNodeLess(a, b PlatformNode) bool {
	switch {
	case a.ID != b.ID:
		return a.ID < b.ID
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	case a.Label != b.Label:
		return a.Label < b.Label
	}
	return attrsKey(a.Attrs) < attrsKey(b.Attrs)
}

func platformEdgeLess(a, b PlatformEdge) bool {
	switch {
	case a.From != b.From:
		return a.From < b.From
	case a.To != b.To:
		return a.To < b.To
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	}
	return attrsKey(a.Attrs) < attrsKey(b.Attrs)
}

// ReadPlatformGraph decodes a platform graph strictly: unknown fields and a
// different schema version are errors, as ReadJSON does for the model.
func ReadPlatformGraph(r io.Reader) (*PlatformGraph, error) {
	var g PlatformGraph
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("decode platform graph: %w", err)
	}
	if g.SchemaVersion != PlatformSchemaVersion {
		return nil, fmt.Errorf("platform graph schema version mismatch: file=%q binary=%q -- regenerate with `make platform-graph`",
			g.SchemaVersion, PlatformSchemaVersion)
	}
	return &g, nil
}

// ValidatePlatformGraph checks what every reader relies on: each node id is
// unique and of a known kind, and each edge is of a known kind between two
// nodes the graph has. cmd/platformgraph refuses to write a graph that fails
// it, and the committed artifact is held to it by a root test.
func ValidatePlatformGraph(g *PlatformGraph) error {
	ids := make(map[ID]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.ID == "" {
			return fmt.Errorf("a %s node has no id", n.Kind)
		}
		if ids[n.ID] {
			return fmt.Errorf("node %s appears twice", n.ID)
		}
		ids[n.ID] = true
		if !platformNodeKinds[n.Kind] {
			return fmt.Errorf("node %s has unknown kind %q", n.ID, n.Kind)
		}
	}
	for _, e := range g.Edges {
		if !platformEdgeKinds[e.Kind] {
			return fmt.Errorf("edge %s -> %s has unknown kind %q", e.From, e.To, e.Kind)
		}
		if !ids[e.From] || !ids[e.To] {
			return fmt.Errorf("edge %s -%s-> %s names a node the graph does not have", e.From, e.Kind, e.To)
		}
	}
	return nil
}
