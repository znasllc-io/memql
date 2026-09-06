//go:build bff || agent

package app

import (
	"context"

	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// resolveBlobStore builds the Azure Blob uploader and returns it with its
// container name, so the agent node can hand both to the workbench / worker
// integrations for server-side fs_write promotion.
//
// It used to also mount `POST|GET /spaces/{partitionId}/attachments`. That
// route and its handler went with the space concept (memql#4990); the Library's
// own byte routes (`POST /artifacts`, `GET /artifacts/{id}/content` and the
// chunked-session family) are the upload path now, and they were already the
// only one any client in this repo used -- Training moved off attachments to
// the Library when it was re-keyed, and nothing else ever called it.
//
// Storage backend is Azure Blob (memql#801); locally that's the Azurite
// emulator with MEMQL_AZURE_BLOB_AUTOCREATE=1 so the container is created on
// boot. When no container is configured this returns a nil uploader and an
// empty container, which every caller already treats as "bytes are not
// promotable here" rather than an error.
func (a *App) resolveBlobStore() (server.FileUploader, string) {
	container := azureblob.ContainerFromEnv()
	if container == "" {
		return nil, ""
	}
	blobClient, err := azureblob.New(context.Background())
	if err != nil {
		a.Logger.Warn("Azure Blob uploader unavailable", "error", err)
		return nil, container
	}
	// Create the container when missing (local Azurite has none
	// pre-provisioned). Best-effort: a failure degrades to inline/pointer
	// rows rather than breaking boot.
	if azureblob.AutoCreateContainerEnabled() {
		if err := blobClient.EnsureContainer(context.Background(), container); err != nil {
			a.Logger.Warn("Azure Blob ensure-container failed",
				"container", container, "error", err)
		}
	}
	return blobClient, container
}
