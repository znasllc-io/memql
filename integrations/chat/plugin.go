package chat

import (
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the chat integration as a plug-in. Always on:
// every node-type binary that handles agent dispatch carries this so
// the recentChat capability resolves consistently. This is a generic,
// product-agnostic engine capability (platform consolidation #2472 /
// Decision 2): the single-utterance-stream-per-space model is
// platform-level, so any product reads the same stream via
// integration.chat.recentChat with zero product Go.
func init() {
	memql.RegisterPlugin("chat", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		// REFUSE rather than default (epic memql#3974): "cannot tell whether a
		// concept is staged" must never resolve to "nothing is staged".
		if pctx.ConceptDataIsStaged == nil {
			return nil, fmt.Errorf("chat plug-in: no ConceptDataIsStaged in plugin context")
		}
		// REFUSE for the same reason (memql#4029): every read here repacks real
		// graph rows into a synthetic node, which defeats the engine's
		// concept-keyed row gate by construction, so this predicate is the whole
		// of the per-row authorization on this path. "Cannot tell whether this
		// caller may see this row" must never resolve to "yes".
		if pctx.AdmitSourceRow == nil {
			return nil, fmt.Errorf("chat plug-in: no AdmitSourceRow in plugin context")
		}
		return NewIntegration(pctx.BunDB, pctx.ConceptDataIsStaged, pctx.AdmitSourceRow), nil
	})
}
