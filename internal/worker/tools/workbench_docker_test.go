//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
	"github.com/znasllc-io/memql-cockpit/internal/worker/workbench"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"os"
	"testing"
	"time"
)

func TestWorkbenchWireRunsIsolatedDockerWithoutOpeningHostConsent(t *testing.T) {
	if os.Getenv("MEMQL_FLEET_DOCKER_TEST") != "1" {
		t.Skip("explicit local Docker qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	docker := workbench.Docker{}
	if err := docker.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	manager := workbench.New(docker, t.TempDir())
	prior := isolatedWorkbenches
	isolatedWorkbenches = manager
	t.Cleanup(func() {
		if err := manager.CloseHome("fixture-home"); err != nil {
			t.Error(err)
		}
		isolatedWorkbenches = prior
	})
	policy := policyWith(t, "workbenches:\n  allow: true\n")
	gate := &fakeGate{decision: consent.Decision{Allowed: false, Reason: "Host control is not permitted"}}
	d := NewHomeDispatcher(quietLogger(), policy, gate, "fixture-home")
	call := func(caller, action string, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
		raw, _ := json.Marshal(map[string]any{action: args})
		return d.Dispatch(ctx, &memqlv1.ToolDispatch{CallId: caller + action, Tool: "workerWorkbench", Action: action, ArgsJson: raw, RunId: "run", Workbench: &memqlv1.WorkbenchConstraint{OwnerUserId: "owner", CallerUserId: caller, RegistrationId: "registration", MachineId: "machine", RunId: "run", ExpiresUnix: time.Now().Add(time.Minute).Unix(), WorkspaceExpiresUnix: time.Now().Add(workbench.Lifetime - time.Minute).Unix()}})
	}
	if _, failure := call("first", "fs_write", map[string]any{"path": "proof.txt", "content": "private"}); failure != nil {
		t.Fatal(failure)
	}
	success, failure := call("first", "fs_read", map[string]any{"path": "proof.txt"})
	if failure != nil {
		t.Fatal(failure)
	}
	var result map[string]any
	if json.Unmarshal(success.ResultJson, &result) != nil || result["content"] != "private" {
		t.Fatalf("wire lost file result: %v", success)
	}
	success, failure = call("second", "fs_stat", map[string]any{"path": "proof.txt"})
	if failure != nil {
		t.Fatal(failure)
	}
	if json.Unmarshal(success.ResultJson, &result) != nil || result["exists"] != false {
		t.Fatal("wire shared files across borrowers")
	}
	raw, _ := json.Marshal(map[string]any{"exec": map[string]any{"cmd": "true"}})
	_, failure = d.Dispatch(ctx, &memqlv1.ToolDispatch{Tool: "workerHost", Action: "exec", ArgsJson: raw})
	if failure == nil || failure.ErrorCode != "consent_required" {
		t.Fatal("isolated workbench enabled host commands")
	}
}
