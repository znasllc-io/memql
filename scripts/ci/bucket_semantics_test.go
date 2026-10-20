package ci

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// trackedFiles returns every file git tracks, which is the population CI
// filters actually score.
func trackedFiles(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = RepoRoot()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned nothing -- the guard fails closed rather than " +
			"reporting a clean tree it never examined")
	}
	return files
}

// Native path sets subtract exclusions. The repository-wide gate bucket is
// intentionally broad because its tests read files outside the import graph;
// other buckets must select real subsets, not every change or no change.
func TestNativePathBucketsHaveRealCoverage(t *testing.T) {
	files := trackedFiles(t)
	for name, patterns := range nativePipeline(t).Select.Buckets {
		match, err := pipelines.PathSet(patterns)
		if err != nil {
			t.Fatal(err)
		}
		matches := 0
		for _, file := range files {
			if match(file) {
				matches++
			}
		}
		if matches == 0 {
			t.Errorf("native bucket %s matches no tracked file", name)
		}
		if matches == len(files) && name != "gates" {
			t.Errorf("native bucket %s unexpectedly matches every tracked file", name)
		}
	}
}
