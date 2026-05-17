package liveknowledge

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
)

// engineAdapter wraps the PluginContext.Engine (a narrow
// IntegrationEngineAccess) into the package's own EngineAccess
// interface. Only need Execute -- liveknowledge dispatch is read-only.
type engineAdapter struct {
	exec memql.IntegrationEngineAccess
}

// Execute satisfies lk.EngineAccess. Forwards to the engine's Execute.
func (a *engineAdapter) Execute(ctx context.Context, query string) (any, error) {
	if a == nil || a.exec == nil {
		return nil, nil
	}
	return a.exec.Execute(ctx, query)
}
