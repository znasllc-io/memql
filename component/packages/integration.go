package packages

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
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

	depsOnce sync.Once
	deps     *Deps
	depsErr  error
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
			Description: "Run one deployment attempt for a package (epic memql#4794). Without confirm, the run parks at awaiting_confirm with the analysis report on the deployment row and nothing else happens; with confirm, it builds, stages, rolls and publishes in the D6 order. Returns {deploymentId, status, awaitingConfirm, deployables}.",
			Handler:     i.handleDeploy,
			ArgsSchema: map[string]string{
				"packageId": "string (required) -- the package to deploy",
				"confirm":   "boolean -- pass true to proceed past the always-present confirm gate",
				"hostnames": "object -- deployable name -> hostname, required on a deployable's FIRST deploy only",
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
			Description: "Archive a package after verifying the typed name and that no deployable is still serving (epic memql#4794, D10).",
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
		PackageId: strings.TrimSpace(stringArg(args, "packageId")),
		Actor:     actorFromContext(ctx),
		Confirmed: boolArg(args, "confirm"),
		Hostnames: stringMapArg(args, "hostnames"),
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
	// put the record and the reality in different states.
	sites, err := deps.Store.sitesForPackage(ctx, packageId)
	if err != nil {
		return nil, err
	}
	var live []string
	for _, s := range sites {
		if rowString(s, "status") != siteStatusArchived {
			live = append(live, rowString(s, "hostname"))
		}
	}
	if len(live) > 0 {
		return nil, refuse(CodePackageHasActiveDeployables,
			"this package still has %d deployable(s) that are not archived (%s). Archive them first -- a package is the source its sites came from, and filing it away while one is still serving would leave the record and the reality disagreeing.",
			len(live), strings.Join(live, ", "))
	}

	if err := deps.Store.setPackageStatus(ctx, packageId, "archived"); err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"packageId": packageId, "name": name, "status": "archived"}), nil
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
		s := &store{engine: i.engine, logger: i.logger}
		i.deps = &Deps{
			Store:           s,
			Fetcher:         newProductionFetcher(s, i.logger),
			Stager:          newBlobStager(),
			Roller:          newDeployControlRoller(i.logger),
			Publisher:       newEnginePublisher(i.engine, s, i.logger),
			Auditor:         &engineAuditor{engine: i.engine, logger: i.logger},
			Credentials:     s.resolveCredential,
			PeekCredentials: s.peekCredential,
			Logger:          i.logger,
			Limits:          DefaultLimits(),
			// Builder is deliberately nil. See builder.go.
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

func stringMapArg(args map[string]any, key string) map[string]string {
	out := map[string]string{}
	if args == nil {
		return out
	}
	raw, _ := args[key].(map[string]any)
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
