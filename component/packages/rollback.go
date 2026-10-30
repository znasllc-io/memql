package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RollbackRequest restores a package to what an earlier deployment left live.
type RollbackRequest struct {
	PackageId    string
	DeploymentId string
	Actor        Actor
}

// RestoredState is what a rollback put back.
type RestoredState struct {
	DeploymentId string              `json:"deploymentId"`
	DslVersion   string              `json:"dslVersion,omitempty"`
	Deployables  []DeployableOutcome `json:"deployables"`
	Rolled       bool                `json:"rolled"`
}

// Rollback executes the D6 order REVERSED: publish back FIRST, then the
// pointer back and a roll.
//
// The reversal is the entire point and it is not symmetry for its own sake.
// Forward, the schema must arrive before the app that uses it -- so stage and
// roll come before publish. Backward, the app must stop using the schema
// before the schema goes away -- so the publish comes back first. Doing it in
// the forward order on the way back would leave, for the width of the rollout,
// new application code talking to old DSL: the one window D6 exists to close.
//
// A rollback restores a TUPLE (dslVersion + each site's bundleRef), not a
// sequence of events. The prior deployment row already records exactly that
// tuple, which is why the timeline being append-only is what makes rollback
// possible at all.
func Rollback(ctx context.Context, d *Deps, req RollbackRequest) (*RestoredState, error) {
	prior, err := d.Store.deploymentById(ctx, req.DeploymentId)
	if err != nil {
		return nil, err
	}
	if prior == nil {
		return nil, refuse(CodeSourceUnreadable,
			"no deployment %q is readable by this caller", req.DeploymentId)
	}
	if !sameShortId(rowString(prior, "packageId"), req.PackageId) {
		return nil, refuse(CodeSourceUnreadable,
			"deployment %q belongs to a different package", req.DeploymentId)
	}
	if got := rowString(prior, "status"); got != StatusSucceeded {
		return nil, refuse(CodeSourceUnreadable,
			"deployment %q finished as %q rather than succeeded, so there is no state on it to restore. Roll back to a deployment that was live.",
			req.DeploymentId, got)
	}

	outcomes, err := deployableOutcomes(prior)
	if err != nil {
		return nil, err
	}
	dslVersion := rowString(prior, "dslVersion")

	// The D9 gate applies to a rollback for the same reason it applies to a
	// deploy: putting a DSL version back is changing what the cluster can do.
	if dslVersion != "" && !req.Actor.MayDeployDsl {
		return nil, refuse(CodeDslRequiresAuthoring,
			"this deployment carried MemQL DSL, so rolling back to it changes what the whole cluster can do -- which is owner or developer only.")
	}

	// A deployment that carried DSL cannot be put back without a surface to
	// stage it on, and a rollback that restored the sites and not the DSL would
	// leave the two apart -- and, worse for what follows, would have no DSL of the
	// earlier deployment's to describe. So it refuses exactly as stageAndRoll does
	// when it has no stager, and BEFORE anything moves, as the gate above does.
	if dslVersion != "" && d.Stager == nil {
		return nil, refuse(CodeSourceUnreadable,
			"this deployment carried MemQL DSL and this cluster has no DSL staging surface configured, so its DSL cannot be put back. Nothing was changed; roll back from a node that has one.")
	}

	restored := &RestoredState{DeploymentId: req.DeploymentId, DslVersion: dslVersion}

	// ---- publish back FIRST ----
	for _, o := range outcomes {
		if o.SiteId == "" || o.BundleRef == "" {
			continue
		}
		if err := d.Publisher.RepointSite(ctx, o.SiteId, o.BundleRef); err != nil {
			return restored, fmt.Errorf("packages: repointing %s: %w", o.SiteId, err)
		}
		restored.Deployables = append(restored.Deployables, o)
	}

	// ---- then the pointer back, and roll ----
	//
	// dslLive says the earlier deployment's DSL is running when this block ends:
	// put back and rolled just now, or found to be the active set already. It is
	// what lets the list below be written, and it stays false for a deployment
	// that recorded no DSL (nothing to put back; the rollback leaves the DSL
	// alone) and for a recorded version that names no domain (nothing restored).
	dslLive := false
	if dslVersion != "" {
		current, rerr := d.Stager.ReadActiveSet(ctx)
		if rerr != nil {
			return restored, rerr
		}
		next := map[string]string{}
		for k, v := range current {
			next[k] = v
		}
		changed := false
		named := 0
		for _, prefix := range strings.Fields(dslVersion) {
			domain, ok := domainFromPrefix(prefix)
			if !ok {
				continue
			}
			named++
			if next[domain] != prefix {
				next[domain] = prefix
				changed = true
			}
		}
		if changed {
			if err := d.Stager.WriteActiveSet(ctx, next); err != nil {
				return restored, err
			}
			if d.Roller == nil {
				return restored, refuse(CodeSourceUnreadable,
					"the pointer was moved back but this node has no rollout surface, so nothing is serving the restored DSL yet. Roll again from a node that has one.")
			}
			if err := d.Roller.Roll(ctx, "package rollback "+req.DeploymentId); err != nil {
				return restored, err
			}
			restored.Rolled = true
		}
		dslLive = named > 0
	}

	// ---- and the list of domains goes back with the DSL ----
	//
	// REWRITTEN FROM THE DEPLOYMENT IT RETURNS TO (design record 2026-10-29, D5),
	// from the report that deployment recorded, and ONLY WHEN ITS DSL IS LIVE:
	// restored just now, or found already active. The list is the bff's reading
	// of which domains this package's running DSL belongs to, so it is never
	// written for DSL that is not running -- the order a deploy keeps, for the
	// same reason, and the gap between the DSL arriving and the list arriving
	// fails closed. A rollback that did not complete returned above and wrote
	// nothing.
	//
	// A deployment that recorded no DSL has none to put back, so the rollback
	// leaves the DSL as it is and the list with it: writing that deployment's own
	// list over DSL a later deployment added would describe DSL that is not what
	// is live.
	//
	// A deployment recorded before the list existed states no namespaces, so
	// there is nothing to restore and the honest list is empty: its gates stay
	// closed until the next deploy writes one. A warning rather than a failure,
	// for the reason the deploy's write is.
	if !dslLive {
		d.log().Info("packages: rolled back; the earlier deployment's DSL was not part of what was restored, so the record of which DSL domains this package ships is left as it was",
			"component", "packages.rollback", "package", req.PackageId, "deployment", req.DeploymentId, "dslVersion", dslVersion)
		return restored, nil
	}
	if derr := d.Store.recordPackageDslDomains(ctx, req.PackageId, dslDomainsFrom(reportFromRow(prior))); derr != nil {
		d.log().Warn("packages: rolled back, but could not record which DSL domains this package ships; its access gates stay as the previous list left them",
			"component", "packages.rollback", "package", req.PackageId, "deployment", req.DeploymentId, "err", derr)
	}

	return restored, nil
}

// domainFromPrefix reads the domain out of packages/<domain>/<hash>/.
func domainFromPrefix(prefix string) (string, bool) {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) < 2 || parts[0] != "packages" {
		return "", false
	}
	return parts[1], true
}

// deployableOutcomes reads the per-deployable outcomes back off a row.
func deployableOutcomes(row map[string]any) ([]DeployableOutcome, error) {
	raw, ok := row["deployables"]
	if !ok || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out []DeployableOutcome
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, fmt.Errorf("packages: this deployment's recorded outcomes are not readable: %w", err)
	}
	return out, nil
}
