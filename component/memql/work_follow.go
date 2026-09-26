package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
)

// followWorkRun observes durable state only. The executing agent/planner may
// be on another replica; disconnecting this viewer never replays the work.
func (e *MemQLEngine) followWorkRun(ctx context.Context, runID string, onText func(string), onEvent func(WorkEvent) error) (string, error) {
	return followWorkRun(ctx, runID, e.workRows, onText, onEvent, 300*time.Millisecond)
}

func (e *MemQLEngine) workRows(ctx context.Context, name, runID string) ([]map[string]any, error) {
	call, err := parser.RenderCall(name, map[string]any{"runId": runID})
	if err != nil {
		return nil, err
	}
	result, err := e.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	return MaterializeRows(result.OutputPayload()), nil
}

type workRowReader func(context.Context, string, string) ([]map[string]any, error)

func followWorkRun(ctx context.Context, runID string, read workRowReader, onText func(string), onEvent func(WorkEvent) error, interval time.Duration) (string, error) {
	seen := map[string]string{}
	texts := map[string]string{}
	var answer strings.Builder
	for {
		// Read status first and evidence second: once terminal, the last progress
		// write necessarily preceded this snapshot. The opposite order loses tails.
		runs, err := read(ctx, "workRunForOwner", runID)
		if err != nil {
			return answer.String(), err
		}
		if len(runs) != 1 {
			return answer.String(), fmt.Errorf("work run is unavailable")
		}
		run := runs[0]
		observations, err := read(ctx, "workObservationsForOwnerRun", runID)
		if err != nil {
			return answer.String(), err
		}
		type item struct {
			key   string
			event WorkEvent
		}
		progress := []item{}
		for _, row := range observations {
			data, _ := row["data"].(map[string]any)
			raw, _ := json.Marshal(data["execution"])
			var event WorkEvent
			if json.Unmarshal(raw, &event) != nil || event.ID == "" {
				continue
			}
			progress = append(progress, item{fmt.Sprint(row["id"]), event})
		}
		sort.SliceStable(progress, func(i, j int) bool { return progress[i].event.At.Before(progress[j].event.At) })
		for _, p := range progress {
			event := p.event
			if event.Kind == "response" {
				prior := texts[p.key]
				if strings.HasPrefix(event.Text, prior) && len(event.Text) > len(prior) {
					delta := event.Text[len(prior):]
					texts[p.key] = event.Text
					answer.WriteString(delta)
					if onText != nil {
						onText(delta)
					}
				}
				continue
			}
			raw, _ := json.Marshal(event)
			if seen[p.key] == string(raw) {
				continue
			}
			seen[p.key] = string(raw)
			if onEvent != nil {
				if err := onEvent(event); err != nil {
					return answer.String(), err
				}
			}
		}
		switch run["status"] {
		case "succeeded":
			steps, err := read(ctx, "workStepsForOwnerRun", runID)
			if err != nil {
				return answer.String(), err
			}
			for _, file := range workResultFiles(steps) {
				if onEvent != nil {
					if err := onEvent(WorkEvent{ID: "file-" + fmt.Sprint(file["fileId"]), Kind: "artifact", Phase: "completed", At: time.Now().UTC(), Name: fmt.Sprint(file["name"]), App: "files", Arguments: file}); err != nil {
						return answer.String(), err
					}
				}
			}
			if answer.Len() == 0 {
				text := workResultText(run, steps)
				answer.WriteString(text)
				if onText != nil {
					onText(text)
				}
			}
			return answer.String(), nil
		case "waiting":
			waiting, _ := run["waitingOn"].(map[string]any)
			kind, _ := waiting["kind"].(string)
			if kind != "retry" && kind != "replan" && kind != "repair" {
				return answer.String(), fmt.Errorf("work is waiting for input; inspect this run in Nexus")
			}
		case "failed", "abandoned", "cancelled":
			message, _ := run["errorMessage"].(string)
			if message == "" {
				message = "work " + fmt.Sprint(run["status"])
			}
			return answer.String(), fmt.Errorf("%s", message)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return answer.String(), ctx.Err()
		case <-timer.C:
		}
	}
}

func workResultText(run map[string]any, steps []map[string]any) string {
	if files := workResultFiles(steps); len(files) > 0 {
		names := make([]string, 0, len(files))
		for _, file := range files {
			names = append(names, fmt.Sprint(file["name"]))
		}
		return "Created in Files: " + strings.Join(names, ", ") + "."
	}
	if outcome, ok := run["outcome"].(map[string]any); ok {
		if result, ok := outcome["returned"]; ok {
			raw, _ := json.Marshal(result)
			return string(raw)
		}
	}
	// Typed deterministic capabilities can return their own concise reply.
	// A model call merely to paraphrase a navigation receipt adds latency.
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i]["status"] != "done" {
			continue
		}
		result, _ := steps[i]["result"].(map[string]any)
		for _, row := range MaterializeRows(result["value"]) {
			if reply, ok := row["reply"].(string); ok && strings.TrimSpace(reply) != "" {
				return reply
			}
		}
	}
	// Every completed template has receipts, even a deterministic one that
	// never invoked an agent. Return those results without spending another call
	// merely to rephrase them or pretending an absent result is an answer.
	var results []any
	for _, step := range steps {
		if step["status"] == "done" {
			if result := step["result"]; result != nil {
				results = append(results, result)
			}
		}
	}
	if len(results) == 0 {
		return "The work completed. Its execution record is available in Nexus."
	}
	raw, _ := json.Marshal(results)
	return string(raw)
}

func workResultFiles(steps []map[string]any) []map[string]any {
	var files []map[string]any
	seen := map[string]bool{}
	for _, step := range steps {
		if step["status"] != "done" {
			continue
		}
		result, _ := step["result"].(map[string]any)
		for _, row := range MaterializeRows(result["value"]) {
			fileID, _ := row["outputFileId"].(string)
			name, _ := row["name"].(string)
			hash, _ := row["sha256"].(string)
			if fileID == "" || name == "" || hash == "" || seen[fileID] {
				continue
			}
			seen[fileID] = true
			files = append(files, map[string]any{"fileId": BareShortId(fileID), "name": name, "sha256": hash})
		}
	}
	return files
}
