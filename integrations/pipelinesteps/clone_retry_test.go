package pipelinesteps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCloneFetchRetriesArePinnedBoundedAndNeverRetryCheckout(t *testing.T) {
	requireTools(t, "git")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	git(t, source, "init", "-q", ".")
	writeFiles(t, source, map[string]string{"old.txt": "pinned\n"})
	git(t, source, "add", "old.txt")
	git(t, source, "commit", "-q", "-m", "pinned")
	sha := git(t, source, "rev-parse", "HEAD")
	writeFiles(t, source, map[string]string{"new.txt": "must not appear\n"})
	git(t, source, "add", "new.txt")
	git(t, source, "commit", "-q", "-m", "later")
	for _, failures := range []int{2, 3} {
		t.Run(strconv.Itoa(failures), func(t *testing.T) {
			fixture, workspace := t.TempDir(), t.TempDir()
			wrapper := `#!/bin/sh
set -eu
for argument in "$@"; do
  if [ "$argument" = fetch ]; then
    count=0
    if [ -f "$FETCH_COUNT" ]; then count=$(cat "$FETCH_COUNT"); fi
    count=$((count+1)); printf '%s' "$count" > "$FETCH_COUNT"
    printf '%s\n' "$@" >> "$FETCH_ARGUMENTS"
    if [ "$count" -le "$FETCH_FAILURES" ]; then
      echo 'fixture: truncated TLS fetch' >&2
      exit 128
    fi
  fi
done
if [ "$1" = checkout ]; then echo checkout >> "$CHECKOUTS"; fi
exec "$REAL_GIT" "$@"
`
			if err := os.WriteFile(filepath.Join(fixture, "git"), []byte(wrapper), 0o755); err != nil {
				t.Fatal(err)
			}
			env := append(gitEnv(t), "PATH="+fixture+":"+os.Getenv("PATH"), "REAL_GIT="+realGit,
				"FETCH_COUNT="+filepath.Join(fixture, "count"), "FETCH_ARGUMENTS="+filepath.Join(fixture, "args"),
				"CHECKOUTS="+filepath.Join(fixture, "checkouts"), "FETCH_FAILURES="+strconv.Itoa(failures),
				"CLONE_URL=file://"+source, "SHA="+sha)
			result := runScript(t, cloneScript, workspace, env)
			count, _ := os.ReadFile(filepath.Join(fixture, "count"))
			arguments, _ := os.ReadFile(filepath.Join(fixture, "args"))
			checkouts, _ := os.ReadFile(filepath.Join(fixture, "checkouts"))
			if string(count) != "3" || strings.Count(string(arguments), sha+"\n") != 3 {
				t.Fatal("fetch retries changed the commit or escaped the bound")
			}
			if failures == 2 {
				if result.code != 0 || string(checkouts) != "checkout\n" || git(t, workspace, "rev-parse", "HEAD") != sha {
					t.Fatalf("transient fetch did not recover exactly once: %d %s", result.code, result.stderr)
				}
				if _, err := os.Stat(filepath.Join(workspace, "new.txt")); err == nil {
					t.Fatal("retry checked out a moving branch tip")
				}
			} else if result.code != 128 || len(checkouts) != 0 {
				t.Fatalf("exhausted fetch fell through to checkout: %d %q", result.code, checkouts)
			}
		})
	}
}
