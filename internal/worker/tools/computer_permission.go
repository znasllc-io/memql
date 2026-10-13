package tools

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

// A typed engine receipt constrains a call; it never opens the machine's own
// consent window or changes local policy. The runner separately binds enrollment.
func (d *Dispatcher) dispatchComputerPermission(ctx context.Context, dispatch *memqlv1.ToolDispatch, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	c := dispatch.GetComputerPermission()
	hash, err := permission.Hash(dispatch.GetTool(), dispatch.GetAction(), args)
	if err != nil || c.GetVersion() != permission.Version || c.GetPermissionId() == "" || c.GetOwnerUserId() == "" || c.GetRegistrationId() == "" || c.GetMachineId() == "" || c.GetArgsHash() != hash || dispatch.GetTool() != "workerHost" {
		return nil, failure("computer_permission_refused", "This permission does not match the exact requested action.")
	}
	if d.consent == nil {
		return nil, failure("consent_required", "Enable computer access on this machine before using this permission.")
	}
	switch c.GetKind() {
	case "directory_read":
		p := argString(args, "path")
		if !permission.ReadAction(dispatch.GetAction()) || !permission.Within(c.GetDirectory(), p) {
			return nil, failure("computer_permission_refused", "This permission covers only reads inside the approved directory.")
		}
		return runScopedRead(ctx, dispatch.GetAction(), p, args, d.policy)
	case "exact_command":
		if dispatch.GetAction() != "exec" || c.GetDirectory() != "" {
			return nil, failure("computer_permission_refused", "This permission covers only the exact approved command.")
		}
		_, cwd, _, inputErr := permission.CommandInput(args)
		if inputErr != nil {
			return nil, failure("computer_permission_refused", inputErr.Error())
		}
		if !permission.CleanAbsolute(cwd) {
			return nil, failure("computer_permission_refused", "The approved command needs an explicit absolute working directory.")
		}
		if err := permissionLocalPath(d.policy, cwd); err != nil {
			return nil, failure("fs_denied", err.Error())
		}
		real, err := filepath.EvalSymlinks(cwd)
		if err != nil || real != cwd {
			return nil, failure("computer_permission_refused", "Request this command again using the working directory's real path.")
		}
		return d.dispatchHost(ctx, dispatch.GetAction(), args, dispatch.GetAgentId(), nil)
	default:
		return nil, failure("computer_permission_refused", "This Cockpit does not support this computer permission.")
	}
}

// Each path component is opened relative to an already-open directory with
// O_NOFOLLOW. A symlink replacement cannot escape either the approved lexical
// directory or the local deny list. No path is reopened after validation.
func runScopedRead(ctx context.Context, action, p string, args map[string]any, policy *Policy) (*memqlv1.Success, *memqlv1.Failure) {
	if err := ctx.Err(); err != nil {
		return nil, failure("cancelled", err.Error())
	}
	if err := permissionLocalPath(policy, p); err != nil {
		return nil, failure("fs_denied", err.Error())
	}
	f, err := openPermissionPath(p, action == "fs_list")
	if err != nil {
		return nil, failure("computer_permission_path_refused", fmt.Sprintf("Cannot read this path without following a link. Request access using its real directory path: %v", err))
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, failure("fs_failed", err.Error())
	}
	switch action {
	case "fs_read":
		if !info.Mode().IsRegular() {
			return nil, failure("fs_denied", "Scoped file reads require a regular file.")
		}
		maxBytes := argInt(args, "maxBytes", 5*1024*1024)
		if maxBytes < 1 || maxBytes > 5*1024*1024 {
			return nil, failure("bad_request", "maxBytes must be between 1 and 5242880.")
		}
		body, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
		if err != nil {
			return nil, failure("fs_failed", err.Error())
		}
		return successJSON(map[string]any{"path": p, "content": string(body), "size": len(body)}, 0, 0, len(body), truncate(string(body), 1024)), nil
	case "fs_list":
		entries, err := f.ReadDir(-1)
		if err != nil {
			return nil, failure("fs_failed", err.Error())
		}
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			row := map[string]any{"name": e.Name(), "type": typeOf(e)}
			// Resolve metadata against the held directory, too: DirEntry.Info
			// would reopen its original pathname after a directory replacement.
			if size, mode, err := permissionEntryMetadata(f, e.Name()); err == nil {
				row["size"] = size
				row["mode"] = mode.String()
			}
			out = append(out, row)
		}
		return successJSON(map[string]any{"path": p, "entries": out}, 0, 0, 0, ""), nil
	case "fs_stat":
		return successJSON(map[string]any{"path": p, "name": info.Name(), "size": info.Size(), "mode": info.Mode().String(), "isDir": info.IsDir(), "modTime": info.ModTime().UTC()}, 0, 0, 0, ""), nil
	default:
		return nil, failure("computer_permission_refused", "This permission does not allow that action.")
	}
}

func permissionLocalPath(policy *Policy, p string) error {
	if err := policy.CheckPath(p); err != nil {
		return err
	}
	if root := policy.WorkspaceRoot(); root != "" {
		abs, err := filepath.Abs(root)
		if err != nil || (abs != "/" && !permission.Within(abs, p)) {
			return fmt.Errorf("the path is outside this machine's configured workspace")
		}
	}
	return nil
}
