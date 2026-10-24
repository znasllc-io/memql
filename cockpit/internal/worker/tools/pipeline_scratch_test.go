//go:build darwin || linux

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeScratchIsOwnedAndRemovedOnEveryCommandOutcome(t *testing.T) {
	allowLocalClones(t)
	fx := newPipelineFixture(t)
	for _, outcome := range []string{"success", "failure", "timeout", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			// Checkout shares the command deadline. Keep the fixture's normal
			// setup budget so a busy host cannot turn an ordinary exit into a
			// timeout before this test reaches the scratch it needs to inspect.
			policy, root := pipelineTestPolicy(t, "")
			inherited := t.TempDir()
			for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(name, inherited)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := &recordedOutput{}
			if outcome == "cancel" {
				out.onChunk = func(c recordedChunk) {
					if strings.Contains(c.data, "scratch-ready") {
						cancel()
					}
				}
			}
			command := `set -e
test "$TMPDIR" = "$TMP" && test "$TMPDIR" = "$TEMP"
test "$(cd "$TMPDIR/../.." && pwd -P)" = "$(pwd -P)"
case "$TMPDIR" in */.git/memql-scratch-*) ;; *) exit 20 ;; esac
made=$(mktemp -d "$TMPDIR/build.XXXXXX")
echo partial > "$made/artifact"
test -z "$(git status --porcelain)"
printf 'scratch=%s\nscratch-ready\n' "$TMPDIR"
`
			var extra map[string]any
			switch outcome {
			case "failure":
				command += "exit 9\n"
			case "timeout":
				// Exercise a real deadline with headroom for checkout. Policy
				// clamping has its own wall-clock test in pipeline_step_test.go.
				extra = map[string]any{"timeoutSec": 10}
				command += "sleep 60\n"
			case "cancel":
				command += "sleep 60\n"
			}
			result, failed := runPipelineStep(ctx, "", stepArgs(t, fx, command, extra), policy, out.emit, out.artifact)
			if outcome == "success" || outcome == "failure" {
				got := decoded(t)(result, failed)
				want := 0
				if outcome == "failure" {
					want = 9
				}
				if got.ExitCode != want {
					t.Fatalf("exit=%d want=%d: %s", got.ExitCode, want, out.all())
				}
			} else {
				want := "timeout"
				if outcome == "cancel" {
					want = "cancelled"
				}
				if result != nil || failed == nil || failed.GetErrorCode() != want {
					t.Fatalf("interrupted work must report %s: %v %v", want, result, failed)
				}
			}
			var scratch string
			for _, line := range strings.Split(out.text(false), "\n") {
				if strings.HasPrefix(line, "scratch=") {
					scratch = strings.TrimPrefix(line, "scratch=")
				}
			}
			if scratch == "" {
				t.Fatalf("command did not reach owned scratch: %s", out.all())
			}
			if !filepath.IsAbs(scratch) {
				t.Fatalf("relative scratch: %s", scratch)
			}
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Fatalf("native scratch remains: %v", err)
			}
			assertNoStepDirs(t, root)
			entries, err := os.ReadDir(inherited)
			if err != nil || len(entries) != 0 {
				t.Fatalf("build wrote outside owned scratch: %v %v", entries, err)
			}
		})
	}
}

func TestNativeScratchCannotBeRedirectedByRequestEnvironment(t *testing.T) {
	for _, field := range []string{"env", "secrets"} {
		for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
			t.Run(field+"/"+name, func(t *testing.T) {
				args := stepArgs(t, pipelineFixture{url: "https://github.com/o/r.git", shas: []string{"", strings.Repeat("a", 40)}}, "true",
					map[string]any{field: map[string]any{name: "/unowned"}})
				if _, err := parsePipelineStep(args); err == nil || !strings.Contains(err.Error(), "scratch cleanup") {
					t.Fatalf("unowned scratch accepted: %v", err)
				}
			})
		}
	}
}
