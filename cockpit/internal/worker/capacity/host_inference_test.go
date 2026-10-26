package capacity

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInferenceActivityPresenceFencesEveryWorkload(t *testing.T) {
	for _, contents := range []string{"", "partial", `{"version":1,"attempt":"unknown"}`} {
		t.Run(contents, func(t *testing.T) {
			root := t.TempDir()
			if err := CheckInferenceIdle(root); err != nil {
				t.Fatal("empty capacity directory is fenced", err)
			}
			path := filepath.Join(root, InferenceActivityFile)
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckInferenceIdle(root); !errors.Is(err, ErrInferenceUnreconciled) {
				t.Fatal("activity presence was interpreted as completion", err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != contents {
				t.Fatal("checking capacity changed the activity witness", err)
			}
		})
	}
}

func TestInferenceActivitySymlinkIsNeverTreatedAsAbsence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, InferenceActivityFile)
	if err := os.Symlink(filepath.Join(root, "missing-target"), path); err != nil {
		t.Skip("symlinks are unavailable on this test host")
	}
	if err := CheckInferenceIdle(root); !errors.Is(err, ErrInferenceUnreconciled) {
		t.Fatal("dangling activity symlink admitted work", err)
	}
	if stat, err := os.Lstat(path); err != nil || stat.Mode()&os.ModeSymlink == 0 {
		t.Fatal("activity symlink was removed", err)
	}
}
