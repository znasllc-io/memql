package worker

import (
	workerservice "github.com/znasllc-io/memql/component/worker"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

// Routing metadata may lag a reconnect. The stream holder rechecks support
// before consuming a one-use permission or sending the operation.
func machineSupportsFileRead(w *workerservice.Worker, tool, action string, args map[string]any) bool {
	if tool != "workerHost" || action != "fs_read" || !permission.ExtendedRead(args) {
		return true
	}
	return w != nil && w.CapabilityDescriptor != nil && w.CapabilityDescriptor.ActionContracts.Supports(permission.ReadContract, permission.ReadVersion)
}
