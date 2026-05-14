//go:build bff || voice || agent || planner || identity

package app

import (
	"github.com/visionarys-io/memql/component/server/polyphonws"
)

// wirePolyphonPrewarm is a no-op on binaries without cognition. The
// /polyphon/preload endpoint still registers (the route lives on the
// shared polyphonws handler) but returns 204 No Content because no
// prewarm callback is wired -- the bridge's preload POST is a soft
// best-effort signal, harmless if dropped.
func (a *App) wirePolyphonPrewarm(_ *polyphonws.Handler) {}
