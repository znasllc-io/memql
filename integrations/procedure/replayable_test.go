package procedure

import (
	"strings"
	"testing"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// replayable_test.go -- a version no dispatcher can run never climbs (epic
// memql#5408, the coordinator's Task 4 decision).

// stepOf canonicalizes one recorded action the way the lift does, so every
// case below is the step a real recording would have produced.
func stepOf(t *testing.T, tool string, args map[string]any) proc.TemplateStep {
	t.Helper()
	acts := proc.Canonicalize([]proc.Step{{StepType: tool, Input: args, Consumed: true}})
	if len(acts) != 1 {
		t.Fatalf("canonicalizing %s %v gave %d actions", tool, args, len(acts))
	}
	return proc.TemplateStep{Tool: acts[0].Tool, Args: acts[0].Args}
}

// TestEveryFormTheDispatchersRunIsReplayable pins the dispatcher contract from
// the accepting side: each spelling a Dispatcher must translate.
func TestEveryFormTheDispatchersRunIsReplayable(t *testing.T) {
	for name, s := range map[string]proc.TemplateStep{
		"a Claude Code command":         stepOf(t, "exec", map[string]any{"command": "mkdir -p out && echo hi > a.txt", "description": "make it"}),
		"a Codex argument vector":       stepOf(t, "exec", map[string]any{"command": []any{"bash", "-lc", "npm test"}}),
		"a Claude Code Write":           stepOf(t, "fs_write", map[string]any{"file_path": "./out/a.txt", "content": "hi\n"}),
		"a Claude Code Edit":            stepOf(t, "fs_write", map[string]any{"file_path": "./a.go", "old_string": "x", "new_string": "y"}),
		"a Claude Code MultiEdit":       stepOf(t, "fs_write", map[string]any{"file_path": "./a.go", "edits": []any{map[string]any{"old_string": "x", "new_string": "y"}}}),
		"a Claude Code Read":            stepOf(t, "fs_read", map[string]any{"file_path": "./a.txt"}),
		"a Codex image view":            stepOf(t, "fs_read", map[string]any{"path": "./a.png"}),
		"a fetch of a URL":              stepOf(t, "fetch", map[string]any{"url": "https://example.test/x", "prompt": "summarise"}),
		"a MemQL tool named by the MCP": stepOf(t, "mcp", map[string]any{"tool": "runQuery", "arguments": map[string]any{"q": "x"}}),
	} {
		if ok, why := replayable(s); !ok {
			t.Errorf("%s is refused: %s", name, why)
		}
	}
}

// TestAStepNoDispatcherRunsIsNotReplayable is the refusing side, and every
// refusal must SAY what is wrong -- the sentence is the ladder's reason.
func TestAStepNoDispatcherRunsIsNotReplayable(t *testing.T) {
	programHole := stepOf(t, "exec", map[string]any{"command": "make build"})
	programHole.Args.Kids[0].Kids[0] = proc.HoleNode("s0.command.0", "string")
	for name, tc := range map[string]struct {
		step proc.TemplateStep
		want string
	}{
		"a level-2 automation step": {proc.TemplateStep{Tool: "automation:sendDigest", Args: proc.Obj(nil)}, "automation sendDigest"},
		"the app's answer":          {proc.TemplateStep{Tool: "app_answer", Args: proc.Obj(nil)}, "no dispatcher runs"},
		"an unknown tool":           {proc.TemplateStep{Tool: "other", Args: proc.Obj(nil)}, "no dispatcher runs"},
		"an exec with no command":   {stepOf(t, "exec", map[string]any{"prompt": "delegate this", "subagent_type": "x"}), "no command"},
		"a background command":      {stepOf(t, "exec", map[string]any{"command": "npm run dev", "run_in_background": true}), "background"},
		"a program that is a hole":  {programHole, "program"},
		"a notebook edit":           {stepOf(t, "fs_write", map[string]any{"notebook_path": "./n.ipynb", "new_source": "x"}), "notebook"},
		"a Codex update":            {stepOf(t, "fs_write", map[string]any{"changes": []any{map[string]any{"path": "./a", "kind": map[string]any{"type": "update"}, "diff": "@@"}}}), "changes"},
		// No dispatcher reads `changes` at all -- not even an add, whose
		// diff is the whole file: such a procedure would climb on dry shadow
		// comparisons, ask a person to approve it, and fail every canary.
		"a Codex file that is added": {stepOf(t, "fs_write", map[string]any{"changes": []any{map[string]any{"path": "./out.txt", "kind": map[string]any{"type": "add"}, "diff": "hello\n"}}}), "changes"},
		"a write with no content":    {stepOf(t, "fs_write", map[string]any{"file_path": "./a", "mode": "0644"}), "form no dispatcher applies"},
		"a Glob listing":             {stepOf(t, "fs_read", map[string]any{"pattern": "**/*.go", "path": "."}), "listing"},
		"a web search":               {stepOf(t, "fetch", map[string]any{"query": "weather"}), "no URL"},
		"an mcp call with no tool":   {stepOf(t, "mcp", map[string]any{"q": "x"}), "does not name the MemQL tool"},
	} {
		ok, why := replayable(tc.step)
		if ok {
			t.Errorf("%s is replayable; no dispatcher runs it", name)
			continue
		}
		if !strings.Contains(why, tc.want) {
			t.Errorf("%s: reason %q does not say %q", name, why, tc.want)
		}
	}
}

// TestAVersionWithAStepNoDispatcherRunsStaysACandidateNamingTheStep: the
// recordings wrote their report through a notebook edit, which no dispatcher
// applies. The version is lifted -- it is what the recordings did -- and held
// at candidate, the reason naming the step and the tool, however cleanly the
// candidate gate and Gate 1 pass.
func TestAVersionWithAStepNoDispatcherRunsStaysACandidateNamingTheStep(t *testing.T) {
	recs := twoRecordings()
	for n := range recs {
		recs[n].writeArgs = map[string]any{"notebook_path": reportPath, "new_source": "hello\n"}
	}
	eng, res := liftFixture(t, recs...)
	if res.Rung != work.RungCandidate {
		t.Fatalf("rung = %s, want candidate: a version no dispatcher runs never climbs", res.Rung)
	}
	reason := argsOf(t, eng.callTo(t, "recordConstructLadder"))["ladderReason"].(string)
	// The second step, numbered from 1 as MemQL OS lists a procedure's steps.
	if !strings.Contains(reason, "step 2 (fs_write)") || !strings.Contains(reason, "notebook") {
		t.Fatalf("reason %q must name the step (from 1), the tool and why", reason)
	}

	// The control: the same corpus written with a Write enters shadow.
	_, ok := liftFixture(t, twoRecordings()...)
	if ok.Rung != work.RungShadow {
		t.Fatalf("the control lifted to %s, so the refusal above proves nothing", ok.Rung)
	}
}
