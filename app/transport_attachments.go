//go:build bff || agent

package app

import (
	"context"

	"github.com/znasllc-io/memql/component/fileprocessor"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// mountAttachmentEndpoints wires the HTTP attachment handler (POST upload +
// GET download) onto the node's mux and returns the blob uploader + container
// so the agent node can additionally hand them to the workbench / worker
// integrations for server-side fs_write promotion.
//
// Lives here (shared by the bff + agent builds) because BOTH nodes serve the
// `/spaces/{partitionId}/attachments` surface: the bff is the frontend-facing node
// the SPA's upload/download requests route to (nginx + the Vite dev proxy send
// `/spaces/...` to the bff), and the agent serves it on its own host for the
// worker/workbench paths. Previously only the agent mounted it -- and only the
// POST verb -- so browser uploads/downloads through the bff 404'd and even on
// the agent the GET download route the handler implements was never registered
// (memql#888).
//
// Storage backend is Azure Blob (memql#801); locally that's the Azurite
// emulator with MEMQL_AZURE_BLOB_AUTOCREATE=1 so the container is created on
// boot. When no container is configured the handler still mounts (uploads fall
// back to inline/pointer rows); only the downloadable-bytes path is inert.
func (a *App) mountAttachmentEndpoints() (server.FileUploader, string) {
	var uploader server.FileUploader
	var downloader server.FileDownloader
	container := azureblob.ContainerFromEnv()
	if container != "" {
		blobClient, err := azureblob.New(context.Background())
		if err != nil {
			a.Logger.Warn("Azure Blob uploader unavailable", "error", err)
		} else {
			uploader = blobClient
			downloader = blobClient // same client serves GET downloads (memql#804)
			// Create the container when missing (local Azurite has none
			// pre-provisioned). Best-effort: a failure degrades to
			// inline/pointer rows rather than breaking boot.
			if azureblob.AutoCreateContainerEnabled() {
				if err := blobClient.EnsureContainer(context.Background(), container); err != nil {
					a.Logger.Warn("Azure Blob ensure-container failed",
						"container", container, "error", err)
				}
			}
		}
	}

	processor := fileprocessor.NewDefaultProcessor(a.engine.VisionProvider())
	engineAdapter := &AttachmentEngineAdapter{Engine: a.engine}
	handler := server.NewAttachmentHandler(server.AttachmentHandlerOptions{
		Logger:     a.Logger,
		Bucket:     container,
		Uploader:   uploader,
		Downloader: downloader,
		Extractor:  processor,
		Store:      server.NewEngineAttachmentStore(engineAdapter),
		PlanStore:  server.NewEnginePlanStore(engineAdapter),
	})
	for _, path := range server.SpaceAttachmentPaths() {
		a.mux.Handle("POST "+path, handler) // upload
		a.mux.Handle("GET "+path, handler)  // download (memql#804/#888)
	}
	return uploader, container
}
