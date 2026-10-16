package app

import (
	"database/sql"
	"strings"

	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/server"
)

// Each front-door/automation replica gets its own adapters to the shared
// journal and retained objects; recovery never depends on an uploader's RAM.
func (a *App) wirePipelineSARIF(uploader server.FileUploader, bucket string) {
	i := a.lookupPipelinesIntegration()
	if i == nil || a.engine == nil || uploader == nil || strings.TrimSpace(bucket) == "" {
		return
	}
	library, ok := a.pipelinesLibraryStoreFor(uploader, bucket).(pipelinerun.SARIFLibrary)
	if !ok {
		a.Logger.Error("pipeline SARIF publication requires native retained-artifact storage")
		return
	}
	i.Configure(func(d *pipelinerun.Deps) {
		d.SARIF = &pipelinerun.SARIFDependencies{Engine: a.engine, Library: library, Database: func() *sql.DB {
			if db := a.BunDB(); db != nil {
				return db.DB
			}
			return nil
		}}
	})
}
