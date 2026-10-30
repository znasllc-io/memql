//go:build !windows

package appsession

import "syscall"

// errReadOnly is what writing under a read-only mount answers.
var errReadOnly = syscall.EROFS
