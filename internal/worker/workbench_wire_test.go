package worker

import (
	"context"
	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"strings"
	"testing"
	"time"
)

func TestWorkbenchAdmissionBindsLiveEnrollmentAndNeverAdmitsHostTools(t *testing.T) {
	for _, change := range []string{"", "owner", "machine", "registration", "caller", "run", "expired", "host-tool", "mixed-permission", "missing-constraint"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now()
			wire := &memqlv1.ToolDispatch{Tool: "workerWorkbench", Action: "fs_list", RunId: "run", Workbench: &memqlv1.WorkbenchConstraint{OwnerUserId: "owner", CallerUserId: "borrower", RegistrationId: "registration", MachineId: "machine", RunId: "run", ExpiresUnix: now.Add(time.Minute).Unix(), WorkspaceExpiresUnix: now.Add(time.Hour).Unix()}}
			switch change {
			case "owner":
				wire.Workbench.OwnerUserId = "foreign"
			case "machine":
				wire.Workbench.MachineId = "foreign"
			case "registration":
				wire.Workbench.RegistrationId = "foreign"
			case "caller":
				wire.Workbench.CallerUserId = ""
			case "run":
				wire.Workbench.RunId = "another-run"
			case "expired":
				wire.Workbench.ExpiresUnix = now.Add(-time.Second).Unix()
			case "host-tool":
				wire.Tool = "workerHost"
			case "mixed-permission":
				wire.ComputerPermission = &memqlv1.ComputerPermissionConstraint{}
			case "missing-constraint":
				wire.Workbench = nil
			}
			stream := &seamStream{}
			conn := &Connection{conn: stream, OwnerUserId: "owner", RegistrationId: "registration"}
			tool := &recordingTools{}
			r := &Runner{cfg: Config{MachineID: "machine"}, tools: tool, logger: quietLogger()}
			r.runToolDispatch(context.Background(), conn, wire)
			if change == "" {
				if tool.calls.Load() != 1 {
					t.Fatal("valid isolated admission was refused")
				}
			} else if tool.calls.Load() != 0 || stream.frames[0].GetToolResult().GetFailure().GetErrorCode() != "workbench_constraint_refused" {
				t.Fatal("invalid admission reached a tool")
			}
		})
	}
}
func TestWorkbenchAdvertisementRequiresActualRuntimeNotConfigLabels(t *testing.T) {
	for _, ready := range []bool{false, true} {
		cfg := Config{Capabilities: []string{"HEADLESS", "WORKBENCH"}, Concurrency: map[string]uint32{"WORKBENCH": 99}, workbenchReady: ready}
		r := buildRegister(cfg, nil, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{})
		found := false
		for _, c := range r.Capabilities {
			found = found || c == "WORKBENCH"
		}
		if found != ready {
			t.Fatal("config manufactured runtime capability")
		}
		if ready && r.Concurrency["WORKBENCH"] != 2 {
			t.Fatal("operator config widened fixed capacity")
		}
	}
}
func TestWorkbenchSetupPreservesOtherFeaturePolicy(t *testing.T) {
	original := "# existing consent\nmodels:\n  allow: [qwen]\napps:\n  homes:\n    production:\n      allow: [codex]\nworkbenches:\n  allow: false\n"
	updated, changed, err := enableWorkbenchPolicy(original)
	if err != nil || !changed {
		t.Fatal(err)
	}
	for _, keep := range []string{"# existing consent", "qwen", "production:", "codex", "workbenches:"} {
		if !strings.Contains(updated, keep) {
			t.Fatal("setup discarded other policy", keep)
		}
	}
	if next, changed, err := enableWorkbenchPolicy(updated); err != nil || changed || next != updated {
		t.Fatal("setup is not idempotent", err)
	}
	if _, _, err := enableWorkbenchPolicy("workbenches: [invalid]\n"); err == nil {
		t.Fatal("setup rewrote an unknown policy shape")
	}
}
