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
	if got := rowString(prior, "packageId"); got != req.PackageId {
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
	if dslVersion != "" && !req.Actor.IsClusterOwner {
		return nil, refuse(CodeDslRequiresClusterOwner,
			"this deployment carried MemQL DSL, so rolling back to it changes what the whole cluster can do -- which is reserved to a cluster owner.")
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
	if dslVersion != "" && d.Stager != nil {
		current, rerr := d.Stager.ReadActiveSet(ctx)
		if rerr != nil {
			return restored, rerr
		}
		next := map[string]string{}
		for k, v := range current {
			next[k] = v
		}
		changed := false
		for _, prefix := range strings.Fields(dslVersion) {
			domain, ok := domainFromPrefix(prefix)
			if !ok {
				continue
			}
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
