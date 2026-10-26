package capacity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// InferenceActivityFile is separate from the lock inode: process death releases
// the kernel lock, but does not establish that a local runtime stopped its work.
const InferenceActivityFile = "inference-active.json"

var ErrInferenceUnreconciled = errors.New("an interrupted inference operation needs reconciliation before this machine can accept more work")

// CheckInferenceIdle is called with pipeline.lock held, shared or exclusive.
// Presence is a fence, including an empty, malformed or symlinked record. Never
// infer completion from its age or from a worker PID, and never clear it here.
// The runtime caller removes only its own witness after explicit completion.
func CheckInferenceIdle(root string) error {
	_, err := os.Lstat(filepath.Join(root, InferenceActivityFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect retained inference operation: %w", err)
	}
	return ErrInferenceUnreconciled
}
