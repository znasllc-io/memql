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
//
// "" MEANS THERE IS NOTHING TO RESUME, and ResumeFrom refuses it (memql#5664).
// It used to fall back to FailedStep, which for a journal whose only failures
// were continued past named one of them: a run still executing on another
// replica, between two statements, then looked resumable to a second replica
// the moment the first one's dispatch lease lapsed, and the second resumed it
// from the continued failure -- running that statement, and every finished
// one after it, again, beside the replica still running the run. A journal
// with no failure at all was already refused for exactly that reason; a
// failure the body continued past is not a failure of the run, so it is
// refused the same way.
//
// The one fallback left is a TEMPLATE THAT NO LONGER HAS THE FAILED STEP. A
// re-plan (epic memql#5127) replaces the run's template from the failed step
// on, re-emitting the completed steps as its prefix, so the step the run broke
// at may be gone. The run then resumes at the new template's first statement
// it never reached: the prefix is served from its journal and the new steps
// run, each as its first attempt.
func statementResumePoint(j *RunJournal, automation *Automation) string {
	failedInBody := false
	for _, step := range automation.Steps {
		if step == nil {
			continue
		}
		state, recorded := j.StepStates[step.ID]
		if step.ID == j.FailedStep {
			failedInBody = true
			if !recorded {
				// A journal that names its failed step and records nothing
				// else about it -- one built by hand; LoadRunJournal records
				// every row's state -- resumes there, as it always has.
				return step.ID
			}
		}
		switch state.Status {
		case "running":
			return step.ID
		case "failed":
			if step.OnError != ErrorStrategyContinue {
				return step.ID
			}
		}
	}
	if j.FailedStep == "" || failedInBody {
		return ""
	}
	for _, step := range automation.Steps {
		if step == nil {
			continue
		}
		if _, reached := j.StepStates[step.ID]; !reached {
			return step.ID
		}
	}
	return ""
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
			state, recorded := j.StepStates[step.ID]
			r.at, r.attempt = step.ID, state.Attempt
			// A resume point the run never reached -- a re-planned
			// template's first new step -- runs as its first attempt, not
			// as the next attempt of a step that has none.
			r.atReached = recorded || step.ID == j.FailedStep
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
