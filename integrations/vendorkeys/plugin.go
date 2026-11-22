package vendorkeys

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// plugin.go -- self-registration.
//
// REGISTERED ON EVERY NODE TYPE, for two reasons that are both about which
// replica a thing happens on.
//
// The resolver is asked by the provider registry of the node that MAKES a
// model call, and that is an agent or a planner as often as the bff a person
// is connected to. A node without it has no keys, so a call forwarded there
// would be paid by the cluster's account or by nothing, whatever the person
// chose. Whether a key pays must not depend on where a call landed.
//
// The capabilities are declared in dsl/router/builtins.memql, which every
// binary loads, and a capability present in the DSL and absent from the
// registry is a boot-time resolution failure; gating the registration by build
// tag would also make whether a person can add a key depend on which replica
// their stream landed on.

func init() {
	memql.RegisterPlugin("vendorkeys", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		// What a call learned about a key is written under a write gate
		// (observe.go). A context without the gate pool's getter would build a
		// resolver that reads keys and can never record that one was refused,
		// which is a key that goes on being tried by every call. Refuse here.
		if pctx.GateDB == nil {
			return nil, fmt.Errorf("vendorkeys plug-in: no GateDB in plugin context: a key's state is written under a write gate on the gate pool (database.Database.GateDB)")
		}
		store := NewStore(engineAdapter{pctx.Engine}, pctx.GateDB, pctx.Logger)
		resolver := NewResolver(store, nil)
		// The seam is installed as soon as the plug-in is built, the way the
		// fleet and app doors are: from here on this node's vendor calls can
		// be paid by a key.
		if pctx.Providers != nil {
			pctx.Providers.SetVendorKeys(resolver)
		}
		// The engine answers who a person may lend something of their own to.
		// It is asked for by the one method this package needs of it; an
		// engine without it lends to nobody new.
		directory, _ := pctx.Engine.(ShareDirectory)
		return NewIntegration(store, resolver, pctx.Providers, directory, pctx.Logger), nil
	})
}

// engineAdapter narrows IntegrationEngineAccess to the one method this package
// uses. Narrow deliberately: nothing here should be able to reach the AI
// provider registry's clients or the tool surface.
type engineAdapter struct{ engine memql.IntegrationEngineAccess }

func (a engineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.engine.Execute(ctx, query)
}
