package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// validateBundleActivationServerOnly refuses a write that makes a
// v1:authoring:bundle read active without internal origin (memql#6022).
//
// Every replica arms every bundle that reads active: the boot re-arm, each
// reconcile pass and the arm broadcast all follow the bundle row. So `active`
// has to mean the gated activation ran -- ActivateApprovedBundle, after the
// gates, or the durable promote -- and both write it through
// activateAuthoringBundle under internal origin. That mutation is @serverOnly,
// but a raw insert(...) or update(...) never consults a mutation's
// @serverOnly (the memql#5623 pattern, as for the construct ladder), and the
// bundle concept declares no row-authz tier: probed against a real database, a
// writer flipped their own draft bundle to active, and ANOTHER writer flipped a
// retired bundle back to active, after which a reconcile pass armed its
// automation as its owner.
//
// KEYED ON THE TRANSITION, not on the field: a write that keeps an active
// bundle active (the read-merge carries status through any other write) and a
// write that moves one out of active (a retire, the breaker's pause) are left
// alone. Only becoming active -- from any other status, or a create born
// active -- needs the server. prior is the stored row, nil on a create.
func validateBundleActivationServerOnly(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if bundleStatusOf(final) != string(BundleActive) || bundleStatusOf(prior) == string(BundleActive) {
		return nil
	}
	return fmt.Errorf(
		"%s: write to `status` refused -- a bundle becomes active only through the gated activation "+
			"(ActivateApprovedBundle, or the durable promote) under internal origin, because every replica "+
			"arms every bundle that reads active. A raw insert() or update() never consults "+
			"activateAuthoringBundle's @serverOnly. See component/memql/authoring_bundle_activation_write_guard.go "+
			"and memql#6022.",
		conceptAuthoringBundle,
	)
}

// conceptAuthoringBundle is the bundle concept's canonical id.
const conceptAuthoringBundle = "v1:authoring:bundle"

func bundleStatusOf(row map[string]any) string {
	s, _ := row["status"].(string)
	return s
}
