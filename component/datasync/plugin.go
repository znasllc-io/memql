package datasync

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the sync runtime as a plug-in.
//
// It registers UNCONDITIONALLY, on every node type, and that is
// deliberate: the inbound dispatcher automation loads on every binary
// (every binary loads every concept), so a node without the executor
// would fail every inbound row from every source -- the same reasoning
// integrations/shopify's own registration records.
//
// The capabilities no-op on a node with no connector bound, which is the
// common case. What they must not do is be ABSENT.
func init() {
	memql.RegisterPlugin("datasync", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(engineAdapter{pctx.Engine}, pctx.Logger), nil
	})
}

// engineAdapter widens the engine's concrete Execute to the runtime's
// one-method seam.
//
// The seam returns `any` rather than *memql.ExecuteResult so this
// package's tests can fake it with flat row envelopes -- the campaigns
// precedent, and the reason component/campaigns' Engine has the same
// signature. memql.MaterializeRows accepts both shapes, so the widening
// costs nothing at the far end.
type engineAdapter struct{ inner memql.IntegrationEngineAccess }

func (a engineAdapter) Execute(ctx context.Context, query string) (any, error) {
	if a.inner == nil {
		return nil, nil
	}
	return a.inner.Execute(ctx, query)
}
