//go:build !darwin && !linux

package tools

import "os"

func execScopedCommandChild([]string) error { return os.ErrPermission }
