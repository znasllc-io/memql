package harnessrecall

import (
	"database/sql"

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
