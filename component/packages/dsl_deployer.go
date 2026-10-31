package packages

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// recordDslDeployer records who staged this package's DSL, which is whose fleet its
// @usesDeployerFleet workflows use (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md, D3).
//
// A RUN THAT MOVED THE DSL NAMES ITS ACTOR. Consent is the act of pressing Deploy, so
// a developer deploying a package they do not own spends their own machines. A run
// that did not move the DSL leaves the person alone, or a website deploy by somebody
// else would quietly change whose machines serve the workflows; it writes only when
// nothing is recorded yet, which is how a source staged before the field gets one.
//
// AN ACTOR WHO IS NO PERSON BLANKS IT when it staged, because the last person did not
// consent to DSL they never saw. A blank refuses at the call.
//
// A warning rather than a failure, as the domain list is.
func (d *Deps) recordDslDeployer(ctx context.Context, packageId, actorUserId, recorded string, staged bool) {
	if !staged && strings.TrimSpace(recorded) != "" {
		return
	}
	who := strings.TrimSpace(actorUserId)
	if auth.NamesNoPerson(who) {
		if !staged {
			return
		}
		who = ""
	}
	if err := d.Store.recordPackageDslDeployer(ctx, packageId, who); err != nil {
		d.log().Warn("packages: could not record who staged this package's DSL; its workflows keep the previous person's fleet",
			"component", "packages.pipeline", "package", packageId, "err", err)
	}
}
