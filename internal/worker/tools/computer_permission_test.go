//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func TestMain(m *testing.M) {
	if handled, err := RunScopedCommandChild(os.Args[1:]); handled {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	os.Exit(m.Run())
}

func TestScopedCommandChildRefusesMissingDirectory(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(self, scopedCommandChildFlag, "/bin/sh", "-c", "printf must-not-run")
	output, err := command.CombinedOutput()
	if err == nil || strings.Contains(string(output), "must-not-run") || !strings.Contains(string(output), "inherited directory") {
		t.Fatalf("missing descriptor was not refused: output=%s error=%v", output, err)
	}
	if handled, err := RunScopedCommandChild([]string{scopedCommandChildFlag, "/bin/other", "-c", "true"}); !handled || err == nil {
		t.Fatal("unexpected child command accepted")
	}
}

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

func TestDirectoryReceiptWorksWithoutWindowAndHonorsDurableLocalStop(t *testing.T) {
	root := realPermissionDirectory(t)
	file := filepath.Join(root, "approved.txt")
	if err := os.WriteFile(file, []byte("approved content"), 0600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	gate := consent.NewPersistentHomes(state).For("cluster-a")
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)
	for _, action := range []string{"fs_list", "fs_stat", "fs_read"} {
		p := file
		if action == "fs_list" {
			p = root
		}
		dispatch := permissionDispatch(t, "directory_read", root, action, map[string]any{"path": p})
		if ok, fail := d.Dispatch(context.Background(), dispatch); fail != nil || ok == nil {
			t.Fatalf("approved %s needed unrelated window: %v", action, fail)
		}
		dispatch.ComputerPermission = nil
		if _, fail := d.Dispatch(context.Background(), dispatch); fail == nil || fail.GetErrorCode() != "consent_required" {
			t.Fatalf("unscoped %s bypassed consent: %v", action, fail)
		}
	}
	for _, action := range []string{"exec", "fs_write", "http_fetch"} {
		if _, fail := d.Dispatch(context.Background(), permissionDispatch(t, "directory_read", root, action, map[string]any{"path": file})); fail == nil {
			t.Fatalf("directory receipt admitted %s", action)
		}
	}
	dispatch := permissionDispatch(t, "directory_read", root, "fs_list", map[string]any{"path": root})
	dispatch.ComputerPermission.ArgsHash = "tampered"
	if _, fail := d.Dispatch(context.Background(), dispatch); fail == nil || fail.GetErrorCode() != "computer_permission_refused" {
		t.Fatal("tampered receipt admitted without a window")
	}
	dispatch = permissionDispatch(t, "directory_read", root, "fs_list", map[string]any{"path": root})
	if err := gate.Revoke(); err != nil {
		t.Fatal(err)
	}
	for _, gate := range []*consent.Manager{gate, consent.NewPersistentHomes(state).For("cluster-a")} {
		d = NewDispatcher(quietLogger(), DefaultPolicy(), gate)
		if _, fail := d.Dispatch(context.Background(), dispatch); fail == nil || fail.GetErrorCode() != "consent_required" {
			t.Fatal("local stop lost before or after restart")
		}
	}
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

func TestComputerPermissionCommandPreservesReviewedDirectoryWhitespace(t *testing.T) {
	root := realPermissionDirectory(t)
	approved := filepath.Join(root, "approved ")
	for _, p := range []string{approved, strings.TrimSpace(approved)} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	policy := DefaultPolicy()
	policy.fs.WorkspaceRoot = approved
	d := NewDispatcher(quietLogger(), policy, &fakeGate{decision: consent.Decision{Allowed: true}})
	ok, fail := d.Dispatch(context.Background(), permissionDispatch(t, "exact_command", "", "exec", map[string]any{"cmd": "pwd -P", "cwd": approved}))
	if fail != nil || ok == nil || ok.GetExitCode() != 0 {
		t.Fatalf("approved command failed: success=%v failure=%v", ok, fail)
	}
	var result map[string]any
	if err := json.Unmarshal(ok.GetResultJson(), &result); err != nil {
		t.Fatal(err)
	}
	if result["stdout"] != approved+"\n" {
		t.Fatalf("executed outside exact reviewed cwd: %q", result["stdout"])
	}
}

func TestComputerPermissionCommandStaysOnOpenedDirectoryAfterReplacement(t *testing.T) {
	root := realPermissionDirectory(t)
	approved, outside := filepath.Join(root, "approved"), filepath.Join(root, "outside")
	for _, p := range []string{approved, outside} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := openPermissionPath(approved, true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	moved := approved + "-moved"
	if err := os.Rename(approved, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, approved); err != nil {
		t.Fatal(err)
	}
	ok, fail := runExecWithDirectory(context.Background(), map[string]any{"cmd": "pwd -P", "cwd": approved}, DefaultPolicy(), directory)
	if fail != nil || ok == nil || ok.GetExitCode() != 0 {
		t.Fatalf("anchored command failed: success=%v failure=%v", ok, fail)
	}
	var result map[string]any
	if err := json.Unmarshal(ok.GetResultJson(), &result); err != nil {
		t.Fatal(err)
	}
	if result["stdout"] != moved+"\n" {
		t.Fatalf("cwd reopened through replacement: %q", result["stdout"])
	}
}

func TestComputerPermissionCommandRetainsLimitsAndCancellation(t *testing.T) {
	root := realPermissionDirectory(t)
	directory, err := openPermissionPath(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	policy := DefaultPolicy()
	policy.shell = ShellPolicy{Allow: []string{"ulimit", "sleep"}, MaxOpenFiles: 64, MaxCPUSeconds: 3}
	ok, fail := runExecWithDirectory(context.Background(), map[string]any{"cmd": "ulimit -n; ulimit -t", "cwd": root}, policy, directory)
	if fail != nil || ok.GetExitCode() != 0 {
		t.Fatalf("limited command failed: success=%v failure=%v", ok, fail)
	}
	var result map[string]any
	if err := json.Unmarshal(ok.GetResultJson(), &result); err != nil {
		t.Fatal(err)
	}
	if result["stdout"] != "64\n3\n" {
		t.Fatalf("lost child limits: %q", result["stdout"])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	ok, fail = runExecWithDirectory(ctx, map[string]any{"cmd": "sleep 30", "cwd": root}, policy, directory)
	if ok != nil || fail.GetErrorCode() != "timeout" || time.Since(started) > 5*time.Second {
		t.Fatalf("scoped cancellation failed: success=%v failure=%v", ok, fail)
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

// A no-argument first command followed by a newline is a normal reviewed
// shell script. Classification must not join it to the next command, and
// execution must retain newlines inside quoted content byte-for-byte.
func TestComputerPermissionMultilineCommandPreservesBytesAndPolicy(t *testing.T) {
	root := realPermissionDirectory(t)
	d := NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: true}})
	args := map[string]any{"cmd": "pwd\nprintf 'first\nsecond\n'", "cwd": root, "timeoutSec": 5}
	ok, fail := d.Dispatch(context.Background(), permissionDispatch(t, "exact_command", "", "exec", args))
	if fail != nil || ok == nil || ok.GetExitCode() != 0 {
		t.Fatalf("reviewed multiline command failed: success=%v failure=%v", ok, fail)
	}
	var result map[string]any
	if err := json.Unmarshal(ok.GetResultJson(), &result); err != nil {
		t.Fatal(err)
	}
	if result["stdout"] != root+"\nfirst\nsecond\n" {
		t.Fatalf("reviewed command bytes changed: %q", result["stdout"])
	}
	for _, command := range []string{"sudo\npwd", "not-allowed\npwd"} {
		args["cmd"] = command
		if _, fail := d.Dispatch(context.Background(), permissionDispatch(t, "exact_command", "", "exec", args)); fail == nil || fail.GetErrorCode() != "denied_by_policy" {
			t.Fatalf("multiline command escaped shell policy: command=%q failure=%v", command, fail)
		}
	}
}
