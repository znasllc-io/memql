//go:build linux && computeruse

package worker

import (
	"os"

	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

func currentPermissionSnapshot() permissionSnapshot {
	return measureLinuxPermissions(tools.DetectDisplayServer(), os.Getenv("DISPLAY"), probeX11Access)
}
