package pipelines

import (
	"strings"
)

// step_check.go -- one GitHub check run per pipeline step, beside the run's
// own check (owner-approved 2026-10-28): a pull request shows every step that
// passed, failed or is running, not only the run's verdict.
//
// THE RUN'S CHECK STAYS THE ONLY ONE ANYTHING REQUIRES. A step check is named
// after its run's check and adds its stage and step, so it always has more
// segments than any context a ruleset names (CheckRunName and its event
// suffixes, IndependentReviewCheckName): a step check cannot satisfy a
// required context, a train's boarding check or a release source, all of
// which ask for an exact name.

// StepCheckName is a step's check run name: its run's check name, then the
// step's stage and name -- MemQL / memql / tests / go-unit #2. GitHub lists a
// commit's check runs by name, so a run's steps sit together, stage by stage.
// Push, release and manual runs keep their run's suffix (MemQL / memql / push
// / checks / build): two runs of different events on one commit select
// different stages and must not overwrite each other's steps.
func StepCheckName(runCheck string, s StepReport) string {
	label := strings.TrimSpace(s.Name)
	if i := strings.IndexByte(s.Key, '#'); i >= 0 && label != "" {
		label += " " + s.Key[i:]
	}
	parts := []string{runCheck}
	if stage := strings.TrimSpace(s.Stage); stage != "" {
		parts = append(parts, stage)
	}
	if label == "" {
		label = strings.TrimSpace(s.Key)
	}
	parts = append(parts, label)
	return strings.Join(parts, " / ")
}

// StepCheckState maps a step's status to its check run's status and, once
// the step has ended, conclusion. A skipped step concludes "skipped", which
// GitHub shows as such; nothing requires a step check, so a skip never
// stands in for a pass.
func StepCheckState(status string) (checkStatus, conclusion string) {
	switch status {
	case "running":
		return "in_progress", ""
	case "succeeded":
		return "completed", "success"
	case "failed", "refused":
		return "completed", "failure"
	case "cancelled":
		return "completed", "cancelled"
	case "skipped":
		return "completed", "skipped"
	}
	return "queued", ""
}

// StepCheckOutput is what a step's check run shows: a title saying how it
// ended, a summary naming the step with its message and code, and, for a
// failed step, the last lines of its output (already masked by the caller,
// as for the run's check).
func StepCheckOutput(s StepReport) (title, summary, text string) {
	key := codeSpan(s.Key)
	message := clipRunes(inlineText(s.Message), 400)
	code := ""
	if s.Code != "" {
		code = codeSpan(s.Code)
	}
	detail := func(lead string) string {
		switch {
		case message != "":
			return lead + " " + withAside(message, code)
		case code != "":
			return lead + " (" + code + ")."
		}
		return lead + "."
	}
	switch s.Status {
	case "succeeded":
		if s.Code == CodePassedEarlier && message != "" {
			// Verified at this commit in another run: there is no duration
			// of this run's to show, only where it passed.
			return "Passed: " + oneLine(s.Message, 200), "Step " + key + " passed: " + message, ""
		}
		took := formatDuration(s.DurationMs)
		return "Passed in " + took, "Step " + key + " passed in " + took + ".", ""
	case "failed", "refused":
		title = "Failed"
		if m := oneLine(s.Message, 200); m != "" {
			title += ": " + m
		}
		if strings.TrimSpace(s.LogTail) != "" {
			tail := strings.ToValidUTF8(s.LogTail, "\uFFFD")
			if !strings.HasSuffix(tail, "\n") {
				tail += "\n"
			}
			if section, ok := fitOutput(s.Key, tail, checkOutputLimit-160); ok {
				text = section
			}
		}
		return title, detail("Step " + key + " failed:"), text
	case "skipped":
		title = "Skipped"
		if m := oneLine(s.Message, 200); m != "" {
			title += ": " + m
		}
		return title, detail("Step " + key + " was skipped:"), ""
	case "cancelled":
		return "Cancelled", detail("Step " + key + " was cancelled:"), ""
	case "running":
		return "Running", "Step " + key + " is running.", ""
	}
	return "Queued", "Step " + key + " is waiting for its turn.", ""
}

// StepCheckExternalID ties a step's check run to its run and step, so a
// driver on another replica finds the check runs a previous one created
// instead of creating them again.
func StepCheckExternalID(runID, key string) string {
	return runID + ":" + key
}

// StepFromExternalID reads a step check's external id back into its run id
// and step key; a run's own check, whose external id is the run id alone,
// answers false.
func StepFromExternalID(id string) (runID, key string, ok bool) {
	runID, key, ok = strings.Cut(id, ":")
	if !ok || runID == "" || key == "" {
		return "", "", false
	}
	return runID, key, true
}
