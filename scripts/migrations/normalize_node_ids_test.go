// Tests for scripts/migrations/normalize-node-ids.sh (capability
// migrations.normalize-node-ids, #2439).
//
// The assertion this file exists for: --apply=false REPORTS and writes
// nothing. The script read --apply with cap_flag and tested it for emptiness,
// so the string "false" counted as set, and
// `--apply=false --confirm=normalize-node-ids` rewrote every legacy id in
// "MemoryNodes" and node_vectors. A boolean is read with cap_bool now, which
// also honours a JSON params object on stdin, as the contract says params
// arrive, and refuses a value that is not a boolean before anything runs.
//
// Hermetic: a stub psql on a PATH prefix answers every statement and logs it.
// No database is contacted.
package migrations

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const nniConfirm = "--confirm=normalize-node-ids"

// nniStubPSQL answers like a database holding legacy ids: every UPDATE moves
// two rows, every COUNT finds none (so the recount after a rewrite is clean),
// and anything else (the connection probe) answers 1. Each statement is
// appended to the log as one line.
const nniStubPSQL = `#!/bin/sh
sql=""
while [ $# -gt 0 ]; do
    if [ "$1" = "-c" ]; then sql="$2"; shift; fi
    shift
done
printf '%s\n' "$sql" | tr '\n' ' ' >> "$NNI_PSQL_LOG"
printf '\n' >> "$NNI_PSQL_LOG"
case "$sql" in
    *UPDATE*) echo 2 ;;
    *'COUNT(*)'*) echo 0 ;;
    *) echo 1 ;;
esac
`

type nniEnvelope struct {
	OK      bool `json:"ok"`
	Changed bool `json:"changed"`
	Result  struct {
		Applied        bool `json:"applied"`
		RewrittenNodes int  `json:"rewrittenNodes"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// nniRun runs the script against the stub with the given stdin (empty for a
// closed one) and returns its exit code, its one envelope and the statements
// psql was handed.
func nniRun(t *testing.T, stdin string, args ...string) (int, nniEnvelope, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(nniStubPSQL), 0o755); err != nil {
		t.Fatalf("write stub psql: %v", err)
	}
	log := filepath.Join(dir, "psql.log")
	script, err := filepath.Abs("normalize-node-ids.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Unreachable by design: the stub answers, and a real psql would fail fast.
	cmd := exec.Command("bash", append([]string{script, "--database-url=postgres://nobody:nobody@127.0.0.1:1/none"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "NNI_PSQL_LOG="+log)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		code = ee.ExitCode()
	} else if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	diag := "--- stdout ---\n" + stdout.String() + "--- stderr ---\n" + stderr.String()
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout must carry exactly one envelope, got %d lines\n%s", len(lines), diag)
	}
	var env nniEnvelope
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("stdout is not an envelope: %v\n%s", err, diag)
	}
	statements, _ := os.ReadFile(log)
	return code, env, string(statements)
}

func TestNormalizeNodeIdsApplyFalseOnlyReports(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		args  []string
	}{
		{name: "--apply=false", args: []string{"--apply=false", nniConfirm}},
		{name: "--apply=0", args: []string{"--apply=0", nniConfirm}},
		{name: "omitted", args: []string{nniConfirm}},
		{name: "apply false on stdin", stdin: `{"apply": false, "confirm": "normalize-node-ids"}`, args: []string{"--params-stdin"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, env, statements := nniRun(t, tc.stdin, tc.args...)
			if code != 0 || !env.OK {
				t.Fatalf("exit %d ok=%v, want a successful report; error: %+v", code, env.OK, env.Error)
			}
			if strings.Contains(statements, "UPDATE") {
				t.Errorf("%s REWROTE stored ids -- a false boolean must take the report-only path; psql was handed:\n%s", tc.name, statements)
			}
			if env.Result.Applied || env.Changed {
				t.Errorf("envelope says applied=%v changed=%v for a report-only run", env.Result.Applied, env.Changed)
			}
		})
	}
}

func TestNormalizeNodeIdsApplyTrueRewrites(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		args  []string
	}{
		{name: "bare --apply", args: []string{"--apply", nniConfirm}},
		{name: "--apply=true", args: []string{"--apply=true", nniConfirm}},
		{name: "apply true on stdin", stdin: `{"apply": true, "confirm": "normalize-node-ids"}`, args: []string{"--params-stdin"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, env, statements := nniRun(t, tc.stdin, tc.args...)
			if code != 0 || !env.OK {
				t.Fatalf("exit %d ok=%v, want a successful rewrite; error: %+v", code, env.OK, env.Error)
			}
			if !strings.Contains(statements, "UPDATE") {
				t.Errorf("%s did not rewrite; psql was handed:\n%s", tc.name, statements)
			}
			if !env.Result.Applied || !env.Changed || env.Result.RewrittenNodes != 2 {
				t.Errorf("envelope says applied=%v changed=%v rewrittenNodes=%d, want true/true/2",
					env.Result.Applied, env.Changed, env.Result.RewrittenNodes)
			}
		})
	}
}

// A value that is neither true nor false is refused before the database is
// contacted, and the apply gate still wants its phrase.
func TestNormalizeNodeIdsRefusesBeforeWriting(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantExit int
	}{
		{name: "a mistyped boolean", args: []string{"--apply=ture", nniConfirm}, wantExit: 2},
		{name: "apply without its phrase", args: []string{"--apply"}, wantExit: 3},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, env, statements := nniRun(t, "", tc.args...)
			if code != tc.wantExit || env.OK || env.Error == nil || env.Error.Code != tc.wantExit {
				t.Fatalf("exit %d, envelope %+v; want a refusal with code %d", code, env, tc.wantExit)
			}
			if strings.Contains(statements, "UPDATE") {
				t.Errorf("refused, yet rewrote stored ids; psql was handed:\n%s", statements)
			}
		})
	}
}
