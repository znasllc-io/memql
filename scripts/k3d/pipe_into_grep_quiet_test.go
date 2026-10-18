package k3d

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pipeIntoQuietGrep matches a pipeline ending in a quiet grep: `| grep -q`, any
// flag cluster containing q (`-qxF`, `-Fxq`), or `--quiet` / `--silent`.
var pipeIntoQuietGrep = regexp.MustCompile(`\|\s*grep\s+(?:-[A-Za-z]*q[A-Za-z]*|--quiet|--silent)\b`)

// Under `set -o pipefail`, `producer | grep -q pattern` is a race, not a test.
// grep -q exits on its first match; a producer still writing -- k3d prints its
// tables in more than one write -- then dies of SIGPIPE, and pipefail makes the
// pipeline's status that 141. The match reads as a miss. Measured against a
// live cluster (memql#6207): `k3d node list | grep -q` reported an existing
// build node missing in 33 of 40 runs, so a re-run of `make up` tried to create
// it again and failed. With one cluster per product, `k3d cluster list` has
// several rows, and the same race reaches the cluster checks in up.sh, down.sh
// and status.sh. Reading the whole stream (grep without -q, output to
// /dev/null) or grepping a here-string has no such race.
//
// Scoped to the local bring-up scripts and the libraries they source, all of
// which run under pipefail.
func TestNoPipeIntoQuietGrepUnderPipefail(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "lib")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.sh"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if pipeIntoQuietGrep.MatchString(line) {
					t.Errorf("%s:%d pipes into a quiet grep, which races under pipefail; use `grep ... >/dev/null` or a here-string:\n  %s", f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}
