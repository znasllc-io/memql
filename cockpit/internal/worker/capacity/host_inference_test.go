package capacity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			for name, check := range map[string]func(string) error{"shared": CheckInferenceIdle, "exclusive": CheckInferenceIdleExclusive} {
				if err := check(root); !errors.Is(err, ErrInferenceUnreconciled) {
					t.Fatalf("%s: activity presence was interpreted as completion: %v", name, err)
				}
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
	// However old it is: nothing a Cockpit writes is a symlink, so nothing says
	// when it ends.
	old := time.Now().Add(-24 * time.Hour)
	_ = os.Chtimes(path, old, old)
	for name, check := range map[string]func(string) error{"shared": CheckInferenceIdle, "exclusive": CheckInferenceIdleExclusive} {
		if err := check(root); !errors.Is(err, ErrInferenceUnreconciled) {
			t.Fatalf("%s: dangling activity symlink admitted work: %v", name, err)
		}
	}
	if stat, err := os.Lstat(path); err != nil || stat.Mode()&os.ModeSymlink == 0 {
		t.Fatal("activity symlink was removed", err)
	}
}

// A fence carries the time after which the call it guards cannot still be
// running. Before it the machine is fenced, whatever happened to the call;
// after it the fence is absent, and a reader that excludes every writer removes
// the stale file.
func TestAnInferenceFenceEndsAtTheTimeItNames(t *testing.T) {
	write := func(t *testing.T, until time.Time) (string, string) {
		t.Helper()
		root := t.TempDir()
		data, err := InferenceRecord("0123456789abcdef0123456789abcdef", until)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, InferenceActivityFile)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return root, path
	}

	t.Run("before its time it fences, and says until when", func(t *testing.T) {
		until := time.Now().Add(10 * time.Minute)
		root, path := write(t, until)
		// A file older than the bound changes nothing: the record's own time decides.
		old := time.Now().Add(-2 * InferenceFenceBound)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		for name, check := range map[string]func(string) error{"shared": CheckInferenceIdle, "exclusive": CheckInferenceIdleExclusive} {
			err := check(root)
			if !errors.Is(err, ErrInferenceUnreconciled) {
				t.Fatalf("%s: a fence still in force admitted work: %v", name, err)
			}
			if !strings.Contains(err.Error(), until.UTC().Format(time.RFC3339)) {
				t.Errorf("%s: %q does not say when the fence ends", name, err)
			}
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("a fence still in force was removed", err)
		}
	})

	t.Run("after its time it is absent, and an exclusive reader removes it", func(t *testing.T) {
		root, path := write(t, time.Now().Add(-time.Second))
		if err := CheckInferenceIdle(root); err != nil {
			t.Fatalf("a fence past its time still fenced: %v", err)
		}
		// A reader holding pipeline.lock shared leaves the file: a model call may
		// be writing its own record at that moment.
		if _, err := os.Stat(path); err != nil {
			t.Fatal("a shared reader removed the record", err)
		}
		if err := CheckInferenceIdleExclusive(root); err != nil {
			t.Fatalf("a fence past its time still fenced: %v", err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("the stale record was kept: %v", err)
		}
	})
}

// A record an older Cockpit wrote names no time. It ends InferenceFenceBound
// after the file was written, so an upgrade clears a fence that a cancelled
// call left; and so does a record whose write did not complete.
func TestAnInferenceRecordWithNoTimeEndsTheBoundAfterItWasWritten(t *testing.T) {
	for _, contents := range []string{`{"version":1,"attempt":"0123456789abcdef0123456789abcdef"}`, "", "partial"} {
		t.Run(contents, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, InferenceActivityFile)
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			recent := time.Now().Add(-InferenceFenceBound + time.Minute)
			if err := os.Chtimes(path, recent, recent); err != nil {
				t.Fatal(err)
			}
			if err := CheckInferenceIdleExclusive(root); !errors.Is(err, ErrInferenceUnreconciled) {
				t.Fatalf("a record younger than the bound admitted work: %v", err)
			}
			stale := time.Now().Add(-InferenceFenceBound - time.Minute)
			if err := os.Chtimes(path, stale, stale); err != nil {
				t.Fatal(err)
			}
			if err := CheckInferenceIdle(root); err != nil {
				t.Fatalf("a record older than the bound still fenced: %v", err)
			}
			if err := CheckInferenceIdleExclusive(root); err != nil {
				t.Fatalf("a record older than the bound still fenced: %v", err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("the stale record was kept: %v", err)
			}
		})
	}
}
