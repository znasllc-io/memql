// Package embedded ships two generated graphs baked into every binary that
// imports it, so a reader sees the same view of the system as the source tree
// at the commit it was built from:
//
//   - topology.model.json, the architecture model: services, packages, types,
//     methods and fields, their imports, embeddings and implements edges, and
//     the DSL automation trigger graph. STRUCTURAL ONLY: the call graph is not
//     committed (memql#5727, owner decision 7); `memql-arch --calls` builds it
//     on demand, and the drift gate does so to check it.
//   - platform.graph.json, the platform graph (model/platform.go): the node
//     roles and what each binary links, the Deployments and Services that run
//     them, the gRPC services, the front door, the mesh routing table, the
//     concepts and their relationships, the automations and the OS navigation.
//     The docs diagrams and the OS's Cluster map read it.
//
// The ids are shared -- a role is service:<type> in both, a package pkg:<path>
// -- and they are the join key component/observe and
// v1:cluster:nodeType.codeReference use too.
//
// Consumers read the embedded bytes, never the files on disk: the files exist
// only as //go:embed sources, which keeps a binary self-contained and gives
// the graphs the versioning guarantee of compiled code.
//
// Regeneration goes through make, and only through make -- `make arch-model`
// and `make platform-graph` are the one place each flag set lives (memql#2844).
// Both files are committed so that downstream modules build without first
// running a generator, and so that a change to the system shows up in review.
package embedded

import (
	"bytes"
	_ "embed"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// The model root is THIS repo (engine-only): the engine binary must not
// embed sibling product repos' topology (engine/product decoupling,
// memql#2428). A product pack contributes its own model via its own arch
// pass -- see memql#2432.
// Regeneration goes through `make arch-model`, which is the ONE place the flag
// set lives (memql#2844). This directive used to carry its own copy, missing
// --reproducible and --cluster, so `make generate` silently reintroduced the
// wall clock, the absolute workspace path and the folder-derived cluster name
// that the gate exists to prevent -- and then the gate red.
//go:generate sh -c "cd ../../.. && make arch-model platform-graph"

//go:embed topology.model.json
var ModelJSON []byte

//go:embed platform.graph.json
var PlatformGraphJSON []byte

// Load returns the embedded model, decoded. Each call decodes
// fresh so callers may mutate the returned graph (e.g. overlay
// runtime observability) without affecting other consumers.
//
// Errors from Load indicate the embedded JSON has drifted from the
// schema version this binary was built against -- in practice that
// only happens if someone hand-edits the generated file. The fix is
// always `make arch-model`.
func Load() (*model.Model, error) {
	return model.ReadJSON(bytes.NewReader(ModelJSON))
}

// LoadPlatformGraph returns the embedded platform graph, decoded fresh on
// each call. An error means the file was hand-edited or written by a
// different schema version; the fix is `make platform-graph`.
func LoadPlatformGraph() (*model.PlatformGraph, error) {
	return model.ReadPlatformGraph(bytes.NewReader(PlatformGraphJSON))
}
