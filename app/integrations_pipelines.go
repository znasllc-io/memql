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
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
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
// replica (MEMQL_NODE_ID), links its check runs to MemQL OS and tells its steps
// the cluster's front-door domain (both from MEMQL_DOMAIN). Only app/ holds
// every one of those.
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
		// The connect preview's delivery suggestion, by the rule the GitHub
		// App this cluster registers is composed by: its webhook is active
		// only on a name GitHub could reach (githubconnect, manifest D5).
		d.WebhookReachable = func() bool {
			return githubconnect.DomainIsPubliclyReachable(os.Getenv("MEMQL_DOMAIN"))
		}
		d.Domain = pipelinesDomain
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

// The OS asks the BFF for readiness, while only agents register a step
// executor. Reuse the authenticated workbench route for this read without
// enabling pipeline execution on the front door.
func (a *App) wirePipelinesReadiness(forwarder pipelinesteps.Forwarder) {
	if integ := a.lookupPipelinesIntegration(); integ != nil {
		reporter := pipelinesteps.NewExecutor(pipelinesteps.ConfigFromEnv(nil), forwarder, nil, a.Logger)
		integ.Configure(func(d *pipelinerun.Deps) { d.RunnerReadiness = reporter })
	}
}

// pipelinesOSOrigin is MemQL OS's origin for this cluster, the base of a check
// run's details link: "https://" + frontdoor.OsHost(MEMQL_DOMAIN), the SAME
// rule the OS's Ingress host, its site row and its OAuth redirect URI are
// composed by (component/envregistry/domain.go). The domain is a value, read
// at each call; with none set the link is omitted rather than invented.
func pipelinesOSOrigin() string {
	domain := pipelinesDomain()
	if domain == "" {
		return ""
	}
	return "https://" + frontdoor.OsHost(domain)
}

// pipelinesDomain is this cluster's front-door domain, MEMQL_DOMAIN, read at
// each call: what a step receives as MEMQL_DOMAIN. "" sends none.
func pipelinesDomain() string { return strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")) }

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
