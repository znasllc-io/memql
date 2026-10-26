// Package embedded retains the committed platform graph. The source topology
// is generated on demand by make arch-model; it is not a runtime embed.
package embedded

import (
	"bytes"
	_ "embed"

	"github.com/znasllc-io/memql/component/architecture/model"
)

//go:generate sh -c "cd ../../.. && make platform-graph"

//go:embed platform.graph.json
var PlatformGraphJSON []byte

// LoadPlatformGraph returns the embedded platform graph, decoded fresh.
func LoadPlatformGraph() (*model.PlatformGraph, error) {
	return model.ReadPlatformGraph(bytes.NewReader(PlatformGraphJSON))
}
