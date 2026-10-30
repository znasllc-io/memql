package siteaccess

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
)

// plugin.go -- self-registration.
//
// REGISTERED ON EVERY NODE TYPE, for integrations/sitepreview's reason exactly:
// the six builtins are declared in dsl/platform/builtins.memql, which EVERY
// binary loads, and a capability present in the DSL and absent from the
// registry is a boot-time resolution failure. Gating the registration by build
// tag would make whether an operator can mint a code depend on which replica
// their browser's stream landed on -- a coin flip at two replicas, which is the
// class of bug memql#4352 closed for workers.
//
// It is a COMPONENT in the module taxonomy and not an integration, and the test
// module_taxonomy_test.go states is what decides it: it calls nobody's API. It
// reads and writes this cluster's own graph and nothing else.

func init() {
	memql.RegisterPlugin("siteAccess", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(engineAdapter{pctx.Engine}, pctx.Logger), nil
	})
}

// engineAdapter narrows IntegrationEngineAccess to the one method this package
// uses. Narrow deliberately: nothing here should be able to reach the AI
// provider registry or the tool surface.
type engineAdapter struct{ engine memql.IntegrationEngineAccess }

func (a engineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.engine.Execute(ctx, query)
}
