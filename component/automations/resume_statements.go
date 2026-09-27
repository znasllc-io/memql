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
//
// A RE-RUN SERVES ITS PREFIX STRICTER (epic memql#5414). The steps before the
// resume point are the head's versions, and none of them runs again: a step
// its condition skipped stays skipped rather than being decided afresh, and a
// read too large to record is read again WITHOUT a row, so the version the
// head names stays the version the run shows. Every step from the resume point
// on -- and any step that does run -- runs one past the highest version it
// recorded (rerunBases).

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
// at automation.Steps[at] (see the file comment). rerun is the request a
// re-run serves, nil for an ordinary resume.
func resumedStatements(j *RunJournal, automation *Automation, at int, rerun *RerunSpec) *resumedList {
	r := &resumedList{done: map[string]*MinimalStepResult{}, continued: map[string]bool{}}
	if rerun != nil {
		r.reread = map[string]bool{}
		r.bases = rerunBases(j, automation, rerun, at)
	}
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
			unrecorded := m == nil || (m.Value == nil && step.Binds != "")
			if unrecorded && isQueryStatement(step) {
				if rerun != nil {
					r.reread[step.ID] = true
				}
				continue // read again
			}
			if m == nil {
				if rerun == nil {
					continue // run again, as a resume always has
				}
				// A re-run never runs a finished prefix step again: one
				// that recorded no result binds nothing.
				m = &MinimalStepResult{StepId: step.ID, Status: "completed"}
			}
			r.done[step.ID] = m
		case state.Status == "failed" && step.OnError == ErrorStrategyContinue:
			r.continued[step.ID] = true
		case state.Status == "skipped" && rerun != nil:
			r.continued[step.ID] = true
		}
	}
	return r
}

// stepRetryable reports whether the resume point may run again without
// AllowSideEffects: IsStepRetryable's rule, and a mutation call, which is a
// function step but a write.
func stepRetryable(step *Step) bool {
	if !IsStepRetryable(step.Type) {
		return false
	}
	return !(step.Type == StepTypeFunction && step.Function != nil &&
		strings.EqualFold(step.Function.Kind, "mutation"))
}
