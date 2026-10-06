package automations

// resume_statements.go -- resuming a statement body from its journal (epic
// memql#5370, task memql#5372).
//
// A statement body resumes by running again from its first statement, over
// names rehydrated from the journal, rather than by jumping into the middle of
// its list: a name is bound by the statement that computed it, and the
// statements before the resume point bound theirs in the run being resumed.
// Each statement before the resume point keeps its recorded disposition:
//
//   - done: it binds the value its receipt recorded (MinimalStepResult.Value)
//     and does not run. A query whose rows were too many to record is read
//     again instead -- it is a read;
//   - failed under `on error continue`: it was continued past, so it stays
//     so, and its name stays absent;
//   - skipped: its condition is not reconsidered;
//   - missing evidence: a missing bound value from a completed effect
//     refuses recovery. An unrecorded query may be read again only under
//     resume's replay admission, without replacing its original receipt.
//
// The statement at the resume point runs on its next attempt, and every one
// after it as it would have. A row under a nested key -- a logic's statement,
// journaled under the statement that called it -- is never a resume point
// (runJournalFromRows drops it): resume re-runs the calling statement, never
// into it.
//
// A RE-RUN (epic memql#5414) serves the prefix's head versions with the same
// preservation rules. Every step from the resume point
// on -- and any step that does run -- runs one past the highest version it
// recorded (rerunBases).

import "fmt"

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
// There is NO fallback for a template that no longer has the failed step. A
// re-plan (epic memql#5127) replaces the run's template from the failed step
// on, and the install says where the run resumes: a `replan` re-run request
// naming the new template's first step, served by PrepareRerun under a claim
// of its own. Without that request such a journal has nothing to resume --
// falling back to the first step the run never reached is what let a second
// replica, after the first one's dispatch lease lapsed, start a re-planned run
// it was still executing.
func statementResumePoint(j *RunJournal, automation *Automation) string {
	for _, step := range automation.Steps {
		if step == nil {
			continue
		}
		state, recorded := j.StepStates[step.ID]
		if step.ID == j.FailedStep && !recorded {
			// A journal that names its failed step and records nothing else
			// about it -- one built by hand; LoadRunJournal records every
			// row's state -- resumes there, as it always has.
			return step.ID
		}
		switch state.Status {
		case "running", "waiting":
			return step.ID
		case "failed":
			if step.OnError != ErrorStrategyContinue {
				return step.ID
			}
		}
	}
	return ""
}

// resumedStatements is what the body knows from its journal when it resumes
// at automation.Steps[at] (see the file comment). rerun is the request a
// re-run serves, nil for an ordinary resume.
func resumedStatements(j *RunJournal, automation *Automation, at int, rerun *RerunSpec) (*resumedList, error) {
	r := &resumedList{done: map[string]*MinimalStepResult{}, continued: map[string]bool{}, reread: map[string]bool{}}
	if rerun != nil {
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
			unrecorded := m == nil || (m.Value == nil && !m.ValueRecorded && step.Binds != "")
			if unrecorded && step.Binds != "" && isQueryStatement(step) {
				r.reread[step.ID] = true
				continue // read again
			}
			if unrecorded && step.Binds != "" {
				return nil, fmt.Errorf("%w: %w: step %q, binding %q", ErrRunJournalInvalid, ErrResumeResultMissing, step.ID, step.Binds)
			}
			if m == nil {
				// Recovery never runs a finished prefix step again: one
				// that recorded no result binds nothing.
				m = &MinimalStepResult{StepId: step.ID, Status: "completed"}
			}
			r.done[step.ID] = m
		case state.Status == "failed" && step.OnError == ErrorStrategyContinue:
			r.continued[step.ID] = true
		case state.Status == "skipped":
			r.continued[step.ID] = true
		}
	}
	return r, nil
}
