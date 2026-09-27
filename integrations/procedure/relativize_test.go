package procedure

import (
	"context"
	"reflect"
	"testing"
	"time"

	proc "github.com/znasllc-io/memql/component/procedure"
)

// relativize_test.go -- a recording's workspace, written as the workspace
// (epic memql#5408, the coordinator's Task 4 decision).

// seedWorkspaceRecording answers the three reads loadCorpus makes for one
// recorded session that wrote one file in its own workspace. The fingerprint
// carries the workspace when fingerprinted is true; otherwise only the
// action's own cwd names it.
func seedWorkspaceRecording(t *testing.T, eng *fakeEngine, runId, workspace, file string, fingerprinted bool) map[string]any {
	t.Helper()
	step := map[string]any{"id": "v1:work:step:" + runId + "-w", "runId": runId, "key": "action-w", "seq": float64(0), "stepType": "fs_write"}
	if fingerprinted {
		step["fingerprint"] = map[string]any{"cwd": workspace, "cwdEntries": float64(0)}
	}
	eng.replyWhen("workStepsForOwnerRun", `"`+runId+`"`, step)
	eng.replyWhen("workObservationsForOwnerRun", `"`+runId+`"`, map[string]any{
		"kind": "tool_result", "stepKey": "action-w",
		"data": map[string]any{
			"tool": "fs_write", "isError": false, "resultType": "string", "cwd": workspace,
			"args": argsJSON(t, map[string]any{"file_path": workspace + "/out/" + file, "content": "hello\n"}),
		},
	})
	return map[string]any{
		"id": runId, "ownerUserId": testOwner, "goalSignature": testSignature, "status": "succeeded",
		"createdAt": testNow.Format(time.RFC3339Nano),
	}
}

// generalizeLoaded is the lift's first three stages over the loaded corpus.
func generalizeLoaded(t *testing.T, eng *fakeEngine) proc.Template {
	t.Helper()
	recs, err := newTestIntegration(eng).loadCorpus(context.Background(), corpusKeyFor())
	if err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("loaded %d recordings, want 2", len(recs))
	}
	corpus := make([][]proc.Action, 0, len(recs))
	for _, rec := range recs {
		corpus = append(corpus, proc.Canonicalize(rec.Steps))
	}
	tmpl := proc.Generalize(corpus)
	tmpl.Holes = proc.Classify(tmpl, corpus)
	return tmpl
}

// TestRecordingsInDifferentWorkspacesGeneralizeToOneFileNameHole is the
// decision's headline. Two sessions ran in /ws/r1 and /ws/r2 and wrote
// <W>/out/report-a.txt and <W>/out/report-b.txt. Read as recorded, the
// workspace segment differs too, and the template carries a SECOND hole --
// one no goal input ever supplies, so the procedure could be compared in
// shadow and never replayed for real. Relativized, the only thing left to
// vary is the file name.
func TestRecordingsInDifferentWorkspacesGeneralizeToOneFileNameHole(t *testing.T) {
	eng := newFakeEngine()
	runs := []map[string]any{
		seedWorkspaceRecording(t, eng, "v1:work:run:r1", "/ws/r1", "report-a.txt", true),
		seedWorkspaceRecording(t, eng, "v1:work:run:r2", "/ws/r2", "report-b.txt", true),
	}
	eng.reply("workRunsForOwnerGoalSignature", runs...)

	tmpl := generalizeLoaded(t, eng)
	if len(tmpl.Holes) != 1 {
		t.Fatalf("the template has %d holes (%+v), want ONE: the file name", len(tmpl.Holes), tmpl.Holes)
	}
	h := tmpl.Holes[0]
	if !reflect.DeepEqual(h.Path, []string{"file_path", "2"}) {
		t.Fatalf("the hole is at %v, want the file name inside ./out/<name>", h.Path)
	}
	root, _ := tmpl.Steps[0].Args.At([]string{"file_path", "0"})
	if root == nil || root.Lit != "." {
		t.Fatalf("the path's first segment is %+v, want the workspace written as `.`", root)
	}
}

// TestAnUnfingerprintedRecordingIsRelativizedAgainstItsActionsCwd: a
// recording whose fingerprint never arrived is still a recording, and the
// directory the action reported running in is the workspace it names.
func TestAnUnfingerprintedRecordingIsRelativizedAgainstItsActionsCwd(t *testing.T) {
	eng := newFakeEngine()
	runs := []map[string]any{
		seedWorkspaceRecording(t, eng, "v1:work:run:r1", "/ws/r1", "report-a.txt", false),
		seedWorkspaceRecording(t, eng, "v1:work:run:r2", "/ws/r2", "report-b.txt", false),
	}
	eng.reply("workRunsForOwnerGoalSignature", runs...)
	if tmpl := generalizeLoaded(t, eng); len(tmpl.Holes) != 1 {
		t.Fatalf("with no fingerprint the template has %d holes, want the file name alone", len(tmpl.Holes))
	}
}

// TestAPathMerelySharingTheWorkspacePrefixIsUntouched: /ws/r10 is not inside
// /ws/r1, and neither are /ws/r1.bak, /ws/r1-old or /x/ws/r1. Rewriting any of
// them would send a replay to a directory the recording never named.
func TestAPathMerelySharingTheWorkspacePrefixIsUntouched(t *testing.T) {
	const ws = "/ws/r1"
	for _, s := range []string{"/ws/r10", "/ws/r10/out.txt", "/ws/r1.bak", "/ws/r1-old/x", "/x/ws/r1", "ws/r1", ""} {
		if got := relativizeWhole(s, ws); got != s {
			t.Errorf("relativizeWhole(%q) = %q, want it untouched", s, got)
		}
		cmd := "cat " + s + " && ls " + s
		if got := relativizeCommand(cmd, ws); got != cmd {
			t.Errorf("relativizeCommand(%q) = %q, want it untouched", cmd, got)
		}
	}
	got := relativizeArgs(map[string]any{"file_path": "/ws/r10/out.txt", "command": "cp /ws/r10/a /ws/r1/b"}, ws)
	if got["file_path"] != "/ws/r10/out.txt" || got["command"] != "cp /ws/r10/a ./b" {
		t.Fatalf("relativizeArgs = %v", got)
	}
}

// TestACommandIsRelativizedInsideWhereverTheWorkspaceIsAWholePath: `cd /ws/r1
// && npm test` is `cd . && npm test` for a replay whose working directory is
// its own workspace -- the same directory the recording's cd reached.
func TestACommandIsRelativizedInsideWhereverTheWorkspaceIsAWholePath(t *testing.T) {
	const ws = "/ws/r1"
	for in, want := range map[string]string{
		"cd /ws/r1 && npm test":               "cd . && npm test",
		"cp /ws/r1/a.txt /ws/r1/out/":         "cp ./a.txt ./out/",
		"tar czf x.tgz -C /ws/r1 .":           "tar czf x.tgz -C . .",
		`bash -lc 'cd "/ws/r1/app" && make'`:  `bash -lc 'cd "./app" && make'`,
		"PATH=/ws/r1/bin:$PATH run":           "PATH=./bin:$PATH run",
		"--out=/ws/r1":                        "--out=.",
		"/ws/r1/tool --in /ws/r10/x /ws/r1/y": "./tool --in /ws/r10/x ./y",
	} {
		if got := relativizeCommand(in, ws); got != want {
			t.Errorf("relativizeCommand(%q) = %q, want %q", in, got, want)
		}
	}
	// An ARGUMENT VECTOR under a command key: each element is rewritten, the
	// shell's -c script inside as a command line of its own.
	vec := relativizeArgs(map[string]any{"command": []any{"bash", "-lc", "cd /ws/r1 && npm test"}}, ws)
	if got := vec["command"].([]any)[2]; got != "cd . && npm test" {
		t.Fatalf("argv element = %q", got)
	}
	// A value that is not a command is rewritten only as a whole: a message
	// that mentions the workspace is content, not a path.
	msg := relativizeArgs(map[string]any{"message": "wrote /ws/r1/out.txt", "path": "/ws/r1"}, ws)
	if msg["message"] != "wrote /ws/r1/out.txt" || msg["path"] != "." {
		t.Fatalf("relativizeArgs = %v", msg)
	}
}

// TestNoWorkspaceRelativizesNothing: without an absolute workspace below the
// root there is nothing to be relative to, and "/" contains every path.
func TestNoWorkspaceRelativizesNothing(t *testing.T) {
	in := map[string]any{"file_path": "/ws/r1/out.txt", "command": "cd /ws/r1"}
	for _, ws := range []string{"", "/", "relative/dir"} {
		if got := relativizeArgs(in, ws); !reflect.DeepEqual(got, in) {
			t.Errorf("workspace %q rewrote %v", ws, got)
		}
	}
}
