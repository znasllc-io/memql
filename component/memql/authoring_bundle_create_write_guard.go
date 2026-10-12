package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// validateBundleCreateServerOnly refuses a write that creates a
// v1:authoring:bundle without internal origin (memql#6022).
//
// An email or routing rule's bundle id is derived from its owner and the rule,
// so the id a person's next activation writes is known before it is written.
// While anyone could create a bundle, a second person could create one at that
// id first: the row was theirs from then on, and the owner's activation --
// writing to a row it did not own -- was refused. So a bundle comes to exist
// only as a server pipeline's write: the rule activations, the planner's
// capture and work compile, the procedure learner and the durable promote,
// each of which derives the id for its own owner or mints a fresh one.
// createAuthoringBundle is @serverOnly, but a raw insert(...) never consults
// that (the memql#5623 pattern, as for the construct and activation guards),
// so the rule is held here too.
//
// KEYED ON CREATION, not on the fields: a write to a bundle that already
// exists -- its gates' verdicts, its activation, a retire, a re-arm that
// writes its own row again -- is the other guards' and row authz's to judge.
// prior is the stored row, nil on a create.
func validateBundleCreateServerOnly(ctx context.Context, prior map[string]any) error {
	if prior != nil || auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	return fmt.Errorf(
		"%s: creating a bundle refused -- a bundle comes to exist only as a server pipeline's write "+
			"under internal origin, because a rule's next bundle id is known before it is written and a "+
			"bundle created there first would block its owner. A raw insert() never consults "+
			"createAuthoringBundle's @serverOnly. See component/memql/authoring_bundle_create_write_guard.go "+
			"and memql#6022.",
		conceptAuthoringBundle,
	)
}
