package automations

// resume_statements.go -- resuming a statement body from its journal (epic
// memql#5370, task memql#5372).
//
// A statement body resumes by running again from its first statement, over
// names rehydrated from the journal, rather than by jumping into the middle of
// its list: a name is bound by the statement that computed it, and the
// statements before the resume point bound theirs in the run being resumed.
// Each statement before the resume point is one of three things:
//
//   - done: it binds the value its receipt recorded (MinimalStepResult.Value)
//     and does not run. A query whose rows were too many to record is read
//     again instead -- it is a read;
//   - failed under `on error continue`: it was continued past, so it stays
//     so, and its name stays absent;
//   - anything else (skipped by its condition, never reached): it runs as it
//     would have, a condition being decided again over the rehydrated names.
//
// The statement at the resume point runs on its next attempt, and every one
// after it as it would have. A row under a nested key -- a logic's statement,
// journaled under the statement that called it -- is never a resume point
// (runJournalFromRows drops it): resume re-runs the calling statement, never
// into it.

import "strings"

// statementResumePoint is the first statement of the body's own list that did
// not finish and was not continued past: failed, or running with no receipt (a
// crash mid-statement). The journal's FailedStep is the first such ROW, in the
// order the rows came back, and a continued failure is one; this is the first
// in the body's order that resume can act on.
func statementResumePoint(j *RunJournal, automation *Automation) string {
	for _, step := range automation.Steps {
		if step == nil {
			continue
		}
		switch j.StepStates[step.ID].Status {
		case "running":
			return step.ID
		case "failed":
			if step.OnError != ErrorStrategyContinue {
				return step.ID
			}
		}
	}
	return j.FailedStep
}

// resumedStatements is what the body knows from its journal when it resumes
// at automation.Steps[at] (see the file comment).
func resumedStatements(j *RunJournal, automation *Automation, at int) *resumedList {
	r := &resumedList{done: map[string]*MinimalStepResult{}, continued: map[string]bool{}}
	for i, step := range automation.Steps {
		if step == nil {
			continue
		}
		if i == at {
			r.at, r.attempt = step.ID, j.StepStates[step.ID].Attempt
			break
		}
		switch state := j.StepStates[step.ID]; {
		case state.Status == "done":
			m := j.Steps[step.ID]
			if m == nil || (m.Value == nil && step.Binds != "" && isQueryStatement(step)) {
				continue // read again
			}
			r.done[step.ID] = m
		case state.Status == "failed" && step.OnError == ErrorStrategyContinue:
			r.continued[step.ID] = true
		}
	}
	return r
}

// stepRetryable reports whether the resume point may run again without
// AllowSideEffects: IsStepRetryable's rule, and a statement body's mutation
// call, which is a function step but a write as surely as a mutation step is.
func stepRetryable(automation *Automation, step *Step) bool {
	if !IsStepRetryable(step.Type) {
		return false
	}
	return !(automation.IsStatementBody() && step.Type == StepTypeFunction && step.Function != nil &&
		strings.EqualFold(step.Function.Kind, "mutation"))
}
