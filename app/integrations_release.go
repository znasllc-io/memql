package app

import (
	"database/sql"
	"strings"

	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/release"
)

// Each serving/automation replica consumes its own client for the same native
// artifact journal and object store. No mutable Library URL becomes authority.
func (a *App) wireReleaseCandidates(uploader server.FileUploader, bucket string) {
	if a.engine == nil || uploader == nil || strings.TrimSpace(bucket) == "" {
		return
	}
	provider, ok := a.engine.IntegrationByName("release").(*release.Integration)
	if !ok {
		return
	}
	store, ok := a.pipelinesLibraryStoreFor(uploader, bucket).(release.CandidateLibrary)
	if !ok {
		a.Logger.Error("release candidates require the native retained-artifact store")
		return
	}
	if err := provider.ConfigureCandidates(release.CandidateDependencies{Library: store, Database: func() *sql.DB {
		if db := a.BunDB(); db != nil {
			return db.DB
		}
		return nil
	}}); err != nil {
		a.Logger.Error("release candidate storage could not be configured", "error", err)
	}
}
