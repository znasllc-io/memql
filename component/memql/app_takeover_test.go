package memql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// takeoverDelegate answers each RunStep in turn, with success past the end of its list.
type takeoverDelegate struct {
	calls   []AppSessionHandover
	answers []error
}

func (d *takeoverDelegate) RunStep(_ context.Context, h AppSessionHandover) (AppSessionOutcome, error) {
	i := len(d.calls)
	d.calls = append(d.calls, h)
	id := fmt.Sprintf("session-%d", i)
	if i < len(d.answers) && d.answers[i] != nil {
		return AppSessionOutcome{SessionId: id}, d.answers[i]
	}
	return AppSessionOutcome{Content: "done", SessionId: id}, nil
}

// takeoverApps offers each app on each machine, recording what selection was asked.
type takeoverApps struct {
	stubApps
	machines []string
	requests []AppCallRequest
	excluded [][]string
}

func (a *takeoverApps) SelectApp(_ context.Context, req AppCallRequest, _ bool, excluded []string) (AppSelection, error) {
	a.requests = append(a.requests, req)
	a.excluded = append(a.excluded, append([]string(nil), excluded...))
	machines := a.machines
	if len(machines) == 0 {
		machines = []string{"laptop"}
	}
	for _, app := range []string{"claude-code", "codex"} {
		for _, machine := range machines {
			if !AppSelectionExcluded(excluded, app, machine) && (req.AppId == "*" || req.AppId == app) {
				return AppSelection{app, machine}, nil
			}
		}
	}
	return AppSelection{}, ErrAppUnavailable
}

func stoppedPartway(kind string) *AppStoppedMidWork {
	return &AppStoppedMidWork{
		AppId: "claude-code", RegistrationId: "laptop", Kind: kind,
		Reason:    "You've hit your weekly limit",
		SessionId: "session-0", ChildRunId: "child-run", RecordedActions: 2,
		LastWords: "I have edited the parser and was about to run the tests.",
		Journal: []AppWorkAction{
			{Tool: "exec", Args: `{"command":"git push origin main"}`, ExitCode: intPtr(0), FromMachine: true},
			{Tool: "fs_write", Args: `{"path":"src/parser.go"}`, FromMachine: true},
		},
		ArtifactIds: []string{"artifact-1"},
	}
}

func intPtr(n int) *int { return &n }

func takeoverRun(t *testing.T, app string, a *takeoverApps, d *takeoverDelegate) (*sessionProvider, error) {
	t.Helper()
	r := sessionRegistry(d)
	r.SetAppInference(a)
	p := r.SessionProvider(app, "", SessionRequest{ActingUserId: "alice", RunId: "run", StepId: "step", Level: "reasoning"}).(*sessionProvider)
	_, err := p.run(delegatedProviderTestContext("alice", "run", "step"), []common.ChatMessage{{Role: "user", Content: "Fix the parser"}}, []common.ToolDefinition{{Name: "tool"}}, nil)
	return p, err
}

// The owner: "instead of just saying, oh, I run out of credits for Codex, it should now
// attempt Claude, for Claude Code to take over". The next app gets the same step, told
// what already happened, on the machine that holds the work.
func TestSessionTakeoverHandsTheRecordToTheNextApp(t *testing.T) {
	stop := stoppedPartway(AppStopOutOfUsage)
	d := &takeoverDelegate{answers: []error{fmt.Errorf("cockpit-app: claude-code run failed: %w", stop)}}
	a := &takeoverApps{}
	p, err := takeoverRun(t, "*", a, d)
	if err != nil {
		t.Fatalf("no takeover: %v", err)
	}
	if len(d.calls) != 2 {
		t.Fatalf("calls: %+v", d.calls)
	}
	first, second := d.calls[0], d.calls[1]
	if first.AppId != "claude-code" || second.AppId != "codex" || second.RegistrationId != "laptop" {
		t.Fatalf("wrong order: %s then %s on %s", first.AppId, second.AppId, second.RegistrationId)
	}
	if len(first.TakingOverFrom) != 0 {
		t.Fatalf("the first session took over from nothing: %+v", first.TakingOverFrom)
	}
	if len(second.TakingOverFrom) != 1 || second.TakingOverFrom[0] != stop {
		t.Fatalf("the record did not travel: %+v", second.TakingOverFrom)
	}
	// The contract stays whole: the delegate adds the record after any override, which
	// could replace this prompt, has been applied.
	if second.Prompt != first.Prompt || second.StepId != first.StepId || second.RunId != first.RunId || second.ToolScope != first.ToolScope {
		t.Fatal("the step's contract changed across apps")
	}
	// A limit is the account's, so the app is out on every machine, and the machine
	// holding the work is asked first.
	if got := a.excluded[1]; len(got) != 1 || got[0] != "claude-code" {
		t.Fatalf("excluded: %v", got)
	}
	if a.requests[0].PreferRegistrationId != "" || a.requests[1].PreferRegistrationId != "laptop" {
		t.Fatalf("preference: %q then %q", a.requests[0].PreferRegistrationId, a.requests[1].PreferRegistrationId)
	}
	if last, ok := p.LastSession(); !ok || last.SessionId != "session-1" {
		t.Fatalf("the answer must be the session that finished: %+v", last)
	}
}

// A login is one machine's, so the same app elsewhere can carry on, and a pin allows it.
func TestSessionTakeoverOnALoginMovesToAnotherMachine(t *testing.T) {
	d := &takeoverDelegate{answers: []error{stoppedPartway(AppStopSignedOut)}}
	a := &takeoverApps{machines: []string{"laptop", "desktop"}}
	if _, err := takeoverRun(t, "claude-code", a, d); err != nil {
		t.Fatalf("no takeover: %v", err)
	}
	if len(d.calls) != 2 || d.calls[1].AppId != "claude-code" || d.calls[1].RegistrationId != "desktop" {
		t.Fatalf("calls: %+v", d.calls)
	}
}

// A person who pinned one app did not ask for another, and when nothing can take over
// the call says what happened and that the work so far is kept.
func TestSessionTakeoverStaysOnAPinnedApp(t *testing.T) {
	d := &takeoverDelegate{answers: []error{stoppedPartway(AppStopOutOfUsage)}}
	_, err := takeoverRun(t, "claude-code", &takeoverApps{}, d)
	if err == nil || len(d.calls) != 1 {
		t.Fatalf("a pinned app was replaced: %v, %+v", err, d.calls)
	}
	var stop *AppStoppedMidWork
	if !errors.As(err, &stop) {
		t.Fatalf("lost the reason: %v", err)
	}
	if !strings.Contains(err.Error(), "no other app is ready to take over") || !strings.Contains(err.Error(), "You've hit your weekly limit") {
		t.Fatalf("the person must read why: %v", err)
	}
}

// An ordinary failure may have left anything behind, and is never taken over.
func TestSessionTakeoverDoesNotFollowAnOrdinaryFailure(t *testing.T) {
	d := &takeoverDelegate{answers: []error{errors.New("connection lost after writing a file")}}
	if _, err := takeoverRun(t, "*", &takeoverApps{}, d); err == nil || len(d.calls) != 1 {
		t.Fatalf("replayed an uncertain step: %v, %+v", err, d.calls)
	}
}

// Two stops in a row: the third app is told about both.
func TestSessionTakeoverCarriesEveryStopSoFar(t *testing.T) {
	second := stoppedPartway(AppStopOutOfUsage)
	second.AppId, second.RegistrationId = "codex", "laptop"
	d := &takeoverDelegate{answers: []error{stoppedPartway(AppStopSignedOut), second}}
	a := &takeoverApps{machines: []string{"laptop", "desktop"}}
	if _, err := takeoverRun(t, "*", a, d); err != nil {
		t.Fatalf("no takeover: %v", err)
	}
	if len(d.calls) != 3 || len(d.calls[2].TakingOverFrom) != 2 {
		t.Fatalf("calls: %+v", d.calls)
	}
}

func TestAppTakeoverBriefOnTheSameMachine(t *testing.T) {
	brief := AppTakeoverBrief([]*AppStoppedMidWork{stoppedPartway(AppStopOutOfUsage)}, "laptop", "codex")
	for _, want := range []string{
		"Claude Code stopped because it ran out of usage",
		"You've hit your weekly limit",
		"Do not start it again",
		`exec {"command":"git push origin main"} exit 0`,
		`fs_write {"path":"src/parser.go"}`,
		"they happened, whatever the account below says",
		"without first checking",
		"same working directory",
		"I have edited the parser",
		"not evidence",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("missing %q in:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "different machine") {
		t.Errorf("wrong machine sentence:\n%s", brief)
	}
}

func TestAppTakeoverBriefOnAnotherMachine(t *testing.T) {
	brief := AppTakeoverBrief([]*AppStoppedMidWork{stoppedPartway(AppStopOutOfUsage)}, "desktop", "codex")
	if !strings.Contains(brief, "different machine") || !strings.Contains(brief, "1 file") {
		t.Errorf("must say the directory is not here and what was delivered:\n%s", brief)
	}
	if got := AppTakeoverInputs([]*AppStoppedMidWork{stoppedPartway(AppStopOutOfUsage)}, "desktop"); len(got) != 1 || got[0] != "artifact-1" {
		t.Errorf("inputs: %v", got)
	}
	if got := AppTakeoverInputs([]*AppStoppedMidWork{stoppedPartway(AppStopOutOfUsage)}, "laptop"); len(got) != 0 {
		t.Errorf("the files are already there: %v", got)
	}
}

// The brief is bounded: a long step must not push the request itself out of the app's
// context, and what is cut is counted rather than dropped silently.
func TestAppTakeoverBriefIsBoundedAndCountsWhatItLeavesOut(t *testing.T) {
	stop := stoppedPartway(AppStopOutOfUsage)
	stop.Journal = nil
	for i := 0; i < appTakeoverMaxActions+40; i++ {
		stop.Journal = append(stop.Journal, AppWorkAction{Tool: "exec", Args: fmt.Sprintf(`{"command":"step %d %s"}`, i, strings.Repeat("x", 2000))})
	}
	stop.Journal = append(stop.Journal, AppWorkAction{Tool: "fs_read", Args: `{"path":"a"}`}, AppWorkAction{Tool: "agent", Args: `{"todo":"b"}`})
	stop.LastWords = strings.Repeat("y", appTakeoverLastWordsRunes*3) + "the last line"
	brief := AppTakeoverBrief([]*AppStoppedMidWork{stop}, "laptop", "codex")
	if !strings.Contains(brief, "40 earlier actions are not listed") {
		t.Errorf("did not count what it cut:\n%.400s", brief)
	}
	if !strings.Contains(brief, "2 reads and notes are left out") {
		t.Errorf("did not count the reads:\n%.400s", brief)
	}
	if strings.Contains(brief, `step 0 `) || !strings.Contains(brief, fmt.Sprintf(`step %d `, appTakeoverMaxActions+39)) {
		t.Error("must keep the most recent actions")
	}
	if !strings.Contains(brief, "the last line") {
		t.Error("must keep the end of its last words")
	}
	if len(brief) > 2*appTakeoverMaxActions*appTakeoverMaxArgsRunes {
		t.Errorf("brief is %d bytes", len(brief))
	}
}

func TestAppWorkJournalFromRows(t *testing.T) {
	rows := []map[string]any{
		{"kind": "note", "content": "not an action", "data": map[string]any{}},
		{"kind": "tool_result", "data": map[string]any{"tool": "fs_write", "seq": float64(4), "appActionId": "toolu_2", "args": `{"path":"a"}`, "isError": false}},
		{"kind": "tool_result", "data": map[string]any{"tool": "exec", "seq": float64(2), "appActionId": "toolu_1", "args": `{"command":"ls"}`, "exitCode": float64(0), "isError": false}},
		{"kind": "tool_result", "data": map[string]any{"tool": "mcp", "seq": float64(3), "appActionId": "mcp-createThing-3", "args": `{"tool":"createThing"}`, "isError": true, "error": "refused"}},
	}
	got := appWorkJournalFromRows(rows)
	if len(got) != 3 || got[0].Tool != "exec" || got[1].Tool != "mcp" || got[2].Tool != "fs_write" {
		t.Fatalf("not in the order it happened: %+v", got)
	}
	if got[0].ExitCode == nil || *got[0].ExitCode != 0 || got[2].ExitCode != nil {
		t.Fatalf("a write has no exit code, and a clean command has one: %+v", got)
	}
	if !got[0].FromMachine || got[1].FromMachine || !got[1].IsError || got[1].Error != "refused" {
		t.Fatalf("%+v", got)
	}
	if n := appWorkActionsFromMachine(got); n != 2 {
		t.Fatalf("the MCP node writes its own steps, and they are not what the machine counted: %d", n)
	}
}

// A chat turn with no step has no record to hand over. It moves only when nothing
// happened, and an answer already shown cannot be taken back.
func TestAppCallDoesNotMoveOnceAnAnswerWasShown(t *testing.T) {
	offTheSharedRateCeiling(t)
	a := &showingApps{}
	r := &ProviderRegistry{}
	r.SetAppInference(a)
	p := &appProvider{registry: r, appId: "*", wildcard: true, actingUserId: "alice", level: "reasoning"}
	_, err := p.call(context.Background(), AppCallRequest{
		Messages:  []common.ChatMessage{{Role: "user", Content: "Hello"}},
		OnContent: func(string) {},
	})
	var quota *AppQuotaRefused
	if !errors.As(err, &quota) || a.calls.Load() != 1 {
		t.Fatalf("got %v after %d calls", err, a.calls.Load())
	}
}

func TestAppCallMovesWhenNothingWasShown(t *testing.T) {
	offTheSharedRateCeiling(t)
	a := &showingApps{silent: true}
	r := &ProviderRegistry{}
	r.SetAppInference(a)
	p := &appProvider{registry: r, appId: "*", wildcard: true, actingUserId: "alice", level: "reasoning"}
	res, err := p.call(context.Background(), AppCallRequest{
		Messages:  []common.ChatMessage{{Role: "user", Content: "Hello"}},
		OnContent: func(string) {},
	})
	if err != nil || res.Content != "done" || a.calls.Load() != 2 {
		t.Fatalf("got %+v, %v after %d calls", res, err, a.calls.Load())
	}
}

// showingApps stops the first app on its limit, having shown part of an answer or not.
type showingApps struct {
	takeoverApps
	silent bool
	calls  atomic.Int32
}

func (a *showingApps) Call(_ context.Context, req AppCallRequest) (AppCallResult, error) {
	if a.calls.Add(1) == 1 {
		if !a.silent && req.OnContent != nil {
			req.OnContent("Partly answered")
		}
		return AppCallResult{}, &AppQuotaRefused{Reason: "You've hit your weekly limit"}
	}
	return AppCallResult{Content: "done"}, nil
}
