package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// conceptPlatformPackageDeployment is v1:platform:packageDeployment's canonical
// concept id, mirroring the concept declared at dsl/platform/concepts.memql.
// Named here beside conceptPlatformSite so the append-only guard can branch on
// it without inlining the literal.
const conceptPlatformPackageDeployment = "v1:platform:packageDeployment"

// Site lifecycle statuses. Mirrors the enum on v1:platform:site.
const (
	siteStatusDraft    = "draft"
	siteStatusLive     = "live"
	siteStatusDisabled = "disabled"
	siteStatusArchived = "archived"
)

// validateSiteStatusTransition is the D10 lifecycle law (epic memql#4794), and
// the reason it is Go rather than DSL is the reason every guard in this
// package is: it judges a TRANSITION, and a mutation body can only see the
// destination.
//
// The law is `draft -> live <-> disabled -> archived`, with archived leaving
// only back to disabled. Two rules follow from that shape and neither is
// expressible in a filter:
//
//   - ARCHIVE REQUIRES DISABLED FIRST. Archiving is the end of a lifecycle,
//     not a shortcut past pausing -- and the pause is what gives a person
//     serving traffic a chance to notice before the site stops answering.
//   - ARCHIVED DOES NOT RESURRECT TO LIVE. Coming back lands on disabled, so
//     re-publishing is a separate, visible decision rather than a side effect
//     of undoing a filing.
//
// # systemOwned rows are exempt from the whole axis
//
// This is the D10 gap section A of the design names: memql#3717 gave
// `systemOwned` a DELETE guard and left status with none, so the platform's own
// portal and OS sites -- the rows re-seeded at every boot, and the ones an
// operator needs in order to manage the cluster at all -- could be disabled or
// archived by anybody who could reach the mutation. A disabled portal is a
// cluster whose console answers 503 until somebody finds a database.
//
// It reads priorSystemOwned, the flag captured from the PRIOR row in
// executeWrite's read-merge, for exactly the reason the delete guard does: a
// raw write smuggling `systemOwned: false` into the same delta as
// `status: "archived"` must not slip past a check that only looked at the
// post-merge value. That same-delta bypass has its own test here, as it does
// there.
//
// # Why a system actor is exempt
//
// Symmetric with the delete guard, and load-bearing for the same reason: the
// SeedMaterializer re-writes the portal and OS site rows on every boot, and
// that write carries `status: "live"`. Refusing it would make a cluster whose
// portal had somehow been disabled unable to re-seed itself back to health --
// the one path that is supposed to always work.
func (e *MemQLEngine) validateSiteStatusTransition(
	ctx context.Context,
	payload map[string]any,
	priorExisted bool,
	priorStatus string,
	priorSystemOwned bool,
	actor string,
) error {
	if payload == nil {
		return nil
	}
	next := strings.TrimSpace(stringFromAny(payload["status"]))
	if next == "" {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	systemActor := isSystemActor(identity, actor)

	// A creation chooses a starting status rather than making a transition.
	// `archived` is refused as a starting point anyway: a site that has never
	// served cannot be retired, and allowing it would give the archive a
	// second, ceremony-free entrance.
	if !priorExisted {
		if next == siteStatusArchived && !systemActor {
			return fmt.Errorf(
				"v1:platform:site: a site cannot be created already archived -- the archive is where a site goes after it has been live and then disabled. Create it, then disable it, then archive it")
		}
		return nil
	}

	prior := strings.TrimSpace(priorStatus)
	if prior == next {
		// A re-write of the same status is not a transition. Publishes,
		// renames and every other partial update inherit `status` through the
		// read-merge, so treating those as transitions would refuse ordinary
		// writes against a disabled or archived row.
		return nil
	}

	if priorSystemOwned && !systemActor {
		hostname := strings.TrimSpace(stringFromAny(payload["hostname"]))
		return fmt.Errorf(
			"v1:platform:site: %q is systemOwned and its status cannot be changed -- it is the surface this cluster is managed through, it is re-seeded live at every boot, and pausing or archiving it would leave nobody a way in until the next restart. See dsl/platform/concepts.memql:site.systemOwned.",
			hostname,
		)
	}

	if systemActor {
		return nil
	}

	switch {
	case next == siteStatusArchived && prior != siteStatusDisabled:
		return fmt.Errorf(
			"v1:platform:site: this site is %q and can only be archived from %q. Disable it first -- archiving is the end of a site's life, and pausing it is the step that gives anyone still using it a chance to notice.",
			prior, siteStatusDisabled)
	case prior == siteStatusArchived && next != siteStatusDisabled:
		return fmt.Errorf(
			"v1:platform:site: this site is archived. Restoring it brings it back to %q, not straight to %q -- publishing it again is its own decision.",
			siteStatusDisabled, next)
	}
	return nil
}

// Terminal packageDeployment statuses (design section C).
//
// `abandoned` joined them with epic memql#4900, and joining THIS map is the
// whole of what makes it terminal: the sweep closes a stranded row with it, and
// from that moment the row accepts no further writes -- including from the node
// that was running it, if it comes back. That is the case worth stating,
// because it is the one that happens: a partitioned node reconnects, resumes
// its pipeline, and tries to advance a row the cluster has already closed. It
// is refused, and the person sees one honest record of a run that was lost
// rather than a row that changed its mind.
var packageDeploymentTerminal = map[string]struct{}{
	"succeeded": {},
	"refused":   {},
	"failed":    {},
	"abandoned": {},
}

// validatePackageDeploymentAppendOnly enforces D7's append-only rule: a
// deployment row at a terminal status accepts no further writes, and a retry is
// a new row.
//
// It reads the PRIOR status rather than the merged payload for the same reason
// every guard beside it does -- a write that also rewrote `status` would
// otherwise be judged against the value it was trying to install.
//
// There is deliberately no system-actor exemption. The pipeline IS the system
// actor here, and it is precisely the writer this rule exists to bind: the
// timeline is only evidence if the thing that produced it cannot go back and
// edit what it recorded. A correction is a new row, which is also what a person
// reading the timeline should see.
func validatePackageDeploymentAppendOnly(priorExisted bool, priorStatus string) error {
	if !priorExisted {
		return nil
	}
	if _, terminal := packageDeploymentTerminal[strings.TrimSpace(priorStatus)]; !terminal {
		return nil
	}
	return fmt.Errorf(
		"v1:platform:packageDeployment: this attempt already finished as %q and the timeline is append-only -- a deployment row records what happened, so a retry opens a new row rather than rewriting this one",
		strings.TrimSpace(priorStatus))
}
