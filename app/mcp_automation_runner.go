//go:build mcp

package app

// mcp_automation_runner.go -- the app-side AutomationRunner the MCP protocol
// head drives for run_automation + @mcp-promoted automations (MCP epic
// memql#1529 Phase 4 #1534).
//
// The engine does not own automations (the automations package imports the
// engine), so the run/reflect surface is assembled here, where the automation
// Loader + a manual Executor + the engine's Gate-2 dry-run sandbox are all in
// scope, and injected onto the MCP server via SetAutomationRunner.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// mcpAutomationRunner implements mcp.AutomationRunner over the live automation
// Loader + a dedicated manual Executor + the engine.
//
// One runner is built at bootstrap and shared across every MCP session
// (#1599): it carries no per-session state -- the engine, event bus, and
// automation loader are all node-shared, and RunAutomation takes the owner
// from the call args. Sharing it means a connector's per-session tools/list
// no longer rebuilds the runner or re-parses the whole automation tree
// ("automations loaded count=36") on every `initialize`.
type mcpAutomationRunner struct {
	loader *automations.Loader
	exec   *automations.Executor
	engine *memql.MemQLEngine
	logger *slog.Logger

	// promoted memoizes the @mcp-promoted automation set. That set is fixed
	// at DSL load time, so it is enumerated once (a single LoadAll) and
	// reused -- guarded for concurrent access from multiple sessions'
	// tools/list. promotedLoaded only flips on a successful load, so a
	// transient load error retries on the next call rather than caching nil.
	promotedMu     sync.Mutex
	promoted       []*automations.Automation
	promotedLoaded bool
}

// newMCPAutomationRunner builds the runner from the app's automation
// infrastructure. The Executor is dedicated to manual MCP runs (separate from
// the core scheduler's), sharing the engine + event bus + step registry so the
// steps, integrations, and AI providers behave identically.
func newMCPAutomationRunner(a *App) *mcpAutomationRunner {
	exec := automations.NewExecutor(automations.ExecutorOptions{
		Logger:       a.Logger,
		Engine:       a.engine,
		EventBus:     a.eventBus,
		StepRegistry: a.stepRegistry,
	})
	return &mcpAutomationRunner{
		loader: a.automationLoader,
		exec:   exec,
		engine: a.engine,
		logger: a.Logger,
	}
}

// RunAutomation executes a named automation's action chain under the owner's
// envelope with input bound as the synthetic trigger event (skips trigger
// matching). dryRun routes through the Gate-2 sandbox so writes are isolated and
// webhooks recorded-and-blocked -- a safe preview.
func (r *mcpAutomationRunner) RunAutomation(ctx context.Context, owner, name string, input map[string]any, dryRun bool) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("automation name is required")
	}
	if r.loader == nil {
		return nil, fmt.Errorf("automation loader is not wired")
	}

	// Resolve the requested name to a runnable automation through the SAME
	// canonical resolver on BOTH the dry-run and live paths (memql#1663), so
	// they cannot diverge. LoadByName accepts the automation name or a wrapped
	// logic construct's name (full `logicXxx` or bare form) and returns a clear
	// "not a runnable entry point" error for an internal-only logic construct.
	auto, err := r.loader.LoadByName(name)
	if err != nil {
		return nil, err
	}
	if auto == nil {
		return nil, fmt.Errorf("automation %q not found", name)
	}
	// The #2605 analogue for automations (memql#2681): a @disabled
	// automation is not runnable, on the manual MCP path as on any other.
	// Dropping it from the advertised surface alone would leave it
	// RUNNABLE by name -- worse than the function case, which at least
	// refused at call time. Refused on both the dry-run and live paths,
	// since the check precedes the dryRun branch.
	if err := automationRunRefusal(auto); err != nil {
		return nil, err
	}

	if dryRun {
		// Fetch the resolved automation's source by its CANONICAL name -- the
		// dry-run sandbox re-parses the source, and resolving via LoadByName
		// first guarantees the same construct the live path would execute.
		//
		// Same construct is not by itself same behaviour: re-compiling drops
		// the tree loader's trust stamp, so the two paths agreed on WHICH
		// automation to run and disagreed on whether its steps could reach
		// @serverOnly constructs (memql#2890). SourceTrusted below closes that.
		src, ok := memql.DSLConstructSource(r.logger, "automation", auto.Name)
		if !ok {
			return nil, fmt.Errorf("automation %q resolved but its source was not found in the DSL tree (dry-run needs its source)", auto.Name)
		}
		req := memql.DryRunRequest{
			AutomationName:   auto.Name,
			AutomationSource: src,
			TriggerEvent:     &memql.DryRunTriggerEvent{Topic: "mcp.run." + auto.Name, Kind: "manual", Payload: input},
			Mode:             memql.DryRunModeIsolated,
			// The source above came out of the REGISTERED TREE, resolved by the
			// canonical name LoadByName returned, so it is the same body the
			// live branch below executes and earns the same trust. This is the
			// only dry-run caller that may set it: the planner's Gate-2 path
			// compiles an LLM-emitted bundle and correctly leaves it false.
			SourceTrusted: true,
		}
		report, err := memql.RunBundleDryRun(ctx, r.engine, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"dryRun": true, "ok": report.OK, "report": report}, nil
	}

	ev := &events.Event{
		Topic:     "mcp.run." + auto.Name,
		Kind:      events.KindNodeCreated,
		Payload:   input,
		Timestamp: time.Now().UTC(),
	}
	// ExecuteWithClientEvent, NOT ExecuteWithEvent: `input` above is
	// caller-supplied and is bound as the trigger event, so this run must not
	// reach the engine with internal origin however trusted the tree-loaded
	// automation's source is (memql#2888). The client cannot choose the
	// construct the body invokes, but it chooses that construct's ARGUMENT --
	// and a @serverOnly construct carries no caller-scope filter by design, so
	// origin is the whole gate.
	execn, err := r.exec.ExecuteWithClientEvent(r.ownerEnvelope(ctx, owner), auto, "mcp", ev)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"dryRun": false, "status": execn.Status, "executionId": execn.ID}
	if execn.Error != "" {
		out["error"] = execn.Error
	}
	return out, nil
}

// RunInlineAutomation compiles ad-hoc automation .memql source submitted at
// call time and runs its action chain directly, NEVER persisting (#1558). It
// reuses the Phase-4 path verbatim: Loader.CompileSource compiles the source
// (same parse -> resolve -> trigger-normalize -> compile -> validate pipeline
// as on-disk automations, against the loader's registry read-only -- nothing is
// registered), then RunBundleDryRun runs the compiled action chain through the
// manual Executor under the engine's Gate-2 ISOLATED sandbox: reads are real,
// mutations are isolated to an ephemeral sandbox partition, and webhooks are
// recorded-and-blocked. There is no durable-write code path here at all, so the
// run can never touch the live store.
func (r *mcpAutomationRunner) RunInlineAutomation(ctx context.Context, owner, source string, input map[string]any) (map[string]any, error) {
	if strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("automation source is required")
	}
	if r.loader == nil {
		return nil, fmt.Errorf("automation loader is not wired")
	}
	// Compile the submitted source to resolve the automation name (and surface
	// parse/bind diagnostics) before running it. The compile is read-only --
	// nothing is registered into the loader's registry.
	const origin = "inline-automation.memql"
	auto, err := r.loader.CompileSource(source, origin)
	if err != nil {
		return nil, err
	}
	if auto == nil || strings.TrimSpace(auto.Name) == "" {
		return nil, fmt.Errorf("inline automation source did not compile to a named automation")
	}
	req := memql.DryRunRequest{
		AutomationName:   auto.Name,
		AutomationSource: source,
		TriggerEvent:     &memql.DryRunTriggerEvent{Topic: "mcp.inline." + auto.Name, Kind: "manual", Payload: input},
		Mode:             memql.DryRunModeIsolated,
	}
	report, err := memql.RunBundleDryRun(r.ownerEnvelope(ctx, owner), r.engine, req)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"inline":     true,
		"persisted":  false,
		"automation": auto.Name,
		"ok":         report.OK,
		"report":     report,
	}, nil
}

// PromotedAutomationTools returns MCP tool descriptors for every @mcp-promoted
// automation, sorted by name.
func (r *mcpAutomationRunner) PromotedAutomationTools() []map[string]any {
	autos := r.promotedAutomations()
	out := make([]map[string]any, 0, len(autos))
	for _, a := range autos {
		desc := a.Description
		if strings.TrimSpace(desc) == "" {
			desc = "Run the automation " + a.Name + "."
		}
		out = append(out, map[string]any{
			"name":        a.Name,
			"description": desc,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input":   map[string]any{"type": "object", "description": "Payload bound as the synthetic trigger event."},
					"dry_run": map[string]any{"type": "boolean", "description": "Preview without committing writes."},
				},
			},
		})
	}
	return out
}

// IsPromotedAutomation reports whether name is an @mcp-promoted automation.
func (r *mcpAutomationRunner) IsPromotedAutomation(name string) bool {
	name = strings.TrimSpace(name)
	for _, a := range r.promotedAutomations() {
		if a.Name == name {
			return true
		}
	}
	return false
}

// promotedAutomations loads every automation and filters to the @mcp-promoted
// ones. Errors degrade to an empty list (the surface is best-effort discovery).
func (r *mcpAutomationRunner) promotedAutomations() []*automations.Automation {
	r.promotedMu.Lock()
	defer r.promotedMu.Unlock()
	if r.promotedLoaded {
		return r.promoted
	}
	if r.loader == nil {
		return nil
	}
	all, err := r.loader.LoadAll()
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("mcp: failed to enumerate automations for @mcp promotion", "error", err)
		}
		return nil // not cached; retry on the next call
	}
	out := advertisableAutomations(all)
	r.promoted = out
	r.promotedLoaded = true
	return r.promoted
}

// ownerEnvelope stamps the owner's writer AccessContext onto ctx when none is
// present, so a manual automation run executes under the caller's identity.
func (r *mcpAutomationRunner) ownerEnvelope(ctx context.Context, owner string) context.Context {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return ctx
	}
	if _, ok := auth.AccessFromContext(ctx); ok {
		return ctx
	}
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
}

// advertisableAutomations returns the @mcp-promoted automations that may
// appear on the advertised MCP tool surface, sorted by name (memql#2681).
//
// @disabled drops an automation from the surface, matching what
// #2647/#2682 did for the function half -- an advertised construct the
// caller cannot run is a dishonest surface. Memoizing the FILTERED set
// stays correct because Loader.LoadAll reads the embedded DSL tree:
// MCPPromoted and enabled-ness are both load-time properties that cannot
// change without a reload, which is the same reason the promotion set was
// already memoized.
func advertisableAutomations(all []*automations.Automation) []*automations.Automation {
	out := make([]*automations.Automation, 0, len(all))
	for _, a := range all {
		if a == nil || !a.MCPPromoted || !a.IsEnabled() {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// automationRunRefusal is the #2605 analogue for automations
// (memql#2681): a @disabled automation is not runnable, on the manual MCP
// path as anywhere else. Dropping it from the advertised surface alone
// would leave it RUNNABLE by name -- worse than the function case, which
// at least refused at call time. Returns nil when the automation may run.
func automationRunRefusal(auto *automations.Automation) error {
	if auto == nil {
		return nil
	}
	if !auto.IsEnabled() {
		return fmt.Errorf("automation %q is @disabled and cannot be run; remove the @disabled annotation to re-enable it", auto.Name)
	}
	return nil
}
