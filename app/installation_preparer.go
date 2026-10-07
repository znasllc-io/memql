package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/releasecatalog"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/core/buildinfo"
	"github.com/znasllc-io/memql/integrations/installation"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// Construct only after the mesh's existing workbench router is wired. This
// uploader and bucket are the serving node’s existing Library transport. The
// adapter does not register a public action or another global pipeline executor.
// The installation entry point remains gated on local rollout qualification.
func (a *App) installationPreparer(uploader server.FileUploader, bucket string) (*installation.Preparer, error) {
	name := strings.TrimSpace(os.Getenv("MEMQL_INSTALLATION_CONFIG_NAME"))
	if name == "" {
		return nil, errors.New("installation preparation has no named receiving configuration")
	}
	if a == nil || a.engine == nil || !deploycontrol.InClusterAvailable() {
		return nil, errors.New("installation preparation requires the serving engine and projected cluster identity")
	}
	workbench := a.lookupWorkbenchIntegration()
	if workbench == nil || workbench.ForwardRouter() == nil {
		return nil, errors.New("installation preparation has no authenticated workbench route")
	}
	api, err := deploycontrol.NewClusterAPI()
	if err != nil {
		return nil, errors.New("installation preparation cluster API is unavailable")
	}
	if uploader == nil || strings.TrimSpace(bucket) == "" {
		return nil, errors.New("installation preparation private source storage is unavailable")
	}
	store, ok := a.pipelinesLibraryStoreFor(uploader, bucket).(*pipelinesLibraryStore)
	if !ok || store == nil {
		return nil, errors.New("installation preparation source lifecycle is unavailable")
	}
	deps := installation.PreparationDependencies{API: api, Database: func() *sql.DB {
		if db := a.BunDB(); db != nil {
			return db.DB
		}
		return nil
	}, Executor: pipelinesteps.NewExecutor(pipelinesteps.ConfigFromEnv(nil), workbench.ForwardRouter(), nil, a.Logger), Files: store, Tokens: a.pipelinesTokenMinterFor(), Catalog: func(ctx context.Context, configuration []byte, credential string) (installation.Catalog, error) {
		return releasecatalog.NewSnapshot(ctx, configuration, credential)
	}, Namespace: deploycontrol.NamespaceFromEnv(), ConfigurationName: name, EngineRevision: buildinfo.Commit()}
	return installation.NewPreparer(deps)
}
