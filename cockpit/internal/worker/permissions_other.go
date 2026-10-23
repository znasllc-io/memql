//go:build (!darwin && !linux) || !computeruse

package worker

import (
	"fmt"
	"runtime"
)

func currentPermissionSnapshot() permissionSnapshot {
	return permissionSnapshot{
		detail: fmt.Sprintf("Permission preflight unsupported on this platform/build (%s/%s); permissions are unknown, not measured denials.", runtime.GOOS, cockpitBuildTag()),
	}
}
