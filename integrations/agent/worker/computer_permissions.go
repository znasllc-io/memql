package worker

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func (d *Dispatcher) SetComputerPermissions(authority permission.Authority) {
	d.permissions = authority
}
func (h *ForwardHandler) SetComputerPermissions(authority permission.Authority) {
	h.permissions = authority
}
func permissionRequest(req Request) permission.Request {
	return permission.Request{OwnerID: req.OwnerUserId, AgentID: req.AgentId, RunID: req.RunId, Tool: req.Tool, Action: req.Action, Args: req.Args, MachineID: req.RequireLabels["machineId"]}
}
func computerPermissionID(req Request) string {
	if req.computerPermission != nil {
		return req.computerPermission.ID
	}
	return ""
}

func claimComputerPermission(ctx context.Context, authority permission.Authority, req Request, w *workerservice.Worker) (*permission.Scope, error) {
	if authority == nil || req.computerPermission == nil {
		return nil, fmt.Errorf("computer permission verification is unavailable")
	}
	if w.CapabilityDescriptor == nil || !w.CapabilityDescriptor.ActionContracts.Supports(permission.Contract, permission.Version) {
		return nil, fmt.Errorf("update MemQL Cockpit on this machine to use this saved permission")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := authority.Claim(ctx, req.computerPermission.ID, permissionRequest(req), w.RegistrationId, w.LabelsSnapshot()["machineId"])
	if err != nil {
		return nil, err
	}
	// Match the exact enrollment identifiers the same stream acknowledged.
	scope.RegistrationID = w.RegistrationId
	return scope, nil
}
func permissionConstraint(req Request) *memqlv1.ComputerPermissionConstraint {
	s := req.computerConstraint
	if s == nil {
		return nil
	}
	hash, _ := permission.Hash(req.Tool, req.Action, req.Args)
	return &memqlv1.ComputerPermissionConstraint{PermissionId: computerPermissionID(req), OwnerUserId: req.computerOwner, RegistrationId: s.RegistrationID, MachineId: s.MachineID, Kind: s.Kind, Directory: s.Directory, ArgsHash: hash, Version: permission.Version}
}

// prepareComputerPermission selects the machine through the ordinary router,
// then binds this request to that exact enrollment and its raw machine identity.
func (d *Dispatcher) prepareComputerPermission(ctx context.Context, owner, agent, run string, operation map[string]any, labels map[string]string) (permission.Scope, *permission.Grant, error) {
	tool, _ := operation["tool"].(string)
	action, _ := operation["action"].(string)
	args, _ := operation["args"].(map[string]any)
	scope := permission.Scope{Version: permission.Version, Tool: tool, Action: action, AllowedModes: []string{"once", "always"}}
	if tool != "workerHost" {
		return scope, nil, computerPermissionInputError("operation.tool", "operation.tool must be workerHost; supply operation={tool: workerHost, action: fs_list, args: {path: <absolute directory>}, directory: <same absolute directory>}")
	}
	if args == nil {
		return scope, nil, computerPermissionInputError("operation.args", "operation.args must be an object containing path for a read, or cmd and cwd for exec")
	}
	var err error
	scope.ArgsHash, err = permission.Hash(tool, action, args)
	if err != nil {
		return scope, nil, err
	}
	switch {
	case tool == "workerHost" && permission.ReadAction(action):
		scope.Kind = "directory_read"
		scope.Directory, _ = operation["directory"].(string)
		p, _ := args["path"].(string)
		if !permission.CleanAbsolute(scope.Directory) {
			return scope, nil, computerPermissionInputError("operation.directory", "operation.directory is required for this read: copy the absolute directory from the selected machine's homeDirectory or a path supplied by the person; do not guess a username or use /, ~ or $HOME")
		}
		if !permission.CleanAbsolute(p) {
			return scope, nil, computerPermissionInputError("operation.args.path", "operation.args.path must be an explicit absolute path; for a home listing, copy the selected machine's homeDirectory")
		}
		if !permission.Within(scope.Directory, p) {
			return scope, nil, computerPermissionInputError("operation.args.path", "operation.args.path is outside operation.directory; request only the directory needed for this read")
		}
	case tool == "workerHost" && action == "exec":
		scope.Kind = "exact_command"
		scope.Command, scope.CWD, scope.TimeoutSeconds, err = permission.CommandInput(args)
		if err != nil {
			return scope, nil, computerPermissionInputError("operation.args", err.Error())
		}
	default:
		return scope, nil, computerPermissionInputError("operation.action", "operation.action must be fs_read, fs_list, fs_stat or exec")
	}
	if d.permissions == nil {
		return scope, nil, fmt.Errorf("computer permission verification is unavailable")
	}
	// A matching receipt avoids parking the run again. Dispatch still rechecks
	// the receipt, routing requirements and machine before consuming anything.
	existing, err := d.permissions.Find(ctx, permission.Request{OwnerID: owner, AgentID: agent, RunID: run, Tool: tool, Action: action, Args: args, MachineID: labels["machineId"]})
	if err != nil {
		return scope, nil, err
	}
	plan, err := d.router.Plan(ctx, owner, workerservice.CapabilityHeadless, labels, nil)
	if err != nil {
		return scope, nil, err
	}
	if err := requireSingleComputerTarget(plan); err != nil {
		return scope, nil, err
	}
	plan.RequireActionContract(permission.Contract, permission.Version)
	if action == "fs_read" && permission.ExtendedRead(args) {
		if _, err := permission.ParseReadOptions(args); err != nil {
			return scope, nil, computerPermissionInputError("operation.args", err.Error())
		}
		plan.RequireActionContract(permission.ReadContract, permission.ReadVersion)
	}
	for _, candidate := range plan.Candidates {
		if candidate.MachineId == "" {
			continue
		}
		if existing != nil && sameSubject(existing.Scope.RegistrationID, candidate.RegistrationId) && existing.Scope.MachineID == candidate.MachineId {
			return existing.Scope, existing, nil
		}
	}
	if len(plan.Candidates) == 0 {
		return scope, nil, fmt.Errorf("connect a machine with an updated MemQL Cockpit to request this permission; no broader permission was requested")
	}
	selected := plan.Candidates[0]
	scope.RegistrationID = selected.RegistrationId
	scope.MachineID = selected.MachineId
	scope.MachineLabel = selected.Label()
	if err := scope.Validate(); err != nil {
		return scope, nil, err
	}
	return scope, nil, nil
}

// Only caller-controlled operation validation belongs here. Receipt, routing
// and approval failures retain their original error and recovery semantics.
func computerPermissionInputError(field, message string) error {
	return memql.NewStructuredError(memql.ErrorCodeInvalidArgument, message).
		WithDetails(map[string]any{"field": field})
}
