//go:build darwin

package selfupdate

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func swap(target, incoming, previous string) error {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return swapFile(target, incoming, previous)
	}
	// Whole bundles must exchange atomically: moving the old directory away
	// first leaves launchd without a runnable worker if the updater crashes.
	if err = unix.RenamexNp(target, incoming, unix.RENAME_SWAP); err != nil {
		return errors.New("Could not atomically replace MemQL.app; the previous app is still installed")
	}
	if err = os.Rename(incoming, previous); err != nil {
		if restoreErr := unix.RenamexNp(target, incoming, unix.RENAME_SWAP); restoreErr != nil {
			return fmt.Errorf("Could not retain the previous app; recovery files: %s", incoming)
		}
		return errors.New("Could not retain the previous app; the original installation was restored")
	}
	return nil
}
func restore(target, previous string) error {
	info, err := os.Stat(previous)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return unix.RenamexNp(target, previous, unix.RENAME_SWAP)
	}
	return os.Rename(previous, target)
}
