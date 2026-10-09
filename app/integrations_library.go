package app

import (
	"database/sql"
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
	integ.SetRevisionFiles(&libraryRevisionFiles{uploader: uploader, bucket: bucket, store: server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: a.engine}, func() *sql.DB {
		if db := a.BunDB(); db != nil {
			return db.DB
		}
		return nil
	})})
	if reader, ok := uploader.(library.BlobFetcher); ok {
		integ.SetBlobFetcher(reader)
	}
}
