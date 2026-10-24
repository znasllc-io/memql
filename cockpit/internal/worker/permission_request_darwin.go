//go:build darwin && computeruse

package worker

func permissionRequestsSupported() bool { return true }

// Called only by an explicit request on the owner-only local control socket.
// Status, heartbeats and tool preflight never call these prompting APIs.
func promptForPermission(permission string) {
	if permission == "accessibility" {
		requestAccessibilityAccess()
	} else {
		requestScreenCaptureAccess()
	}
}
