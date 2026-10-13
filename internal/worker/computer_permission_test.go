package worker

import (
	"context"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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
