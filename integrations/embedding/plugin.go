package embedding

import (
	"database/sql"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the embedding integration as a plug-in. Always on:
// the DSL 'embed' / 'findSimilar' / 'store' capabilities are broadly
// useful across node types.
//
// DB access is wired via a lazy getter because MemoryNodesDatabase.BunDB()
// returns nil until its Start() hook runs, which happens after the plug-in
// factory fires. The getter closure resolves the handle at capability-
// invocation time.
func init() {
	memql.RegisterPlugin("embedding", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.EmbeddingProviderByName == nil {
			return nil, fmt.Errorf("embedding plug-in: no EmbeddingProviderByName in plugin context")
		}
		if pctx.ResolvePartitionFromContext == nil {
			return nil, fmt.Errorf("embedding plug-in: no ResolvePartitionFromContext in plugin context")
		}

		// REFUSE rather than default (epic memql#3974): "cannot tell whether a
		// concept is staged" must never resolve to "nothing is staged".
		if pctx.ConceptDataIsStaged == nil {
			return nil, fmt.Errorf("embedding plug-in: no ConceptDataIsStaged in plugin context")
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
		integ.SetPartitionFunc(pctx.ResolvePartitionFromContext)
		return integ, nil
	})
}
