package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// Row ownership cannot mint a delegated agent scope or revive its lifecycle.
// Judge the final payload after read-merge and hooks, including raw writes.
func validateAppSessionScopeServerOnly(ctx context.Context, prior, final map[string]any) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	for _, field := range []string{"toolScope", "ownerUserId", "runId", "stepId", "workerId", "sessionRunId", "status"} {
		if payloadFieldChanged(prior, final, field) {
			return fmt.Errorf("app session field %q is server-managed", field)
		}
	}
	return nil
}
