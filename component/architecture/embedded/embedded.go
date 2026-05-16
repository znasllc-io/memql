// Package embedded ships the workspace's architecture model baked
// into every binary that imports it. The JSON file is regenerated
// by `go generate` (which invokes memql-arch over the full workspace)
// and embedded via //go:embed so consumers -- chiefly memQL Cockpit --
// see the same view of the system as the source tree at the commit
// they were built from.
//
// Consumers never read topology.model.json from disk; that file
// exists only as the //go:embed source. Doing so keeps the cockpit
// binary self-contained (no $XDG lookup, no install-path search) and
// gives the model the same versioning guarantee as the rest of the
// compiled code: if you check out a commit and build, the embedded
// model matches that commit.
//
// Regeneration:
//
//	cd memql/component/architecture/embedded
//	go generate
//
// or, from the memql repo root:
//
//	go generate ./component/architecture/embedded/...
//
// The generated file is intentionally committed to the repo so
// downstream modules (memql-cockpit) build cleanly without first
// installing memql-arch, and so architectural changes show up in
// code review the same way any other source change does.
package embedded

import (
	_ "embed"
	"bytes"

	"github.com/visionarys-io/memql/component/architecture/model"
)

//go:generate go run ../../../cmd/memql-arch --root ../../../.. --out topology.model.json --calls

//go:embed topology.model.json
var ModelJSON []byte

// Load returns the embedded model, decoded. Each call decodes
// fresh so callers may mutate the returned graph (e.g. overlay
// runtime observability) without affecting other consumers.
//
// Errors from Load indicate the embedded JSON has drifted from the
// schema version this binary was built against -- in practice that
// only happens if someone hand-edits the generated file. The fix is
// always "go generate ./component/architecture/embedded/...".
func Load() (*model.Model, error) {
	return model.ReadJSON(bytes.NewReader(ModelJSON))
}
