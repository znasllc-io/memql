package packages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
)

// integrationName is the plug-in name. Spelled as a STRING LITERAL in
// RegisterPlugin below rather than as this constant, and that is not an
// oversight -- the taxonomy gate (module_taxonomy_test.go) finds every
// registration by scanning source for the literal, because a computed name
// could not be classified at PR time. TestPackagesRegistrationNameIsTheLiteral
// asserts the two still agree.
const integrationName = "packages"

// Integration exposes the package capabilities.
//
// IT LIVES IN component/, NOT integrations/, and the module graph is why. The
// pipeline publishes through component/edge's Publisher -- the same one POST
// /sites/{id}/bundles uses -- and component/edge is in the ROOT module, which
// already requires integrations. component/sitepublish moved here for exactly
// this reason (memql#4345), and module_taxonomy_test.go records it: importing
// edge from integrations makes the module graph a cycle, which go.work resolves
// and CI's module-boundaries lane does not.
//
// The design names integrations/packages/ as the location. This is the same
// package under the one constraint that location cannot satisfy.
type Integration struct {
	engine Engine
	logger *slog.Logger

	// workbench is the build surface (epic memql#4900, task memql#4901),
	// injected by app/ after both plug-ins are materialized rather than
	// resolved here -- this package holds a one-method Engine and has no way
	// to reach the integration registry, which is exactly the narrowness that
	// makes the pipeline testable with no cluster.
	//
	// NIL IS AN ANSWER. A node with no build surface keeps Deps.Builder nil,
	// and a package needing a build gets the typed refusal it has always got.
	workbench workbenchRunner
	// fleet is the same seam for the machine route (task memql#4904). Nil on
	// every node type that holds no worker streams, which is every one but
	// the agent.
	fleet Builder

	depsOnce sync.Once
	deps     *Deps
	depsErr  error
}

// SetWorkbench installs the in-cluster build surface. Called once, from app/,
// before the first deploy; a later call is ignored because Deps is built once
// and a builder that changed under a running pipeline would make two halves of
// one deploy disagree about where they built.
func (i *Integration) SetWorkbench(runner workbenchRunner) {
	i.workbench = runner
}

// SetFleetBuilder installs the machine route (task memql#4904).
func (i *Integration) SetFleetBuilder(b Builder) {
	i.fleet = b
}

// NewIntegration wires the engine handle. The factory is in init(); this
// constructor is what tests call with a stub engine.
func NewIntegration(engine Engine, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{engine: engine, logger: logger}
}

func init() {
	memql.RegisterPlugin("packages", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Engine, pctx.Logger), nil
	})
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "analyze",
			Description: "Analyze a package source offline and return the report, without deploying anything (epic memql#4794, D12). Runs the same Init-grade gates strict boot runs, so 'this DSL would refuse boot' is an answer here rather than a crashlooping node later. Returns the section E report; a refusal carries a stable code.",
			Handler:     i.handleAnalyze,
			ArgsSchema: map[string]string{
				"packageId": "string (required) -- the v1:platform:package row to analyze",
			},
		},
		{
			Name:        "deploy",
			Description: "Run one deployment attempt for a package (epic memql#4794). Without confirm, the run parks at awaiting_confirm with the analysis report on the deployment row and nothing else happens; with confirm, it builds, stages, rolls and publishes in the D6 order. placements (epic memql#4885, D8) is per deployable name -- {hostname, accountId, ownDomain} -- read on a deployable's FIRST deploy only: the site is created at hostname, then the account write and the domain binding run under the caller's actor as the same two calls the page makes, and a refused one lands on the outcome (accountRefusal / domainRefusal) without failing the publish. Returns {deploymentId, status, awaitingConfirm, deployables, report}.",
			Handler:     i.handleDeploy,
			ArgsSchema: map[string]string{
				"packageId":  "string (required) -- the package to deploy",
				"confirm":    "boolean -- pass true to proceed past the always-present confirm gate",
				"placements": "object -- deployable name -> {hostname, accountId, ownDomain}; hostname is required on a deployable's FIRST deploy, the other two are optional and applied after the site exists",
			},
		},
		{
			Name:        "rollback",
			Description: "Restore a package to a previous deployment's (dslVersion, site bundleRefs) tuple (epic memql#4794, D7). Executes the D6 order REVERSED -- publish back first, then the pointer back and a roll -- so the app and the schema never disagree in either direction.",
			Handler:     i.handleRollback,
			ArgsSchema: map[string]string{
				"packageId":    "string (required) -- the package to roll back",
				"deploymentId": "string (required) -- the earlier deployment whose state to restore",
			},
		},
		{
			Name:        "archiveSite",
			Description: "Archive a site after verifying the typed hostname (epic memql#4794, D10).",
			Handler:     i.handleArchiveSite,
			ArgsSchema: map[string]string{
				"siteId":          "string (required)",
				"confirmHostname": "string (required) -- the site's own hostname, typed as confirmation",
			},
		},
		{
			Name:        "restoreSite",
			Description: "Bring an archived site back to disabled (epic memql#4794, D10).",
			Handler:     i.handleRestoreSite,
			ArgsSchema:  map[string]string{"siteId": "string (required)"},
		},
		{
			Name:        "archivePackage",
			Description: "Archive a package and every app it produced (epic memql#4794 D10, epic memql#4885). Verifies the typed name, refuses with package_has_active_deployables naming the LIVE hostnames while any app is still serving -- pausing stays the person's decision -- and otherwise archives each paused or never-published app through the same guarded status write the site archive uses (a draft is walked through disabled first), sites first and the package last. Apps already archived are left alone. Returns {packageId, name, status, archivedSites}.",
			Handler:     i.handleArchivePackage,
			ArgsSchema: map[string]string{
				"packageId":   "string (required)",
				"confirmName": "string (required) -- the package's own name, typed as confirmation",
			},
		},
		{
			Name:        "noteUpstreamFromWebhook",
			Description: "Match a verified inbound webhook delivery to the packages tracking that repository and record the version it announced (epic memql#4794, D11). Writes only latestKnownVersion and updateAvailable; never starts a deployment. A delivery matching no package is a no-op.",
			Handler:     i.handleNoteUpstreamFromWebhook,
			ArgsSchema: map[string]string{
				"inboundRequestId": "string -- the staged v1:platform:inboundRequest row",
				"source":           "string -- the allowlisted source segment",
				"body":             "string -- the verified raw body",
			},
		},
		{
			Name:        "pollUpstream",
			Description: "Walk every repo-sourced package and record any upstream that moved (epic memql#4794, D11). The polling fallback for clusters no webhook reaches. Writes only latestKnownVersion and updateAvailable, and only where something changed.",
			Handler:     i.handlePollUpstream,
			ArgsSchema:  map[string]string{},
		},
		{
			Name:        "sweepAbandoned",
			Description: "Close every run whose node stopped saying it was alive (epic memql#4900, task memql#4902). A run whose heartbeatAt is older than MEMQL_PACKAGES_ABANDONED_AFTER_SECONDS is closed with the terminal status 'abandoned' and a typed error naming the node that was running it and when it was last heard; a run that is merely slow is untouched. NEVER writes 'failed': nothing failed that this sweep can name, and the D6 order guarantees every site is still serving what it was serving. Returns {checked, abandoned}.",
			Handler:     i.handleSweepAbandoned,
			ArgsSchema:  map[string]string{},
		},
		{
			Name:        "setAutoDeploy",
			Description: "Turn a source's auto-deploy switch on or off (epic memql#4900, task memql#4903). Runs the owned setPackageAutoDeploy mutation under the CALLER's actor, so the write guard admits the source's owner or a cluster owner and nobody else. Returns {packageId, autoDeploy}.",
			Handler:     i.handleSetAutoDeploy,
			ArgsSchema: map[string]string{
				"packageId":  "string (required) -- the v1:platform:package row to switch",
				"autoDeploy": "boolean (required) -- true arms it; false restores the click",
			},
		},
		{
			Name:        "restorePackage",
			Description: "Bring an archived package back to active (epic memql#4794, D10).",
			Handler:     i.handleRestorePackage,
			ArgsSchema:  map[string]string{"packageId": "string (required)"},
		},
		{
			Name:        "sourceCredentialCreate",
			Description: "Store a personal source credential (epic memql#4885, D10). The token crosses the wire once, inside this call, is sealed under MEMQL_MASTER_KEY on this node, and lands on a v1:platform:sourceCredential row owned by the caller; it appears in no row, log line or reply. Only github.com is admitted as a host today. Returns {credentialId, fingerprint}.",
			Handler:     i.handleSourceCredentialCreate,
			ArgsSchema: map[string]string{
				"host":  "string (required) -- github.com is the only host admitted today",
				"label": "string (required) -- the person's own name for the credential",
				"token": "string (required) -- the access token; read once, sealed, never echoed",
			},
		},
		{
			Name:        "sourceCredentialRevoke",
			Description: "Revoke one of the caller's source credentials (epic memql#4885, D10). Flips status and stamps revokedAt through the owned mutation, so the write guard admits the row's owner (or a cluster owner) and nobody else; the row is kept as history. Returns {credentialId, status}.",
			Handler:     i.handleSourceCredentialRevoke,
			ArgsSchema:  map[string]string{"credentialId": "string (required)"},
		},
		{
			Name:        "sourceProbe",
			Description: "Ask whether this cluster can read a repository before a source is committed to (epic memql#4885, D11). Parses the URL, resolves the named credential under the CALLER's actor the way a fetch does, asks GitHub for the repository, and answers {host, reachable, private, defaultBranch, reason} where reason is exactly one of ok, not_found_or_private, credential_cannot_see_it, credential_not_found, credential_revoked, source_host_unsupported, rate_limited -- a typed reason, never the API's own body. Writes nothing and stamps nothing, not even lastUsedAt. A GitHub this cluster cannot reach is an error, not a reason.",
			Handler:     i.handleSourceProbe,
			ArgsSchema: map[string]string{
				"repoUrl":      "string (required) -- the repository URL as typed",
				"credentialId": "string -- one of the caller's v1:platform:sourceCredential rows to probe under; empty probes anonymously",
			},
		},
		{
			Name:        "sourceRepositories",
			Description: "List the repositories the caller's GitHub App grant can reach (epic memql#4912, C7). Resolves the caller's active grant -- or the one named by credentialId -- reads its installations LIVE from GitHub and walks each one's repositories, and answers {repositories, installations, pending, nextPage, reason}. Every refusal is a typed reason rather than an error, so the picker renders in place: github_app_not_configured (this cluster has no app, so only the token path is offered), credential_not_found (no grant, or not the caller's), credential_revoked, reconnect_required, rate_limited. Writes nothing except the grant's own installation ids, refreshed from what it just read.",
			Handler:     i.handleSourceRepositories,
			ArgsSchema: map[string]string{
				"credentialId": "string -- a github_app grant of the caller's; empty resolves their active grant",
				"page":         "int -- 1-based page through each installation's repositories, 100 per page; 0 means the first",
			},
		},
		{
			Name:        "artifactProbe",
			Description: "Ask what kind of tree a zip in the caller's Library is (epic memql#4885, D11). Opens it through the same fetch a deploy uses -- the caller's bytes, OpenZip under the packages limits -- and answers {isPackage, isBuiltSite, fileCount, totalBytes}: isPackage when memql-package.yaml sits at the root, isBuiltSite when index.html does and there is no manifest, neither otherwise. Writes nothing.",
			Handler:     i.handleArtifactProbe,
			ArgsSchema: map[string]string{
				"artifactId": "string (required) -- the v1:library:artifact index row of the zip",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (i *Integration) handleAnalyze(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	packageId := strings.TrimSpace(stringArg(args, "packageId"))
	if packageId == "" {
		return nil, refuse(CodeSourceUnreadable, "packageId is required")
	}
	pkg, err := deps.Store.packageById(ctx, packageId)
	if err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, refuse(CodeSourceUnreadable, "no package %q is readable by this caller", packageId)
	}
	snapshot, ferr := deps.fetch(ctx, pkg)
	if ferr != nil {
		return nil, ferr
	}
	defer snapshot.Close()

	rep, aerr := Analyze(snapshot.Tree, Options{SourceVersion: snapshot.Version, Limits: deps.Limits, Logger: i.logger})
	return resultNode(map[string]any{"report": rep, "ok": rep.OK}), aerr
}

func (i *Integration) handleDeploy(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	out, derr := Deploy(ctx, deps, DeployRequest{
		PackageId:  strings.TrimSpace(stringArg(args, "packageId")),
		Actor:      actorFromContext(ctx),
		Confirmed:  boolArg(args, "confirm"),
		Placements: placementsArg(args, "placements"),
		// `automatic` is NOT read from the args and there is no argument for
		// it. "A person did this" is what a call over the wire means, and a
		// caller able to claim otherwise could put a run in the timeline
		// marked as something nobody chose.
		FromDeploymentId: strings.TrimSpace(stringArg(args, "fromDeploymentId")),
	})
	if out == nil {
		return nil, derr
	}
	return resultNode(map[string]any{
		"deploymentId":    out.DeploymentId,
		"status":          out.Status,
		"awaitingConfirm": out.AwaitingConfirm,
		"deployables":     out.Deployables,
		"report":          out.Report,
	}), derr
}

func (i *Integration) handleRollback(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	res, rerr := Rollback(ctx, deps, RollbackRequest{
		PackageId:    strings.TrimSpace(stringArg(args, "packageId")),
		DeploymentId: strings.TrimSpace(stringArg(args, "deploymentId")),
		Actor:        actorFromContext(ctx),
	})
	return resultNode(map[string]any{"restored": res}), rerr
}

func (i *Integration) handleArchiveSite(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	siteId := strings.TrimSpace(stringArg(args, "siteId"))
	site, err := deps.Store.siteById(ctx, siteId)
	if err != nil {
		return nil, err
	}
	if site == nil {
		return nil, refuse(CodeSourceUnreadable, "no site %q is readable by this caller", siteId)
	}
	hostname := rowString(site, "hostname")
	if strings.TrimSpace(stringArg(args, "confirmHostname")) != hostname {
		return nil, refuse("archive_confirmation_mismatch",
			"that is not this site's hostname. Type %q exactly to archive it.", hostname)
	}
	// The disable-first rule and the systemOwned exemption are NOT checked
	// here. They are the write guard's, beside executeWrite, so they hold for
	// every writer rather than only for callers who came through this door.
	if err := deps.Store.setSiteStatus(ctx, siteId, siteStatusArchived); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"siteId": siteId, "hostname": hostname, "status": siteStatusArchived}), nil
}

func (i *Integration) handleRestoreSite(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	siteId := strings.TrimSpace(stringArg(args, "siteId"))
	if err := deps.Store.setSiteStatus(ctx, siteId, siteStatusDisabled); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"siteId": siteId, "status": siteStatusDisabled}), nil
}

func (i *Integration) handleArchivePackage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	packageId := strings.TrimSpace(stringArg(args, "packageId"))
	pkg, err := deps.Store.packageById(ctx, packageId)
	if err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, refuse(CodeSourceUnreadable, "no package %q is readable by this caller", packageId)
	}
	name := rowString(pkg, "name")
	if strings.TrimSpace(stringArg(args, "confirmName")) != name {
		return nil, refuse("archive_confirmation_mismatch",
			"that is not this package's name. Type %q exactly to archive it.", name)
	}

	// THE D10 CROSS-ROW RULE. A package is the source its deployables came
	// from, and filing it away while one is still serving the internet would
	// put the record and the reality in different states. So a LIVE app
	// refuses the whole call, naming only the live hostnames, before any
	// site is touched: pausing is the step that gives anyone still using it
	// a chance to notice, and it stays the person's decision.
	//
	// EVERYTHING ELSE CASCADES (epic memql#4885, design sections A and F --
	// "archive this source and every app it produced"). A disabled app is
	// archived; a draft one -- a first deploy never made live, the commonest
	// state a composed source is abandoned in -- is walked through disabled
	// first, because the status guard admits `archived` from `disabled`
	// alone and a draft resolves for nobody, so the pause it insists on is
	// the law's own path with nobody to notice. An app already archived is
	// left alone. Each write is the same stamped setSiteStatus the site
	// archive uses, and the guard beside executeWrite still decides every
	// one: a refusal surfaces and the cascade STOPS there, sites first and
	// the package last, so the record never claims more than the reality.
	sites, err := deps.Store.sitesForPackage(ctx, packageId)
	if err != nil {
		return nil, err
	}
	var live []string
	for _, s := range sites {
		if rowString(s, "status") == siteStatusLive {
			live = append(live, rowString(s, "hostname"))
		}
	}
	if len(live) > 0 {
		return nil, refuse(CodePackageHasActiveDeployables,
			"this package still has %d deployable(s) still serving (%s). Pause them first -- archiving is the end of a deployable's life, and pausing is the step that gives anyone still using it a chance to notice. Every paused or never-published app is archived with the package.",
			len(live), strings.Join(live, ", "))
	}

	var archivedSites []string
	for _, s := range sites {
		siteId := rowString(s, "id")
		switch rowString(s, "status") {
		case siteStatusArchived:
			continue
		case siteStatusDraft:
			if err := deps.Store.setSiteStatus(ctx, siteId, siteStatusDisabled); err != nil {
				return nil, err
			}
		}
		if err := deps.Store.setSiteStatus(ctx, siteId, siteStatusArchived); err != nil {
			return nil, err
		}
		archivedSites = append(archivedSites, rowString(s, "hostname"))
	}

	if err := deps.Store.setPackageStatus(ctx, packageId, "archived"); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"packageId":     packageId,
		"name":          name,
		"status":        "archived",
		"archivedSites": archivedSites,
	}), nil
}

// handleSetAutoDeploy flips the switch. Under the CALLER's actor, unstamped:
// the mutation is owned and the write guard is the authorization.
func (i *Integration) handleSetAutoDeploy(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	packageId := strings.TrimSpace(stringArg(args, "packageId"))
	if packageId == "" {
		return nil, refuse(CodeSourceUnreadable, "packageId is required")
	}
	on := boolArg(args, "autoDeploy")
	if err := deps.Store.setAutoDeploy(ctx, packageId, on); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"packageId": packageId, "autoDeploy": on}), nil
}

func (i *Integration) handleRestorePackage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	packageId := strings.TrimSpace(stringArg(args, "packageId"))
	if err := deps.Store.setPackageStatus(ctx, packageId, "active"); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"packageId": packageId, "status": "active"}), nil
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

// resolve builds the production Deps LAZILY, once, on first use rather than at
// construction -- the same reasoning component/sitepublish records: the plug-in
// factory runs on every node type and receives only PluginContext, so resolving
// object storage eagerly would build an Azure client on nodes that never
// deploy, or fail the factory on a cluster that hosts no packages at all.
func (i *Integration) resolve() (*Deps, error) {
	i.depsOnce.Do(func() {
		if i.engine == nil {
			i.depsErr = fmt.Errorf("packages: no engine")
			return
		}
		// ONE GitHub App client per node, shared by the store (which renews
		// user tokens), the fetcher and the poll (which mint installation
		// tokens) and the probe (which reads branches and the manifest). One
		// client is one installation-token cache; a second would double the
		// mints against the same rate limit and make "was this cached" a
		// question with two answers. Built even when the cluster has no app
		// configured -- every call then answers github_app_not_configured,
		// which is the operator's fact rather than a nil to remember.
		gh := githubapp.FromEnv()
		s := &store{engine: i.engine, logger: i.logger, github: gh}
		i.deps = &Deps{
			Store:           s,
			Fetcher:         newProductionFetcher(s, i.logger, gh),
			Builder:         NewWorkbenchBuilder(i.workbench, i.logger),
			FleetBuilder:    i.fleet,
			Stager:          newBlobStager(),
			Roller:          newDeployControlRoller(i.logger),
			Publisher:       newEnginePublisher(i.engine, s, i.logger),
			Auditor:         &engineAuditor{engine: i.engine, logger: i.logger},
			Credentials:     s.resolveCredential,
			PeekCredentials: s.peekCredential,
			GitHubApp:       gh,
			Logger:          i.logger,
			Limits:          DefaultLimits(),
		}
	})
	return i.deps, i.depsErr
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, _ := args[key].(string)
	return v
}

func boolArg(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

// placementsArg reads the D8 wire shape: an object of deployable name to
// {hostname, accountId, ownDomain}, every key optional, values trimmed. An
// entry that is not an object is dropped rather than read as an empty
// placement -- the publish stage then refuses the deployable by name for its
// missing hostname, which is a better answer than a silent empty.
func placementsArg(args map[string]any, key string) map[string]Placement {
	out := map[string]Placement{}
	if args == nil {
		return out
	}
	raw, _ := args[key].(map[string]any)
	for name, v := range raw {
		fields, ok := v.(map[string]any)
		if !ok {
			continue
		}
		out[name] = Placement{
			Hostname:  strings.TrimSpace(stringArg(fields, "hostname")),
			AccountId: strings.TrimSpace(stringArg(fields, "accountId")),
			OwnDomain: strings.TrimSpace(stringArg(fields, "ownDomain")),
		}
	}
	return out
}
