package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

const conceptWorkRun = "v1:work:run"

// A source hash is integrity, not authority. Protect the snapshot at the raw
// write boundary too: insert() and authored mutations do not consult the
// createWorkRun mutation's @serverOnly annotation. Once present, even an
// internal heartbeat or update must preserve the admitted definition.
func validateWorkSpineImmutable(ctx context.Context, prior, final map[string]any) error {
	if !payloadFieldChanged(prior, final, "spine") {
		return nil
	}
	if !payloadFieldUnset(prior["spine"]) {
		return fmt.Errorf("%s: spine is immutable; open a new goal to select another workflow", conceptWorkRun)
	}
	if !auth.OriginFromContext(ctx).IsInternal() {
		return fmt.Errorf("%s: spine is admitted only by the native work runtime", conceptWorkRun)
	}
	return nil
}
