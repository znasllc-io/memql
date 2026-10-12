package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// bundleVerdictStatuses are the statuses that claim a gate passed: Gate 1's
// validated and Gate 2's dryRunPassed.
var bundleVerdictStatuses = map[BundleStatus]bool{
	BundleValidated:    true,
	BundleDryRunPassed: true,
}

// bundleVerdictReports are the fields that carry a gate's verdict: the
// structured report each gate records.
var bundleVerdictReports = []string{"validationReport", "dryRunReport"}

// validateBundleVerdictServerOnly refuses a write that records a gate's verdict
// on a v1:authoring:bundle without internal origin (memql#6022).
//
// A verdict is the claim that a gate ran. A work template runs from a bundle
// only once it reads validated, and a bundle activates only from dryRunPassed,
// so the verdicts are recorded by the server pipelines that run the gates,
// through recordBundleValidation and recordBundleDryRun under internal origin.
// Those are @serverOnly, but a raw insert(...) or update(...) never consults
// that (the memql#5623 pattern, as for activation and creation), so the rule
// is held here too.
//
// KEYED ON THE CLAIM, not on the row: a write that moves a bundle INTO a
// passing status, or that changes a gate's report, needs the server. A write
// that keeps the verdicts as they are (the read-merge carries them through any
// other write) and one that moves a bundle out of a passing status -- a
// retire, the breaker's pause, a failure -- are left alone. prior is the
// stored row, nil on a create (which the create guard already holds).
func validateBundleVerdictServerOnly(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if status := bundleStatusOf(final); bundleVerdictStatuses[BundleStatus(status)] && bundleStatusOf(prior) != status {
		return errBundleVerdict("status")
	}
	for _, field := range bundleVerdictReports {
		if payloadFieldChanged(prior, final, field) {
			return errBundleVerdict(field)
		}
	}
	return nil
}

func errBundleVerdict(field string) error {
	return fmt.Errorf(
		"%s: write to `%s` refused -- a gate verdict is recorded only by the server pipelines that run the gate, "+
			"under internal origin, because a work template runs from a validated bundle and a bundle activates "+
			"from dryRunPassed. A raw insert() or update() never consults recordBundleValidation's or "+
			"recordBundleDryRun's @serverOnly. See component/memql/authoring_bundle_verdict_write_guard.go and memql#6022.",
		conceptAuthoringBundle, field)
}
