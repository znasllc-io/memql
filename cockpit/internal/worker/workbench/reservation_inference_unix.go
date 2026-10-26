//go:build darwin || linux

package workbench

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// CheckInferenceReservations permits live, bounded workspaces to survive a
// model/tool/model cycle. The caller holds pipeline.lock shared and budgets
// Slots * MemoryMiB even for slots that have not been created yet. That fixed
// bound makes concurrent creation safe after this short journal inspection.
// Unlocked retained records are uncertain effects, never available capacity.
func CheckInferenceReservations(root string) error {
	admission, err := lockFile(filepath.Join(root, "workbench-admission.lock"), true)
	if err != nil {
		return err
	}
	defer admission.Close()
	stat, err := admission.Stat()
	if err != nil || stat.Size() != 0 {
		return ErrInterrupted
	}
	paths, err := filepath.Glob(filepath.Join(root, "workbench-*.lock"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if filepath.Base(path) == "workbench-admission.lock" {
			continue
		}
		known := false
		for slot := 0; slot < Slots; slot++ {
			known = known || filepath.Base(path) == fmt.Sprintf("workbench-%d.lock", slot)
		}
		if !known {
			return ErrInterrupted
		}
		if err := checkInferenceSlot(path); err != nil {
			return err
		}
	}
	return nil
}

func checkInferenceSlot(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "workbench inference reservation")
	defer file.Close()
	// Never wait on a workspace: its slot belongs to the whole goal, including
	// the model call asking for this inspection. Creation is serialized by the
	// admission lock; retirement can only remove a reservation and its effects.
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	live := errors.Is(err, syscall.EWOULDBLOCK)
	if err != nil && !live {
		return err
	}
	record, err := readRecord(file)
	if err != nil {
		return err
	}
	if !live && record.Name != "" {
		return ErrInterrupted
	}
	return nil
}
