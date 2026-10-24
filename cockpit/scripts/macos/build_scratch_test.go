package macos_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real capability EXIT trap: cleanup must preserve its single
// JSON result and original failure, including when errexit or a signal fires.
func TestBuildScratchCleanupPreservesCapabilityResult(t *testing.T) {
	capability, err := filepath.Abs("../../../scripts/lib/capability.sh")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs("build-scratch.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, finish string
		code         int
		remains      bool
	}{
		{"success", "cleanup_build_scratch || cap_fail 5 cleanup; cap_ok", 0, false},
		{"explicit failure", "cap_fail 3 refused", 3, false},
		{"errexit", "false", 1, false},
		{"cancel", "kill -TERM $$", 143, false},
		{"cleanup failure", "function rm() { return 1; }; cleanup_build_scratch || cap_fail 5 cleanup", 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			neighbor := filepath.Join(root, "keep")
			if err := os.WriteFile(neighbor, []byte("unowned"), 0600); err != nil {
				t.Fatal(err)
			}
			script := `set -euo pipefail
source "$1"
source "$2"
cap_init test.scratch test
export TMPDIR="$3"
create_build_scratch test-build
echo partial > "$COCKPIT_BUILD_SCRATCH/partial"
` + tc.finish
			cmd := exec.Command("bash", "-c", script, "test", capability, helper, root)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					code = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Fatalf("exit=%d want=%d: %s %s", code, tc.code, stdout.String(), stderr.String())
			}
			var envelope struct {
				OK    bool `json:"ok"`
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("not exactly one envelope: %s: %v", stdout.String(), err)
			}
			if envelope.OK != (tc.code == 0) || (tc.code != 0 && (envelope.Error == nil || envelope.Error.Code != tc.code)) {
				t.Fatalf("result lost original status: %s", stdout.String())
			}
			matches, err := filepath.Glob(filepath.Join(root, "test-build.*"))
			if err != nil || (len(matches) != 0) != tc.remains {
				t.Fatalf("scratch after exit: %v %v", matches, err)
			}
			if b, err := os.ReadFile(neighbor); err != nil || string(b) != "unowned" {
				t.Fatal("cleanup touched unowned neighbor")
			}
		})
	}
}

func TestPackagingFailureRemovesNestedBuildScratch(t *testing.T) {
	for _, script := range []string{"package-app.sh", "package-menubar.sh"} {
		t.Run(script, func(t *testing.T) {
			root := t.TempDir()
			bin := t.TempDir()
			// Force the first compile to fail even on a Linux CI host. The
			// menu package reaches both parent and child scratch allocations.
			for _, tool := range []string{"swiftc", "codesign"} {
				if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 45\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMPDIR", root)
			args := []string{script, "--arch=arm64", "--output=" + t.TempDir()}
			if script == "package-app.sh" {
				args = append(args, "--worker=/missing-worker")
			}
			cmd := exec.Command("bash", args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err == nil {
				t.Fatal("broken build succeeded")
			}
			if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"ok":false`) {
				t.Fatalf("invalid failure: %s %s", stdout.String(), stderr.String())
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("packaging left scratch: %v %v", entries, err)
			}
		})
	}
}
