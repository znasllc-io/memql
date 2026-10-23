//go:build !darwin && !linux

package tools

import "os"

func openPermissionPath(string, bool) (*os.File, error) { return nil, os.ErrPermission }
func permissionEntryMetadata(*os.File, string) (int64, os.FileMode, error) {
	return 0, 0, os.ErrPermission
}
