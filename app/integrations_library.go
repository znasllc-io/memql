package app

import (
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/library"
)

// The registered Library instance handles document review calls. It must read
// the same stored bytes as artifact downloads, including on another replica.
func (a *App) wireLibraryIntegration(uploader server.FileUploader) {
	if a.engine == nil {
		return
	}
	integ, ok := a.engine.IntegrationByName("library").(*library.Integration)
	if !ok {
		return
	}
	if reader, ok := uploader.(library.BlobFetcher); ok {
		integ.SetBlobFetcher(reader)
	}
}
