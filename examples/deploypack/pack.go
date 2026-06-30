// Package deploypack is the memQL DEPLOY PACK (Epic 2 / #2095) -- the dogfood
// pack that packages memQL's own deployment workflow as a memQL pack. It is the
// production-shaped sibling of examples/referencepack: same pack primitives,
// but the capabilities are the REAL deploy effects that component/deploycontrol
// performs imperatively today.
//
//   - dsl.RegisterTree(domain, Tree())     -- mount the pack's embedded
//     dsl/builtins.memql under the "deploypack" namespace.
//   - memql.RegisterPluginForContract(...) -- register the Go IntegrationProvider
//     against an explicit Plugin SDK contract version.
//   - an IntegrationProvider whose Capabilities() back the four deploy builtins
//     (commitOverlay / argoSync / runPromote / recordBack) via the
//     @executor("integration.deploypack.<cap>") FQNs.
//
// SAFETY: every effect routes through the EXISTING component/deploycontrol
// Executor boundary (promote.sh / git / kubectl argo rollouts). This pack is
// ADDITIVE -- it does NOT replace the imperative orchestration (that is E2.3 /
// E2.5); it exposes the same effects declaratively so chained automations can
// fire them on deployment status transitions. The live azure deploy path
// (runPromote -> scripts/release/promote.sh via the Argo Rollout) is invoked
// through the same RunPromote the Deploy Console uses -- unchanged.
//
// Like referencepack, the pack carries NO unconditional self-registering
// init() in this file: linking it in does NOT load it. Registration is opt-in
// -- tests call Register(...) explicitly; register_deploypack.go carries the
// //go:build deploypack init() (the build-tag-gated auto-register pattern). The
// `deploypack` tag is not set in any default node binary, so the pack never
// auto-loads where it is not wanted.
package deploypack

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns. dsl.RegisterTree validates it via
// ValidatePackDomain and panics on a collision (namespace ownership). The
// embedded subtree is mounted under "deploypack/" in the unified DSL tree.
const Domain = "deploypack"

// ContractVersion is the Plugin SDK contract version this pack was built
// against, pinned so the loader fails loudly if the pack is linked into a core
// whose memql.PluginContractVersion is incompatible.
const ContractVersion = memql.PluginContractVersion

// integrationName is the IntegrationProvider's stable identifier -- the middle
// segment of every capability FQN "integration.<integrationName>.<cap>". It
// MUST match the @executor namespace in dsl/builtins.memql.
const integrationName = "deploypack"

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree, rooted so dsl/builtins.memql
// appears directly. Pass to dsl.RegisterTree(Domain, Tree()) to mount it.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		panic("deploypack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// engineExecer is the narrow slice of the engine the recordBack effect needs:
// run a DSL mutation. Both memql.IntegrationEngineAccess (pctx.Engine) and the
// full engine satisfy it, so the pack stays testable with a fake.
type engineExecer interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// Provider is the deploy pack's IntegrationProvider. It owns the deploycontrol
// Executor (the SAME side-effect boundary the Deploy Console uses) and an
// engine handle for the record-back mutation.
type Provider struct {
	exec   deploycontrol.Executor
	engine engineExecer
}

// IntegrationName implements memql.IntegrationProvider.
func (p *Provider) IntegrationName() string { return integrationName }

// Capabilities implements memql.IntegrationProvider. Each capability Name
// combines with IntegrationName() into the FQN the matching builtin's
// @executor names: integration.deploypack.<Name>.
func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "commitOverlay",
			Description: "Stage + commit the env's kustomize overlay via the deploycontrol Executor (git). Model A: the committed overlay is the GitOps source of truth.",
			Handler:     p.commitOverlay,
			ArgsSchema: map[string]string{
				"env":     "string (required) - staging | prod",
				"message": "string (optional) - commit message",
			},
		},
		{
			Name:        "argoSync",
			Description: "Push the committed overlay so ArgoCD reconciles it (git push via the deploycontrol Executor). Never bypasses ArgoCD with a direct apply.",
			Handler:     p.argoSync,
			ArgsSchema: map[string]string{
				"env": "string (required) - staging | prod",
			},
		},
		{
			Name:        "runPromote",
			Description: "Run scripts/release/promote.sh --version --env via the deploycontrol Executor -- THE live azure deploy effect (digest-pin overlay + Argo Rollout cutover). Unchanged from the Deploy Console path.",
			Handler:     p.runPromote,
			ArgsSchema: map[string]string{
				"version": "string (required) - release version to promote",
				"env":     "string (required) - staging | prod",
			},
		},
		{
			Name:        "recordBack",
			Description: "Model A record-back: append the reconciled deployment status (+ optional per-node spec) into the deployment concept via the E2.1 mutations.",
			Handler:     p.recordBack,
			ArgsSchema: map[string]string{
				"deploymentId": "string (required) - deployment to record against",
				"status":       "string (optional) - reconciled lifecycle status",
				"nodeType":     "string (optional) - per-node spec node type",
				"version":      "string (optional) - per-node version pin (empty = engine-as-spine)",
				"replicas":     "int (optional) - per-node replica count",
				"imageDigest":  "string (optional) - resolved image digest",
			},
		},
		{
			Name:        "observeReconciledState",
			Description: "Model A read leg: read the ArgoCD app's reconciled state via the Executor (kubectl -n argocd get app -o json) and map it with deploycontrol.MapArgoStatus. Returns synced/healthy/revision/outOfSync.",
			Handler:     p.observeReconciledState,
			ArgsSchema: map[string]string{
				"app": "string (optional) - ArgoCD application name (defaults to the configured app)",
			},
		},
	}
}

// resultNode wraps a handler's effect output in a single object node so the
// engine dispatch path has a uniform return shape. The `success` field lets a
// driving automation branch on the effect's outcome (e.g. promote succeeded ->
// transition the deployment to succeeded, else failed) WITHOUT the handler
// returning a Go error -- a Go error aborts the automation step, so a recoverable
// effect failure that the lifecycle must react to is reported in-band instead.
func resultNode(id, effect, output string, success bool) []memorynodes.MemoryNode {
	payload, _ := json.Marshal(map[string]any{
		"effect":      effect,
		"output":      output,
		"success":     success,
		"recordedAt":  time.Now().UTC().Format(time.RFC3339),
		"integration": integrationName,
	})
	return []memorynodes.MemoryNode{{
		ID:        id,
		Concept:   "integration:deploypack:effect",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}

func argString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

// commitOverlay stages the env's overlay and commits it via the Executor's git
// boundary. Model A: the pack authors + commits the overlay; ArgoCD reconciles.
func (p *Provider) commitOverlay(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	env := argString(args, "env")
	if env == "" {
		return nil, fmt.Errorf("deploypack.commitOverlay requires env")
	}
	if p.exec == nil {
		return nil, fmt.Errorf("deploypack.commitOverlay: no Executor wired")
	}
	overlayPath := fmt.Sprintf("deploy/k8s/overlays/%s/kustomization.yaml", env)
	message := argString(args, "message")
	if message == "" {
		message = fmt.Sprintf("deploypack: commit %s overlay", env)
	}
	if _, err := p.exec.Git(ctx, "add", overlayPath); err != nil {
		return nil, fmt.Errorf("deploypack.commitOverlay: git add %s: %w", overlayPath, err)
	}
	out, err := p.exec.Git(ctx, "commit", "-m", message, "--", overlayPath)
	if err != nil {
		return nil, fmt.Errorf("deploypack.commitOverlay: git commit: %w (%s)", err, out)
	}
	return resultNode("deploypack-commit:"+env, "commitOverlay", out, true), nil
}

// argoSync pushes the committed overlay so ArgoCD reconciles it. The pack never
// applies to the cluster directly -- ArgoCD owns reconciliation.
func (p *Provider) argoSync(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	env := argString(args, "env")
	if env == "" {
		return nil, fmt.Errorf("deploypack.argoSync requires env")
	}
	if p.exec == nil {
		return nil, fmt.Errorf("deploypack.argoSync: no Executor wired")
	}
	out, err := p.exec.Git(ctx, "push")
	if err != nil {
		return nil, fmt.Errorf("deploypack.argoSync: git push: %w (%s)", err, out)
	}
	return resultNode("deploypack-sync:"+env, "argoSync", out, true), nil
}

// runPromote runs the live azure deploy effect (promote.sh via the Argo
// Rollout) through the SAME RunPromote the Deploy Console uses. The `env` arg
// is normalized through deploycontrol.ConsoleEnvFor, so a caller may pass the
// deployment.environment enum (production / staging) or the console env
// (prod / staging) -- the SAME mapping the Go driver applies, kept in one place.
//
// A promote FAILURE is reported in-band (success=false in the result node), NOT
// as a Go error: this effect drives a deployment LIFECYCLE automation (#2096),
// and a Go error would abort the automation step before it could transition the
// deployment to `failed`. A misconfiguration (missing version / unmapped env /
// no Executor) is still a Go error -- that is a wiring fault, not a deploy
// outcome the lifecycle reacts to.
func (p *Provider) runPromote(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	version := argString(args, "version")
	env := deploycontrol.ConsoleEnvFor(argString(args, "env"))
	if version == "" {
		return nil, fmt.Errorf("deploypack.runPromote requires version")
	}
	if env == "" {
		return nil, fmt.Errorf("deploypack.runPromote: unsupported env (want staging|production|prod)")
	}
	if p.exec == nil {
		return nil, fmt.Errorf("deploypack.runPromote: no Executor wired")
	}
	out, err := p.exec.RunPromote(ctx, version, env)
	if err != nil {
		// In-band failure: the lifecycle automation reads success=false and
		// transitions the deployment to failed. Carry the error in output.
		return resultNode("deploypack-promote:"+env+":"+version, "runPromote",
			fmt.Sprintf("%v: %s", err, out), false), nil
	}
	return resultNode("deploypack-promote:"+env+":"+version, "runPromote", out, true), nil
}

// recordBack appends the reconciled state into the deployment concept via the
// E2.1 mutations: a status transition (updateDeploymentStatus) and/or a
// per-node spec (createDeploymentNodeSpec). Model A: the committed overlay
// stays the source of truth; this mirrors the GitOps-observed state into the
// data layer.
func (p *Provider) recordBack(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deploymentID := argString(args, "deploymentId")
	if deploymentID == "" {
		return nil, fmt.Errorf("deploypack.recordBack requires deploymentId")
	}
	if p.engine == nil {
		return nil, fmt.Errorf("deploypack.recordBack: no engine wired")
	}

	var effects []string

	if status := argString(args, "status"); status != "" {
		call := fmt.Sprintf("mutation updateDeploymentStatus(deploymentId: %q, status: %q)", deploymentID, status)
		if _, err := p.engine.Execute(ctx, call); err != nil {
			return nil, fmt.Errorf("deploypack.recordBack: updateDeploymentStatus: %w", err)
		}
		effects = append(effects, "status="+status)
	}

	if nodeType := argString(args, "nodeType"); nodeType != "" {
		version := argString(args, "version")
		imageDigest := argString(args, "imageDigest")
		replicas := 1
		switch r := args["replicas"].(type) {
		case int:
			replicas = r
		case int64:
			replicas = int(r)
		case float64:
			replicas = int(r)
		}
		call := fmt.Sprintf(
			"mutation createDeploymentNodeSpec(deploymentId: %q, nodeType: %q, version: %q, replicas: %d, imageDigest: %q)",
			deploymentID, nodeType, version, replicas, imageDigest,
		)
		if _, err := p.engine.Execute(ctx, call); err != nil {
			return nil, fmt.Errorf("deploypack.recordBack: createDeploymentNodeSpec: %w", err)
		}
		effects = append(effects, "nodeSpec="+nodeType)
	}

	return resultNode("deploypack-recordback:"+deploymentID, "recordBack", strings.Join(effects, ","), true), nil
}

// defaultArgoApp is the ArgoCD application name observeReconciledState reads
// when the caller passes no app. MEMQL_DEPLOY_ARGO_APP overrides; "memql" is the
// app the deploycontrol status path reads (kubectl -n argocd get app memql).
func (p *Provider) defaultArgoApp() string {
	if app := strings.TrimSpace(os.Getenv("MEMQL_DEPLOY_ARGO_APP")); app != "" {
		return app
	}
	return "memql"
}

// observeReconciledState is the Model A READ leg (#2097): read the ArgoCD
// application's reconciled state through the SAME Executor boundary the
// deploycontrol status path uses (kubectl -n argocd get app <app> -o json) and
// map it with deploycontrol.MapArgoStatus. It returns the observed sync/health
// in the result node so the record-back automation can mirror it into the
// deployment concept. Read-only: it never mutates the cluster.
func (p *Provider) observeReconciledState(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if p.exec == nil {
		return nil, fmt.Errorf("deploypack.observeReconciledState: no Executor wired")
	}
	app := argString(args, "app")
	if app == "" {
		app = p.defaultArgoApp()
	}
	raw, err := p.exec.KubectlJSON(ctx, "-n", "argocd", "get", "app", app, "-o", "json")
	if err != nil {
		// In-band failure (like runPromote): a transient read failure must not
		// abort the record-back automation step -- it simply observes "not yet
		// synced" and the loop re-observes on the next deployment update.
		return observationNode(app, "", "", "", false, false, fmt.Sprintf("%v", err)), nil
	}
	argo, err := deploycontrol.MapArgoStatus(raw)
	if err != nil {
		return observationNode(app, "", "", "", false, false, fmt.Sprintf("%v", err)), nil
	}
	synced := argo.GetSyncStatus() == "Synced"
	healthy := argo.GetHealthStatus() == "Healthy"
	return observationNode(app, argo.GetSyncStatus(), argo.GetHealthStatus(),
		argo.GetLastSyncRevision(), synced, healthy, ""), nil
}

// observationNode builds the result node observeReconciledState returns. The
// boolean synced/healthy + the reconciled status strings let the record-back
// automation branch (synced && healthy -> append succeeded) without re-parsing.
func observationNode(app, syncStatus, healthStatus, revision string, synced, healthy bool, errMsg string) []memorynodes.MemoryNode {
	payload, _ := json.Marshal(map[string]any{
		"effect":       "observeReconciledState",
		"app":          app,
		"syncStatus":   syncStatus,
		"healthStatus": healthStatus,
		"revision":     revision,
		"synced":       synced,
		"healthy":      healthy,
		"error":        errMsg,
		"recordedAt":   time.Now().UTC().Format(time.RFC3339),
		"integration":  integrationName,
	})
	return []memorynodes.MemoryNode{{
		ID:        "deploypack-observe:" + app,
		Concept:   "integration:deploypack:observation",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}

// NewProvider builds the pack's IntegrationProvider from a PluginContext. The
// deploy Executor is anchored at MEMQL_DEPLOY_REPO_ROOT (or the process working
// directory), mirroring app/integrations_deploy_control.go's RepoRoot
// resolution so the pack drives the SAME promote.sh / git wiring. The engine
// handle (pctx.Engine) backs the record-back mutation.
func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	repoRoot := os.Getenv("MEMQL_DEPLOY_REPO_ROOT")
	return &Provider{
		exec:   deploycontrol.NewExecutor(repoRoot),
		engine: pctx.Engine,
	}, nil
}

// NewProviderWithDeps builds the provider with explicit dependencies. Used by
// tests to inject a fake Executor + engine so the deploy effects can be
// exercised without a real cluster or checkout.
func NewProviderWithDeps(exec deploycontrol.Executor, engine engineExecer) *Provider {
	return &Provider{exec: exec, engine: engine}
}

// Register wires the pack into the engine registries: mount the embedded tree
// and register the Go IntegrationProvider against the pinned contract version.
// domain is a parameter (not hardcoded Domain) so a test can register under a
// throwaway namespace and tear it down with dsl.UnregisterTree.
func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
}
