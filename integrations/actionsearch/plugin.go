package actionsearch

import (
	"database/sql"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the actionSearch integration as an always-on plug-in
// (anchored from app/plugins_core.go), so every node-type binary with a
// database + embedding provider exposes searchActions() over the action
// library (#1758). It opts out cleanly when the binary has no embedding
// provider (it cannot embed the query text); searchActions then resolves to
// "no handler" rather than failing startup -- mirroring harnessRecall.
func init() {
	memql.RegisterPlugin("actionSearch", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.EmbeddingProviderByName == nil {
			return nil, nil
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
		return integ, nil
	})
}
