package training

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/visionarys-io/memql/component/memql"
)

// init self-registers the training integration as an always-on
// plug-in. The DSL capability (trainAgent) drives the CoPresent
// Training panel's "Train" button -- updates the agent's
// capabilities + warms the vector index for any newly-added domain
// content.
//
// Like the knowledge + embedding integrations, DB access is wired
// via a lazy getter because MemoryNodesDatabase.BunDB() only returns
// a live handle after its Start() hook fires, which happens after
// the plug-in factory runs.
//
// The factory also kicks off the background retry-poll goroutine
// (see retry.go). The goroutine wakes every 60s, looks for queued
// trainAgentRetryStep Plans whose nextAttemptAt is past, and runs
// them. Backed by context.Background() so it lives for the process
// lifetime; in-flight retry-Plans are picked up on the next process
// boot since the schedule lives in the DB.
func init() {
	memql.RegisterPlugin("training", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.EmbeddingProviderByName == nil {
			return nil, fmt.Errorf("training plug-in: no EmbeddingProviderByName in plugin context")
		}
		if pctx.ResolvePartitionFromContext == nil {
			return nil, fmt.Errorf("training plug-in: no ResolvePartitionFromContext in plugin context")
		}
		if pctx.Engine == nil {
			return nil, fmt.Errorf("training plug-in: no Engine in plugin context")
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

		// Spawn the background retry-poll goroutine. context.Background
		// because IntegrationProvider has no Start/Stop hooks today and
		// the goroutine is process-lifetime by design. See retry.go for
		// the loop body.
		integ.startRetryLoop(context.Background())

		return integ, nil
	})
}
