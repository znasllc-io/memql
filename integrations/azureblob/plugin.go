package azureblob

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the Azure Blob storage integration as a plug-in. Only
// materializes when a container is configured via MEMQL_AZURE_BLOB_CONTAINER.
// Absent configuration is silent; uploader-construction failures (e.g. a
// missing/invalid connection string) log a warning and opt the plug-in out --
// storage is optional, never a hard failure.
func init() {
	memql.RegisterPlugin("storage", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		container := ContainerFromEnv()
		if container == "" {
			return nil, nil
		}
		uploader, err := New(context.Background())
		if err != nil {
			pctx.Logger.Warn("Azure Blob storage plug-in opting out", "error", err)
			return nil, nil
		}
		return NewStorageIntegration(uploader, container), nil
	})
}
