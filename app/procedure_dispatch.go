package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/mcp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/planner"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// procedure_dispatch.go -- what the replay's dispatchers share (epic
// memql#5408, task memql#5411): running one materialized step on a HOST --
// the workbench, or the owner's machine -- and reporting it in the terms
// component/work compares.
//
// A HOST IS A dispatchHost CAPABILITY, NOT A PACKAGE. The workbench and the
// worker each publish one, behind their own gates -- the environment hint,
// the safety classifier, the exec allowlist, per-task approval, the kill
// switch, the standing scope. The replay drives them through that ONE entry
// point, the way runScript does (app/integrations_skills.go), so it cannot
// skip a gate, and this file needs neither package: the worker's is behind the
// agent build tag and the workbench's is everywhere.
//
// THE HANDLER IS LOOKED UP WHEN A STEP RUNS, not when the seam is installed.
// The worker integration registers in the agent's transport phase, after
// integrationsCore installs these seams; resolving by name at dispatch time is
// what makes the wiring order irrelevant, and a surface that is missing then
// is refused by name rather than frozen as nil at boot.

// procedureCapability is the shape of memql.IntegrationCapability.Handler.
// Declared here because the field's type is unexported; Go func types are
// structural, so the assignment is exact.
type procedureCapability func(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error)

// procedureCapabilityByName plucks one integration's capability handler off
// the engine's registry, or nil.
func procedureCapabilityByName(engine *memql.MemQLEngine, integration, capability string) procedureCapability {
	if engine == nil {
		return nil
	}
	provider := engine.IntegrationByName(integration)
	if provider == nil {
		return nil
	}
	for _, c := range provider.Capabilities() {
		if c.Name == capability && c.Handler != nil {
			return procedureCapability(c.Handler)
		}
	}
	return nil
}

// callProcedureCapability runs a capability and decodes the one node it
// answers with.
func callProcedureCapability(ctx context.Context, handler procedureCapability, envelope map[string]any) (map[string]any, error) {
	nodes, err := handler(ctx, envelope, 1)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("it answered nothing")
	}
	var body map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &body); err != nil {
		return nil, fmt.Errorf("its answer was unreadable: %w", err)
	}
	return body, nil
}

// procedureHost is one surface a replay's steps run on.
type procedureHost interface {
	// label names the surface in a message.
	label() string
	// call runs one dispatchHost action. A Go error means the surface could
	// not be asked at all; anything it answered is a reply.
	call(ctx context.Context, action string, args map[string]any) (procedureHostReply, error)
	// resolvePath maps a path a step names onto the one this surface is asked
	// for, and returns the spelling an observation reports.
	resolvePath(ctx context.Context, p string) (hostPath, reported string, err error)
	// prepareCommand is the exec arguments a step's command runs with here.
	prepareCommand(ctx context.Context, cmd procedureCommandLine) (map[string]any, error)
	// delivers reports whether an effect on this surface reaches outside the
	// replay's own workspace -- the person's machine does, the workbench's
	// per-run directory does not.
	delivers() bool
}

// procedureAgentResolver answers the owner's reasoning agent.
type procedureAgentResolver func(ctx context.Context, owner string) (planner.ReasoningAgent, error)

// procedureToolCatalog is the MemQL tool registry, narrowed to what an `mcp`
// step needs.
type procedureToolCatalog interface {
	// kindOf answers what the named tool's handler calls -- a construct kind
	// (query, mutation, logic, builtin, automation) or "webhook" -- and false
	// when no such tool is registered.
	kindOf(name string) (kind string, ok bool)
	// execute runs it as the context's actor and returns the marshalled
	// ToolCallResult.
	execute(ctx context.Context, name string, args map[string]any) (string, error)
}

// procedureMCPServer is the name a MemQL tool call arrives under when an app
// spells the server (`mcp__memql__<tool>`, Codex's {server, tool}).
const procedureMCPServer = "memql"

// procedureReadCap bounds a read. It is the workbench's own fs_read ceiling;
// asking for more is answered with a truncated read, which cannot be digested
// or edited.
const procedureReadCap = 1 << 20

// runProcedureStep runs one materialized step on host.
func runProcedureStep(
	ctx context.Context,
	host procedureHost,
	tools procedureToolCatalog,
	agents procedureAgentResolver,
	req procedure.DispatchRequest,
) (procedure.DispatchResult, error) {
	args := req.Args
	if args == nil {
		args = map[string]any{}
	}
	var (
		res procedure.DispatchResult
		err error
	)
	switch req.Tool {
	case work.StepTypeExec:
		res, err = runProcedureExec(ctx, host, req.Timeout, args)
	case work.StepTypeFSWrite:
		res, err = runProcedureWrite(ctx, host, req.Sandbox, args)
	case work.StepTypeFSRead:
		res, err = runProcedureRead(ctx, host, args)
	case work.StepTypeFetch:
		res, err = runProcedureFetch(ctx, host, args)
	case work.StepTypeMCP:
		res, err = runProcedureMCP(ctx, tools, agents, req, args)
	default:
		err = fmt.Errorf("%q is not a tool a replay can run (exec, fs_write, fs_read, fetch, mcp)", req.Tool)
	}
	if err != nil {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: step %s (%s): %w", req.StepKey, req.Tool, err)
	}
	return res, nil
}

// runProcedureExec runs a recorded command, bounded by the step's timeout.
func runProcedureExec(ctx context.Context, host procedureHost, timeout time.Duration, args map[string]any) (procedure.DispatchResult, error) {
	if background, _ := args["run_in_background"].(bool); background {
		return procedure.DispatchResult{}, fmt.Errorf("it ran in the background, so what the app recorded is a job handle rather than the command's result, and no replay of it could compare")
	}
	cmd, err := procedureCommandOf(args)
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	execArgs, err := host.prepareCommand(ctx, cmd)
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	if t := procedureTimeoutSec(timeout, args); t > 0 {
		execArgs["timeoutSec"] = t
	}
	reply, err := host.call(ctx, "exec", execArgs)
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	if procedureRefusedBeforeRunning[reply.ErrorCode] {
		return procedure.DispatchResult{}, procedureRefusal(host.label(), "the command", reply)
	}
	obs, out := procedureExecObservation(reply)
	return procedure.DispatchResult{Observation: obs, Output: out, Delivered: host.delivers()}, nil
}

// runProcedureWrite writes a file: a Write's whole content, or an Edit's or a
// MultiEdit's replacements applied to what the file holds.
func runProcedureWrite(ctx context.Context, host procedureHost, sandbox bool, args map[string]any) (procedure.DispatchResult, error) {
	p, err := procedureFilePathOf(args)
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	hostPath, reported, err := host.resolvePath(ctx, p)
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	plan, err := procedureWritePlanOf(args)
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	out := map[string]any{"action": "fs_write", "kind": plan.Kind, "path": reported}

	content := plan.Content
	if plan.Kind != "write" {
		current, exists, err := readProcedureFile(ctx, host, hostPath)
		if err != nil {
			return procedure.DispatchResult{}, err
		}
		edited, refusal := applyProcedureEdits(current, exists, plan.Edits)
		if refusal != nil {
			// THE APP'S OWN REFUSAL, reported as its tool would have: the
			// step ran, found the file other than the procedure expects, and
			// wrote nothing. The comparison is what stops the replay here.
			out["error"] = refusal.Error()
			return procedure.DispatchResult{Observation: procedureFileObservation("write", false, reported, ""), Output: out}, nil
		}
		content = edited
	}

	var ok bool
	if sandbox {
		reply, err := host.call(ctx, "exec", map[string]any{
			"cmd":   procedureSandboxWriteCommand(hostPath),
			"stdin": content,
		})
		if err != nil {
			return procedure.DispatchResult{}, err
		}
		if procedureRefusedBeforeRunning[reply.ErrorCode] {
			return procedure.DispatchResult{}, procedureRefusal(host.label(), "the write", reply)
		}
		code, hasCode := procedurePayloadInt(reply.Payload["exitCode"])
		ok = hasCode && code == 0 && reply.ErrorCode == ""
		if !ok {
			out["errorCode"], out["errorMessage"] = reply.ErrorCode, reply.ErrorMessage
		}
	} else {
		reply, err := host.call(ctx, "fs_write", map[string]any{"path": hostPath, "content": content})
		if err != nil {
			return procedure.DispatchResult{}, err
		}
		if procedureRefusedBeforeRunning[reply.ErrorCode] {
			return procedure.DispatchResult{}, procedureRefusal(host.label(), "the write", reply)
		}
		ok = reply.OK
		if !ok {
			out["errorCode"], out["errorMessage"] = reply.ErrorCode, reply.ErrorMessage
		}
	}
	digest := ""
	if ok {
		digest = procedureDigest(content)
		out["bytes"] = len(content)
		out["digest"] = digest
	}
	return procedure.DispatchResult{
		Observation: procedureFileObservation("write", ok, reported, digest),
		Output:      out,
		Delivered:   ok && host.delivers(),
	}, nil
}

// readProcedureFile reads the whole of a file an edit applies to. exists is
// false for a file that is not there -- the one case an empty old_string may
// create -- and an error means it could not be read at all, which is a
// replay that cannot run rather than one the app would have refused.
func readProcedureFile(ctx context.Context, host procedureHost, hostPath string) (string, bool, error) {
	reply, err := host.call(ctx, "fs_read", map[string]any{"path": hostPath, "maxBytes": procedureReadCap})
	if err != nil {
		return "", false, err
	}
	if reply.OK {
		if truncated, _ := reply.Payload["truncated"].(bool); truncated {
			return "", false, fmt.Errorf("%s holds more than %d bytes, and an edit applied to part of a file would write a different file", hostPath, procedureReadCap)
		}
		content, _ := reply.Payload["content"].(string)
		return content, true, nil
	}
	if procedureRefusedBeforeRunning[reply.ErrorCode] {
		return "", false, procedureRefusal(host.label(), "the read an edit needs", reply)
	}
	stat, err := host.call(ctx, "fs_stat", map[string]any{"path": hostPath})
	if err != nil {
		return "", false, err
	}
	if exists, known := stat.Payload["exists"].(bool); stat.OK && known && !exists {
		return "", false, nil
	}
	return "", false, fmt.Errorf("%s could not be read for the edit (%s): %s", hostPath, reply.ErrorCode, reply.ErrorMessage)
}

// runProcedureRead reads a file and reports its digest. Claude Code's
// offset/limit choose which LINES the app was shown; the file's bytes are the
// same whichever were, and its digest is what a read is compared on.
func runProcedureRead(ctx context.Context, host procedureHost, args map[string]any) (procedure.DispatchResult, error) {
	p, err := procedureFilePathOf(args)
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	hostPath, reported, err := host.resolvePath(ctx, p)
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	reply, err := host.call(ctx, "fs_read", map[string]any{"path": hostPath, "maxBytes": procedureReadCap})
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	if procedureRefusedBeforeRunning[reply.ErrorCode] {
		return procedure.DispatchResult{}, procedureRefusal(host.label(), "the read", reply)
	}
	out := map[string]any{"action": "fs_read", "path": reported}
	if !reply.OK {
		out["errorCode"], out["errorMessage"] = reply.ErrorCode, reply.ErrorMessage
		return procedure.DispatchResult{Observation: procedureFileObservation("read", false, reported, ""), Output: out}, nil
	}
	content, _ := reply.Payload["content"].(string)
	digest := ""
	if truncated, _ := reply.Payload["truncated"].(bool); truncated {
		// A truncated read is no measurement of the file. The digest stays
		// absent, and a comparison that expects one refuses it.
		out["truncated"] = true
	} else {
		digest = procedureDigest(content)
		out["digest"] = digest
	}
	out["bytes"] = len(content)
	return procedure.DispatchResult{Observation: procedureFileObservation("read", true, reported, digest), Output: out}, nil
}

// runProcedureFetch fetches a URL. Always a GET: the app's WebFetch reads a
// page, and its prompt -- the question a model answered about that page -- is
// the one part no replay performs.
func runProcedureFetch(ctx context.Context, host procedureHost, args map[string]any) (procedure.DispatchResult, error) {
	url, _ := args["url"].(string)
	if strings.TrimSpace(url) == "" {
		return procedure.DispatchResult{}, fmt.Errorf("it names no url (its arguments are %s)", procedureKeyList(args))
	}
	reply, err := host.call(ctx, "http_fetch", map[string]any{"url": strings.TrimSpace(url), "method": "GET"})
	if err != nil {
		return procedure.DispatchResult{}, err
	}
	if procedureRefusedBeforeRunning[reply.ErrorCode] {
		return procedure.DispatchResult{}, procedureRefusal(host.label(), "the fetch", reply)
	}
	obs, out := procedureFetchObservation(reply)
	return procedure.DispatchResult{Observation: obs, Output: out}, nil
}

// runProcedureMCP runs a recorded call back into MemQL. It runs IN THE
// CLUSTER whatever the procedure's target, because that is where the app's
// call ran too: over MCP, as its owner.
//
// A SHADOW REPLAY RUNS ONLY A TOOL THAT READS. Its handler must call a query;
// a mutation, a logic, a builtin, an automation or a webhook could write, and
// a shadow writes nothing.
func runProcedureMCP(
	ctx context.Context,
	tools procedureToolCatalog,
	agents procedureAgentResolver,
	req procedure.DispatchRequest,
	args map[string]any,
) (procedure.DispatchResult, error) {
	if tools == nil {
		return procedure.DispatchResult{}, fmt.Errorf("this node has no MemQL tool registry to run it with")
	}
	name, _ := args["tool"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return procedure.DispatchResult{}, fmt.Errorf("it names no MemQL tool (its arguments are %s)", procedureKeyList(args))
	}
	if server, _ := args["server"].(string); strings.TrimSpace(server) != "" && strings.TrimSpace(server) != procedureMCPServer {
		return procedure.DispatchResult{}, fmt.Errorf("it called %q on the %q MCP server, and only a call back into MemQL is replayable", name, server)
	}
	kind, registered := tools.kindOf(name)
	if !registered {
		return procedure.DispatchResult{}, fmt.Errorf("%q is not a registered MemQL tool, so there is nothing here to call", name)
	}
	readOnly := kind == "query"
	if req.Sandbox && !readOnly {
		return procedure.DispatchResult{}, fmt.Errorf("a shadow replay runs only a tool that reads, and %q calls a %s", name, kind)
	}
	if agents == nil {
		return procedure.DispatchResult{}, fmt.Errorf("this node cannot resolve the agent a tool call acts as")
	}
	agent, err := agents(ctx, req.OwnerUserId)
	if err != nil {
		return procedure.DispatchResult{}, fmt.Errorf("the owner's reasoning agent, which a tool call acts as: %w", err)
	}

	var callArgs map[string]any
	if raw, present := args["arguments"]; present && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return procedure.DispatchResult{}, fmt.Errorf("its arguments are a %T, not an object", raw)
		}
		callArgs = make(map[string]any, len(m))
		for k, v := range m {
			callArgs[k] = v
		}
	}

	// AS THE APP'S CALL RAN: the owner's actor, an acting agent (a tool is
	// agent-only), the MCP marker that lets a recorded @autoInjected value
	// stand where the server has none, and the server's own owner and agent,
	// which always win over a recorded one.
	callCtx := auth.ContextWithUserActor(ctx, req.OwnerUserId)
	callCtx = memql.WithActingAgentRole(callCtx, agent.RoleSlug)
	callCtx = memql.WithActingAgentId(callCtx, agent.Id)
	callCtx = memql.WithMCPToolExecution(callCtx)
	callCtx = common.ContextWithToolDefaults(callCtx, map[string]any{"ownerUserId": req.OwnerUserId, "agentId": agent.Id})

	raw, err := tools.execute(callCtx, name, callArgs)
	if err != nil {
		return procedure.DispatchResult{}, fmt.Errorf("%q did not run: %w", name, err)
	}
	isError, resultType := mcp.RecordedToolObservables(raw)
	return procedure.DispatchResult{
		Observation: work.StepObservation{IsError: &isError, ResultType: resultType},
		Output: map[string]any{
			"action":     "mcp",
			"tool":       name,
			"kind":       kind,
			"isError":    isError,
			"resultType": resultType,
			"digest":     procedureDigest(raw),
		},
		// A MemQL row write is outside the replay's workspace by definition.
		Delivered: !readOnly && !isError,
	}, nil
}

// engineProcedureTools is the tool registry the engine holds.
type engineProcedureTools struct{ engine *memql.MemQLEngine }

func (t engineProcedureTools) kindOf(name string) (string, bool) {
	if t.engine == nil || t.engine.Tools() == nil {
		return "", false
	}
	tool, err := t.engine.Tools().Get(strings.TrimSpace(name))
	if err != nil || tool == nil || tool.Handler == nil {
		return "", false
	}
	switch tool.Handler.Type {
	case "function":
		kind, _, ok := t.engine.ToolCallee(name)
		if !ok {
			return "function", true
		}
		return kind, true
	case "query":
		return procedureQueryHandlerKind(tool.Handler.Query), true
	}
	return tool.Handler.Type, true
}

func (t engineProcedureTools) execute(ctx context.Context, name string, args map[string]any) (string, error) {
	if t.engine == nil {
		return "", fmt.Errorf("no engine")
	}
	return t.engine.ExecuteToolByName(ctx, name, args)
}

// procedureQueryHandlerKind answers which construct kind a tool's QUERY
// handler calls. The handler type does not say: `@handler(type="query",
// query="mutation updateCalendarEvent(...)")` is a query HANDLER that writes.
// It is one construct call, or that call inside paginate(...), and anything
// this cannot read answers "unknown" -- which is not "query", so a shadow
// refuses it.
func procedureQueryHandlerKind(src string) string {
	n, err := langparser.ParseV1Expression(src)
	if err != nil {
		return "unknown"
	}
	call, ok := ast.Unparen(n).(*ast.CallExpr)
	if !ok || call == nil {
		return "unknown"
	}
	if call.Kind == "" && call.Receiver == nil && call.Name == "paginate" && len(call.Args) > 0 {
		inner, ok := ast.Unparen(call.Args[0]).(*ast.CallExpr)
		if !ok || inner == nil {
			return "unknown"
		}
		call = inner
	}
	if call.Kind == "" {
		return "unknown"
	}
	return call.Kind
}

// procedureEngineQuerier adapts the engine to the planner's narrow reader, so
// the replay resolves the reasoning agent through the planner's own rule.
type procedureEngineQuerier struct{ engine *memql.MemQLEngine }

func (q procedureEngineQuerier) Execute(ctx context.Context, query string) (any, error) {
	if q.engine == nil {
		return nil, fmt.Errorf("no engine")
	}
	return q.engine.Execute(ctx, query)
}

// procedureReasoningAgents resolves the owner's reasoning agent with the rule
// compile uses (integrations/planner.ResolveReasoningAgent).
func procedureReasoningAgents(engine *memql.MemQLEngine) procedureAgentResolver {
	return func(ctx context.Context, owner string) (planner.ReasoningAgent, error) {
		if strings.TrimSpace(owner) == "" {
			return planner.ReasoningAgent{}, fmt.Errorf("the step names no owner")
		}
		return planner.ResolveReasoningAgent(ctx, procedureEngineQuerier{engine: engine}, owner)
	}
}
