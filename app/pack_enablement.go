package app

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// pack_enablement.go is the boot half of per-instance pack enablement
// (module-registry design, docs/superpowers/specs/2026-08-20-module-
// registry-design.md section 4.2). The durable state is one
// v1:platform:packState row per pack in the shared graph -- absence means
// enabled -- and THIS is the one place a node reads it: phase 3, after the
// database has started and become ready, before engine.Init runs a single
// DSL loader. Everything downstream (the loaders via dsl.
// SkipsBehavioralLoad, materializePlugins in phase 4, pack-conditional
// wiring like the harness reconciler) consults the projection taken here,
// so a node cannot half-apply a flip.
//
// The read window is the same in-between window provider auth resolution
// already uses (component/memql engine_variables.go canResolve): the bun
// handle is live, nothing DSL-shaped exists yet. The read is raw SQL
// (memql.ReadPackStates) precisely because no executor exists this early.

// loadPackEnablement reads v1:platform:packState and hands the disabled
// set to the DSL layer + the App. Called from engineAndBus between
// SetDatabaseGetter and engine.Init; fatal on a read the database refuses
// (a node that cannot read shared state must not guess at it), while a
// first boot with no relation yet reads as empty -- see ReadPackStates.
func (a *App) loadPackEnablement() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	states, err := memql.ReadPackStates(ctx, a.db.BunDB())
	if err != nil {
		a.fatal("pack enablement read failed; refusing to boot with unknown pack state",
			"error", err, "component", memql.ComponentName)
		return
	}

	disabled := memql.DisabledPackDomainsFromStates(states)
	a.disabledPackDomains = make(map[string]struct{}, len(disabled))
	for _, d := range disabled {
		a.disabledPackDomains[d] = struct{}{}
	}
	a.engine.SetDisabledPackDomains(disabled)

	if len(disabled) > 0 {
		a.Logger.Info("packs disabled by v1:platform:packState; loading mounted-inert",
			"component", memql.ComponentName,
			"disabledPacks", disabled)
	}
}

// packDisabled reports whether phase 4+ wiring should skip a pack's Go
// half. Only meaningful after loadPackEnablement has run (phase 3).
func (a *App) packDisabled(domain string) bool {
	_, ok := a.disabledPackDomains[domain]
	return ok
}
