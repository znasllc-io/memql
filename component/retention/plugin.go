package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// Use the instance's existing archive destination; no per-node policy copy.
const archiveContainerEnv = "MEMQL_AZURE_BLOB_CONTAINER"

type Integration struct {
	context   memql.PluginContext
	archive   Archive
	container string
}

func init() {
	memql.RegisterPlugin("retention", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.Concepts == nil || pctx.AdmitSourceRow == nil || pctx.InvalidateConcept == nil {
			return nil, fmt.Errorf("retention requires live concepts, row admission and cache invalidation")
		}
		i := &Integration{context: pctx, container: strings.TrimSpace(os.Getenv(archiveContainerEnv))}
		if i.container != "" {
			archive, err := azureblob.New(context.Background())
			if err == nil {
				i.archive = archive
			} else if pctx.Logger != nil {
				pctx.Logger.Warn("concept retention archive unavailable; archived policies keep their records", "error", err)
			}
		}
		return i, nil
	})
}

func (*Integration) IntegrationName() string { return "retention" }
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{{Name: "sweep", Description: "Archive and expire versions according to each concept's retention declaration. Cluster owner only.", ArgsSchema: map[string]string{"dryRun": "bool -- report the first page per concept without changing data or progress"}, Handler: i.sweep}}
}
func (i *Integration) sweep(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || !actor.IsClusterOwner() {
		return nil, fmt.Errorf("concept retention requires a cluster owner")
	}
	dry, _ := args["dryRun"].(bool)
	if i.context.BunDB == nil {
		return nil, fmt.Errorf("concept retention database unavailable")
	}
	s := Sweeper{DB: i.context.BunDB(), Concepts: i.context.Concepts, Admit: i.context.AdmitSourceRow, Invalidate: i.context.InvalidateConcept, Archive: i.archive, Container: i.container}
	report, err := s.Run(ctx, dry)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "conceptRetentionSweep", Concept: "integration:retention:sweep", Type: "object", CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}
