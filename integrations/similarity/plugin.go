package similarity

import (
	"database/sql"
	"fmt"

	"github.com/visionarys-io/memql/component/memql"
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

		integ := New(pctx.Logger)
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
