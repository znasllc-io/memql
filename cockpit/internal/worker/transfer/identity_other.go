//go:build !darwin && !linux

package transfer

import (
	"os"
)

func fileIdentity(i os.FileInfo) string { return i.Name() + ":" + i.ModTime().UTC().String() }
