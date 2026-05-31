package knowledge

import (
	"database/sql"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the knowledge integration as an always-on plug-in.
// The DSL capabilities (ingest, lookup) are used by the agent builder's
// domain picker, by seed automations that populate copresent-ui docs on
// startup, and by the agent replier's up-front operator-turn retrieval step.
//
// Like the embedding integration, DB access is wired via a lazy getter
// because MemoryNodesDatabase.BunDB() only returns a live handle after
// its Start() hook fires, which happens after the plug-in factory runs.
func init() {
	memql.RegisterPlugin("knowledge", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.EmbeddingProviderByName == nil {
			return nil, fmt.Errorf("knowledge plug-in: no EmbeddingProviderByName in plugin context")
		}
		if pctx.ResolvePartitionFromContext == nil {
			return nil, fmt.Errorf("knowledge plug-in: no ResolvePartitionFromContext in plugin context")
		}
		if pctx.Engine == nil {
			return nil, fmt.Errorf("knowledge plug-in: no Engine in plugin context")
		}

		integ := New(pctx.Logger)
		integ.SetEngine(pctx.Engine)
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
