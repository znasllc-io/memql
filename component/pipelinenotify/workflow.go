package pipelinenotify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/pipelines"
)

type messageCopy struct {
	Username string   `json:"username"`
	Subject  string   `json:"subject"`
	Headline string   `json:"headline"`
	Title    string   `json:"title"`
	Lines    []string `json:"lines"`
	Color    int      `json:"color"`
}

// Format untrusted values for the medium before combining them with the DSL
// recipe. Escaping, mention suppression and wire limits remain mandatory here.
func notificationCopy(n Notification, esc escaper) (messageCopy, error) {
	value, err := workflowhost.Run(context.Background(), "pipelineNotificationCopy", map[string]any{
		"eventName": string(n.Event), "outcome": string(n.Outcome), "pipeline": esc.line(n.Pipeline), "version": esc(versionText(n)),
		"sha": esc.line(pipelines.ShortCommit(n.SHA)), "branch": esc.line(n.Branch), "prNumber": n.PullRequest, "prLabel": strconv.Itoa(n.PullRequest),
		"commitTitle": esc.line(n.Title), "failedStage": esc(stageOfStep(oneLineText(n.FailedStep))), "failedStep": esc.line(n.FailedStep), "failedCode": esc.line(n.FailedCode),
		"failedMessage": esc(fitRunes(oneLineText(n.FailedMessage), maxFailedMessageRunes)), "stages": n.Stages, "stageCount": strconv.Itoa(n.Stages),
		"durationMs": n.DurationMs, "duration": pipelines.FormatDuration(time.Duration(n.DurationMs) * time.Millisecond),
	}, workflowhost.Options{})
	if err != nil {
		return messageCopy{}, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return messageCopy{}, err
	}
	var copy messageCopy
	if err := json.Unmarshal(encoded, &copy); err != nil {
		return copy, fmt.Errorf("notification workflow: %w", err)
	}
	if copy.Title == "" {
		return copy, fmt.Errorf("notification workflow returned no title")
	}
	return copy, nil
}
