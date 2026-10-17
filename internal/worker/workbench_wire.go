package worker

import (
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql-cockpit/internal/worker/workbench"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"time"
)

func validateWorkbenchEnrollment(cfg Config, conn *Connection, d *memqlv1.ToolDispatch, now time.Time) error {
	if d.GetTool() != "workerWorkbench" && d.GetWorkbench() == nil {
		return nil
	}
	c := d.GetWorkbench()
	machine, err := machineIDFor(cfg)
	if err != nil || c == nil || d.GetTool() != "workerWorkbench" || d.GetComputerPermission() != nil || conn == nil || c.GetOwnerUserId() != conn.OwnerUserId || c.GetRegistrationId() != conn.RegistrationId || c.GetMachineId() != machine || c.GetRunId() != d.GetRunId() || c.GetCallerUserId() == "" || c.GetRunId() == "" || c.GetExpiresUnix() < now.Unix() || c.GetExpiresUnix() > now.Add(time.Minute).Unix()+5 {
		return fmt.Errorf("workbench admission belongs to another machine, enrollment or goal, or has expired")
	}
	return nil
}
func applyWorkbenchAdvertisement(r *memqlv1.Register, ready bool) {
	caps := make([]string, 0, len(r.Capabilities)+1)
	for _, c := range r.Capabilities {
		if c != workbench.Capability {
			caps = append(caps, c)
		}
	}
	r.Capabilities = caps
	delete(r.Concurrency, workbench.Capability)
	if !ready {
		return
	}
	var descriptor map[string]any
	if json.Unmarshal([]byte(r.CapabilityDescriptorJson), &descriptor) != nil {
		return
	}
	contracts, ok := descriptor["actionContracts"].(map[string]any)
	if !ok {
		contracts = map[string]any{}
		descriptor["actionContracts"] = contracts
	}
	contracts["workerWorkbench"] = workbench.Contract
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return
	}
	r.CapabilityDescriptorJson = string(raw)
	r.Capabilities = append(r.Capabilities, workbench.Capability)
	if r.Concurrency == nil {
		r.Concurrency = map[string]uint32{}
	}
	r.Concurrency[workbench.Capability] = workbench.Slots
}
