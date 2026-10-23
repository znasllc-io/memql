//go:build !darwin || !computeruse

package worker

func permissionRequestsSupported() bool { return false }
func promptForPermission(string)        {}
