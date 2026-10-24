//go:build !linux && !darwin

package appsession

import "os"

// openForRecord opens a file for the recording to read. The pipe and the
// link race openread_unix.go closes are unix hazards.
func openForRecord(path string) (*os.File, error) {
	return os.Open(path)
}
