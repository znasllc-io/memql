//go:build agent

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/integrations/skills"
)

// script_forward_hop_test.go -- a runScript step whose labels require a
// machine held by a SIBLING REPLICA reaches that machine, and the script is
// verified on the far side before it runs (epic memql#4970, spec section J's
// cross-node case).
//
// ===========================================================================
// WHY THIS TEST IS IN-PROCESS AND NOT IN clustere2e
// ===========================================================================
// A live-cluster gate is skipped on every CI lane and every developer
// machine, and a gate skipped by default cannot be what stands between a
// feature and the bug it prevents -- which is the reasoning memql#4352
// recorded when the worker forward's own gate was written as an in-process
// hop test rather than a cluster lane. This is that test with a script on top
// of it: the real ForwardRouter, the real ForwardHandler, the real
// Dispatcher, the real Integration envelope, and the real
// skills.Runner composition. `meshLink` (forward_hop_test.go) plays
// NodeService.Stream.
//
// FALSIFIABILITY. Make attemptRemote dispatch locally instead of forwarding,
// or make the runner skip its read-back, and these fail.

// scriptSkills is a SkillResolver holding one skill with one `any` script.
type scriptSkills struct{ scripts []skills.Script }

func (s *scriptSkills) SkillScripts(context.Context, string) (skills.SkillScripts, bool, error) {
	return skills.SkillScripts{SkillID: "skill-1", Slug: "reconcile", Active: true, Scripts: s.scripts}, true, nil
}

// scriptLibrary is an ArtifactReader over one blob.
type scriptLibrary struct{ body []byte }

func (l *scriptLibrary) ReadArtifact(context.Context, string) (skills.ScriptBytes, error) {
	sum := sha256.Sum256(l.body)
	return skills.ScriptBytes{Data: l.body, Sha256: hex.EncodeToString(sum[:]), Name: "reconcile.sh"}, nil
}

// remoteDisk is the machine's side: a filesystem in a map, driven by the
// three primitives runScript composes. It is what the cockpit would be.
type remoteDisk struct {
	files map[string][]byte
	ran   []string
}

func (d *remoteDisk) dispatch() workerservice.DispatchFunc {
	return func(_ context.Context, call *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		var envelope map[string]any
		_ = json.Unmarshal([]byte(call.GetArgsJson()), &envelope)
		action, _ := envelope["action"].(string)
		args, _ := envelope[action].(map[string]any)
		path, _ := args["path"].(string)

		switch action {
		case "fs_write":
			content, _ := args["content"].(string)
			d.files[path] = []byte(content)
			return okJSON(call, map[string]any{"path": path, "bytes": len(content)})
		case "fs_read":
			body, present := d.files[path]
			if !present {
				return &memqlv1.ToolResult{
					CallId: call.GetCallId(),
					Payload: &memqlv1.ToolResult_Failure{
						Failure: &memqlv1.Failure{ErrorCode: "fs_read_failed", ErrorMessage: "no such file"},
					},
				}, nil
			}
			return okJSON(call, map[string]any{"path": path, "content": string(body), "truncated": false})
		case "exec":
			cmd, _ := args["cmd"].(string)
			d.ran = append(d.ran, cmd)
			return okJSON(call, map[string]any{"exitCode": float64(0), "stdout": "done", "stderr": ""})
		}
		return okJSON(call, map[string]any{})
	}
}

func okJSON(call *memqlv1.ToolDispatch, payload map[string]any) (*memqlv1.ToolResult, error) {
	body, _ := json.Marshal(payload)
	return &memqlv1.ToolResult{
		CallId:  call.GetCallId(),
		Payload: &memqlv1.ToolResult_Success{Success: &memqlv1.Success{ResultJson: body}},
	}, nil
}

const hopScript = "#!/usr/bin/env bash\necho reconciled\n"

// scriptHop stands the whole path up: node A serves the turn and holds no
// machine, node B holds the stream, and the runner reaches it through the
// integration's own dispatchHost handler.
func newScriptHop(t *testing.T, disk *remoteDisk) (*skills.Runner, *hop) {
	t.Helper()
	h := newHop(t, disk.dispatch(), func(c *Candidate) {
		c.Labels = map[string]string{"os": "darwin"}
	})
	integration := NewIntegration(h.dispatch, nil, nil, testLogger())
	var handler skills.CapabilityHandler
	for _, capability := range integration.Capabilities() {
		if capability.Name == "dispatchHost" {
			handler = skills.CapabilityHandler(capability.Handler)
		}
	}
	if handler == nil {
		t.Fatal("the worker integration published no dispatchHost capability")
	}
	runner := skills.NewRunner(
		&scriptSkills{scripts: []skills.Script{{Platform: "any", ArtifactID: "artifact-1", Entry: "bash {script}"}}},
		&scriptLibrary{body: []byte(hopScript)},
		nil, // no workbench on this node: the fleet is the only surface
		skills.NewFleetSurface(handler),
	)
	return runner, h
}

func scriptRequest(h *hop) skills.Request {
	return skills.Request{
		SkillID:       "skill-1",
		PlanID:        "plan-1",
		AgentID:       "agent-1",
		OwnerID:       h.owner,
		RequireLabels: map[string]string{"os": "darwin"},
	}
}

func TestAScriptStepReachesAMachineHeldByASiblingReplica(t *testing.T) {
	disk := &remoteDisk{files: map[string][]byte{}}
	runner, h := newScriptHop(t, disk)

	receipt, err := runner.Run(authorityCtx(t, h.owner), scriptRequest(h))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.link.wg.Wait()

	if receipt.Surface != skills.SurfaceMachine {
		t.Fatalf("surface = %q, want the fleet", receipt.Surface)
	}
	if !receipt.Verified || !receipt.Shipped {
		t.Fatalf("receipt = %+v, want shipped and verified", receipt)
	}
	// The bytes are on the FAR side, which is the whole claim: node A holds
	// no machine, so every one of these calls crossed the hop.
	if got := string(disk.files[receipt.Path]); got != hopScript {
		t.Fatalf("the machine holds %q", got)
	}
	if len(disk.ran) != 1 || !strings.Contains(disk.ran[0], receipt.Path) {
		t.Fatalf("the machine ran %v, want the shipped script", disk.ran)
	}
	if got := h.store.lastInvocation(t).WorkerId; got != "laptop" {
		t.Fatalf("invocation recorded worker %q", got)
	}
}

// THE VERIFICATION CROSSES THE HOP TOO. A design that verified locally --
// hashing the bytes it was about to send -- would pass this test while
// proving nothing, so the assertion is that the far side's own disk was read
// back before exec.
func TestTheFarSideIsWhatIsVerified(t *testing.T) {
	disk := &remoteDisk{files: map[string][]byte{}}
	runner, h := newScriptHop(t, disk)
	if _, err := runner.Run(authorityCtx(t, h.owner), scriptRequest(h)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.link.wg.Wait()

	actions := []string{}
	for _, call := range h.store.invocations {
		actions = append(actions, call.Action)
	}
	if len(actions) < 3 {
		t.Fatalf("actions = %v, want a write, a read-back and an exec", actions)
	}
	if actions[len(actions)-1] != "exec" {
		t.Fatalf("actions = %v, want exec last", actions)
	}
	sawReadAfterWrite := false
	wrote := false
	for _, action := range actions {
		if action == "fs_write" {
			wrote = true
		}
		if action == "fs_read" && wrote {
			sawReadAfterWrite = true
		}
	}
	if !sawReadAfterWrite {
		t.Fatalf("actions = %v, want the shipped bytes read back off the machine", actions)
	}
}

// A machine that corrupts what it stores must not run the script. This is the
// case the read-back exists for, and the only way to reach it is to make the
// far side lie.
func TestAMachineThatStoredSomethingElseRunsNothing(t *testing.T) {
	disk := &remoteDisk{files: map[string][]byte{}}
	runner, h := newScriptHop(t, disk)

	// The disk drops a byte on the way in.
	base := disk.dispatch()
	h.registry.Remove("laptop")
	live := &workerservice.Worker{
		RegistrationId: "laptop",
		OwnerUserId:    h.owner,
		Capabilities:   []string{workerservice.CapabilityHeadless},
		Concurrency:    map[string]uint32{workerservice.CapabilityHeadless: 2},
	}
	live.SetDispatchFunc(func(ctx context.Context, call *memqlv1.ToolDispatch, onChunk func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		res, err := base(ctx, call, onChunk)
		if err != nil || res == nil {
			return res, err
		}
		var envelope map[string]any
		_ = json.Unmarshal([]byte(call.GetArgsJson()), &envelope)
		if action, _ := envelope["action"].(string); action == "fs_read" {
			success, ok := res.GetPayload().(*memqlv1.ToolResult_Success)
			if !ok {
				return res, err
			}
			var body map[string]any
			_ = json.Unmarshal(success.Success.GetResultJson(), &body)
			body["content"] = "tampered"
			out, _ := json.Marshal(body)
			success.Success.ResultJson = out
		}
		return res, err
	}, func() {})
	h.registry.Add(live)

	_, err := runner.Run(authorityCtx(t, h.owner), scriptRequest(h))
	if err == nil {
		t.Fatal("a script the machine had altered was run")
	}
	if !strings.Contains(err.Error(), skills.ErrScriptHashMismatch) {
		t.Fatalf("err = %v, want %s", err, skills.ErrScriptHashMismatch)
	}
	if len(disk.ran) != 0 {
		t.Fatalf("the machine ran %v after a hash mismatch", disk.ran)
	}
}
