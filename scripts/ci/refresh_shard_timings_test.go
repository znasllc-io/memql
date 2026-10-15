package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// refresh-shard-timings.sh reads job logs through `gh api`. GitHub's job logs
// carry terminal escape sequences, and since gh 2.102 that call refuses such a
// response unless --allow-escape-sequences is passed; an older gh does not
// know the flag at all. The script's EXIT trap also read a local that had gone
// out of scope, so `set -u` aborted the run. The stub gh answers as the named
// version does; the stub go records the merge instead of rewriting the
// tracked table.
func TestRefreshShardTimingsFetchesEscapedLogsOnAnyGh(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		help     string // what `gh api --help` prints about the flag
		wantFlag bool
	}{
		{"gh 2.102 requires the flag", "      --allow-escape-sequences   Allow printing terminal escape sequences", true},
		{"an older gh does not know it", "      --jq string   Query to select values from the response", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls.log")
			gh := `#!/usr/bin/env bash
echo "gh $*" >> "$STUB_LOG"
case "$*" in
  "api --help") echo "$STUB_HELP" ;;
  "run list"*) echo 101 ;;
  *"actions/runs/101/jobs"*) printf '201\tgo-tests (default-1)\n202\tdb-tests (memql-1)\n' ;;
  *"actions/jobs/"*"/logs"*)
    if [[ "$STUB_HELP" == *allow-escape-sequences* && "$*" != *--allow-escape-sequences* ]]; then
      echo "the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway" >&2
      exit 1
    fi
    if [[ "$STUB_HELP" != *allow-escape-sequences* && "$*" == *--allow-escape-sequences* ]]; then
      echo "unknown flag: --allow-escape-sequences" >&2
      exit 1
    fi
    printf '\033[36;1m2026-10-10T00:00:00Z ok  \tgithub.com/znasllc-io/memql/component/x\t12.5s\033[0m\n' ;;
esac
`
			goStub := "#!/usr/bin/env bash\necho \"go $*\" >> \"$STUB_LOG\"\n"
			for name, body := range map[string]string{"gh": gh, "go": goStub} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", filepath.Join(root, "scripts", "ci", "refresh-shard-timings.sh"), "--runs=1", "--repo=acme/x")
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+calls, "STUB_HELP="+c.help)
			out, err := cmd.CombinedOutput()
			if err != nil || strings.Contains(string(out), "unbound variable") {
				t.Fatalf("refresh failed (%v):\n%s", err, out)
			}
			log, _ := os.ReadFile(calls)
			got := string(log)
			for _, want := range []string{"go run ./scripts/ci/affected timings --lane=go", "go run ./scripts/ci/affected timings --lane=db"} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			if hasFlag := strings.Contains(got, "gh api --allow-escape-sequences repos/acme/x/actions/jobs/201/logs"); hasFlag != c.wantFlag {
				t.Errorf("log fetch passed the escape flag = %v, want %v:\n%s", hasFlag, c.wantFlag, got)
			}
		})
	}
}
