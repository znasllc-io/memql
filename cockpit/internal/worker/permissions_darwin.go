//go:build darwin && computeruse

package worker

func currentPermissionSnapshot() permissionSnapshot {
	return measureMacOSPermissions(preflightAccessibilityAccess, preflightScreenCaptureAccess)
}
