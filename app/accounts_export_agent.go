//go:build agent

package app

import (
	"github.com/znasllc-io/memql/integrations/accounts"
)

// accounts_export_agent.go -- the agent node RUNS account exports (account
// export, design record docs/superpowers/specs/2026-11-12-account-export-design.md,
// section H: "The runner that works exports is wired on agent nodes only").
//
// The calls are the accounts plug-in's and are served by every node type
// (integrations/accounts/plugin.go); a person prepares and starts an export
// from whichever node serves their stream, and the record it writes is in the
// database. The runner -- the sweep of stale drafts, the take-over of a run
// whose node stopped, the claim and the supervision of one run -- lives here,
// because an export holds a read snapshot open and streams for up to an hour,
// which is the agent node's kind of work and not the bff's. Every agent replica
// runs one runner; which of them runs an export is decided in the database
// (integrations/accounts/export_runner.go), one at a time on the installation.
//
// Registered as a dependency, so it starts after the engine and stops before
// it; a stop hands a running attempt back to the queue for another replica.
// The plug-in installs the work function (Exports.SetExportWork) when it
// materializes; a runner over an Exports with none sweeps and takes over but
// claims nothing, and says so once.
func (a *App) wireAccountExportRunner() {
	if a.engine == nil {
		return
	}
	provider, _ := a.engine.IntegrationByName(accounts.IntegrationName).(*accounts.Provider)
	if provider == nil || provider.Exports == nil {
		a.Logger.Warn("account export runner not wired: the accounts plug-in did not materialize on this agent node, so no export is run here",
			"component", "accounts")
		return
	}
	runner, err := accounts.NewExportRunner(provider.Exports, a.startupNodeID)
	if err != nil {
		a.fatal("account export runner could not be built", "error", err, "component", "accounts")
	}
	a.Dependencies = append(a.Dependencies, runner)
	a.Logger.Info("account export runner wired: this node claims and runs account exports, one at a time on the installation",
		"component", "accounts")
}
