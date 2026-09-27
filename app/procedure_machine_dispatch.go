//go:build agent

package app

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// procedure_machine_dispatch.go -- a learned procedure's steps on the OWNER'S
// MACHINE (epic memql#5408, task memql#5411, D4): the procedures whose
// footprint names that machine's own files, reached through the worker's
// `dispatchHost` capability. Agent-only, because the worker streams terminate
// on the agent node.
//
// THE CONSENT IS THE OWNER'S REASONING AGENT'S. Every call goes through the
// worker dispatcher's own gates -- per-task approval (a run id), the kill
// switch, the agent's standing computer-use scope, the safety classifier --
// under the agent compile resolves for the owner (their assistant, else their
// seeded planner; integrations/planner.ResolveReasoningAgent). A replay
// asks for nothing an agent of theirs was not already allowed to do, and an
// owner with neither agent is refused by name before anything is sent.
//
// NEVER IN A SHADOW. A shadow of a machine-local procedure runs DRY in the
// runner and never dispatches; a request that says Sandbox is refused here as
// well, because the person's machine is the one place a shadow must never
// reach, and a second lock on that door costs one comparison.
//
// THE IDEMPOTENCY KEY rides the worker's correlationId, which is what its
// invocation record carries, so a machine-side effect can be matched to the
// replay step that caused it.
//
// ./ PATHS AND COMMANDS RUN IN THE REPLAY'S OWN WORKSPACE on the machine:
// <the owner's delegation workspaceRoot>/<replay run>. Every recording ran with
// its session workspace as the working directory and relativization rewrote
// that workspace to `.`, so a relative path means "this run's workspace" --
// and without a root there is no such directory to mean, so those steps are
// refused rather than run wherever the worker happens to be. An absolute path
// is the machine's own file, which is what makes the procedure machine-local,
// and is used as recorded.

// procedureMachineReadyMemory bounds how many replay workspaces the dispatcher
// remembers having created. Forgetting one costs one idempotent `mkdir -p`.
const procedureMachineReadyMemory = 256

// procedureReadyDirs remembers the machine workspaces already created.
type procedureReadyDirs struct {
	mu    sync.Mutex
	ready map[string]bool
	order []string
}

func newProcedureReadyDirs() *procedureReadyDirs {
	return &procedureReadyDirs{ready: map[string]bool{}}
}

func (r *procedureReadyDirs) has(dir string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready[dir]
}

func (r *procedureReadyDirs) add(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready[dir] {
		return
	}
	r.ready[dir] = true
	r.order = append(r.order, dir)
	if len(r.order) > procedureMachineReadyMemory {
		delete(r.ready, r.order[0])
		r.order = r.order[1:]
	}
}

// procedureMachineWorkspace is a replay run's directory on the machine.
func procedureMachineWorkspace(root, runId string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("the owner's delegation policy names no workspaceRoot, so a replay has no directory on the machine to run commands and ./ paths in")
	}
	if !path.IsAbs(root) {
		return "", fmt.Errorf("the owner's delegation policy workspaceRoot %q is not an absolute path", root)
	}
	id := memql.BareShortId(runId)
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return "", fmt.Errorf("the replay run id %q cannot name a directory", runId)
	}
	return path.Join(root, id), nil
}

// machineProcedureHost is the owner's machine, for one replay run.
type machineProcedureHost struct {
	handler       procedureCapability
	agentId       string
	owner         string
	runId         string
	stepId        string
	correlationId string
	// root answers the owner's delegation workspaceRoot.
	root  func(ctx context.Context) (string, error)
	ready *procedureReadyDirs

	anchorOnce sync.Once
	anchor     string
	anchorErr  error
}

func (h *machineProcedureHost) label() string { return "the machine" }

func (h *machineProcedureHost) call(ctx context.Context, action string, args map[string]any) (procedureHostReply, error) {
	envelope := map[string]any{
		"action":      action,
		"args":        args,
		"agentId":     h.agentId,
		"ownerUserId": h.owner,
		"runId":       h.runId,
	}
	if h.stepId != "" {
		envelope["stepId"] = h.stepId
	}
	if h.correlationId != "" {
		envelope["correlationId"] = h.correlationId
	}
	body, err := callProcedureCapability(ctx, h.handler, envelope)
	if err != nil {
		return procedureHostReply{}, fmt.Errorf("the machine could not be asked to %s: %w", action, err)
	}
	// The worker's node payload is {ok, output, errorCode, errorMessage, ...},
	// `output` being the cockpit's own JSON for the action -- the same keys
	// the workbench answers with.
	payload, _ := body["output"].(map[string]any)
	return procedureHostReply{
		OK:           body["ok"] == true,
		ErrorCode:    procedureString(body["errorCode"]),
		ErrorMessage: procedureString(body["errorMessage"]),
		Payload:      payload,
	}, nil
}

// workspace is this replay's directory on the machine.
func (h *machineProcedureHost) workspace(ctx context.Context) (string, error) {
	h.anchorOnce.Do(func() {
		if h.root == nil {
			h.anchorErr = fmt.Errorf("this node cannot read the owner's delegation policy")
			return
		}
		root, err := h.root(ctx)
		if err != nil {
			h.anchorErr = fmt.Errorf("the owner's delegation policy could not be read: %w", err)
			return
		}
		h.anchor, h.anchorErr = procedureMachineWorkspace(root, h.runId)
	})
	return h.anchor, h.anchorErr
}

// resolvePath uses an absolute path as recorded and puts a relative one in
// the replay's workspace.
//
// A GOAL SUPPLIES PARAMETERS, AND A PARAMETER CAN SIT IN A PATH. The values are
// checked where they are bound; this is the second lock, on the path the
// machine is actually asked for. A relative path must stay inside the replay's
// workspace (procedureWorkspaceRelative), and an absolute one -- literal as the
// recording wrote it -- may not climb out of the directory it names: a `..`
// segment is the one way a bound value could move it somewhere else, and it is
// refused rather than cleaned away.
func (h *machineProcedureHost) resolvePath(ctx context.Context, p string) (string, string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return "", "", fmt.Errorf("the path is empty")
	case strings.HasPrefix(p, "~"):
		return "", "", fmt.Errorf("%q names a home directory, which a replay cannot resolve for the machine", p)
	case path.IsAbs(p):
		if procedurePathClimbs(p) {
			return "", "", fmt.Errorf("%q climbs out of the directory it names with `..`; an absolute path is run as the recording wrote it, never resolved somewhere else", p)
		}
		clean := path.Clean(p)
		return clean, clean, nil
	}
	rel, err := procedureWorkspaceRelative(p)
	if err != nil {
		return "", "", err
	}
	dir, err := h.workspace(ctx)
	if err != nil {
		return "", "", err
	}
	return path.Join(dir, rel), rel, nil
}

// prepareCommand runs a command in the replay's workspace, made first. A
// Codex vector keeps its shell: the machine has no allowlist to fit it to.
func (h *machineProcedureHost) prepareCommand(ctx context.Context, cmd procedureCommandLine) (map[string]any, error) {
	line := cmd.Line
	if len(cmd.Argv) > 0 {
		line = procedureJoinArgv(cmd.Argv)
	}
	dir, err := h.workspace(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.ensure(ctx, dir); err != nil {
		return nil, err
	}
	return map[string]any{"cmd": line, "cwd": dir}, nil
}

// ensure makes the replay's workspace, once.
func (h *machineProcedureHost) ensure(ctx context.Context, dir string) error {
	if h.ready != nil && h.ready.has(dir) {
		return nil
	}
	reply, err := h.call(ctx, "exec", map[string]any{"cmd": "mkdir -p " + procedureShellQuote(dir), "timeoutSec": procedureProbeTimeoutSec})
	if err != nil {
		return err
	}
	// An unreachable machine is the target's failure, not the procedure's,
	// and it is the step's answer whatever the step was about to run.
	if procedureTargetUnavailable[reply.ErrorCode] {
		return &procedureUnavailable{surface: h.label(), action: "exec", reply: reply}
	}
	if code, ok := procedurePayloadInt(reply.Payload["exitCode"]); reply.ErrorCode != "" || !ok || code != 0 {
		return fmt.Errorf("the replay's workspace %s could not be made on the machine (%s): %s", dir, reply.ErrorCode, reply.ErrorMessage)
	}
	if h.ready != nil {
		h.ready.add(dir)
	}
	return nil
}

// delivers is true: an effect on the person's machine is outside any
// workspace a replay owns.
func (h *machineProcedureHost) delivers(string) bool { return true }

// procedureMachineDispatcher is the procedure.Dispatcher for
// work.TargetMachine.
type procedureMachineDispatcher struct {
	handler       func() procedureCapability
	tools         procedureToolCatalog
	agents        procedureAgentResolver
	workspaceRoot func(ctx context.Context, owner string) (string, error)
	ready         *procedureReadyDirs
}

var _ procedure.Dispatcher = (*procedureMachineDispatcher)(nil)

// newProcedureMachineDispatcher builds the dispatcher over this node's
// engine: the worker's dispatchHost, the MemQL tool registry, the planner's
// reasoning-agent rule, and the owner's delegation policy.
func newProcedureMachineDispatcher(engine *memql.MemQLEngine) *procedureMachineDispatcher {
	store := &agentworker.EngineStore{Engine: engine}
	return &procedureMachineDispatcher{
		handler: func() procedureCapability { return procedureCapabilityByName(engine, "agentworker", "dispatchHost") },
		tools:   engineProcedureTools{engine: engine},
		agents:  procedureReasoningAgents(engine),
		workspaceRoot: func(ctx context.Context, owner string) (string, error) {
			policy, err := store.DelegationPolicy(ctx, owner)
			if err != nil {
				return "", err
			}
			return policy.WorkspaceRoot, nil
		},
		ready: newProcedureReadyDirs(),
	}
}

// Dispatch runs one step on the owner's machine.
func (d *procedureMachineDispatcher) Dispatch(ctx context.Context, req procedure.DispatchRequest) (procedure.DispatchResult, error) {
	if req.Sandbox {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: step %s is a shadow's, and a shadow never touches the person's machine -- a machine-local procedure's shadow runs dry", req.StepKey)
	}
	if req.Target != work.TargetMachine {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: the machine dispatcher was handed a %q step", req.Target)
	}
	owner := strings.TrimSpace(req.OwnerUserId)
	if owner == "" {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: step %s names no owner, and a machine-touching step cannot run unattributed", req.StepKey)
	}
	if strings.TrimSpace(req.RunId) == "" {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: step %s names no replay run, which the worker's per-task approval is keyed on", req.StepKey)
	}
	ctx = auth.ContextWithUserActor(ctx, owner)
	host, err := d.host(ctx, owner, req.RunId, req.StepKey, req.IdempotencyKey, req.AgentId)
	if err != nil {
		return procedure.DispatchResult{}, fmt.Errorf("procedure dispatch: step %s: %w", req.StepKey, err)
	}
	return runProcedureStep(ctx, host, d.tools, d.agents, req)
}

// host is the machine for one replay run, under the owner's reasoning agent.
func (d *procedureMachineDispatcher) host(ctx context.Context, owner, runId, stepId, correlationId, agentId string) (*machineProcedureHost, error) {
	agentId = strings.TrimSpace(agentId)
	if agentId == "" {
		agent, err := d.agents(ctx, owner)
		if err != nil {
			return nil, fmt.Errorf("a step on the machine runs under the owner's reasoning agent, whose standing computer-use scope is the consent, and none resolves: %w", err)
		}
		agentId = agent.Id
	}
	handler := d.handler()
	if handler == nil {
		return nil, fmt.Errorf("this node has no worker dispatchHost registered")
	}
	return &machineProcedureHost{
		handler:       handler,
		agentId:       agentId,
		owner:         owner,
		runId:         strings.TrimSpace(runId),
		stepId:        strings.TrimSpace(stepId),
		correlationId: strings.TrimSpace(correlationId),
		root: func(ctx context.Context) (string, error) {
			if d.workspaceRoot == nil {
				return "", fmt.Errorf("no delegation policy reader")
			}
			return d.workspaceRoot(ctx, owner)
		},
		ready: d.ready,
	}, nil
}

// probeHost is the machine host a probe runs on.
func (d *procedureMachineDispatcher) probeHost(ctx context.Context, owner, runId string) (procedureHost, error) {
	return d.host(ctx, owner, runId, "", "", "")
}
