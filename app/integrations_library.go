package app

import (
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/library"
)

// The registered Library instance handles document review calls. It must read
// the same stored bytes as artifact downloads, including on another replica.
func (a *App) wireLibraryIntegration(uploader server.FileUploader, bucket string) {
	if a.engine == nil {
		return
	}
	integ, ok := a.engine.IntegrationByName("library").(*library.Integration)
	if !ok {
		return
	}
	if work := a.lookupWorkIntegration(); work != nil {
		integ.SetReviewGoals(work)
	}
	// The store's file-version gate runs on the gate pool (memql#6022), not on the
	// request pool the engine under it works on.
	imageStore := server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: a.engine}, a.gateDBGetter())
	integ.SetRevisionFiles(&libraryRevisionFiles{uploader: uploader, bucket: bucket, store: imageStore, authority: a.engine})
	integ.SetImageAssets(&libraryImageAssets{engine: &AttachmentEngineAdapter{Engine: a.engine}, store: imageStore, uploader: uploader, bucket: bucket})
	if reader, ok := uploader.(library.BlobFetcher); ok {
		integ.SetBlobFetcher(reader)
	}
}
