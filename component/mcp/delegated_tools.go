package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/num"
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
	rerun, _ := run["rerun"].(map[string]any)
	if cancelled, _ := run["cancelRequested"].(bool); cancelled ||
		memql.BareShortId(stringValue(run["ownerUserId"])) != memql.BareShortId(scope.Run.OwnerUserId) ||
		memql.BareShortId(stringValue(run["goalId"])) != memql.BareShortId(scope.Run.GoalId) ||
		stringValue(rerun["requestId"]) != scope.Run.RerunRequestId {
		return ctx, nil, fmt.Errorf("app session: the original work execution is no longer current")
	}
	grant, _ := run["executionAuthority"].(map[string]any)
	if run["executionAuthority"] != nil && grant == nil {
		return ctx, nil, fmt.Errorf("app session: invalid recorded work authority")
	}
	ctx = memql.WithActingAgentId(ctx, scope.AgentID)
	ctx = memql.WithActingAgentRole(ctx, scope.AgentRole)
	ctx = common.ContextWithRun(ctx, scope.Run)
	ctx, err = delegatedForwardAuthority(ctx, ac, sessionID, eng, grant)
	if err != nil {
		return ctx, nil, err
	}
	return ctx, &scope, nil
}

// Only an authenticated HTTP app-session credential can originate a machine
// hop. A verified delegated run can restore its captured authority, clamped to
// the owner's current role, while retaining the app credential and its expiry.
// Generic app sessions and no-auth MCP configurations acquire no owner role.
// narrowing: GUARDED -- credential expiry must be a whole int64 Unix second;
// num.WholeInt64 refuses non-finite, fractional, or out-of-range claims.
func delegatedForwardAuthority(ctx context.Context, ac *auth.AccessContext, sessionID string, eng Engine, grant map[string]any) (context.Context, error) {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		if len(grant) != 0 {
			return ctx, fmt.Errorf("app session: recorded work authority requires a verified credential")
		}
		return ctx, nil
	}
	if AppSessionFromClaims(claims) != sessionID || memql.BareShortId(stringValue(claims["sub"])) != memql.BareShortId(ac.UserId) {
		return ctx, fmt.Errorf("app session: credential does not match its owned session")
	}
	var seconds int64
	switch value := claims["exp"].(type) {
	case float64:
		var valid bool
		seconds, valid = num.WholeInt64(value)
		if !valid {
			return ctx, fmt.Errorf("app session: credential has an invalid expiry")
		}
	case int64:
		seconds = value
	case int:
		seconds = int64(value)
	case json.Number:
		var err error
		seconds, err = value.Int64()
		if err != nil {
			return ctx, fmt.Errorf("app session: credential has an invalid expiry")
		}
	}
	now := time.Now()
	expires := time.Unix(seconds, 0)
	if seconds <= 0 || !now.Before(expires) {
		return ctx, fmt.Errorf("app session: credential has no active expiry")
	}
	if len(grant) != 0 {
		resolver := auth.NewIdentityResolver(auth.QueryRunnerFunc(func(readCtx context.Context, query string) (any, error) {
			res, err := eng.Execute(readCtx, query)
			if err != nil || res == nil {
				return nil, err
			}
			if flat, ok := res.FlatOutput(); ok {
				return flat, nil
			}
			return firstRow(res), nil
		}), slog.Default())
		restored, err := auth.ContextWithPersistedOwner(ctx, ac.UserId, grant, resolver)
		if err != nil {
			return ctx, fmt.Errorf("app session: restore recorded work authority: %w", err)
		}
		ac, _ = auth.AccessFromContext(restored)
		parent, _ := auth.ForwardedAuthorityFromContext(restored)
		if !parent.ExpiresAt.IsZero() && parent.ExpiresAt.Before(expires) {
			expires = parent.ExpiresAt
		}
		// Copy only resolved access: replacing claims/token here would turn
		// the app credential into its parent's credential on downstream hops.
		ctx = auth.ContextWithAccess(ctx, ac)
	}
	authority, err := auth.ForwardedAuthorityForUser(ac, auth.ForwardedClassAppSession, "", expires, now)
	if err != nil {
		return ctx, err
	}
	if _, err := auth.VerifyForwardedAuthority(authority, now); err != nil {
		return ctx, err
	}
	return auth.ContextWithForwardedAuthority(ctx, authority), nil
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
		schema := tool.InputSchema
		if tool.Name != common.DelegatedResponseTool {
			if eng.Tools() == nil {
				return nil, fmt.Errorf("app session: tool registry unavailable")
			}
			registered, err := eng.Tools().Get(tool.Name)
			if err != nil || registered == nil || !eng.ToolListed(ctx, registered) {
				return nil, fmt.Errorf("app session: required tool %q is unavailable for the delegated agent", tool.Name)
			}
			schema, err = delegatedInputSchema(tool, registered.AutoInjectedFields, scope.Defaults[tool.Name])
			if err != nil {
				return nil, err
			}
		}
		result = append(result, map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": schema})
	}
	return result, nil
}

// Server-owned arguments are not inputs the app must discover or invent. The
// durable contract stays intact for dispatch; only the model-facing copy loses
// these fields. Refuse incomplete required stamps during preflight.
func delegatedInputSchema(tool common.ToolDefinition, injected []string, defaults map[string]any) (any, error) {
	if len(injected) == 0 {
		return tool.InputSchema, nil
	}
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	properties, _ := schema["properties"].(map[string]any)
	removed := make(map[string]bool, len(injected))
	for _, name := range injected {
		removed[name] = true
		delete(properties, name)
	}
	required, _ := schema["required"].([]any)
	kept := make([]any, 0, len(required))
	for _, value := range required {
		name, _ := value.(string)
		if !removed[name] {
			kept = append(kept, value)
			continue
		}
		stamp, present := defaults[name]
		if !present || stamp == nil || stamp == "" {
			return nil, fmt.Errorf("app session: required server argument %q for tool %q is unresolved", name, tool.Name)
		}
	}
	if _, present := schema["required"]; present {
		schema["required"] = kept
	}
	return schema, nil
}

func callDelegatedTool(ctx context.Context, eng Engine, sessionID, name string, args map[string]any) (result map[string]any) {
	recordCtx := ctx
	defer func() { recordAppSessionToolCall(recordCtx, sessionID, name, args, result) }()
	ctx, scope, err := delegatedScope(ctx, eng, sessionID)
	if err != nil {
		return errorResult(err.Error())
	}
	// Recording can embed its observation on the owner's machine too. Keep
	// the verified credential, without inheriting the agent's execution marks
	// or tool defaults. Never manufacture authority for an unverified scope.
	if authority, ok := auth.ForwardedAuthorityFromContext(ctx); ok {
		recordCtx = auth.ContextWithForwardedAuthority(recordCtx, authority)
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
		result = errorResult(fmt.Sprintf("tool %q failed: %v", name, err))
		encoded, _ := json.Marshal(result)
		raw = string(encoded)
	} else {
		result = toolResultFromJSON(raw)
	}
	if recorder, ok := eng.(interface {
		RecordDelegatedToolSource(context.Context, string, map[string]any, string) error
	}); ok {
		if err := recorder.RecordDelegatedToolSource(context.WithoutCancel(ctx), name, args, raw); err != nil {
			// The action already happened. Do not disguise a receipt failure as
			// an execution failure that the app might replay.
			slog.Warn("delegated tool source could not be saved", "tool", name, "session_id", sessionID, "error", err)
		}
	}
	return result
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
