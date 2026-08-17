package harnessrecall

import (
	"database/sql"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the harnessRecall integration as an always-on
// plug-in. Anchored from app/plugins_core.go so every node-type binary
// with a database + embedding provider exposes recall() -- the harness
// memory query is product-agnostic and any concept with a content
// vector in node_vectors is a valid recall source.
//
// Returns (nil, nil) to opt out when the binary has no embedding
// provider (recall needs to embed the query text); the recall builtin
// then resolves to "no handler" rather than failing startup.
func init() {
	memql.RegisterPlugin("harnessRecall", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.EmbeddingProviderByName == nil {
			// No embedding provider on this node-type binary; recall
			// cannot embed the query text. Opt out cleanly.
			return nil, nil
		}
		// REFUSE rather than default (epic memql#3974): nil means this node
		// cannot tell whether a concept's rows are staged, and "cannot tell"
		// must never resolve to "nothing is staged".
		if pctx.ConceptDataIsStaged == nil {
			return nil, fmt.Errorf("harnessRecall plug-in: no ConceptDataIsStaged in plugin context")
		}
		integ := New(pctx.Logger)
		integ.SetStagedConceptPredicate(pctx.ConceptDataIsStaged)
		integ.SetDBGetter(func() *sql.DB {
			bunDB := pctx.BunDB()
			if bunDB == nil {
				return nil
			}
			return bunDB.DB
		})
		integ.SetEmbeddingProvider(pctx.EmbeddingProviderByName)
		return integ, nil
	})
}
