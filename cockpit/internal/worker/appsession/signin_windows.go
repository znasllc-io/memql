//go:build windows

package appsession

import "errors"

// errReadOnly never matches here: there is no such mount to meet.
var errReadOnly = errors.New("read-only file system")
