package packages

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// recordDslDeployer records who is staging this package's DSL, which is whose fleet its
// @usesDeployerFleet workflows use (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md, D3).
//
// CONSENT IS THE ACT OF PRESSING DEPLOY, so a developer deploying a package they do not
// own spends their own machines. An actor who is no person blanks the record, because
// the last person did not consent to DSL they never saw, and a blank refuses at the call.
func (d *Deps) recordDslDeployer(ctx context.Context, packageId, actorUserId string) error {
	who := strings.TrimSpace(actorUserId)
	if auth.NamesNoPerson(who) {
		who = ""
	}
	return d.Store.recordPackageDslDeployer(ctx, packageId, who)
}

// recordDslDeployerBeforeStaging is recordDslDeployer at the one moment it must not be
// missed: BEFORE THE POINTER MOVES. Afterwards a restart for any reason runs the new DSL,
// and a run that fails in the roll returns before anything later is reached, while its
// retry finds the pointer already moved and staged nothing.
//
// A package that declares no fleet workflow only warns, as the domain list does: a row
// that refuses bookkeeping must not stop its sites deploying. One that does stops here,
// with the pointer unmoved, because going on would run its new workflows on the previous
// person's machines.
func (d *Deps) recordDslDeployerBeforeStaging(ctx context.Context, packageId, actorUserId string, fleetWorkflows bool) error {
	err := d.recordDslDeployer(ctx, packageId, actorUserId)
	if err == nil {
		return nil
	}
	if fleetWorkflows {
		return fmt.Errorf("could not record who is staging this package's DSL, and it declares workflows that run on that person's machines, so nothing was staged: %w", err)
	}
	d.log().Warn("packages: could not record who is staging this package's DSL",
		"component", "packages.pipeline", "package", packageId, "err", err)
	return nil
}

// fillDslDeployer gives a package staged before the field existed its person, on a run
// that did not move the DSL. A run that did recorded one already, and one that did not
// must not move it, or a website deploy by somebody else would quietly change whose
// machines serve the workflows. A failure stays blank, which refuses at the call.
func (d *Deps) fillDslDeployer(ctx context.Context, packageId, actorUserId, recorded string) {
	if strings.TrimSpace(recorded) != "" || auth.NamesNoPerson(strings.TrimSpace(actorUserId)) {
		return
	}
	if err := d.recordDslDeployer(ctx, packageId, actorUserId); err != nil {
		d.log().Warn("packages: could not record who staged this package's DSL",
			"component", "packages.pipeline", "package", packageId, "err", err)
	}
}

// declaresFleetWorkflows reports whether a report names any workflow that will use the
// deployer's machines.
func declaresFleetWorkflows(rep *Report) bool {
	if rep == nil {
		return false
	}
	for _, domain := range rep.DslDomains {
		if len(domain.FleetWorkflows) > 0 {
			return true
		}
	}
	return false
}
