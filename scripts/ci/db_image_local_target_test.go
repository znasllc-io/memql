package ci

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The legacy builder visits intervening stages even when the final stage has
// no dependency on them. The local wrapper must stop at operand, before the
// native builder's verified stage introduces RUN --mount.
func TestLocalDatabaseBuildStopsBeforeBuildKitVerification(t *testing.T) {
	for _, builderExit := range []int{0, 42} {
		t.Run(strconv.Itoa(builderExit), func(t *testing.T) {
			bin := t.TempDir()
			docker := `#!/usr/bin/env bash
set -eu
[[ "$1" == build ]] || exit 91
shift
target=''
while (($#)); do
  if [[ "$1" == --target ]]; then target="$2"; shift; fi
  shift
done
if [[ "$target" != operand ]]; then
  echo 'the --mount option requires BuildKit: the legacy builder reached verified' >&2
  exit 1
fi
if [[ "$MEMQL_TEST_BUILD_EXIT" != 0 ]]; then
  echo 'database builder failed' >&2
fi
exit "$MEMQL_TEST_BUILD_EXIT"
`
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", filepath.Join(dbImageRepoRoot(t), "scripts/db-image/build.sh"), "--smokeTest=false")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"DOCKER_BUILDKIT=0", "MEMQL_TEST_BUILD_EXIT="+strconv.Itoa(builderExit))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			var result struct {
				OK     bool `json:"ok"`
				Result struct {
					Image       string `json:"image"`
					SmokeTested bool   `json:"smokeTested"`
				} `json:"result"`
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if parseErr := json.Unmarshal(out, &result); parseErr != nil {
				t.Fatalf("capability envelope: %v\n%s\n%s", parseErr, out, stderr.String())
			}
			if builderExit == 0 {
				if err != nil || !result.OK || result.Result.Image != "memql-db:16-dev" || result.Result.SmokeTested {
					t.Fatalf("local operand build failed or changed the requested validation: %v\n%s\n%s", err, out, stderr.String())
				}
			} else {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 5 || result.OK || result.Error.Code != 5 || !strings.Contains(stderr.String(), "database builder failed") {
					t.Fatalf("builder failure was lost: %v\n%s\n%s", err, out, stderr.String())
				}
			}
		})
	}
}
