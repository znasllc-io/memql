package wholesalepack

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
)

// origin.go -- WHICH BUILTINS ONLY THE STORE'S AUTOMATION MAY CALL.
//
// A builtin takes no @serverOnly, so one an automation alone may call says so
// in its handler, before it reads anything: a trusted automation calls with
// internal origin, and every other caller is a client. reviewspack's
// notifyModerator and sendDueRequests draw the same line the same way.

// requireInternal refuses a call that did not arrive with internal origin.
func requireInternal(ctx context.Context, construct string) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	return fmt.Errorf("wholesale: %s is called by the store's automation, not by a client", construct)
}
