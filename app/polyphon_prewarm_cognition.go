//go:build cognition || (!bff && !voice && !agent && !planner && !identity)

package app

import (
	"github.com/visionarys-io/memql/component/server/polyphonws"
	"github.com/visionarys-io/memql/integrations/cognition"
)

// wirePolyphonPrewarm connects the polyphon prewarm HTTP endpoint to
// the cognition integration's per-space cache warmer. Cognition-bearing
// binaries (cognition + default single-binary builds) include this
// variant; bff / voice / agent / planner / identity get the no-op
// stub in polyphon_prewarm_stub.go.
//
// The build-tag split keeps cognition out of the bff / voice / etc.
// binaries' dependency tree -- they don't need the cache loaders or
// the embedding registry that cognition pulls in.
func (a *App) wirePolyphonPrewarm(handler *polyphonws.Handler) {
	if a.cognitionIntegration == nil {
		return
	}
	integ, ok := a.cognitionIntegration.(*cognition.CognitionIntegration)
	if !ok || integ == nil {
		return
	}
	handler.SetPrewarmFunc(integ.PrewarmSpaceContext)
	a.Logger.Info("polyphon: prewarm endpoint wired to cognition")
}
