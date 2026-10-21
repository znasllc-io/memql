// Path routing cannot disable its own drift guards.
package dev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var pathRoutingGuardPackages = []string{"scripts/ci", "scripts/dev"}

const coverageGuardTest = "TestEveryTrackedFileReachesAConsumedBucket"

func TestPathRoutingLaneIsNotGatedOnTheRoutingItGuards(t *testing.T) {
	stage, step := nativeStep(t, "path-routing")
	if len(stage.On) > 0 || len(stage.Needs) > 0 || step.When != nil {
		t.Fatal("path routing guards must run unconditionally")
	}
}
func TestPathRoutingLaneRunsTheRoutingGuards(t *testing.T) {
	patterns := nativeSuite(t, "path-routing")
	for _, pkg := range pathRoutingGuardPackages {
		covered := false
		for _, pat := range patterns {
			if coveredBy(pkg, pat) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("path routing omits %s", pkg)
		}
	}
}

// The lane is worth nothing if the guard has moved out from under it. Derived
// from the source rather than assumed, so relocating the coverage guard fails
// here instead of leaving the lane running a package that objects to nothing.
func TestCoverageGuardStillLivesInAPackageTheLaneRuns(t *testing.T) {
	root := repoRoot(t)

	found := ""
	for _, pkg := range pathRoutingGuardPackages {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(pkg)))
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pkg), e.Name()))
			if err != nil {
				t.Fatalf("read %s/%s: %v", pkg, e.Name(), err)
			}
			if strings.Contains(string(raw), "func "+coverageGuardTest+"(") {
				found = pkg + "/" + e.Name()
			}
		}
	}
	if found == "" {
		t.Errorf("no file in %v declares func %s. The lane runs those packages precisely "+
			"to schedule that guard unconditionally; if the guard moved, move the lane's "+
			"package list with it rather than leaving a lane that verifies nothing "+
			"(memql#3451).", pathRoutingGuardPackages, coverageGuardTest)
	}
}
