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
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// mcpAutomationRunner implements mcp.AutomationRunner over the live automation
// Loader + a dedicated manual Executor + the engine.
type mcpAutomationRunner struct {
	loader *automations.Loader
	exec   *automations.Executor
	engine *memql.MemQLEngine
	logger *slog.Logger
}

// newMCPAutomationRunner builds the runner from the app's automation
// infrastructure. The Executor is dedicated to manual MCP runs (separate from
// the core scheduler's), sharing the engine + event bus + step registry so the
// steps, integrations, and SI providers behave identically.
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

	if dryRun {
		src, ok := memql.DSLConstructSource(r.logger, "automation", name)
		if !ok {
			return nil, fmt.Errorf("automation %q not found in the DSL tree (dry-run needs its source)", name)
		}
		req := memql.DryRunRequest{
			AutomationName:   name,
			AutomationSource: src,
			TriggerEvent:     &memql.DryRunTriggerEvent{Topic: "mcp.run." + name, Kind: "manual", Payload: input},
			Mode:             memql.DryRunModeIsolated,
		}
		report, err := memql.RunBundleDryRun(ctx, r.engine, req)
		if err != nil {
			return nil, err
		}
		return map[string]any{"dryRun": true, "ok": report.OK, "report": report}, nil
	}

	auto, err := r.loader.LoadByName(name)
	if err != nil {
		return nil, err
	}
	if auto == nil {
		return nil, fmt.Errorf("automation %q not found", name)
	}
	ev := &events.Event{
		Topic:     "mcp.run." + name,
		Kind:      events.KindNodeCreated,
		Payload:   input,
		Timestamp: time.Now().UTC(),
	}
	execn, err := r.exec.ExecuteWithEvent(r.ownerEnvelope(ctx, owner), auto, "mcp", ev)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"dryRun": false, "status": execn.Status, "executionId": execn.ID}
	if execn.Error != "" {
		out["error"] = execn.Error
	}
	return out, nil
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
	if r.loader == nil {
		return nil
	}
	all, err := r.loader.LoadAll()
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("mcp: failed to enumerate automations for @mcp promotion", "error", err)
		}
		return nil
	}
	out := make([]*automations.Automation, 0)
	for _, a := range all {
		if a != nil && a.MCPPromoted {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
