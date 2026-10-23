//go:build !linux && !darwin

package backup

import "os"

// No native stable identity available on this build: never guess from a name.
func fileIdentity(os.FileInfo) string { return "" }
