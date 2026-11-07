package app

import (
	"database/sql"
	"os"
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
	if pipelines := a.lookupPipelinesIntegration(); pipelines != nil {
		if err := provider.ConfigureReleaseBuilds(pipelines); err != nil {
			a.Logger.Error("release build host could not be configured", "error", err)
		}
	}
	if err := provider.ConfigureInstallationHealth(a.installationMigrationHealth); err != nil {
		a.Logger.Error("installation serving health could not be configured", "error", err)
	}
	if packages := a.lookupPackagesIntegration(); packages != nil {
		if err := provider.ConfigureInstallationOverlay(packages); err != nil {
			a.Logger.Error("installation overlay proposals could not be configured", "error", err)
		}
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
	// Keep the same private Library transport for the receiver. Its executor
	// needs the authenticated router installed later by the cluster phase.
	a.installationUploader, a.installationBucket = uploader, bucket
}

func (a *App) wireInstallationSelection() {
	// This native host is opt-in: no receiving configuration means no update surface.
	if a == nil || a.engine == nil || a.installationUploader == nil || strings.TrimSpace(a.installationBucket) == "" {
		return
	}
	if strings.TrimSpace(os.Getenv("MEMQL_INSTALLATION_CONFIG_NAME")) != "" {
		provider, ok := a.engine.IntegrationByName("release").(*release.Integration)
		if !ok {
			return
		}
		preparer, err := a.installationPreparer(a.installationUploader, a.installationBucket)
		if err != nil {
			a.Logger.Error("installation selection could not be configured", "error", err)
			return
		}
		if err := provider.ConfigureInstallation(preparer); err != nil {
			a.Logger.Error("installation selection host could not be bound", "error", err)
		}
	}
}
