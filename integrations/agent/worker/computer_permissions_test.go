//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
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
	req.RequireLabels = map[string]string{"machineId": "machine"}
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

// Keep the previous release's reader here: a mixed-version agent must not turn
// a newly issued typed receipt into its older, wider run-scoped authorization.
func legacy026ComputerScope(row map[string]any, req Request, now time.Time) string {
	if row["decision"] != "approved" || memql.BareShortId(scopeString(row["ownerUserId"])) != memql.BareShortId(req.OwnerUserId) || memql.BareShortId(scopeString(row["runId"])) != memql.BareShortId(req.RunId) {
		return ""
	}
	expires, err := time.Parse(time.RFC3339Nano, scopeString(row["expiresAt"]))
	if err != nil || !now.Before(expires) {
		return ""
	}
	subject, _ := row["subject"].(map[string]any)
	if work.ArtifactHash(subject) != scopeString(row["artifactHash"]) || memql.BareShortId(scopeString(subject["agentId"])) != memql.BareShortId(req.AgentId) {
		return ""
	}
	for key, value := range LabelsFromArgs(subject["requireLabels"]) {
		if req.RequireLabels[key] != value {
			return ""
		}
	}
	if allowed := scopeString(subject["scope"]); allowed == "full" || allowed == "observe" {
		return allowed
	}
	return ""
}

func TestRequestedComputerPermissionHasNoLegacyGrant(t *testing.T) {
	for _, tc := range []struct {
		name, requestedScope, action string
		args                         map[string]any
		typed                        bool
	}{
		{"typed directory", "observe", "fs_read", map[string]any{"path": "/docs/report"}, true},
		{"typed command", "full", "exec", map[string]any{"cmd": "pwd", "cwd": "/docs", "timeoutSec": 30}, true},
		{"legacy observe", "observe", "fs_read", map[string]any{"path": "/docs/report"}, false},
		{"legacy full", "full", "exec", map[string]any{"cmd": "pwd", "cwd": "/docs"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dispatched *memqlv1.ToolDispatch
			h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				dispatched = d
				return okResult(d.GetCallId()), nil
			}, func(c *Candidate) {
				c.MachineId = "machine"
				c.ActionContracts = workerservice.ActionContracts{permission.Contract: permission.Version}
			})
			w := h.registry.WorkerById("laptop")
			w.Labels = map[string]string{"machineId": "machine"}
			w.CapabilityDescriptor = &workerservice.CapabilityDescriptor{ActionContracts: workerservice.ActionContracts{permission.Contract: permission.Version}}
			h.dispatch.SetComputerPermissions(&permissionTestAuthority{})
			req := h.request()
			req.Action, req.Args = tc.action, tc.args
			ctx := common.ContextWithRun(authorityCtx(t, h.owner), common.RunContext{OwnerUserId: h.owner, RunId: req.RunId, GoalId: "goal"})
			var subject map[string]any
			captured := errors.New("captured before persistence")
			i := &Integration{engine: &memql.MemQLEngine{}, dispatcher: h.dispatch, ScopeRequester: func(_ context.Context, owner, run string, s map[string]any) (string, error) {
				if owner != req.OwnerUserId || run != req.RunId {
					t.Fatal("lost owned run")
				}
				subject = s
				return "", captured
			}}
			args := map[string]any{"intent": "inspect", "requestedScope": tc.requestedScope, "summary": "Inspect requested files", "agentId": req.AgentId}
			if tc.typed {
				args["operation"] = map[string]any{"tool": req.Tool, "action": req.Action, "args": req.Args, "directory": "/docs"}
			}
			if _, err := i.handleRequestScope(ctx, args, 0); !errors.Is(err, captured) {
				t.Fatalf("request did not reach approval boundary: %v", err)
			}
			now := fleetNow()
			row := map[string]any{"ownerUserId": req.OwnerUserId, "runId": req.RunId, "decision": "approved", "expiresAt": now.Add(time.Hour).Format(time.RFC3339Nano), "subject": subject, "artifactHash": work.ArtifactHash(subject)}
			if !tc.typed {
				if got := approvedComputerScope([]map[string]any{row}, req, now); got != "" {
					t.Fatal("a machine-bound approval admitted an untargeted call")
				}
				req.RequireLabels = map[string]string{"machineId": "machine"}
				if got := legacy026ComputerScope(row, req, now); got != tc.requestedScope {
					t.Fatalf("legacy request changed: got %q", got)
				}
				if got := approvedComputerScope([]map[string]any{row}, req, now); got != tc.requestedScope {
					t.Fatalf("current legacy reader changed: got %q", got)
				}
				return
			}
			if got := legacy026ComputerScope(row, req, now); got != "" {
				t.Fatalf("typed receipt widened by previous engine to %q", got)
			}
			scope, err := permission.Parse(subject["computerPermission"])
			if err != nil || !scope.Matches(permissionRequest(req), "once") || !scope.Matches(permissionRequest(req), "always") {
				t.Fatalf("typed exact operation lost: %+v %v", scope, err)
			}
			grant := &permission.Grant{ID: "receipt", Scope: scope}
			h.dispatch.SetComputerPermissions(&permissionTestAuthority{grant: grant, origin: true})
			holder := &permissionTestAuthority{grant: grant}
			h.link.handler.SetComputerPermissions(holder)
			res, err := h.dispatch.Dispatch(ctx, req)
			if err != nil || !res.OK || dispatched == nil || dispatched.GetComputerPermission() == nil || holder.claims != 1 {
				t.Fatalf("typed request lost authorized cross-replica dispatch: %+v %v", res, err)
			}
		})
	}
}

func TestComputerPermissionDefaultTimeoutKeepsExactWireHashAcrossHop(t *testing.T) {
	for _, explicitApproval := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-approval-%v", explicitApproval), func(t *testing.T) {
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
			req.Action, req.Args = "exec", map[string]any{"cmd": "pwd", "cwd": "/fixture/repository"}
			approved := map[string]any{"cmd": "pwd", "cwd": "/fixture/repository"}
			if explicitApproval {
				approved["timeoutSec"] = 60
			} else {
				req.Args["timeoutSec"] = 60
			}
			hash, _ := permission.Hash(req.Tool, req.Action, approved)
			grant := &permission.Grant{ID: "receipt", Scope: permission.Scope{Version: 1, Kind: "exact_command", RegistrationID: "laptop", MachineID: "machine", MachineLabel: "Laptop", Tool: req.Tool, Action: req.Action, Command: "pwd", CWD: "/fixture/repository", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}}
			origin, holder := &permissionTestAuthority{grant: grant, origin: true}, &permissionTestAuthority{grant: grant}
			h.dispatch.SetComputerPermissions(origin)
			h.link.handler.SetComputerPermissions(holder)
			ctx := common.ContextWithRun(authorityCtx(t, h.owner), common.RunContext{OwnerUserId: h.owner, RunId: req.RunId, GoalId: "goal"})
			result, err := h.dispatch.Dispatch(ctx, req)
			if err != nil || !result.OK || got == nil {
				t.Fatalf("equivalent command did not cross hop: %+v %v", result, err)
			}
			var envelope map[string]any
			if err := json.Unmarshal(got.GetArgsJson(), &envelope); err != nil {
				t.Fatal(err)
			}
			wireArgs, ok := envelope[req.Action].(map[string]any)
			if !ok {
				t.Fatal("wire envelope lost action arguments")
			}
			wireHash, _ := permission.Hash(req.Tool, req.Action, wireArgs)
			requestedHash, _ := permission.Hash(req.Tool, req.Action, req.Args)
			if wireHash != requestedHash || wireHash == hash || got.GetComputerPermission().GetArgsHash() != wireHash {
				t.Fatal("transport lost the exact invocation hash")
			}
			if origin.claims != 0 || holder.claims != 1 {
				t.Fatalf("wrong claim owner: origin=%d holder=%d", origin.claims, holder.claims)
			}
			got = nil
			result, err = h.dispatch.Dispatch(ctx, req)
			if err != nil || result.OK || got != nil || !result.RefusedBeforeStart {
				t.Fatalf("once replay dispatched: %+v %v", result, err)
			}
		})
	}
}
