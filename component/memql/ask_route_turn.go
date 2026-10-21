package memql

// A routed message is a person's turn that reached goals already running in
// its conversation instead of opening a new one. The turn records where it
// went: one target per goal it was delivered to, and one stop question per
// goal the person asked to drop. Everything here is durable transcript state,
// so routing on one replica and execution on another share no memory.

// AskRouting is what a routed turn did. Reply is shown before a split
// request's own answer; Statement is what intake received when that was not
// the message as written (a split message's new request, or a settled
// question's two messages); Resolved marks a clarification a later message
// settled; Skipped says why routing fell back to intake.
type AskRouting struct {
	Targets   []AskRouteTarget  `json:"targets,omitempty"`
	Stops     []AskStopQuestion `json:"stops,omitempty"`
	Reply     string            `json:"reply,omitempty"`
	Statement string            `json:"statement,omitempty"`
	Clarify   bool              `json:"clarify,omitempty"`
	Resolved  bool              `json:"resolved,omitempty"`
	Skipped   string            `json:"skipped,omitempty"`
}

// AskRouteTarget is one goal a message was delivered to. With no ApprovalID
// it is WORK GUIDANCE, read by the run's agent loop for the rest of the run's
// life: retries, repairs, re-plans and a person's re-run of a step keep it.
// With an ApprovalID it went through decideApproval and is never read as
// guidance. Text is exactly what the goal receives.
type AskRouteTarget struct {
	GoalID     string `json:"goalId"`
	RunID      string `json:"runId"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	ApprovalID string `json:"approvalId,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Text       string `json:"text"`
}

// AskStopQuestion asks the person to confirm stopping one goal. Nothing is
// cancelled until they answer stop (askStopDecision).
type AskStopQuestion struct {
	GoalID   string `json:"goalId"`
	RunID    string `json:"runId"`
	Title    string `json:"title"`
	Decision string `json:"decision,omitempty"`
}

// What became of a routed message at one goal.
const (
	routeQueued       = "queued"
	routeReceived     = "received"
	routeAnswered     = "answered"
	routeNotApproved  = "notApproved"
	routeSaved        = "saved"
	routeNotDelivered = "notDelivered"
	routeNotReceived  = "notReceived"
)

// routeDelivered reports whether a message reached its goal: queued for or
// read by its agent as work guidance, or taken by the request it waits on.
func routeDelivered(status string) bool {
	switch status {
	case routeQueued, routeReceived, routeAnswered, routeNotApproved, routeSaved:
		return true
	}
	return false
}

// Why a message did not reach a goal.
const (
	routeReasonEnded   = "ended"
	routeReasonReview  = "review"
	routeReasonLimit   = "limit"
	routeReasonRefused = "refused"
)

// A stop question's answer. Ended is never chosen: the snapshot projects it
// for an unanswered question whose goal is no longer active.
const (
	stopDecisionStopped = "stopped"
	stopDecisionKept    = "kept"
	stopDecisionEnded   = "ended"
)

// askRouteGoal is one active goal of a conversation as routing sees it. The
// Handle (g1, g2, ...) is the only name a model ever sees; ids stay in Go.
type askRouteGoal struct {
	Handle, TurnID, GoalID, RunID, Title, Request string
	Run, Approval                                 map[string]any
	Updates                                       []string
}

// activeWorkRun reports whether run is a goal's root execution that a message
// can still reach: preparing, working or waiting, and not asked to stop.
func activeWorkRun(run map[string]any) bool {
	switch stringArg(run, "status") {
	case "compiling", "running", "waiting":
	default:
		return false
	}
	return run["cancelRequested"] != true && stringArg(run, "goalId") != "" && stringArg(run, "parentRunId") == ""
}

// runTitle is the goal's current title: the one a later message gave the run,
// else the one intake chose.
func runTitle(run map[string]any) string {
	if title := stringArg(run, "title"); title != "" {
		return title
	}
	classification, _ := run["classification"].(map[string]any)
	return stringArg(classification, "workTitle")
}

// workGuidanceTarget is turn's work guidance for run: its target for that run
// with no approval, queued or already received. Routed guidance is about the
// goal, so it holds for the run's whole life, whatever attempt is executing
// (2026-10-19 ruling). A turn reaches a run at most once.
func workGuidanceTarget(turn *AskTurn, run map[string]any) *AskRouteTarget {
	runID := BareShortId(stringArg(run, "id"))
	if turn.Routed == nil || runID == "" {
		return nil
	}
	for n := range turn.Routed.Targets {
		target := &turn.Routed.Targets[n]
		if BareShortId(target.RunID) == runID && target.ApprovalID == "" && (target.Status == routeQueued || target.Status == routeReceived) {
			return target
		}
	}
	return nil
}

// projectAskRouting re-projects a routed turn over its goals' current runs.
// A queued message is Not received only when its run ended (or was asked to
// stop) before reading it; a retry, repair, re-plan or re-run is the same run.
// An unanswered stop question whose goal ended is ended; every label takes the
// goal's current title. It reads only, and what it projects is a reader's
// view that no save path stores (refreshAskTranscript).
func projectAskRouting(routed *AskRouting, readRun func(string) (map[string]any, error)) error {
	if routed == nil {
		return nil
	}
	for n := range routed.Targets {
		target := &routed.Targets[n]
		run, err := readRun(target.RunID)
		if err != nil {
			return err
		}
		if title := runTitle(run); title != "" {
			target.Title = title
		}
		if target.Status == routeQueued && !activeWorkRun(run) {
			target.Status = routeNotReceived
		}
	}
	for n := range routed.Stops {
		stop := &routed.Stops[n]
		run, err := readRun(stop.RunID)
		if err != nil {
			return err
		}
		if title := runTitle(run); title != "" {
			stop.Title = title
		}
		if stop.Decision == "" && !activeWorkRun(run) {
			stop.Decision = stopDecisionEnded
		}
	}
	return nil
}
