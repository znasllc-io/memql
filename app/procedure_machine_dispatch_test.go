//go:build agent

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/integrations/planner"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// procedure_machine_dispatch_test.go -- the machine dispatcher against a fake
// worker dispatchHost that answers the way integrations/agent/worker does:
// {ok, output, errorCode, errorMessage}, the cockpit's own JSON under
// `output`.

func newFakeMachine() *fakeHostWorkspace {
	return &fakeHostWorkspace{envelope: "output", files: map[string]string{}, refuse: map[string]string{}}
}

const testMachineRoot = "/Users/someone/memql-work"

func newTestMachineDispatcher(m *fakeHostWorkspace, root string) *procedureMachineDispatcher {
	return &procedureMachineDispatcher{
		handler:       m.handler,
		agents:        testAgents,
		workspaceRoot: func(context.Context, string) (string, error) { return root, nil },
		ready:         newProcedureReadyDirs(),
	}
}

func machineStep(tool string, args map[string]any) procedure.DispatchRequest {
	req := workbenchStep(tool, args)
	req.Target = work.TargetMachine
	return req
}

// The consent is the owner's reasoning agent's standing scope, so every call
// names it; the idempotency key rides the worker's correlationId, which its
// invocation record carries.
func TestAMachineStepRunsAsTheOwnersReasoningAgentCarryingItsIdempotencyKey(t *testing.T) {
	m := newFakeMachine()
	res, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(),
		machineStep("fs_write", map[string]any{"file_path": "/Users/someone/notes/today.md", "content": "- ship it\n"}))
	if err != nil {
		t.Fatal(err)
	}
	call := m.calls[0]
	for key, want := range map[string]any{
		"action":        "fs_write",
		"agentId":       testReasoningAgent.Id,
		"ownerUserId":   "v1:identity:user:owner",
		"runId":         "v1:work:run:replay1",
		"stepId":        "step0",
		"correlationId": "idem-0",
	} {
		if call[key] != want {
			t.Errorf("envelope %s = %v, want %v", key, call[key], want)
		}
	}
	want := work.ContentDigest{Op: "write", Path: "/Users/someone/notes/today.md", Digest: procedureDigest("- ship it\n")}
	if o := res.Observation; o.IsError == nil || *o.IsError || len(o.Contents) != 1 || o.Contents[0] != want {
		t.Fatalf("observation = %+v, want %+v", o, want)
	}
	if !res.Delivered {
		t.Fatal("a write to the person's machine is outside any workspace a replay owns")
	}
}

func TestAMachineStepUsesTheAgentTheRequestNames(t *testing.T) {
	m := newFakeMachine()
	d := newTestMachineDispatcher(m, testMachineRoot)
	d.agents = func(context.Context, string) (planner.ReasoningAgent, error) {
		t.Fatal("the reasoning agent was resolved although the request named one")
		return planner.ReasoningAgent{}, nil
	}
	req := machineStep("fs_read", map[string]any{"file_path": "/etc/hosts"})
	req.AgentId = "v1:agents:agent:named"
	if _, err := d.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if m.calls[0]["agentId"] != "v1:agents:agent:named" {
		t.Fatalf("agentId = %v", m.calls[0]["agentId"])
	}
}

func TestAMachineStepWithNoReasoningAgentIsRefusedBeforeAnythingIsSent(t *testing.T) {
	m := newFakeMachine()
	d := newTestMachineDispatcher(m, testMachineRoot)
	d.agents = func(context.Context, string) (planner.ReasoningAgent, error) {
		return planner.ReasoningAgent{}, errors.New("work compile: no active assistant or seeded planner exists for this goal's owner")
	}
	_, err := d.Dispatch(context.Background(), machineStep("fs_read", map[string]any{"file_path": "/etc/hosts"}))
	if err == nil || !strings.Contains(err.Error(), "reasoning agent") || !strings.Contains(err.Error(), "no active assistant or seeded planner") {
		t.Fatalf("err = %v", err)
	}
	if len(m.calls) != 0 {
		t.Fatalf("a step with no agent reached the machine: %v", m.actions())
	}
}

// A SHADOW NEVER TOUCHES THE PERSON'S MACHINE. The runner runs a
// machine-local shadow dry; this is the second lock on the same door.
func TestAMachineNeverRunsAShadowsStep(t *testing.T) {
	m := newFakeMachine()
	req := machineStep("fs_read", map[string]any{"file_path": "/etc/hosts"})
	req.Sandbox = true
	if _, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(), req); err == nil || !strings.Contains(err.Error(), "shadow") {
		t.Fatalf("err = %v", err)
	}
	if len(m.calls) != 0 {
		t.Fatalf("a shadow reached the machine: %v", m.actions())
	}
}

func TestTheMachineDispatcherRefusesWhatIsNotItsStep(t *testing.T) {
	m := newFakeMachine()
	d := newTestMachineDispatcher(m, testMachineRoot)
	for name, mutate := range map[string]func(*procedure.DispatchRequest){
		"workbench target": func(r *procedure.DispatchRequest) { r.Target = work.TargetWorkbench },
		"no owner":         func(r *procedure.DispatchRequest) { r.OwnerUserId = "" },
		"no run":           func(r *procedure.DispatchRequest) { r.RunId = "" },
	} {
		req := machineStep("fs_read", map[string]any{"file_path": "/etc/hosts"})
		mutate(&req)
		if _, err := d.Dispatch(context.Background(), req); err == nil {
			t.Errorf("%s was dispatched", name)
		}
	}
	missing := newTestMachineDispatcher(m, testMachineRoot)
	missing.handler = func() procedureCapability { return nil }
	if _, err := missing.Dispatch(context.Background(), machineStep("fs_read", map[string]any{"file_path": "/etc/hosts"})); err == nil || !strings.Contains(err.Error(), "no worker dispatchHost") {
		t.Fatalf("a node with no worker = %v", err)
	}
	if len(m.calls) != 0 {
		t.Fatalf("a refused step reached the machine: %v", m.actions())
	}
}

// Every recording ran with its session workspace as the working directory,
// so a command replays in the replay's own: made once, then used.
func TestAMachineCommandRunsInTheReplaysOwnWorkspace(t *testing.T) {
	m := newFakeMachine()
	m.exec = func(map[string]any) map[string]any { return map[string]any{"exitCode": 0, "stdout": "ok\n"} }
	d := newTestMachineDispatcher(m, testMachineRoot)
	for i := 0; i < 2; i++ {
		res, err := d.Dispatch(context.Background(), machineStep("exec", map[string]any{"command": "cd . && make build"}))
		if err != nil {
			t.Fatal(err)
		}
		if o := res.Observation; o.ExitCode == nil || *o.ExitCode != 0 || !res.Delivered {
			t.Fatalf("observation = %+v, delivered %v", o, res.Delivered)
		}
	}
	cmds := execCommands(m)
	want := []string{"mkdir -p " + testMachineRoot + "/replay1", "cd . && make build", "cd . && make build"}
	if strings.Join(cmds, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %q, want %q -- the workspace made once", cmds, want)
	}
	if args := m.lastArgs(); args["cwd"] != testMachineRoot+"/replay1" {
		t.Fatalf("exec args = %+v", args)
	}
}

// The step's timeout reaches the machine's command the way it reaches the
// workbench's: from the request, beside the template.
func TestTheRequestsTimeoutReachesTheMachineExec(t *testing.T) {
	m := newFakeMachine()
	req := machineStep("exec", map[string]any{"command": "make build"})
	req.Timeout = 180 * time.Second
	if _, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if args := m.lastArgs(); args["cmd"] != "make build" || args["timeoutSec"] != 180 {
		t.Fatalf("exec args = %+v, want the command with timeoutSec 180", args)
	}
}

func TestAMachineCommandKeepsCodexsShell(t *testing.T) {
	m := newFakeMachine()
	if _, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(),
		machineStep("exec", map[string]any{"command": []any{"bash", "-lc", "make test"}})); err != nil {
		t.Fatal(err)
	}
	if args := m.lastArgs(); args["cmd"] != "bash -lc 'make test'" {
		t.Fatalf("exec args = %+v, want the vector as recorded", args)
	}
}

// WITHOUT A ROOT THERE IS NO WORKSPACE TO MEAN, and a command or a ./ path
// is refused rather than run wherever the worker happens to be.
func TestWithNoWorkspaceRootACommandAndARelativePathAreRefused(t *testing.T) {
	for tool, args := range map[string]map[string]any{
		"exec":     {"command": "make"},
		"fs_write": {"file_path": "./out/x.txt", "content": "x"},
	} {
		m := newFakeMachine()
		_, err := newTestMachineDispatcher(m, "").Dispatch(context.Background(), machineStep(tool, args))
		if err == nil || !strings.Contains(err.Error(), "workspaceRoot") {
			t.Errorf("%s -> %v", tool, err)
		}
		if len(m.calls) != 0 {
			t.Errorf("%s reached the machine: %v", tool, m.actions())
		}
	}
	// An absolute path needs no workspace.
	m := newFakeMachine()
	if _, err := newTestMachineDispatcher(m, "").Dispatch(context.Background(), machineStep("fs_read", map[string]any{"file_path": "/etc/hosts"})); err != nil {
		t.Fatalf("an absolute read = %v", err)
	}
}

func TestARelativePathOnTheMachineLandsInTheReplaysWorkspace(t *testing.T) {
	m := newFakeMachine()
	res, err := newTestMachineDispatcher(m, testMachineRoot+"/").Dispatch(context.Background(),
		machineStep("fs_write", map[string]any{"file_path": "./out/x.txt", "content": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if args := m.lastArgs(); args["path"] != testMachineRoot+"/replay1/out/x.txt" {
		t.Fatalf("fs_write args = %+v", args)
	}
	if o := res.Observation; len(o.Contents) != 1 || o.Contents[0].Path != "out/x.txt" {
		t.Fatalf("observation = %+v, want the workspace-relative path the recording has", o)
	}
}

// The worker's gates run before a dispatch leaves the node, so a denial is
// a step that did not run -- a Go error, never an observation.
func TestAMachineGateDenialIsAnError(t *testing.T) {
	for _, code := range []string{"denied_by_scope", "kill_switch_engaged", "denied_no_per_task_approval", "no_worker_available", "worker_busy"} {
		m := newFakeMachine()
		m.refuse["fs_write"] = code
		_, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(),
			machineStep("fs_write", map[string]any{"file_path": "/tmp/x", "content": "x"}))
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Errorf("%s -> %v", code, err)
		}
	}
	// A lost answer MAY have run, so it is an observation, and the step is
	// not handed back to the app as one it can safely repeat.
	m := newFakeMachine()
	m.refuse["fs_write"] = "worker_disconnected"
	res, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(),
		machineStep("fs_write", map[string]any{"file_path": "/tmp/x", "content": "x"}))
	if err != nil || res.Observation.IsError == nil || !*res.Observation.IsError {
		t.Fatalf("worker_disconnected = %+v, %v", res.Observation, err)
	}
}

// A DROPPED STREAM OR AN UNREACHABLE REPLICA SAYS NOTHING ABOUT THE PROCEDURE.
// The step is a failed observation, as before, and Unavailable -- which is what
// keeps the ladder from counting a machine that went to sleep against the
// procedure. The workspace a command needs made first answers the same way.
func TestAnUnreachableMachineIsUnavailableNotAFailedProcedure(t *testing.T) {
	for _, code := range []string{"worker_disconnected", "worker_unreachable"} {
		for name, req := range map[string]procedure.DispatchRequest{
			"fs_write": machineStep("fs_write", map[string]any{"file_path": "/tmp/x", "content": "x"}),
			"fs_read":  machineStep("fs_read", map[string]any{"file_path": "/etc/hosts"}),
			"exec":     machineStep("exec", map[string]any{"command": "make"}),
		} {
			m := newFakeMachine()
			for _, action := range []string{"exec", "fs_write", "fs_read", "fs_stat"} {
				m.refuse[action] = code
			}
			res, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(), req)
			if err != nil {
				t.Errorf("%s %s: %v -- an unreachable machine is a result the runner reads", code, name, err)
				continue
			}
			if !res.Unavailable || res.Observation.IsError == nil || !*res.Observation.IsError || res.Delivered {
				t.Errorf("%s %s = %+v unavailable %v delivered %v; want a failed, undelivered, Unavailable step",
					code, name, res.Observation, res.Unavailable, res.Delivered)
			}
		}
	}
	// A timeout is the procedure's: counted, not excused.
	m := newFakeMachine()
	m.exec = func(map[string]any) map[string]any { return map[string]any{"exitCode": 0} }
	d := newTestMachineDispatcher(m, testMachineRoot)
	if _, err := d.Dispatch(context.Background(), machineStep("exec", map[string]any{"command": "true"})); err != nil {
		t.Fatal(err)
	}
	m.refuse["exec"] = "timeout"
	res, err := d.Dispatch(context.Background(), machineStep("exec", map[string]any{"command": "make"}))
	if err != nil || res.Unavailable || res.Observation.IsError == nil || !*res.Observation.IsError {
		t.Fatalf("a timeout = %+v unavailable %v, %v", res.Observation, res.Unavailable, err)
	}
}

// A GOAL SUPPLIES A PARAMETER, AND A PARAMETER CAN SIT IN A PATH. Whatever the
// value, a relative path that climbs out of the replay's workspace is refused
// before anything is sent -- the one directory a replay owns on the person's
// machine -- and one that stays inside lands there.
func TestAMachinePathThatLeavesTheReplaysWorkspaceIsRefused(t *testing.T) {
	for _, p := range []string{"../.ssh/authorized_keys", "a/../../x", "./out/../../x"} {
		for tool, args := range map[string]map[string]any{
			"fs_write": {"file_path": p, "content": "ssh-ed25519 AAAA planted\n"},
			"fs_read":  {"file_path": p},
		} {
			m := newFakeMachine()
			_, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(), machineStep(tool, args))
			if err == nil || !strings.Contains(err.Error(), "leaves the replay's workspace") {
				t.Errorf("%s %q -> %v, want a refusal that it leaves the workspace", tool, p, err)
			}
			if len(m.calls) != 0 {
				t.Errorf("%s %q reached the machine: %v", tool, p, m.actions())
			}
		}
	}

	m := newFakeMachine()
	res, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(),
		machineStep("fs_write", map[string]any{"file_path": "./out/a.txt", "content": "a\n"}))
	if err != nil {
		t.Fatalf("a path inside the workspace = %v", err)
	}
	if args := m.lastArgs(); args["path"] != testMachineRoot+"/replay1/out/a.txt" {
		t.Fatalf("fs_write args = %+v, want the file in the replay's workspace", args)
	}
	if o := res.Observation; len(o.Contents) != 1 || o.Contents[0].Path != "out/a.txt" {
		t.Fatalf("observation = %+v", o)
	}
}

// AN ABSOLUTE PATH IS USED AS THE RECORDING WROTE IT -- it is what makes the
// procedure machine-local -- and the one way a goal-supplied value could move
// it is a `..` segment climbing out of the directory it names. That is
// refused; the path as written is not.
func TestAnAbsoluteMachinePathIsLiteralAndMayNotClimb(t *testing.T) {
	for _, p := range []string{"/Users/someone/notes/../../.ssh/authorized_keys", "/Users/someone/notes/.."} {
		m := newFakeMachine()
		_, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(),
			machineStep("fs_write", map[string]any{"file_path": p, "content": "x"}))
		if err == nil || !strings.Contains(err.Error(), "..") {
			t.Errorf("%q -> %v, want it refused for climbing", p, err)
		}
		if len(m.calls) != 0 {
			t.Errorf("%q reached the machine: %v", p, m.actions())
		}
	}
	m := newFakeMachine()
	if _, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(),
		machineStep("fs_write", map[string]any{"file_path": "/Users/someone/notes/./today.md", "content": "x"})); err != nil {
		t.Fatalf("an absolute path that climbs nowhere = %v", err)
	}
	if args := m.lastArgs(); args["path"] != "/Users/someone/notes/today.md" {
		t.Fatalf("fs_write args = %+v", args)
	}
}

func TestAHomePathOnTheMachineIsRefused(t *testing.T) {
	m := newFakeMachine()
	if _, err := newTestMachineDispatcher(m, testMachineRoot).Dispatch(context.Background(),
		machineStep("fs_read", map[string]any{"file_path": "~/notes.md"})); err == nil || len(m.calls) != 0 {
		t.Fatalf("a home path was dispatched: %v, %v", err, m.actions())
	}
}

func TestTheMachineWorkspaceIsTheRootAndTheBareRunId(t *testing.T) {
	if got, err := procedureMachineWorkspace("/Users/someone/work/", "v1:work:run:abc123"); err != nil || got != "/Users/someone/work/abc123" {
		t.Fatalf("workspace = %q, %v", got, err)
	}
	for root, run := range map[string]string{"": "v1:work:run:a", "work": "v1:work:run:a", "/w": ""} {
		if got, err := procedureMachineWorkspace(root, run); err == nil {
			t.Errorf("(%q, %q) -> %q", root, run, got)
		}
	}
}

func TestTheMachineRemembersABoundedNumberOfWorkspaces(t *testing.T) {
	r := newProcedureReadyDirs()
	for i := 0; i < procedureMachineReadyMemory+10; i++ {
		r.add("/w/" + strings.Repeat("x", i+1))
	}
	if len(r.ready) != procedureMachineReadyMemory || len(r.order) != procedureMachineReadyMemory {
		t.Fatalf("remembered %d / %d", len(r.ready), len(r.order))
	}
	if r.has("/w/x") || !r.has("/w/"+strings.Repeat("x", procedureMachineReadyMemory+10)) {
		t.Fatal("the oldest was kept or the newest forgotten")
	}
}
