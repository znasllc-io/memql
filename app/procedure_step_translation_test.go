package app

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// procedure_step_translation_test.go -- a learned procedure's arguments are
// the APP'S (Claude Code's, Codex's, the MCP recorder's), and every spelling
// the replay translates is pinned here, with the observation each tool
// reports. Pure: no host, no engine.

func TestClaudesBashCommandIsTakenAsTheLineItRecorded(t *testing.T) {
	cmd, err := procedureCommandOf(map[string]any{"command": "npm test -- --grep 'a b'", "description": "run the tests", "timeout": float64(120000)})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Line != "npm test -- --grep 'a b'" || cmd.Argv != nil {
		t.Fatalf("command = %+v, want the line verbatim", cmd)
	}
}

func TestCodexsArgumentVectorIsKeptAsAVector(t *testing.T) {
	cmd, err := procedureCommandOf(map[string]any{"command": []any{"grep", "-r", "a b", "*.go", float64(3)}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"grep", "-r", "a b", "*.go", "3"}
	if strings.Join(cmd.Argv, "|") != strings.Join(want, "|") || cmd.Line != "" {
		t.Fatalf("command = %+v, want the vector %v", cmd, want)
	}
}

// An argument vector ran with NO shell, so joining it for `/bin/sh -c` must
// keep every element literal -- a glob, a separator and a quote included.
func TestAVectorJoinsSoTheShellReadsEveryElementBack(t *testing.T) {
	got := procedureJoinArgv([]string{"grep", "-r", "a b", "*.go", "don't", "x;y", "", "a/b.c"})
	want := `grep -r 'a b' '*.go' 'don'\''t' 'x;y' '' a/b.c`
	if got != want {
		t.Fatalf("joined = %s\nwant     %s", got, want)
	}
	// A leading NAME=value is an assignment to a shell, and was the program
	// to the vector; later ones are ordinary arguments either way.
	if got := procedureJoinArgv([]string{"FOO=bar", "x=1"}); got != `'FOO=bar' x=1` {
		t.Fatalf("a leading assignment-shaped word = %s", got)
	}
}

func TestACommandInAnySpellingItCannotReadIsRefused(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"no command":       {"description": "x"},
		"empty line":       {"command": "  "},
		"empty vector":     {"command": []any{}},
		"object element":   {"command": []any{"ls", map[string]any{}}},
		"not a line":       {"command": map[string]any{"a": 1}},
		"blank first word": {"command": []any{" ", "x"}},
	} {
		if _, err := procedureCommandOf(args); err == nil {
			t.Errorf("%s: %v was read as a command", name, args)
		}
	}
	if cmd, err := procedureCommandOf(map[string]any{"cmd": "ls"}); err != nil || cmd.Line != "ls" {
		t.Errorf("the workbench's own `cmd` spelling = %+v, %v", cmd, err)
	}
}

func TestCodexsShellVectorGivesUpItsScript(t *testing.T) {
	for _, argv := range [][]string{
		{"bash", "-lc", "npm test"},
		{"/bin/bash", "-lc", "npm test"},
		{"sh", "-c", "npm test"},
		{"zsh", "-cl", "npm test"},
	} {
		if script, ok := procedureUnwrapShellScript(argv); !ok || script != "npm test" {
			t.Errorf("%v -> %q, %v", argv, script, ok)
		}
	}
	for _, argv := range [][]string{
		{"bash", "-lc", "npm test", "extra"}, // $0 to the script: not expressible bare
		{"python3", "-c", "print(1)"},        // not a shell
		{"bash", "-x", "npm test"},           // not a -c script
		{"bash", "-lc", " "},
		{"bash"},
	} {
		if script, ok := procedureUnwrapShellScript(argv); ok {
			t.Errorf("%v was unwrapped to %q", argv, script)
		}
	}
}

// Relativization turned `cd /ws/run1 && npm test` into `cd . && npm test`,
// and `cd` is not on the workbench's exec allowlist. The prefix becomes the
// working directory, which is the same command.
func TestALeadingCdBecomesTheWorkingDirectory(t *testing.T) {
	for line, want := range map[string][2]string{
		"cd . && npm test":                {"npm test", "."},
		"cd ./app && npm test":            {"npm test", "app"},
		"cd app && cd sub && make":        {"make", "app/sub"},
		"cd 'my dir' && ls":               {"ls", "my dir"},
		"cd app/.. && ls":                 {"ls", "."},
		"npm test":                        {"npm test", "."},
		"  cd ./app&&npm test && echo ok": {"npm test && echo ok", "app"},
	} {
		rest, dir, ok := procedureSplitLeadingCd(line)
		if !ok || rest != want[0] || dir != want[1] {
			t.Errorf("%q -> (%q, %q, %v), want (%q, %q)", line, rest, dir, ok, want[0], want[1])
		}
	}
	for _, line := range []string{"cd /abs && ls", "cd ../out && ls", "cd ~/x && ls", "cd app && cd ../.. && ls"} {
		if rest, dir, ok := procedureSplitLeadingCd(line); ok || rest != line || dir != "." {
			t.Errorf("%q -> (%q, %q, %v), want it untouched and refused", line, rest, dir, ok)
		}
	}
}

func TestClaudesTimeoutIsMillisecondsRoundedUpToSeconds(t *testing.T) {
	for in, want := range map[any]int{float64(120000): 120, float64(1500): 2, float64(1): 1, float64(-5): 0, "600": 0, nil: 0} {
		if got := procedureTimeoutSec(map[string]any{"timeout": in}); got != want {
			t.Errorf("timeout %v -> %d, want %d", in, got, want)
		}
	}
}

func TestAFileStepsPathIsReadFromTheAppsOwnKey(t *testing.T) {
	for _, key := range []string{"file_path", "path", "filePath", "file"} {
		if p, err := procedureFilePathOf(map[string]any{key: " ./a.txt "}); err != nil || p != "./a.txt" {
			t.Errorf("%s -> %q, %v", key, p, err)
		}
	}
	if _, err := procedureFilePathOf(map[string]any{"content": "x"}); err == nil {
		t.Error("a step with no path was given one")
	}
}

func TestAWorkspacePathIsReportedRelativeAndMayNotLeave(t *testing.T) {
	for in, want := range map[string]string{"./out/x.txt": "out/x.txt", "out//x.txt": "out/x.txt", ".": ".", "a/../b": "b", "./": "."} {
		if got, err := procedureWorkspaceRelative(in); err != nil || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "/Users/x/notes.md", "~/notes.md", "../x", "a/../../b", ".."} {
		if got, err := procedureWorkspaceRelative(in); err == nil {
			t.Errorf("%q was accepted as %q", in, got)
		}
	}
}

func TestAPathClimbsOnlyThroughADotDotSegment(t *testing.T) {
	for p, want := range map[string]bool{
		"/Users/x/notes/../../.ssh/authorized_keys": true,
		"/Users/x/..":                  true,
		"../x":                         true,
		"/Users/x/notes/today.md":      false,
		"/Users/x/./notes/today.md":    false,
		"/Users/x/notes/..hidden/a.md": false,
		"/Users/x/notes/a..b.md":       false,
	} {
		if got := procedurePathClimbs(p); got != want {
			t.Errorf("%q climbs = %v, want %v", p, got, want)
		}
	}
}

func TestTheWriteShapeDecidesWriteEditOrMultiEdit(t *testing.T) {
	write, err := procedureWritePlanOf(map[string]any{"file_path": "a", "content": "hi\n"})
	if err != nil || write.Kind != "write" || write.Content != "hi\n" {
		t.Fatalf("Write = %+v, %v", write, err)
	}
	edit, err := procedureWritePlanOf(map[string]any{"file_path": "a", "old_string": "x", "new_string": "y", "replace_all": true})
	if err != nil || edit.Kind != "edit" || len(edit.Edits) != 1 || edit.Edits[0] != (procedureFileEdit{Old: "x", New: "y", ReplaceAll: true}) {
		t.Fatalf("Edit = %+v, %v", edit, err)
	}
	multi, err := procedureWritePlanOf(map[string]any{"file_path": "a", "edits": []any{
		map[string]any{"old_string": "a", "new_string": "b"},
		map[string]any{"old_string": "c", "new_string": "d", "replace_all": true},
	}})
	if err != nil || multi.Kind != "multiEdit" || len(multi.Edits) != 2 || !multi.Edits[1].ReplaceAll {
		t.Fatalf("MultiEdit = %+v, %v", multi, err)
	}
	for name, args := range map[string]map[string]any{
		"nothing":          {"file_path": "a"},
		"content not text": {"file_path": "a", "content": map[string]any{}},
		"edits empty":      {"file_path": "a", "edits": []any{}},
		"edit not object":  {"file_path": "a", "edits": []any{"x"}},
		"new not text":     {"file_path": "a", "old_string": "x", "new_string": float64(1)},
	} {
		if plan, err := procedureWritePlanOf(args); err == nil {
			t.Errorf("%s: read as %+v", name, plan)
		}
	}
}

// Edit refuses EXACTLY where the app's Edit would: absent, ambiguous without
// replace_all, a no-op, an empty old_string on a file that has content, and a
// missing file.
func TestAnEditRefusesWhereTheAppsEditWould(t *testing.T) {
	one := func(old, new string, all bool) []procedureFileEdit {
		return []procedureFileEdit{{Old: old, New: new, ReplaceAll: all}}
	}
	if got, err := applyProcedureEdits("a b a", true, one("b", "c", false)); err != nil || got != "a c a" {
		t.Fatalf("a unique match -> %q, %v", got, err)
	}
	if got, err := applyProcedureEdits("a b a", true, one("a", "z", true)); err != nil || got != "z b z" {
		t.Fatalf("replace_all -> %q, %v", got, err)
	}
	for name, tc := range map[string]struct {
		content string
		exists  bool
		edits   []procedureFileEdit
		says    string
	}{
		"absent":         {"a b", true, one("q", "z", false), "not found"},
		"ambiguous":      {"a b a", true, one("a", "z", false), "found 2 matches"},
		"no-op":          {"a b", true, one("a", "a", false), "no changes"},
		"create on full": {"a b", true, one("", "new", false), "already exists"},
		"missing file":   {"", false, one("a", "b", false), "does not exist"},
	} {
		if got, err := applyProcedureEdits(tc.content, tc.exists, tc.edits); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s -> %q, %v; want a refusal saying %q", name, got, err, tc.says)
		}
	}
	for name, exists := range map[string]bool{"missing": false, "empty": true} {
		if got, err := applyProcedureEdits("", exists, one("", "created\n", false)); err != nil || got != "created\n" {
			t.Errorf("an empty old_string on a %s file -> %q, %v", name, got, err)
		}
	}
}

// A MultiEdit applies in order, each edit on the previous one's result, and
// is all or nothing.
func TestAMultiEditIsSequentialAndAllOrNothing(t *testing.T) {
	got, err := applyProcedureEdits("one two", true, []procedureFileEdit{{Old: "one", New: "three"}, {Old: "three two", New: "done"}})
	if err != nil || got != "done" {
		t.Fatalf("sequential -> %q, %v", got, err)
	}
	if got, err := applyProcedureEdits("one two", true, []procedureFileEdit{{Old: "one", New: "1"}, {Old: "missing", New: "x"}}); err == nil || !strings.HasPrefix(err.Error(), "edit 2:") {
		t.Fatalf("a refused second edit -> %q, %v; want the whole step refused, naming edit 2", got, err)
	}
}

func TestADigestIsLowercaseHexSHA256OfTheBytes(t *testing.T) {
	if got := procedureDigest(""); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("digest of nothing = %s", got)
	}
	if got := procedureDigest("hello\n"); got != "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03" {
		t.Fatalf("digest of hello = %s", got)
	}
}

// Everything the replay itself puts on a workbench command line must be
// admitted by the workbench's REAL allowlist, or the replay would be refused
// by its own plumbing.
func TestTheCommandsTheReplayWritesAreOnTheWorkbenchAllowlist(t *testing.T) {
	for _, cmd := range []string{
		procedureSandboxWriteCommand("report.txt"),
		procedureSandboxWriteCommand("out/deep/report.txt"),
		procedureSandboxWriteCommand("my dir/report.txt"),
		procedureVersionCommand("go"),
		procedureVersionCommand("node"),
		procedureVersionCommand("python3"),
	} {
		if err := workbench.EnforceExecAllowlist(cmd); err != nil {
			t.Errorf("%q: %v", cmd, err)
		}
	}
	if got := procedureSandboxWriteCommand("out/report.txt"); got != "mkdir -p out && cat > out/report.txt" {
		t.Errorf("sandbox write = %q", got)
	}
	if got := procedureSandboxWriteCommand("my dir/it's.txt"); got != `mkdir -p 'my dir' && cat > 'my dir/it'\''s.txt'` {
		t.Errorf("sandbox write with quoting = %q", got)
	}
}

func TestAnExecReportsItsExitCodeItsErrorAndTheTypeOfItsOutput(t *testing.T) {
	obs, out := procedureExecObservation(procedureHostReply{OK: true, Payload: map[string]any{"exitCode": float64(0), "stdout": "{\"ok\":true}\n"}})
	if obs.IsError == nil || *obs.IsError || obs.ExitCode == nil || *obs.ExitCode != 0 || obs.ResultType != "object" {
		t.Fatalf("a clean JSON command = %+v", obs)
	}
	if out["stdoutDigest"] != procedureDigest("{\"ok\":true}\n") {
		t.Fatalf("output = %+v", out)
	}
	obs, out = procedureExecObservation(procedureHostReply{Payload: map[string]any{"exitCode": float64(2), "stdout": "", "stderr": "boom"}})
	if obs.IsError == nil || !*obs.IsError || obs.ExitCode == nil || *obs.ExitCode != 2 || obs.ResultType != "string" {
		t.Fatalf("a failing command = %+v", obs)
	}
	if out["stderrTail"] != "boom" {
		t.Fatalf("a failing command's receipt lost why: %+v", out)
	}
	// A command stopped before an exit code -- a timeout -- is an error with
	// NOTHING measured, which the comparison refuses rather than matches.
	obs, _ = procedureExecObservation(procedureHostReply{ErrorCode: "timeout", ErrorMessage: "exec exceeded 60s"})
	if obs.IsError == nil || !*obs.IsError || obs.ExitCode != nil || obs.ResultType != "" {
		t.Fatalf("a timed-out command = %+v", obs)
	}
}

func TestAFetchIsTypedByItsBodyAndAFailedOneByNothing(t *testing.T) {
	obs, _ := procedureFetchObservation(procedureHostReply{OK: true, Payload: map[string]any{"status": float64(200), "body": "[1,2]"}})
	if obs.IsError == nil || *obs.IsError || obs.ResultType != "array" {
		t.Fatalf("200 with a JSON array = %+v", obs)
	}
	obs, _ = procedureFetchObservation(procedureHostReply{Payload: map[string]any{"status": float64(404), "body": "<html>no</html>"}})
	if obs.IsError == nil || !*obs.IsError || obs.ResultType != "string" {
		t.Fatalf("404 = %+v", obs)
	}
	obs, _ = procedureFetchObservation(procedureHostReply{ErrorCode: "request_failed", ErrorMessage: "dial tcp"})
	if obs.IsError == nil || !*obs.IsError || obs.ResultType != "" {
		t.Fatalf("no response = %+v", obs)
	}
}

func TestAFileObservationCarriesItsContentOnlyWhenItSucceeded(t *testing.T) {
	ok := procedureFileObservation("write", true, "out/x.txt", "abc")
	if ok.IsError == nil || *ok.IsError || len(ok.Contents) != 1 || ok.Contents[0] != (work.ContentDigest{Op: "write", Path: "out/x.txt", Digest: "abc"}) {
		t.Fatalf("a successful write = %+v", ok)
	}
	failed := procedureFileObservation("read", false, "out/x.txt", "")
	if failed.IsError == nil || !*failed.IsError || len(failed.Contents) != 0 {
		t.Fatalf("a failed read = %+v", failed)
	}
}

func TestUnameIsReadInGosNames(t *testing.T) {
	for line, want := range map[string][2]string{
		"Linux x86_64":  {"linux", "amd64"},
		"Darwin arm64":  {"darwin", "arm64"},
		"Linux aarch64": {"linux", "arm64"},
		"Linux i686":    {"linux", "386"},
		"Linux armv7l":  {"linux", "arm"},
		"FreeBSD amd64": {"freebsd", "amd64"},
	} {
		goos, goarch, ok := procedurePlatformOf(line)
		if !ok || goos != want[0] || goarch != want[1] {
			t.Errorf("%q -> (%q, %q, %v), want %v", line, goos, goarch, ok, want)
		}
	}
	if _, _, ok := procedurePlatformOf("Linux"); ok {
		t.Error("one field was read as a platform")
	}
}

func TestAVersionIsTheFirstLineOnStdoutOrElseStderr(t *testing.T) {
	if got := procedureVersionLine(map[string]any{"stdout": "\n  v22.1.0\nmore"}); got != "v22.1.0" {
		t.Errorf("stdout -> %q", got)
	}
	if got := procedureVersionLine(map[string]any{"stdout": "", "stderr": "openjdk 21.0.2 2024-01-16\n"}); got != "openjdk 21.0.2 2024-01-16" {
		t.Errorf("stderr -> %q", got)
	}
	if got := procedureVersionLine(map[string]any{}); got != "" {
		t.Errorf("nothing -> %q", got)
	}
	if procedureVersionCommand("go") != "go version" || procedureVersionCommand("node") != "node --version" {
		t.Error("go is the one tool that takes no version flag")
	}
	for _, name := range []string{"rm -rf /", "a;b", "$(x)", ""} {
		if procedureToolWord.MatchString(name) {
			t.Errorf("%q would be put on a command line as a tool name", name)
		}
	}
}

func TestAPayloadNumberIsAWholeIntOrNothing(t *testing.T) {
	for in, want := range map[any]int{float64(3): 3, 7: 7, int64(9): 9} {
		if got, ok := procedurePayloadInt(in); !ok || got != want {
			t.Errorf("%v -> %d, %v", in, got, ok)
		}
	}
	for _, in := range []any{float64(1.5), float64(1e30), "3", nil} {
		if got, ok := procedurePayloadInt(in); ok {
			t.Errorf("%v was read as %d", in, got)
		}
	}
}
