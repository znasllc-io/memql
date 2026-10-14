package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func TestComputerPermissionUsesExactLiveEnrollmentBeforeDispatch(t *testing.T) {
	for _, mismatch := range []string{"owner", "registration", "machine", ""} {
		t.Run(mismatch, func(t *testing.T) {
			stream := &seamStream{}
			conn := &Connection{conn: stream, OwnerUserId: "owner", RegistrationId: "registration"}
			tool := &recordingTools{}
			r := &Runner{cfg: Config{MachineID: "machine"}, tools: tool, logger: quietLogger()}
			constraint := &memqlv1.ComputerPermissionConstraint{OwnerUserId: "owner", RegistrationId: "registration", MachineId: "machine"}
			switch mismatch {
			case "owner":
				constraint.OwnerUserId = "another-owner"
			case "registration":
				constraint.RegistrationId = "another-enrollment"
			case "machine":
				constraint.MachineId = "another-machine"
			}
			r.runToolDispatch(context.Background(), conn, &memqlv1.ToolDispatch{CallId: "call", ComputerPermission: constraint})
			if len(stream.frames) != 1 {
				t.Fatalf("expected one result, got %d", len(stream.frames))
			}
			result := stream.frames[0].GetToolResult()
			if mismatch == "" {
				if tool.calls.Load() != 1 || result.GetFailure() != nil {
					t.Fatalf("exact enrollment did not reach dispatcher: %v", result)
				}
			} else if tool.calls.Load() != 0 || result.GetFailure().GetErrorCode() != "computer_permission_refused" {
				t.Fatalf("mismatched enrollment reached dispatcher: %v", result)
			}
		})
	}
}

func TestDirectoryPermissionReachesRealFilesystemThroughRunner(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "listing-proof.txt"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"path": root}
	hash, err := permission.Hash("workerHost", "fs_list", args)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"fs_list": args})
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &memqlv1.ToolDispatch{CallId: "scoped-read", Tool: "workerHost", Action: "fs_list", ArgsJson: raw,
		ComputerPermission: &memqlv1.ComputerPermissionConstraint{Version: permission.Version, PermissionId: "approval", OwnerUserId: "owner", RegistrationId: "registration", MachineId: "machine", Kind: "directory_read", Directory: root, ArgsHash: hash}}
	state := t.TempDir()
	for _, phase := range []string{"fresh", "restarted", "other-enrollment", "locally-stopped", "stop-after-restart"} {
		t.Run(phase, func(t *testing.T) {
			gate := consent.NewPersistentHomes(state).For("cluster-a")
			stream := &seamStream{}
			conn := &Connection{conn: stream, OwnerUserId: "owner", RegistrationId: "registration"}
			if phase == "other-enrollment" {
				conn.RegistrationId = "new-registration"
			}
			if phase == "locally-stopped" {
				if err := gate.Revoke(); err != nil {
					t.Fatal(err)
				}
			}
			r := &Runner{cfg: Config{MachineID: "machine"}, tools: tools.NewHomeDispatcher(quietLogger(), tools.DefaultPolicy(), gate, "cluster-a"), logger: quietLogger()}
			r.runToolDispatch(context.Background(), conn, dispatch)
			if len(stream.frames) != 1 {
				t.Fatalf("expected one result, got %d", len(stream.frames))
			}
			result := stream.frames[0].GetToolResult()
			wantError := ""
			switch phase {
			case "other-enrollment":
				wantError = "computer_permission_refused"
			case "locally-stopped", "stop-after-restart":
				wantError = "consent_required"
			}
			if wantError != "" {
				if result.GetFailure().GetErrorCode() != wantError {
					t.Fatalf("expected %s: %v", wantError, result)
				}
				return
			}
			if result.GetFailure() != nil {
				t.Fatalf("approved read failed: %v", result)
			}
			var listing struct {
				Entries []struct {
					Name string `json:"name"`
				} `json:"entries"`
			}
			if err := json.Unmarshal(result.GetSuccess().GetResultJson(), &listing); err != nil {
				t.Fatal(err)
			}
			if len(listing.Entries) != 1 || listing.Entries[0].Name != "listing-proof.txt" {
				t.Fatalf("wrong real listing: %+v", listing)
			}
		})
	}
}
