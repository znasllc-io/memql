package azureblob

import (
	"context"

	"github.com/znasllc-io/memql/component/memql"
)

// StorageIntegration is the node's blob storage as the integration registry
// sees it: registered under the name "storage", and offering the DSL nothing.
//
// The registration is what other surfaces read. The `storage` readiness module
// is `hostedBy: integrations: [storage]` (component/envregistry/manifest.yaml),
// so a node reports a storage verdict only where this materialized, and
// integrationStatus and the module registry list it. That is all it is for.
//
// Blob storage is written by native producers, each composing its object names
// under its own authority through its own client. A capability here would act
// with the node's storage credential for whoever called it, so a new writer
// gets its own narrow path rather than a general upload.
type StorageIntegration struct{}

// IntegrationName returns the name the storage readiness module is hosted by.
func (StorageIntegration) IntegrationName() string { return "storage" }

// Capabilities returns nil: blob storage is not DSL-callable.
func (StorageIntegration) Capabilities() []memql.IntegrationCapability { return nil }

// init self-registers the plug-in. It materializes only where a container is
// configured via MEMQL_AZURE_BLOB_CONTAINER and a client can be built from the
// connection string -- the client is built here only to learn that it can be,
// and is not kept. Absent configuration is silent; a client that cannot be
// built (a missing or invalid connection string) logs a warning and opts the
// plug-in out -- storage is optional, never a hard failure.
func init() {
	memql.RegisterPlugin("storage", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if ContainerFromEnv() == "" {
			return nil, nil
		}
		if _, err := New(context.Background()); err != nil {
			pctx.Logger.Warn("Azure Blob storage plug-in opting out", "error", err)
			return nil, nil
		}
		return StorageIntegration{}, nil
	})
}
