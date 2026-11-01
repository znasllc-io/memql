//go:build agent

package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/planner"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/common"
)

// A session that did work and then stopped on its plan, as the runner reports it.
func stoppedResult() workerservice.RunResult {
	return workerservice.RunResult{
		SessionId: "v1:worker:appSession:s1", WorkerId: "laptop", Status: workerservice.AppSessionStatusFailed,
		StopReason: workerservice.AppStopOutOfUsage, ErrorMessage: "You've hit your usage limit.",
		Model: "gpt-5.5", Usage: workerservice.AppSessionUsage{InputTokens: 900, OutputTokens: 300, Known: true},
		RecordedActions: 2, Answer: "I edited the parser.", ProducedArtifactIds: []string{"a1"},
	}
}

func TestAppStoppedMidWorkTakesTheMachinesWordAndACompleteRecord(t *testing.T) {
	stop := appStoppedMidWork(stoppedResult(), "codex")
	if stop == nil || stop.AppId != "codex" || stop.RegistrationId != "laptop" || stop.Kind != workerservice.AppStopOutOfUsage ||
		stop.RecordedActions != 2 || stop.LastWords != "I edited the parser." || len(stop.ArtifactIds) != 1 {
		t.Fatalf("got %+v", stop)
	}
	for name, change := range map[string]func(*workerservice.RunResult){
		// Words that merely resemble a limit do not move work that has been done.
		"no word from the machine": func(r *workerservice.RunResult) { r.StopReason = "" },
		"a dropped action":         func(r *workerservice.RunResult) { r.DroppedActions = 1 },
		"an answer already given":  func(r *workerservice.RunResult) { r.Result = []byte(`{"done":true}`) },
		"a run that ended":         func(r *workerservice.RunResult) { r.Status = workerservice.AppSessionStatusEnded },
		"a run that was cancelled": func(r *workerservice.RunResult) { r.Status = workerservice.AppSessionStatusCancelled },
	} {
		result := stoppedResult()
		change(&result)
		if stop := appStoppedMidWork(result, "codex"); stop != nil {
			t.Errorf("%s took over: %+v", name, stop)
		}
	}
}

// A chat turn that spent tokens and ran nothing can simply ask the next app. One that
// ran something has no step to hand over.
func TestAppStoppedWithNoActionsMovesOnlyWhenNothingRan(t *testing.T) {
	result := stoppedResult()
	result.RecordedActions = 0
	var quota *memqlengine.AppQuotaRefused
	if err := appStoppedWithNoActions(result); !errors.As(err, &quota) {
		t.Fatalf("got %v", err)
	}
	result.StopReason = workerservice.AppStopSignedOut
	var signedOut *memqlengine.AppAuthRefused
	if err := appStoppedWithNoActions(result); !errors.As(err, &signedOut) {
		t.Fatalf("got %v", err)
	}
	result.RecordedActions = 1
	if err := appStoppedWithNoActions(result); err != nil {
		t.Fatalf("a chat call that acted moved: %v", err)
	}
}

// journalEngine answers the journal's read as the rows would, and records the writes.
type journalEngine struct {
	rows    []any
	readErr error
	actors  []string
	notes   []string
}

func (e *journalEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if strings.HasPrefix(q, "query workObservationsForOwnerRun(") {
		actor := ""
		if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
			actor = ac.UserId
		}
		e.actors = append(e.actors, actor)
		if e.readErr != nil {
			return nil, e.readErr
		}
		return memqlengine.NewResultWithOutput(e.rows), nil
	}
	if strings.HasPrefix(q, "mutation createWorkObservation(") {
		e.notes = append(e.notes, q)
	}
	return nil, nil
}

func journalRow(tool, actionId string, seq int) map[string]any {
	return map[string]any{"kind": "tool_result", "data": map[string]any{"tool": tool, "appActionId": actionId, "seq": float64(seq), "args": `{"path":"a"}`, "isError": false}}
}

func stoppedExecutor() *recordingExecutor {
	return &recordingExecutor{
		out: planner.ExecutorResult{Output: map[string]any{"sessionId": "v1:worker:appSession:s1"}},
		err: appStoppedMidWork(stoppedResult(), "codex"),
	}
}

func stepHandover() memqlengine.AppSessionHandover {
	return memqlengine.AppSessionHandover{ActingUserId: "u1", AppId: "codex", RegistrationId: "laptop", RunId: "v1:work:run:r1", StepId: "draft", Prompt: "Fix the parser"}
}

// The journal is what establishes what happened, read as the session's owner, on
// whichever replica holds the session: the MCP node writes its half elsewhere.
func TestDelegateCompletesAStopWithItsJournal(t *testing.T) {
	eng := &journalEngine{rows: []any{journalRow("exec", "toolu_1", 1), journalRow("mcp", "mcp-createThing-2", 2), journalRow("fs_write", "toolu_2", 3)}}
	d := newAppSessionDelegateFor(stoppedExecutor(), discardingJournal(), eng, nil)
	_, err := d.RunStep(context.Background(), stepHandover())
	var stop *memqlengine.AppStoppedMidWork
	if !errors.As(err, &stop) {
		t.Fatalf("got %v", err)
	}
	if len(stop.Journal) != 3 || stop.ChildRunId == "" {
		t.Fatalf("the record did not reach the stop: %+v", stop)
	}
	if len(eng.actors) != 1 || eng.actors[0] != "u1" {
		t.Fatalf("the journal must be read as its owner: %v", eng.actors)
	}
}

// With no way to say what happened, nothing takes over, and the step fails with the
// app's own sentence rather than a second agent working on a guess.
func TestDelegateTakesBackAStopWithoutACompleteRecord(t *testing.T) {
	for name, tt := range map[string]struct {
		journal *workjournal.Journal
		engine  *journalEngine
		want    string
	}{
		"a write the recording swallowed": {
			discardingJournal(), &journalEngine{rows: []any{journalRow("exec", "toolu_1", 1), journalRow("mcp", "mcp-createThing-2", 2)}},
			"fewer actions",
		},
		"a journal that cannot be read": {
			discardingJournal(), &journalEngine{readErr: errors.New("no database")},
			"could not be read",
		},
		"a node that recorded nothing": {
			nil, &journalEngine{},
			"recorded no journal",
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newAppSessionDelegateFor(stoppedExecutor(), tt.journal, tt.engine, nil)
			_, err := d.RunStep(context.Background(), stepHandover())
			var stop *memqlengine.AppStoppedMidWork
			if err == nil || errors.As(err, &stop) {
				t.Fatalf("still a takeover: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "You've hit your usage limit.") {
				t.Fatalf("the person must read why: %v", err)
			}
		})
	}
}

func takingOver(h memqlengine.AppSessionHandover) memqlengine.AppSessionHandover {
	stop := appStoppedMidWork(stoppedResult(), "codex")
	stop.ChildRunId = "v1:work:run:first"
	stop.Journal = []memqlengine.AppWorkAction{{Tool: "exec", Args: `{"command":"git push"}`, FromMachine: true}}
	h.AppId = "claude-code"
	h.TakingOverFrom = []*memqlengine.AppStoppedMidWork{stop}
	return h
}

func TestDelegateGivesTheNextAppTheRecordOnTheSameMachine(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s2"}}}
	eng := &journalEngine{}
	d := newAppSessionDelegateFor(ex, discardingJournal(), eng, nil)
	if _, err := d.RunStep(context.Background(), takingOver(stepHandover())); err != nil {
		t.Fatal(err)
	}
	prompt, _ := ex.got.Input["prompt"].(string)
	if !strings.HasPrefix(prompt, "Fix the parser") || !strings.Contains(prompt, "[MemQL handover]") || !strings.Contains(prompt, "git push") || !strings.Contains(prompt, "same working directory") {
		t.Fatalf("got %q", prompt)
	}
	if len(ex.got.Inputs) != 0 {
		t.Fatalf("the files are already there: %v", ex.got.Inputs)
	}
	// A person reading the work can see why two apps served one step.
	if len(eng.notes) != 1 || !strings.Contains(eng.notes[0], "claude-code is taking over") || !strings.Contains(eng.notes[0], "takeover") {
		t.Fatalf("notes: %v", eng.notes)
	}
}

func TestDelegateDeliversWhatReachedTheLibraryToAnotherMachine(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s2"}}}
	d := newAppSessionDelegateFor(ex, discardingJournal(), &journalEngine{}, nil)
	h := takingOver(stepHandover())
	h.RegistrationId = "desktop"
	if _, err := d.RunStep(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	prompt, _ := ex.got.Input["prompt"].(string)
	if !strings.Contains(prompt, "different machine") {
		t.Fatalf("got %q", prompt)
	}
	if len(ex.got.Inputs) != 1 || ex.got.Inputs[0] != "a1" {
		t.Fatalf("inputs: %v", ex.got.Inputs)
	}
}

// A person's whole-prompt override replaces the handed-over prompt. It must not replace
// the record of what has already happened.
func TestTheRecordSurvivesAWholePromptOverride(t *testing.T) {
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s2"}}}
	d := newAppSessionDelegateFor(ex, discardingJournal(), &journalEngine{}, nil)
	ctx := overriddenSessionStep(&common.StepOverride{Prompt: "Do it differently", WholePrompt: true}, nil)
	h := takingOver(stepHandover())
	h.StepId = "revise"
	if _, err := d.RunStep(ctx, h); err != nil {
		t.Fatal(err)
	}
	prompt, _ := ex.got.Input["prompt"].(string)
	if !strings.HasPrefix(prompt, "Do it differently") || !strings.Contains(prompt, "[MemQL handover]") || strings.Contains(prompt, "Fix the parser") {
		t.Fatalf("got %q", prompt)
	}
}

// A takeover's recording starts in the middle, so it must not become a procedure.
func TestATakeoverSessionIsNoRecordingOfTheGoal(t *testing.T) {
	var opened []string
	journal := workjournal.New(workjournal.ExecutorFunc(func(_ context.Context, q string) (any, error) {
		if strings.HasPrefix(q, "mutation createWorkRun(") {
			opened = append(opened, q)
		}
		return nil, nil
	}), nil, "node-a")
	eng := &parentRunEngine{parent: map[string]any{"id": "v1:work:run:r1", "goalSignature": "sig-of-the-goal"}}
	ex := &recordingExecutor{out: planner.ExecutorResult{Output: map[string]any{"sessionId": "s2"}}}
	d := newAppSessionDelegateFor(ex, journal, eng, nil)
	if _, err := d.RunStep(context.Background(), takingOver(stepHandover())); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || strings.Contains(opened[0], "sig-of-the-goal") {
		t.Fatalf("a recording from the middle joined the goal's corpus: %v", opened)
	}
}
