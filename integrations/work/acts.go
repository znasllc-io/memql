package work

// acts.go -- what the person-facing acts of epic memql#5414 share: re-running
// a step, branching a run, moving its head and judging a version (design
// record docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D18 to D23).
//
// EVERY DECISION IS component/work's. Which versions a re-run executes, which
// ones a head move restores, where a branch's prefix points and what a
// snapshot holds are pure functions over values there; this file reads the
// rows those functions need, in the shape they need them, and names each
// refusal in words a surface can print.
//
// THE ACT IS RECORDED ON THE RUN ROW, AND THAT IS THE CROSS-NODE PLUMBING. A
// person's click arrives at a bff replica, and the run executes on an agent
// replica that holds none of the first one's memory. So nothing here keeps
// state between calls and nothing is dispatched from here: the act writes
// run.rerun (and, for a branch, a new run carrying it), the run row's own
// `running` event reaches the agents, and exactly one of them claims it and
// serves the request off the row.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
)

// Refusal codes. Stable, so a surface can print its own copy for each; the
// override_*, feedback_* and snapshot_content_omitted codes are component/work's
// own and arrive inside its errors.
const (
	codeRunNotFound            = "run_not_found"
	codeRunNotFinished         = "run_not_finished"
	codeRunNotExecutable       = "run_not_executable"
	codeStepNotInRun           = "step_not_in_run"
	codeStepNested             = "step_nested"
	codeVersionNotFound        = "version_not_found"
	codeVersionNotDone         = "version_not_done"
	codeFeedbackTargetNotFound = "feedback_target_not_found"
)

// Rerun reasons, run.rerun.reason's closed set.
const (
	rerunReasonRerun    = "rerun"
	rerunReasonHeadMove = "headMove"
	rerunReasonBranch   = "branch"
)

// ActRefusal is an act refused for a reason a surface can name.
type ActRefusal struct {
	Code    string
	Message string
}

func (e *ActRefusal) Error() string { return "work: " + e.Code + ": " + e.Message }

func refuse(code, format string, a ...any) error {
	return &ActRefusal{Code: code, Message: fmt.Sprintf(format, a...)}
}

// headRefusal names a refusal from one of component/work's head decisions. Its
// errors already say which step and which version; this only puts the code in
// front, so every act refuses in the same shape.
func headRefusal(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, work.ErrNestedStep):
		return refuse(codeStepNested, "%s", err.Error())
	case errors.Is(err, work.ErrStepNotInRun):
		return refuse(codeStepNotInRun, "%s", err.Error())
	case errors.Is(err, work.ErrVersionNotFound):
		return refuse(codeVersionNotFound, "%s", err.Error())
	case errors.Is(err, work.ErrVersionNotDone):
		return refuse(codeVersionNotDone, "%s", err.Error())
	}
	return err
}

// ---------------------------------------------------------------------------
// The run an act is about
// ---------------------------------------------------------------------------

// actRun is one run an act was asked about, read through the CALLER's own
// actor: workRunForOwner filters on ownerUserId == actor.userId, so a run the
// caller does not own is simply not there, and that absence is the whole of
// the ownership check -- a cluster owner acting on somebody else's run reads
// nothing too, which is the owner-only rule the acts are specified with.
type actRun struct {
	row map[string]any
	// id is the run's row id, which every write names.
	id string
	// owner is copied off the row: the authority every write borrows (store.go
	// RULE 2), never a value taken from a request.
	owner string
	// order is the run's TOP-LEVEL step keys in execution order. The executor
	// records nested statements in stepOrder too (a logic's statements, a loop
	// body), and the head names top-level steps only -- a nested step belongs
	// to the version of the step that holds it -- so every head decision here
	// runs over the top-level order. The journal's basis is computed from the
	// head, which names nothing else, so the two agree.
	order []string
}

func (i *Integration) readActRun(ctx context.Context, runId string) (actRun, error) {
	row, err := i.store().runForOwner(ctx, runId)
	if err != nil {
		return actRun{}, err
	}
	if row == nil {
		return actRun{}, refuse(codeRunNotFound, "no run %q is readable by this caller", runId)
	}
	id := rowString(row, "id")
	if id == "" {
		id = runId
	}
	return actRun{
		row:   row,
		id:    id,
		owner: rowString(row, "ownerUserId"),
		order: topLevelOrder(rowStringSlice(row, "stepOrder")),
	}, nil
}

// requireFinished refuses an act on a run that has not stopped. Re-running a
// step under a live execution would race it: two executions writing versions
// of the same step rows, each believing it holds the head.
func (r actRun) requireFinished() error {
	status := rowString(r.row, "status")
	if isTerminalRunStatus(status) {
		return nil
	}
	if status == "" {
		status = "in an unknown state"
	}
	return refuse(codeRunNotFinished, "run %s is %s; a run is changed only after it has stopped, never under a live execution", r.id, status)
}

// requireExecutable refuses a run the executor cannot run again. An app
// session's RECORDING is the record of a session, not an automation:
// dispatched, it would fail on a template nobody registered, and the failed
// status would take the recording out of the procedure corpus it belongs to.
// A run that never chose a template has nothing to run, and a run with no goal
// is one the dispatcher leaves to the scheduler that owns it.
func (r actRun) requireExecutable() error {
	switch name := rowString(r.row, "automationName"); {
	case name == appSessionTemplate:
		where := ""
		if parent := rowString(r.row, "parentRunId"); parent != "" {
			where = " in run " + parent
		}
		return refuse(codeRunNotExecutable, "run %s is an app session's recording; re-run or branch the step that delegated it%s", r.id, where)
	case name == "" || name == compilingAutomationName:
		return refuse(codeRunNotExecutable, "run %s never chose a template, so there is nothing to run again", r.id)
	case rowString(r.row, "goalId") == "":
		return refuse(codeRunNotExecutable, "run %s serves no goal; only a goal's run is executed again on a person's word", r.id)
	}
	return nil
}

// requireTopLevel refuses a step key that is not one of the run's own
// top-level steps, through the pure decision's own rule rather than a second
// copy of it.
func (r actRun) requireTopLevel(key string) error {
	_, err := work.PlanRerun(r.order, nil, key)
	return headRefusal(err)
}

// topLevelOrder keeps the keys of a stepOrder that name top-level steps, in
// order, each once.
func topLevelOrder(order []string) []string {
	out := make([]string, 0, len(order))
	seen := map[string]bool{}
	for _, k := range order {
		k = strings.TrimSpace(k)
		if k == "" || strings.Contains(k, "/") || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Versions and the head
// ---------------------------------------------------------------------------

// runVersions is every recorded version of one run's steps, folded: the row
// each (key, version) ended at, and the head decisions' view of the top-level
// ones.
type runVersions struct {
	rows  map[string]map[int]map[string]any
	byKey map[string][]work.StepVersion
}

// readVersions reads a run's versions (StepVersions) and shapes them for
// component/work. A version's basis is reconstructed by work.VersionOf when
// the journal omitted it, which is the pristine case.
func (i *Integration) readVersions(ctx context.Context, runId string, order []string) (runVersions, error) {
	rows, err := i.StepVersions(ctx, runId)
	if err != nil {
		return runVersions{}, err
	}
	return shapeVersions(rows, order), nil
}

func shapeVersions(rows []map[string]any, order []string) runVersions {
	v := runVersions{rows: map[string]map[int]map[string]any{}, byKey: map[string][]work.StepVersion{}}
	inOrder := map[string]bool{}
	for _, k := range order {
		inOrder[k] = true
	}
	for _, row := range rows {
		key := rowString(row, "key")
		if key == "" {
			continue
		}
		version := rowVersion(row)
		if v.rows[key] == nil {
			v.rows[key] = map[int]map[string]any{}
		}
		v.rows[key][version] = row
		if inOrder[key] {
			v.byKey[key] = append(v.byKey[key], work.VersionOf(order, key, version, row["basis"], rowString(row, "status")))
		}
	}
	for k := range v.byKey {
		sort.Slice(v.byKey[k], func(a, b int) bool { return v.byKey[k][a].Version < v.byKey[k][b].Version })
	}
	return v
}

// row is the folded row of one version, or nil.
func (v runVersions) row(key string, version int) map[string]any {
	if v.rows == nil {
		return nil
	}
	return v.rows[key][version]
}

// newest is the highest version recorded for a key, 0 when there is none.
func (v runVersions) newest(key string) int {
	max := 0
	for n := range v.rows[key] {
		if n > max {
			max = n
		}
	}
	return max
}

// headOf is the run's head: the stored one, or -- for a run written before
// the head existed -- each top-level step's newest version, which is what
// every collapsed read already shows as current.
func headOf(r actRun, v runVersions) work.Head {
	if h := work.ParseHead(r.row["head"]); h != nil {
		return h
	}
	return work.NewestHead(r.order, v.byKey)
}

// rowVersion is the version one step row carries: `version`, or `attempt` on
// a row written before versions were a field (the journal writes them equal).
func rowVersion(row map[string]any) int {
	if n := rowInt(row, "version"); n > 0 {
		return n
	}
	if n := rowInt(row, "attempt"); n > 0 {
		return n
	}
	return 1
}

// ---------------------------------------------------------------------------
// Overrides and guidance
// ---------------------------------------------------------------------------

// overrideFromArgs reads what a person asked to change about the version a
// re-run or a branch opens, and refuses what the engine could not honour
// before anything is read or written.
func overrideFromArgs(args map[string]any, requestedBy string) (work.Override, error) {
	o := work.Override{
		Level:       argString(args, "level"),
		Model:       argString(args, "model"),
		Effort:      argString(args, "effort"),
		Prompt:      argString(args, "prompt"),
		Inputs:      argMap(args, "inputs"),
		RequestedBy: requestedBy,
	}
	if err := work.ValidateOverride(o); err != nil {
		return o, fmt.Errorf("work: %w", err)
	}
	return o, nil
}

// withGuidance attaches the dislike the replaced version carries (design D23,
// repair): the next attempt is told what was wrong instead of resampling. It
// is left off when the person's own prompt already says it -- they rewrote the
// instruction with the problem in mind, and repeating their words back to the
// model as a second message would only weigh them twice.
func withGuidance(o work.Override, g *work.Guidance) work.Override {
	if g == nil {
		return o
	}
	if reason := strings.TrimSpace(g.Reason); reason != "" &&
		strings.Contains(strings.ToLower(o.Prompt), strings.ToLower(reason)) {
		return o
	}
	o.Guidance = g
	return o
}

// dislikeGuidance is the guidance one step version carries: the NEWEST verdict
// a person gave on it, when that verdict is a dislike. A later like or
// neutral on the same version is their current judgment, and a re-run steered
// by a dislike they have since withdrawn would be steered by a view they no
// longer hold -- the same "newest verdict wins" reading D15's gate gives.
func (i *Integration) dislikeGuidance(ctx context.Context, owner, runId, key string, version int) (*work.Guidance, error) {
	if key == "" || version <= 0 {
		return nil, nil
	}
	observations, err := i.store().query(ownerActor(ctx, owner),
		"query "+call("workObservationsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return nil, err
	}
	newest := newestFeedbackOn(observations, key, version)
	if newest == nil {
		return nil, nil
	}
	data := rowMap(newest, "data")
	if work.ParseVerdict(rowString(data, "verdict")) != work.VerdictDislike {
		return nil, nil
	}
	return &work.Guidance{
		Axes:       work.ParseAxes(data["axes"]),
		Reason:     rowString(data, "reason"),
		FeedbackId: rowString(newest, "id"),
	}, nil
}

// newestFeedbackOn is the newest feedback observation naming one step version.
func newestFeedbackOn(observations []map[string]any, key string, version int) map[string]any {
	var newest map[string]any
	var newestAt time.Time
	for _, o := range observations {
		if rowString(o, "kind") != "feedback" {
			continue
		}
		target := rowMap(rowMap(o, "data"), "target")
		if rowString(target, "stepKey") != key || rowInt(target, "version") != version {
			continue
		}
		at, _ := rowTime(o, "createdAt")
		if newest == nil || !at.Before(newestAt) {
			newest, newestAt = o, at
		}
	}
	return newest
}

// ---------------------------------------------------------------------------
// The request the agent serves
// ---------------------------------------------------------------------------

// rerunRequest is run.rerun: what the agent that claims the run executes, and
// on whose behalf.
//
// THE REQUEST ID IS FRESH ON EVERY ACT, head moves included: the agent claims
// each request once, under the run and this id, so a reused id is a request
// never served (its claim is already spent) and a missing one is a request no
// claim can name.
//
// THE VERSIONS ARE THE PLAN'S (component/work.PlanRerun): the version each
// re-executed step runs as, one past the highest recorded. The executor takes
// them from here rather than from the step rows, because after a head move
// re-asserted an earlier version the newest row no longer carries the highest
// number, and a version reused is an idempotency key reused.
func rerunRequest(reason, stepKey string, o work.Override, versions map[string]int, snapshot *work.Snapshot, workspace, requestedBy string, at time.Time) map[string]any {
	out := map[string]any{
		"requestId":   id.NewShortId(),
		"reason":      reason,
		"stepKey":     stepKey,
		"override":    o.Object(),
		"versions":    versions,
		"requestedBy": requestedBy,
		"requestedAt": rfc(at),
	}
	if snapshot != nil {
		out["snapshot"] = snapshot.Object()
	}
	if workspace != "" {
		out["workspace"] = workspace
	}
	return out
}

// reopenFields are what flipping a finished run back to `running` must also
// say, because the update is a read-merge and every terminal field would
// otherwise survive into the execution the act starts.
//
//   - cancelRequested is the one that would break it outright: the executor
//     polls it at each step boundary, and a re-run of a run somebody once
//     cancelled would cancel itself at the first one.
//   - errorCode, errorMessage and finishedAt describe an end the run is no
//     longer at, and the close of a SUCCESSFUL re-run names none of them --
//     so a failed run re-run to success would read as succeeded with the old
//     failure's code on it, which is a code queries filter on.
//   - heartbeatAt is stamped now, because the abandoned sweep measures silence
//     from it: a run re-run an hour after it finished carries an hour-old
//     heartbeat, and the sweep's next pass would judge it dead before the
//     claiming replica had written its first one.
func reopenFields(now time.Time) map[string]any {
	return map[string]any{
		"status":          runStatusRunning,
		"heartbeatAt":     rfc(now),
		"finishedAt":      "",
		"errorCode":       "",
		"errorMessage":    "",
		"cancelRequested": false,
		"cancelledBy":     "",
		"waitingOn":       map[string]any{},
	}
}

// bareRunId is a run's short id, for the names a workspace directory and a
// triggeredBy carry -- a canonical id's colons are not a directory name every
// machine accepts.
func bareRunId(runId string) string {
	return memqlengine.BareShortId(runId)
}

// replyNodes wraps several answers as distinct nodes. A builtin reply crosses
// the wire as ONE map keyed by node id, so two nodes sharing an id collapse to
// one; each answer carries its own id.
func (i *Integration) replyNodes(entries []replyEntry) []memorynodes.MemoryNode {
	out := make([]memorynodes.MemoryNode, 0, len(entries))
	for _, e := range entries {
		node := i.resultNode(e.payload)[0]
		node.ID = e.id
		if !e.at.IsZero() {
			node.CreatedAt = e.at.UTC()
		}
		out = append(out, node)
	}
	return out
}

type replyEntry struct {
	id      string
	at      time.Time
	payload map[string]any
}
