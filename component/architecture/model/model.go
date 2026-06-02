// Package model defines the in-memory and on-disk schema for memQL's
// architecture graph: the intermediate representation that sits between
// static analysis of the Go source and the cockpit's diagram renderer.
//
// The model is intentionally renderer-agnostic. The same Model can be
// drawn natively in the cockpit TUI (cli/canvas), exported to standard
// notations (PlantUML, D2, Mermaid) for docs, or joined against
// observability data by the stable, fully-qualified node IDs.
//
// A Model is a typed graph: Nodes carry the entities (cluster, service,
// package, type, function, ...) and Edges carry the relationships
// between them (imports, implements, embeds, calls, ...). The graph is
// not a tree -- a single Type node, for example, may sit under one
// Package via a "contains" edge while also being the target of many
// "implements" edges from interfaces in unrelated packages.
package model

// SchemaVersion identifies the on-disk format. Bump when the field set
// changes in a backward-incompatible way; the loader checks this before
// unmarshaling so older cockpits don't try to read newer payloads.
const SchemaVersion = "1.0"

// Kind names the entity type stored at a Node. The set is closed so
// that renderers can switch on it exhaustively. Add new kinds with
// care: cockpit's drill-down navigator and the layout engine both
// dispatch on Kind.
type Kind string

const (
	KindCluster   Kind = "cluster"
	KindService   Kind = "service"
	KindPackage   Kind = "package"
	KindType      Kind = "type"      // struct (concrete)
	KindInterface Kind = "interface" // interface type
	KindFunc      Kind = "func"      // top-level function
	KindMethod    Kind = "method"    // method with receiver
	KindField     Kind = "field"     // struct field
)

// EdgeKind names the relationship stored on an Edge. Direction is
// always From -> To; for symmetric relationships (e.g. mutual deps)
// emit two edges. Renderers may filter by EdgeKind to produce a
// specific UML diagram type (e.g. EdgeImports drives the package
// diagram, EdgeCalls drives sequence diagrams).
type EdgeKind string

const (
	// EdgeContains is the structural parent->child relationship:
	// cluster contains service, service contains package, package
	// contains type, type contains method/field. Used by the
	// drill-down navigator to decide what zoom-in lands you on.
	EdgeContains EdgeKind = "contains"

	// EdgeImports is package -> package: a Go import. Edge weight in
	// Attrs["count"] is the number of imported symbols if computed.
	EdgeImports EdgeKind = "imports"

	// EdgeImplements is type -> interface: the concrete type satisfies
	// the interface's method set. Detected via go/types.Implements.
	EdgeImplements EdgeKind = "implements"

	// EdgeEmbeds is type -> type (or type -> interface): Go struct or
	// interface embedding. Distinct from contains so renderers can
	// show inheritance-style arrows in class diagrams.
	EdgeEmbeds EdgeKind = "embeds"

	// EdgeCalls is func/method -> func/method: a static call edge as
	// reported by golang.org/x/tools/go/callgraph. May be approximate
	// for interface dispatch depending on the algorithm used (CHA/RTA/
	// VTA). Attrs["algorithm"] records which.
	EdgeCalls EdgeKind = "calls"

	// EdgeHasField is type -> field. Field nodes carry the field's
	// declared type in Attrs["type"]; renderers use that to draw
	// composition arrows when the field type resolves to another
	// Node in the model.
	EdgeHasField EdgeKind = "has_field"

	// EdgeDependsOn is a service-level edge derived from package
	// imports crossing service boundaries plus any explicit
	// declarations in arch.yaml. Drives the L2 component diagram.
	EdgeDependsOn EdgeKind = "depends_on"

	// EdgeRunsOn places a Service onto a deployment target (Cloud Run
	// service, k8s pod, local container, etc.). Populated by the
	// deployment extractor reading deploy/k8s/ manifests + scripts/deploy/.
	EdgeRunsOn EdgeKind = "runs_on"
)

// ID is a stable, fully-qualified identifier for a Node. Format is
// deterministic per Kind so observability traces and external tooling
// can join against it without consulting the model:
//
//	cluster   "cluster:<name>"
//	service   "service:<name>"
//	package   "pkg:<import-path>"
//	type      "type:<import-path>.<TypeName>"
//	interface "iface:<import-path>.<InterfaceName>"
//	func      "func:<import-path>.<FuncName>"
//	method    "method:<import-path>.(<RecvType>).<MethodName>"
//	field     "field:<import-path>.<TypeName>.<FieldName>"
//
// Renderers should treat the prefix as opaque and use Kind instead;
// the format is documented so external systems can construct IDs
// without dragging in the model package.
type ID string

// SourceRef points back at the file (and optionally line) the entity
// was extracted from. File paths are workspace-relative so the model
// stays portable across machines and CI.
type SourceRef struct {
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
}

// Node is one entity in the graph. Attrs is the escape hatch for
// kind-specific metadata that doesn't deserve a typed field yet
// (e.g. struct tags, exported-ness, gRPC service name); renderers
// must tolerate unknown keys.
type Node struct {
	ID     ID                `json:"id"`
	Kind   Kind              `json:"kind"`
	Name   string            `json:"name"`
	Parent ID                `json:"parent,omitempty"`
	Doc    string            `json:"doc,omitempty"`
	Source *SourceRef        `json:"source,omitempty"`
	Attrs  map[string]string `json:"attrs,omitempty"`
}

// Edge is one directed relationship. Attrs follows the same
// tolerate-unknown rule as Node.Attrs. The pair (From, To, Kind)
// is conceptually a multi-set key -- duplicates are allowed when
// they carry different attributes (e.g. two distinct call sites
// on the same call graph edge).
type Edge struct {
	From  ID                `json:"from"`
	To    ID                `json:"to"`
	Kind  EdgeKind          `json:"kind"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Model is the top-level container. Persisted as JSON
// (topology.model.json) at the workspace root by the extractor and
// loaded by the cockpit at startup.
//
// SchemaVersion is checked before unmarshaling. GeneratedAt is RFC3339.
// Workspace is the absolute path the extractor was run from, used by
// the cockpit only for display ("topology generated from <path>").
type Model struct {
	SchemaVersion string `json:"schema_version"`
	GeneratedAt   string `json:"generated_at"`
	Workspace     string `json:"workspace"`
	Nodes         []Node `json:"nodes"`
	Edges         []Edge `json:"edges"`
}
