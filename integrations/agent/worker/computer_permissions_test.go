//go:build agent

package worker

import (
	"context"
	"fmt"
	"sync"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

type permissionTestAuthority struct {
	mu       sync.Mutex
	grant    *permission.Grant
	claims   int
	origin   bool
	ctxOwner string
}

func (a *permissionTestAuthority) Find(context.Context, permission.Request) (*permission.Grant, error) {
	return a.grant, nil
}
func (a *permissionTestAuthority) Claim(ctx context.Context, id string, req permission.Request, registration, machine string) (*permission.Scope, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claims++
	if a.origin {
		return nil, fmt.Errorf("origin must not consume")
	}
	if a.claims > 1 {
		return nil, fmt.Errorf("already consumed")
	}
	if id != a.grant.ID || registration != a.grant.Scope.RegistrationID || machine != a.grant.Scope.MachineID || !a.grant.Scope.Matches(req, "once") {
		return nil, fmt.Errorf("wrong grant")
	}
	copy := a.grant.Scope
	return &copy, nil
}
func TestComputerPermissionConsumedOnlyByStreamHolderAcrossHop(t *testing.T) {
	var got *memqlv1.ToolDispatch
	h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		got = d
		return okResult(d.GetCallId()), nil
	}, func(c *Candidate) {
		c.MachineId = "machine"
		c.ActionContracts = workerservice.ActionContracts{permission.Contract: permission.Version}
	})
	w := h.registry.WorkerById("laptop")
	w.Labels = map[string]string{"machineId": "machine"}
	w.CapabilityDescriptor = &workerservice.CapabilityDescriptor{ActionContracts: workerservice.ActionContracts{permission.Contract: permission.Version}}
	req := h.request()
	req.Args = map[string]any{"path": "/docs/report"}
	hash, _ := permission.Hash(req.Tool, req.Action, req.Args)
	grant := &permission.Grant{ID: "receipt", Scope: permission.Scope{Version: 1, Kind: "directory_read", RegistrationID: "laptop", MachineID: "machine", MachineLabel: "Laptop", Tool: req.Tool, Action: req.Action, Directory: "/docs", ArgsHash: hash, AllowedModes: []string{"once", "always"}}}
	origin := &permissionTestAuthority{grant: grant, origin: true}
	holder := &permissionTestAuthority{grant: grant}
	h.dispatch.SetComputerPermissions(origin)
	h.link.handler.SetComputerPermissions(holder)
	ctx := common.ContextWithRun(authorityCtx(t, h.owner), common.RunContext{OwnerUserId: h.owner, RunId: req.RunId, GoalId: "goal"})
	res, err := h.dispatch.Dispatch(ctx, req)
	if err != nil || !res.OK || got == nil {
		t.Fatalf("hop: %+v %v", res, err)
	}
	if origin.claims != 0 || holder.claims != 1 {
		t.Fatalf("consumed on wrong replica: origin=%d holder=%d", origin.claims, holder.claims)
	}
	c := got.GetComputerPermission()
	if c.GetPermissionId() != "receipt" || c.GetMachineId() != "machine" || c.GetOwnerUserId() != w.OwnerUserId || c.GetArgsHash() != hash {
		t.Fatalf("lost constraint: %v", c)
	}
	got = nil
	res, err = h.dispatch.Dispatch(ctx, req)
	if err != nil || res.OK || got != nil || !res.RefusedBeforeStart {
		t.Fatalf("replay reached machine: %+v %v", res, err)
	}
}
func TestComputerPermissionOldMachineNeverSilentlyFallsBack(t *testing.T) {
	sent := false
	h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		sent = true
		return okResult(d.GetCallId()), nil
	}, func(c *Candidate) { c.MachineId = "machine" })
	req := h.request()
	hash, _ := permission.Hash(req.Tool, req.Action, req.Args)
	authority := &permissionTestAuthority{grant: &permission.Grant{ID: "receipt", Scope: permission.Scope{Version: 1, Kind: "directory_read", RegistrationID: "laptop", MachineID: "machine", MachineLabel: "Laptop", Tool: req.Tool, Action: req.Action, Directory: "/docs", ArgsHash: hash, AllowedModes: []string{"once", "always"}}}}
	h.dispatch.SetComputerPermissions(authority)
	ctx := common.ContextWithRun(authorityCtx(t, h.owner), common.RunContext{OwnerUserId: h.owner, RunId: req.RunId, GoalId: "goal"})
	res, err := h.dispatch.Dispatch(ctx, req)
	if err != nil || res.OK || sent || authority.claims != 0 {
		t.Fatalf("unsupported worker received scoped call: %+v %v", res, err)
	}
}
