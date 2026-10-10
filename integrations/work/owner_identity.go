package work

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// Compilation and remedies resolve the current owner through the same engine
// adapter. Their call-local contexts keep the captured grant and never escape
// through an integration method's return value.
func (i *Integration) ownerIdentityResolver() *auth.IdentityResolver {
	return auth.NewIdentityResolver(auth.QueryRunnerFunc(func(ctx context.Context, q string) (any, error) {
		result, err := i.engine.Execute(ctx, q)
		if err != nil || result == nil {
			return nil, err
		}
		return result.OutputPayload(), nil
	}), i.logger)
}
