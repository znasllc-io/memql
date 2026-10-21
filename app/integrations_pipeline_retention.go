package app

import (
	"database/sql"
	"strings"

	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/server"
)

func (a *App) wirePipelineArtifactRetention(uploader server.FileUploader, bucket string) {
	i := a.lookupPipelinesIntegration()
	if i == nil || a.engine == nil || uploader == nil || strings.TrimSpace(bucket) == "" {
		return
	}
	library, ok := a.pipelinesLibraryStoreFor(uploader, bucket).(pipelinerun.ArtifactRetentionLibrary)
	if !ok {
		a.Logger.Error("pipeline artifact retention requires native storage fences")
		return
	}
	i.Configure(func(d *pipelinerun.Deps) {
		d.ArtifactRetention = &pipelinerun.ArtifactRetentionDependencies{Engine: a.engine, Library: library, Database: func() *sql.DB {
			if db := a.BunDB(); db != nil {
				return db.DB
			}
			return nil
		}}
	})
}
