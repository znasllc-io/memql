package app

import (
	"context"
	"database/sql"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/workjournal"
)

// integrations_pipelines.go hands the pipelines plug-in (epic memql#5477) the
// ports only app/ can build.
//
// ===========================================================================
// WIRED HERE BECAUSE ONLY app/ CAN SEE ALL OF THEM
// ===========================================================================
// The plug-in factory gets a PluginContext: an engine and a secret resolver,
// which is the DSL store and nothing else. A pipeline also borrows
// Deployables' GitHub App client and verified grant path (the packages
// plug-in), serializes replicas on a Postgres advisory lock (the DIRECT
// database, which a transaction pooler would silently break), names this
// replica (MEMQL_NODE_ID) and links its check runs to MemQL OS (the cluster's
// domain). Only app/ holds every one of those.
//
// NO BUILD TAG. A webhook is staged on the bff and opens its run there, the
// poll is placed on agent replicas and opens runs there, and a person's
// re-run or cancel lands on whichever node serves their stream -- so every
// node type opens runs, and every one needs these ports. What only an agent
// does -- claiming and DRIVING a run -- is wired beside the agent's own
// integrations (Task 10b), never here.
func (a *App) wirePipelines() {
	integ := a.lookupPipelinesIntegration()
	if integ == nil {
		// Not an error: the plug-in is anchored in plugins_core.go and
		// materializes everywhere, so its absence is a build that left it
		// out on purpose.
		return
	}
	pkgs := a.lookupPackagesIntegration()
	getDB := a.directDBGetter()
	nodeID := strings.TrimSpace(os.Getenv("MEMQL_NODE_ID"))
	integ.Configure(func(d *pipelinerun.Deps) {
		if pkgs != nil {
			// The packages integration itself, NOT its client: the port asks
			// it for the client at each call, so the client is built from
			// what wirePackageGitHubApp installed rather than frozen to the
			// environment by an early ask.
			d.GitHub = pipelinerun.NewGitHub(pkgs)
		} else {
			a.Logger.Warn("pipelines: the packages integration was not found, so this node can neither mint a pipeline's token nor write a check run")
		}
		d.Gate = func(ctx context.Context, key string, fn func(context.Context) error) error {
			var db *sql.DB
			if bdb := getDB(); bdb != nil {
				db = bdb.DB
			}
			// Nil fails CLOSED inside WithGate, before fn runs.
			return githubconnect.WithGate(ctx, db, key, fn)
		}
		d.NodeID = nodeID
		d.OSOrigin = pipelinesOSOrigin
		if a.engine != nil {
			d.Journal = workjournal.New(
				workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
					return a.engine.Execute(ctx, q)
				}),
				a.Logger,
				nodeID,
			)
		}
	})
}

// pipelinesOSOrigin is MemQL OS's origin for this cluster, the base of a check
// run's details link: "https://" + frontdoor.OsHost(MEMQL_DOMAIN), the SAME
// rule the OS's Ingress host, its site row and its OAuth redirect URI are
// composed by (component/envregistry/domain.go). The domain is a value, read
// at each call; with none set the link is omitted rather than invented.
func pipelinesOSOrigin() string {
	domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
	if domain == "" {
		return ""
	}
	return "https://" + frontdoor.OsHost(domain)
}

// lookupPipelinesIntegration recovers the materialized pipelines plug-in, or
// nil.
func (a *App) lookupPipelinesIntegration() *pipelinerun.Integration {
	if a.engine == nil {
		return nil
	}
	provider := a.engine.IntegrationByName(pipelinerun.IntegrationName)
	if provider == nil {
		return nil
	}
	integ, _ := provider.(*pipelinerun.Integration)
	return integ
}
