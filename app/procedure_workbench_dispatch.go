package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// procedure_workbench_dispatch.go -- a learned procedure's steps on the
// WORKBENCH (epic memql#5408, task memql#5411, D4): the per-run sandboxed
// directory in the cluster, reached through the workbench plug-in's
// `dispatchHost` capability.
//
// THE WORKSPACE IS THE REPLAY RUN'S. The workbench keys a workspace by run id,
// and every call here names the replay run, so a shadow replay starts in an
// empty directory that the app's own session never touched -- nothing the app
// did can make the replay look right -- and two replays never share one.
//
// A SHADOW WRITES NOTHING A PERSON SEES. The workbench promotes every
// successful fs_write into the run owner's Library; a shadow's writes go
// through exec instead (procedureSandboxWriteCommand), which lands the same
// bytes and writes no row. A canary's or trusted replay's write DOES go
// through fs_write, and is reported Delivered for exactly that reason. A shadow's MCP call may only read. Its commands
// still run -- the directory is the isolation, and a command that reaches the
// network reaches it from the workbench, never from the person's machine.
//
// THE WORKBENCH TAKES NO IDEMPOTENCY KEY. Its dispatchHost has nowhere to put
// one, so a step's key goes no further than the replay run's own step receipt,
// which is what keeps the runner from dispatching a step twice.

// workbenchProcedureHost is the workbench, for one replay run.
type workbenchProcedureHost struct {
	handler procedureCapability
	runId   string
	agentId string
	stepId  string
}

func (h *workbenchProcedureHost) label() string { return "the workbench" }

func (h *workbenchProcedureHost) call(ctx context.Context, action string, args map[string]any) (procedureHostReply, error) {
	envelope := map[string]any{"action": action, "args": args, "runId": h.runId}
	if h.agentId != "" {
		envelope["agentId"] = h.agentId
	}
	if h.stepId != "" {
		envelope["stepId"] = h.stepId
	}
	body, err := callProcedureCapability(ctx, h.handler, envelope)
	if err != nil {
		return procedureHostReply{}, fmt.Errorf("the workbench could not be asked to %s: %w", action, err)
	}
	// The workbench's node payload is {ok, action, payload, errorCode,
	// errorMessage} -- dispatchResult, on the local and the forwarded path
	// alike.
	payload, _ := body["payload"].(map[string]any)
	return procedureHostReply{
		OK:           body["ok"] == true,
		ErrorCode:    procedureString(body["errorCode"]),
		ErrorMessage: procedureString(body["errorMessage"]),
		Payload:      payload,
	}, nil
}

// resolvePath keeps a path inside the run's workspace. The workbench refuses
// an absolute path itself; refusing it here says why -- a procedure that
// names one was learned on a machine -- before anything is dispatched.
func (h *workbenchProcedureHost) resolvePath(_ context.Context, p string) (string, string, error) {
	rel, err := procedureWorkspaceRelative(p)
	if err != nil {
		return "", "", err
	}
	return rel, rel, nil
}

// prepareCommand fits a recorded command to the workbench's exec: a Codex
// shell vector gives up its script, and a leading `cd <dir> &&` becomes the
// working directory. Both exist because the exec allowlist admits neither a
// shell binary nor `cd`; both are the command the recording ran, run the way
// the workbench runs commands. See procedureUnwrapShellScript and
// procedureSplitLeadingCd for exactly where they are not identical.
func (h *workbenchProcedureHost) prepareCommand(_ context.Context, cmd procedureCommandLine) (map[string]any, error) {
	line := cmd.Line
	if len(cmd.Argv) > 0 {
		if script, ok := procedureUnwrapShellScript(cmd.Argv); ok {
			line = script
		} else {
			line = procedureJoinArgv(cmd.Argv)
		}
	}
	args := map[string]any{"cmd": line}
	if rest, dir, ok := procedureSplitLeadingCd(line); ok && strings.TrimSpace(rest) != "" {
		args["cmd"] = rest
		if dir != "." {
			args["cwd"] = dir
		}
	}
	return args, nil
}

// delivers is true for an fs_write alone. The workbench promotes every
// successful fs_write into the run owner's Library as a generated output -- a
// row the person sees, outside the run's directory -- so a canary or trusted
// write is a delivery the app must never repeat. A command, a read and a fetch
// stay in the directory (a command that reaches the network is the runner's to
// classify, not this host's), and a shadow's writes go through exec, which
// promotes nothing.
func (h *workbenchProcedureHost) delivers(action string) bool { return action == "fs_write" }

// procedureWorkbenchDispatcher is the procedure.Dispatcher for
// work.TargetWorkbench.
type procedureWorkbenchDispatcher struct {
	// handler is the workbench plug-in's dispatchHost, looked up per step.
	handler func() procedureCapability
	tools   procedureToolCatalog
	agents  procedureAgentResolver
}

var _ procedure.Dispatcher = (*procedureWorkbenchDispatcher)(nil)

// newProcedureWorkbenchDispatcher builds the dispatcher over this node's
// engine: the registered workbench plug-in, the MemQL tool registry, and the
// planner's reasoning-agent rule.
func newProcedureWorkbenchDispatcher(engine *memql.MemQLEngine) *procedureWorkbenchDispatcher {
	return &procedureWorkbenchDispatcher{
		handler: func() procedureCapability { return procedureCapabilityByName(engine, "workbench", "dispatchHost") },
		tools:   engineProcedureTools{engine: engine},
		agents:  procedureReasoningAgents(engine),
	}
}

// Dispatch runs one step in the replay run's workspace.
func (d *procedureWorkbenchDispatcher) Dispatch(ctx context.Context, req procedure.DispatchRequest) (procedure.DispatchResult, error) {
	if req.Target != work.TargetWorkbench {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: the workbench dispatcher was handed a %q step", req.Target)
	}
	owner := strings.TrimSpace(req.OwnerUserId)
	if owner == "" {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: step %s names no owner, and a workspace written under a blank actor is readable by nobody", req.StepKey)
	}
	if strings.TrimSpace(req.RunId) == "" {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: step %s names no replay run, which is what keys its workspace", req.StepKey)
	}
	handler := d.handler()
	if handler == nil {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: this node has no workbench dispatchHost registered, so step %s cannot run here", req.StepKey)
	}
	host := &workbenchProcedureHost{
		handler: handler,
		runId:   strings.TrimSpace(req.RunId),
		agentId: strings.TrimSpace(req.AgentId),
		stepId:  strings.TrimSpace(req.StepKey),
	}
	return runProcedureStep(auth.ContextWithUserActor(ctx, owner), host, d.tools, d.agents, req)
}

// procedureString reads a string field, trimmed.
func procedureString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
