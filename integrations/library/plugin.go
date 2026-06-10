package library

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the library edit integration as a core plug-in.
// Always on: the document-version edit + restore capabilities back both
// the user-facing edit path (BFF) and the assistant editDocument tool
// (agent node), so they must be present on every node-type binary. The
// plug-in needs the engine handle to re-enter Execute for the read-then-
// append dance, so the factory plucks it off PluginContext.
//
// Anchored from app/plugins_core.go via a blank import so the init()
// runs at process start.
func init() {
	memql.RegisterPlugin("library", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Engine), nil
	})
}
