package procedure

import (
	"context"
	"reflect"
	"strings"
	"testing"

	proc "github.com/znasllc-io/memql/component/procedure"
)

// network_replay_test.go -- the runner's two uses of sendsOutside (review
// finding: shadow must not repeat external side effects).

// withCommand rewrites one step of the stored procedure to a literal command,
// its template and its symbol alike.
func withCommand(t *testing.T, w *replayWorld, idx int, command string) {
	t.Helper()
	acts := proc.Canonicalize([]proc.Step{{StepType: "exec", Input: map[string]any{"command": command}, Consumed: true}})
	if len(acts) != 1 {
		t.Fatalf("canonicalizing %q gave %d actions", command, len(acts))
	}
	w.withDecoded(t, func(p *Procedure) {
		p.Steps[idx].Tool, p.Steps[idx].Args = "exec", acts[0].Args
		p.Symbols[idx].Tool, p.Symbols[idx].Template = "exec", acts[0].Args
	})
}

const webhook = "curl -X POST https://hooks.example.test/deploy"

func shadowWithAppArgs(t *testing.T, w *replayWorld, recording string, bindings map[string]string, appArgs []map[string]any) ReplayOutcome {
	t.Helper()
	out, err := w.i.Replay(context.Background(), ReplayRequest{
		OwnerUserId: replayOwner, ConstructId: w.constructId, Mode: ReplayShadow, GoalRunId: recording,
		Bindings: bindings, AppActions: appObservations(), AppArgs: appArgs,
	})
	if err != nil {
		t.Fatalf("Replay (shadow): %v", err)
	}
	return out
}

// TestAShadowComparesACommandThatSendsSomethingOutDry: the procedure's first
// step posts a webhook. Beside every matching recording, a shadow on the
// workbench would post it again. It is compared DRY -- the call it would make
// held to the app's own -- and never dispatched; the step that sends nothing
// is replayed in the sandbox as before. A call the app did not make is a
// mismatch, dry as well, with nothing dispatched at all.
func TestAShadowComparesACommandThatSendsSomethingOutDry(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	withCommand(t, w, 0, webhook)
	appArgs := []map[string]any{{"command": webhook}, {"file_path": relativeReportPath, "content": "hello\n"}}
	out := shadowWithAppArgs(t, w, "v1:work:run:rec-c", map[string]string{"s0.command.7": "c.txt"}, appArgs)
	if !out.Match {
		t.Fatalf("outcome = %+v, want a match", out)
	}
	if keys := dispatchedKeys(w.d.recorded()); !reflect.DeepEqual(keys, []string{"step1"}) {
		t.Fatalf("dispatched %v: the webhook was posted from the sandbox", keys)
	}
	if s := w.work.stepsOf(out.ReplayRunId)["step0"]; s["status"] != "skipped" {
		t.Fatalf("the dry step's row = %v, want it recorded as compared, not run", s)
	}

	other := newReplayWorld(t, "shadow")
	withCommand(t, other, 0, webhook)
	appArgs[0] = map[string]any{"command": "curl -X POST https://hooks.example.test/another"}
	mismatch := shadowWithAppArgs(t, other, "v1:work:run:rec-d", map[string]string{"s0.command.7": "d.txt"}, appArgs)
	if mismatch.Match || mismatch.DivergedStep != 0 || len(other.d.recorded()) != 0 {
		t.Fatalf("a send the app never made = %+v with %d dispatches, want a dry mismatch at step 0", mismatch, len(other.d.recorded()))
	}
}

// TestALaterStepReadsWhatTheAppsCallAnswered: a step compared dry produces
// nothing, so a data-flow hole that reads its output takes the value the
// app's own call gave it -- exactly as a wholly dry comparison does.
func TestALaterStepReadsWhatTheAppsCallAnswered(t *testing.T) {
	w := newReplayWorld(t, "shadow")
	withCommand(t, w, 0, webhook)
	w.withDecoded(t, func(p *Procedure) {
		fp, ok := p.Steps[1].Args.At([]string{"file_path"})
		if !ok || len(fp.Kids) != 3 {
			t.Fatalf("the fixture's write path is %+v, want ./out/<name>", fp)
		}
		fp.Kids[2] = proc.HoleNode("s1.file_path.2", "string")
		p.Holes = append(p.Holes, proc.Hole{Id: "s1.file_path.2", StepIndex: 1, Path: []string{"file_path", "2"}, Type: "string",
			Class: proc.HoleDataFlow, Ref: &proc.DataFlowRef{StepIndex: 0, Path: []string{"stdout"}}})
	})
	appArgs := []map[string]any{{"command": webhook}, {"file_path": relativeReportPath, "content": "hello\n"}}
	out := shadowWithAppArgs(t, w, "v1:work:run:rec-c",
		map[string]string{"s0.command.7": "c.txt", "s1.file_path.2": "report.txt"}, appArgs)
	if !out.Match {
		t.Fatalf("outcome = %+v, want a match: the write's name comes from the app's own call", out)
	}
	calls := w.d.recorded()
	if len(calls) != 1 || calls[0].Args["file_path"] != relativeReportPath {
		t.Fatalf("dispatched %+v, want only the write, bound from the app's call", calls)
	}
}

// TestAServedCommandThatSendsSomethingOutIsASideEffect: on a canary or
// trusted replay the step runs for real, and on the workbench the dispatcher
// says it delivered nothing -- its files stay in the replay's own workspace.
// A push reaches the remote regardless, so the app is told not to repeat it.
func TestAServedCommandThatSendsSomethingOutIsASideEffect(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	withCommand(t, w, 0, "git push origin main")
	w.d.alter["step1"] = func(r *DispatchResult) {
		r.Observation.Contents[0].Digest = "sha256:" + strings.Repeat("0", 64)
	}
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Diverged || len(out.Completed) != 1 || !out.Completed[0].SideEffect {
		t.Fatalf("outcome = %+v, want the push completed and named a side effect", out)
	}
	prompt := w.f.recorded()[0].Guidance.Prompt
	if !containsAll(prompt, "do not repeat them", "git push origin main") {
		t.Fatalf("the app was not told the push already happened:\n%s", prompt)
	}
}
