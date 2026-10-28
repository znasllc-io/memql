package memql

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// ContextWithFreshAuthorization starts a new authorization evaluation after a
// long-running operation. It changes no actor, token, deadline or native fence.
// The next ordinary engine entry installs its own empty authorization memos;
// all reads bypass the result cache. Previously resolved request-local answers
// must not outlive a grant, membership or role revocation on another replica.
func ContextWithFreshAuthorization(ctx context.Context) context.Context {
	return ContextWithFreshRead(auth.ContextWithFreshGrantMemo(freshAuthorizationContext{ctx}))
}

type freshAuthorizationContext struct{ context.Context }

func (c freshAuthorizationContext) Value(key any) any {
	switch key.(type) {
	case accountScopeMemoKey, rankScopeMemoKey, personRoleMemoKey:
		return nil
	default:
		return c.Context.Value(key)
	}
}
