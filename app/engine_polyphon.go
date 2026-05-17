//go:build !agent && !planner

package app

import "github.com/znasllc-io/memql/component/polyphon"

// initPolyphonScoreEngine creates the Polyphon cognition for turn-taking
// decisions. Only compiled for cognition and standalone builds.
func (a *App) initPolyphonScoreEngine() {
	a.polyphonScoreEngine = polyphon.NewScoreEngine(a.Logger)
	a.Logger.Info("polyphon score engine initialized for turn-taking decisions")
}
