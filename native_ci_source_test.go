package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// Required checks and publication now execute through the native manifest.
// Repository-owned Actions must not silently return beside that delivery path.
// CODEOWNERS, Dependabot's package updates and CodeQL configuration remain.
func TestRepositoryCISourceIsNative(t *testing.T) {
	if len(engineManifest(t).Pipeline.Stages) == 0 {
		t.Fatal("native pipeline has no stages")
	}
	for _, root := range []string{".github/workflows", ".github/actions"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() && repowalk.SkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			if !entry.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
				t.Errorf("%s restores an Actions execution path; declare the work in memql-package.yaml", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
