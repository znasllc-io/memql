package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// delegatedScope reads the server-written contract on EVERY request. A token
// may outlive its session; a cached MCP connection must not keep its authority.
func delegatedScope(ctx context.Context, eng Engine, sessionID string) (context.Context, *common.DelegatedTools, error) {
	if eng == nil {
		return ctx, nil, fmt.Errorf("app session: engine unavailable")
	}
	ctx = memql.ContextWithFreshRead(ctx)
	call, err := namedCallQuery("appSessionById", map[string]any{"sessionId": sessionID})
	if err != nil {
		return ctx, nil, err
	}
	res, err := eng.Execute(ctx, call)
	if err != nil {
		return ctx, nil, err
	}
	row := firstRow(res)
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" || row == nil || memql.BareShortId(stringValue(row["ownerUserId"])) != memql.BareShortId(ac.UserId) {
		return ctx, nil, fmt.Errorf("app session: no owned session is available")
	}
	if status := stringValue(row["status"]); status != "starting" && status != "running" {
		return ctx, nil, fmt.Errorf("app session is no longer active")
	}
	var scope common.DelegatedTools
	if raw := row["toolScope"]; raw != nil {
		data, err := json.Marshal(raw)
		if err != nil {
			return ctx, nil, err
		}
		if err := json.Unmarshal(data, &scope); err != nil {
			return ctx, nil, fmt.Errorf("app session: invalid tool contract: %w", err)
		}
	}
	if len(scope.Tools) == 0 {
		return ctx, &scope, nil
	}
	if scope.AgentID == "" || scope.AgentRole == "" || scope.Run.RunId == "" ||
		memql.BareShortId(scope.Run.OwnerUserId) != memql.BareShortId(ac.UserId) ||
		memql.BareShortId(scope.Run.RunId) != memql.BareShortId(stringValue(row["runId"])) || memql.BareShortId(scope.Run.StepKey) != memql.BareShortId(stringValue(row["stepId"])) {
		return ctx, nil, fmt.Errorf("app session: tool contract does not match its owned run")
	}
	call, err = namedCallQuery("workRunForOwner", map[string]any{"runId": scope.Run.RunId})
	if err != nil {
		return ctx, nil, err
	}
	res, err = eng.Execute(ctx, call)
	if err != nil {
		return ctx, nil, err
	}
	run := firstRow(res)
	if run == nil || stringValue(run["status"]) != "running" {
		return ctx, nil, fmt.Errorf("app session: the original work run is not running")
	}
	ctx = memql.WithActingAgentId(ctx, scope.AgentID)
	ctx = memql.WithActingAgentRole(ctx, scope.AgentRole)
	ctx = common.ContextWithRun(ctx, scope.Run)
	return ctx, &scope, nil
}

func stringValue(v any) string { s, _ := v.(string); return strings.TrimSpace(s) }

// Listing validates the complete contract: a partial list would spend a model
// turn discovering a deployment mismatch. Human MCP clients keep their own menu.
func delegatedMCPTools(ctx context.Context, eng Engine, sessionID string) ([]map[string]any, error) {
	ctx, scope, err := delegatedScope(ctx, eng, sessionID)
	if err != nil {
		return nil, err
	}
	result := appSessionToolDefs()
	for _, tool := range scope.Tools {
		if tool.Name != common.DelegatedResponseTool {
			if eng.Tools() == nil {
				return nil, fmt.Errorf("app session: tool registry unavailable")
			}
			registered, err := eng.Tools().Get(tool.Name)
			if err != nil || registered == nil || !eng.ToolListed(ctx, registered) {
				return nil, fmt.Errorf("app session: required tool %q is unavailable for the delegated agent", tool.Name)
			}
		}
		result = append(result, map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": tool.InputSchema})
	}
	return result, nil
}

func callDelegatedTool(ctx context.Context, eng Engine, sessionID, name string, args map[string]any) (result map[string]any) {
	recordCtx := ctx
	defer func() { recordAppSessionToolCall(recordCtx, sessionID, name, args, result) }()
	ctx, scope, err := delegatedScope(ctx, eng, sessionID)
	if err != nil {
		return errorResult(err.Error())
	}
	if name == toolNextTask {
		return handleNextTask(ctx, eng, sessionID)
	}
	if name == toolSubmit {
		return handleSubmit(ctx, eng, sessionID, args)
	}
	allowed := false
	for _, tool := range scope.Tools {
		if tool.Name == name {
			allowed = true
			break
		}
	}
	if !allowed {
		return errorResult(fmt.Sprintf("tool %q was not delegated to this app session", name))
	}
	if name == common.DelegatedResponseTool {
		// The parent agent loop validates and consumes this terminal signal.
		// It is never executed as another model turn or an arbitrary tool call.
		return handleSubmit(ctx, eng, sessionID, map[string]any{"result": map[string]any{"terminalTool": name, "arguments": args}})
	}
	// Apply the agent's own stamps, including nested provenance. Never mark
	// this as a human MCP call: auto-injected fields must remain unforgeable.
	args = overlayToolDefaults(args, scope.Defaults[name])
	ctx = common.ContextWithToolDefaults(ctx, scope.Defaults[name])
	raw, err := eng.ExecuteToolByName(ctx, name, args)
	if err != nil {
		return errorResult(fmt.Sprintf("tool %q failed: %v", name, err))
	}
	return toolResultFromJSON(raw)
}

func overlayToolDefaults(args, defaults map[string]any) map[string]any {
	out := make(map[string]any, len(args)+len(defaults))
	for key, value := range args {
		out[key] = value
	}
	for key, value := range defaults {
		if nested, ok := value.(map[string]any); ok {
			original, _ := out[key].(map[string]any)
			out[key] = overlayToolDefaults(original, nested)
		} else {
			out[key] = value
		}
	}
	return out
}
