//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func permissionDispatch(t *testing.T, kind, directory, action string, args map[string]any) *memqlv1.ToolDispatch {
	t.Helper()
	hash, err := permission.Hash("workerHost", action, args)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"action": action, action: args})
	return &memqlv1.ToolDispatch{Tool: "workerHost", Action: action, AgentId: "agent", ArgsJson: raw, ComputerPermission: &memqlv1.ComputerPermissionConstraint{Version: 1, PermissionId: "receipt", OwnerUserId: "owner", RegistrationId: "worker", MachineId: "machine", Kind: kind, Directory: directory, ArgsHash: hash}}
}
func realPermissionDirectory(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestComputerPermissionMetadataStaysOnOpenedDirectoryAfterReplacement(t *testing.T) {
	root := realPermissionDirectory(t)
	directory, outside := filepath.Join(root, "approved"), filepath.Join(root, "outside")
	for _, p := range []string{directory, outside} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "file"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("private data outside the approved directory"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openPermissionPath(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(directory, directory+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, directory); err != nil {
		t.Fatal(err)
	}
	size, mode, err := permissionEntryMetadata(f, "file")
	if err != nil || size != 4 || mode.Perm() != 0600 {
		t.Fatalf("metadata escaped opened directory: size=%d mode=%v error=%v", size, mode, err)
	}
}

func TestComputerPermissionLocalWorkspaceUsesDirectoryBoundary(t *testing.T) {
	policy := DefaultPolicy()
	policy.fs.WorkspaceRoot = "/fixture/repository"
	if err := permissionLocalPath(policy, "/fixture/repository/file"); err != nil {
		t.Fatal(err)
	}
	if err := permissionLocalPath(policy, "/fixture/repository-other/file"); err == nil {
		t.Fatal("same-prefix sibling escaped local workspace ceiling")
	}
	policy.fs.WorkspaceRoot = "/"
	if err := permissionLocalPath(policy, "/fixture/repository/file"); err != nil {
		t.Fatalf("broad local ceiling refused narrower approved path: %v", err)
	}
}
func TestComputerPermissionDirectoryReadRejectsSymlinkAndPolicyEscapes(t *testing.T) {
	root := realPermissionDirectory(t)
	outside := realPermissionDirectory(t)
	if err := os.WriteFile(filepath.Join(root, "report"), []byte("approved bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	policy := DefaultPolicy()
	gate := &fakeGate{decision: consent.Decision{Allowed: true}}
	d := NewDispatcher(quietLogger(), policy, gate)
	for _, action := range []string{"fs_read", "fs_stat", "fs_list"} {
		p := filepath.Join(root, "report")
		if action == "fs_list" {
			p = root
		}
		ok, fail := d.Dispatch(context.Background(), permissionDispatch(t, "directory_read", root, action, map[string]any{"path": p}))
		if fail != nil || ok == nil {
			t.Fatalf("%s: %v", action, fail)
		}
	}
	for _, p := range []string{filepath.Join(root, "escape", "secret"), filepath.Join(root, "alias"), filepath.Join(outside, "secret"), root + "/../secret"} {
		ok, fail := d.Dispatch(context.Background(), permissionDispatch(t, "directory_read", root, "fs_read", map[string]any{"path": p}))
		if fail == nil || ok != nil {
			t.Fatalf("escaped through %s", p)
		}
	}
	policy.fs.Deny = append(policy.fs.Deny, filepath.Join(root, "report"))
	if _, fail := d.Dispatch(context.Background(), permissionDispatch(t, "directory_read", root, "fs_read", map[string]any{"path": filepath.Join(root, "report")})); fail == nil || fail.GetErrorCode() != "fs_denied" {
		t.Fatalf("local deny overridden: %v", fail)
	}
	if _, fail := d.Dispatch(context.Background(), permissionDispatch(t, "directory_read", root, "fs_write", map[string]any{"path": filepath.Join(root, "new"), "content": "x"})); fail == nil {
		t.Fatal("read permission allowed write")
	}
}
func TestComputerPermissionRequiresLocalConsentAndExactCommandInputs(t *testing.T) {
	root := realPermissionDirectory(t)
	args := map[string]any{"cmd": "printf approved", "cwd": root, "env": map[string]any{"LANG": "C"}, "stdin": "original"}
	dispatch := permissionDispatch(t, "exact_command", "", "exec", args)
	closed := NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: false, Reason: "Enable access on this machine"}})
	if _, fail := closed.Dispatch(context.Background(), dispatch); fail == nil || fail.GetErrorCode() != "consent_required" || !strings.Contains(fail.GetErrorMessage(), "Enable access") {
		t.Fatalf("local consent ceiling: %v", fail)
	}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: true}})
	for _, key := range []string{"cmd", "cwd", "env", "stdin"} {
		copy := map[string]any{}
		for k, v := range args {
			copy[k] = v
		}
		copy[key] = "changed"
		raw, _ := json.Marshal(map[string]any{"action": "exec", "exec": copy})
		dispatch.ArgsJson = raw
		if _, fail := d.Dispatch(context.Background(), dispatch); fail == nil || fail.GetErrorCode() != "computer_permission_refused" {
			t.Fatalf("changed %s accepted: %v", key, fail)
		}
	}
	if _, fail := d.Dispatch(context.Background(), permissionDispatch(t, "exact_command", "", "exec", args)); fail == nil {
		t.Fatal("undisclosed env/stdin accepted even with matching hash")
	}
	ok, fail := d.Dispatch(context.Background(), permissionDispatch(t, "exact_command", "", "exec", map[string]any{"cmd": "echo approved", "cwd": root, "timeoutSec": 5}))
	if fail != nil || ok == nil {
		t.Fatalf("reviewable exact command failed: %v", fail)
	}
	denied := permissionDispatch(t, "exact_command", "", "exec", map[string]any{"cmd": "sudo true", "cwd": root})
	if _, fail := d.Dispatch(context.Background(), denied); fail == nil || fail.GetErrorCode() != "denied_by_policy" {
		t.Fatalf("saved command overrode local shell deny: %v", fail)
	}
}
