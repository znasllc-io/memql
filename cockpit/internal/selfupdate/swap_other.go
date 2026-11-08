//go:build !darwin

package selfupdate

import "os"

func swap(target, incoming, previous string) error { return swapFile(target, incoming, previous) }
func restore(target, previous string) error        { return os.Rename(previous, target) }
