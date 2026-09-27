package proving

// fixture_app.go -- the proving world's APP (epic memql#5408, the program
// record's section 7): a fake harness standing where Claude Code or Codex
// stands in production.
//
// WHAT IT IS. It serves a goal by performing the scenario's actions on the
// fake machine, and it reports every action as the cockpit does -- one
// normalized component/worker.ActionEvent per action, the session's
// fingerprint on the first -- through the SAME writer a delegated session is
// recorded through in production (integrations/work.SessionWriter, behind
// component/worker.SessionRecorder). So a recording the learner reads here is
// a recording in production's own rows, not a proving-suite shape of one.
//
// WHAT IT IS NOT. It is not a provider, and it plays no cassette. An app
// session is the intelligence in the loop -- the thing a learned procedure
// exists to stop paying for -- so every session the fixture app runs counts as
// ONE MODEL REACH, whatever it then does. That count is the lifecycle's model
// instrument: a goal served with no session reached no model.
//
// IT HONOURS GUIDANCE. Handed a goal back by a diverged replay, it skips the
// actions the guidance says already ran. That is the behaviour the platform's
// guidance asks of a real app, and it is what makes the durability figure a
// measurement of the GUIDANCE: if the replay named the wrong steps, or none,
// the app redoes a delivered one and the world records the duplicate.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

const (
	// fixtureAppId is the app the recordings are made by, and what a learned
	// procedure's provenance names (recordedFrom.app).
	fixtureAppId      = "fixture"
	fixtureAppVersion = "1"
	// fixtureModel is what the app REPORTS serving with (design D9), carried
	// onto the recording run's summary like any app's.
	fixtureModel = "fixture"
)

// FixtureApp is the proving world's app. One per arm of one scenario.
type FixtureApp struct {
	world *ProcedureWorld
	// recorder is nil on the baseline arm: a bare loop records nothing.
	recorder workerservice.SessionRecorder
	steps    []scenario.Step
	// owner scopes the session ids, which the session writer derives its goal
	// and run ids from: an id reused by a later run on the same database
	// would be a second version of somebody else's row.
	owner string
	now   func() time.Time

	mu        sync.Mutex
	calls     int
	goals     map[string]appGoal
	handovers []handoverSeen
}

// appGoal is a goal as the app is given it.
type appGoal struct {
	OwnerUserId string
	// GoalRunId is the goal's own run. The app's recording opens beneath it,
	// which is what carries the goal signature and the goal's input onto the
	// recording (gap G2).
	GoalRunId string
	Statement string
	Variables map[string]string
}

// appSession is what one session did.
type appSession struct {
	SessionId string
	// RunId is the recording run, "" when nothing records (the baseline).
	RunId string
	// Performed are the indexes of the actions the app performed, in order.
	Performed []int
	// Failed reports that an action failed and the session stopped there.
	Failed   bool
	FailedAt int
}

// handoverSeen is one goal handed back to the app, and what the app then did.
type handoverSeen struct {
	Order   HandoverOrder
	Session appSession
}

func newFixtureApp(pw *ProcedureWorld, steps []scenario.Step, recorder workerservice.SessionRecorder, owner string) *FixtureApp {
	return &FixtureApp{
		world: pw, recorder: recorder, steps: steps, owner: owner,
		now:   func() time.Time { return time.Now().UTC() },
		goals: map[string]appGoal{},
	}
}

// expect tells the app about a goal it may be handed back. The app keys it by
// the goal's run, which is what a fallback names.
func (a *FixtureApp) expect(g appGoal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.goals[g.GoalRunId] = g
}

// Calls is how many sessions the app ran -- each one a model reach.
func (a *FixtureApp) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// handoversSince returns the handovers after the first n, for the one goal
// the driver is measuring.
func (a *FixtureApp) handoversSince(n int) []handoverSeen {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n >= len(a.handovers) {
		return nil
	}
	return append([]handoverSeen(nil), a.handovers[n:]...)
}

func (a *FixtureApp) handoverCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.handovers)
}

// Serve performs a goal from its first action: an ordinary session.
func (a *FixtureApp) Serve(ctx context.Context, g appGoal) (appSession, error) {
	return a.session(ctx, g, nil, scenario.Render(g.Statement, g.Variables))
}

// Handover is the app's side of a fallback (AppHandover): a goal a replay
// could not finish, handed back with what the replay had already done.
//
// The guidance names completed steps by the PROCEDURE's step index. The
// lifecycle's procedure is learned from exactly these actions in this order,
// so that is also the action's index here -- and the tool is checked as well,
// so a guidance that named some other step is not honoured by accident. A
// mismatch makes the app redo the action, which the world then records as a
// duplicate: the loud direction, not the quiet one.
func (a *FixtureApp) Handover(ctx context.Context, h HandoverOrder) (HandoverResult, error) {
	a.mu.Lock()
	g, ok := a.goals[h.GoalRunId]
	a.mu.Unlock()
	if !ok {
		return HandoverResult{}, fmt.Errorf("proving: the fixture app was handed back goal run %q, which it was never given", h.GoalRunId)
	}
	skip := map[int]bool{}
	for _, c := range h.Completed {
		if c.Index >= 0 && c.Index < len(a.steps) && a.steps[c.Index].Type == c.Tool {
			skip[c.Index] = true
		}
	}
	prompt := h.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = scenario.Render(g.Statement, g.Variables)
	}
	sess, err := a.session(ctx, g, skip, prompt)
	a.mu.Lock()
	a.handovers = append(a.handovers, handoverSeen{Order: h, Session: sess})
	a.mu.Unlock()
	if err != nil {
		return HandoverResult{}, err
	}
	return HandoverResult{ChildRunId: sess.RunId, SessionId: sess.SessionId}, nil
}

// session runs one app session: every action not skipped, in order, stopping
// at the first that fails -- the app reports a failure rather than guessing
// past it. A recording error is RETURNED, and the lifecycle treats it as the
// harness failing: in production a lost row costs a recording, but here the
// recording is the thing under test.
func (a *FixtureApp) session(ctx context.Context, g appGoal, skip map[int]bool, prompt string) (appSession, error) {
	a.mu.Lock()
	a.calls++
	n := a.calls
	a.mu.Unlock()

	sess := appSession{SessionId: fmt.Sprintf("fixture-%s-%d", a.owner, n), FailedAt: -1}
	workspace := "/workspace/" + sess.SessionId

	if a.recorder != nil {
		runId, err := a.recorder.OpenRecording(ctx, workerservice.RecordingOpen{
			SessionId:   sess.SessionId,
			OwnerUserId: g.OwnerUserId,
			App:         fixtureAppId,
			Prompt:      prompt,
			Workspace:   workspace,
			ParentRunId: g.GoalRunId,
		})
		if err != nil {
			return sess, fmt.Errorf("proving: opening the fixture app's recording: %w", err)
		}
		sess.RunId = runId
	}

	seq := 0
	for i, st := range a.steps {
		if skip[i] {
			continue
		}
		command := scenario.Render(st.Target, g.Variables)
		started := a.now()
		ans := a.world.exec(command, fmt.Sprintf("%s:%s:1", sess.SessionId, st.Key), false)
		seq++
		sess.Performed = append(sess.Performed, i)
		if a.recorder != nil {
			exit := ans.ExitCode
			rec := workerservice.RecordedAction{
				SessionId:   sess.SessionId,
				OwnerUserId: g.OwnerUserId,
				RunId:       sess.RunId,
				Seq:         seq,
				Action: workerservice.ActionEvent{
					Id:           st.Key,
					Seq:          uint64(seq),
					Tool:         st.Type,
					Args:         map[string]any{"command": command},
					Cwd:          workspace,
					ExitCode:     &exit,
					IsError:      ans.IsError,
					ResultDigest: textDigest(ans.Stdout),
					ResultType:   work.InferTextType(ans.Stdout),
					Error:        ans.Error,
				},
				StartedAt:  started,
				FinishedAt: a.now(),
			}
			if seq == 1 {
				rec.Fingerprint = a.world.fingerprint(workspace, started)
			}
			if err := a.recorder.RecordAction(ctx, rec); err != nil {
				return sess, fmt.Errorf("proving: recording the fixture app's action %s: %w", st.Key, err)
			}
		}
		if ans.IsError {
			sess.Failed, sess.FailedAt = true, i
			break
		}
	}

	if a.recorder != nil {
		status, exitCode := workerservice.AppSessionStatusEnded, 0
		if sess.Failed {
			status, exitCode = workerservice.AppSessionStatusFailed, 1
		}
		answer, _ := json.Marshal(map[string]any{"completed": !sess.Failed, "actions": len(sess.Performed)})
		closing := workerservice.RecordingClose{
			SessionId:       sess.SessionId,
			OwnerUserId:     g.OwnerUserId,
			RunId:           sess.RunId,
			Seq:             seq + 1,
			Status:          status,
			Answer:          answer,
			ExitCode:        exitCode,
			RecordedActions: seq,
			Model:           fixtureModel,
			FinishedAt:      a.now(),
		}
		if sess.Failed {
			closing.ErrorMessage = "an action failed: " + a.steps[sess.FailedAt].Key
		}
		if err := a.recorder.CloseRecording(ctx, closing); err != nil {
			return sess, fmt.Errorf("proving: closing the fixture app's recording: %w", err)
		}
	}
	return sess, nil
}

// textDigest is the result digest the cockpit reports for a text result: a
// sha256 over it. It is recorded and never compared across executors -- the
// comparison reads only the exit code, the error flag and the result's type.
func textDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
