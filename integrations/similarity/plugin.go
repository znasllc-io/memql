package similarity

import (
	"database/sql"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the similarity integration as an always-on
// plug-in. Anchored from app/plugins_core.go so every node-type
// binary has `similarTo` available -- the DSL operator is product-
// agnostic and any concept with a content vector is fair game.
func init() {
	memql.RegisterPlugin("similarity", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.EmbeddingProviderByName == nil {
			return nil, fmt.Errorf("similarity plug-in: no EmbeddingProviderByName in plugin context")
		}
		if pctx.ResolvePartitionFromContext == nil {
			return nil, fmt.Errorf("similarity plug-in: no ResolvePartitionFromContext in plugin context")
		}
		// REFUSE rather than default (epic memql#3974). A nil predicate means
		// "this node cannot tell whether a concept's data is staged", and the
		// only safe reading of that is a boot failure -- the alternative,
		// treating nil as "nothing is staged", is a node that serves staged
		// rows into agent context and looks completely healthy doing it.
		if pctx.ConceptDataIsStaged == nil {
			return nil, fmt.Errorf("similarity plug-in: no ConceptDataIsStaged in plugin context")
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
