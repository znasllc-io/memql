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

type workFollowModeKey struct{}

const followBackground = "background"
const followSnapshot = "snapshot"

type workPending struct {
	Waiting                          bool
	Title, Workload, Acknowledgement string
}

func (p *workPending) Error() string {
	if p.Waiting {
		return "This work needs your input in Nexus."
	}
	return "work continues in the background"
}

func followWorkRun(ctx context.Context, runID string, read workRowReader, onText func(string), onEvent func(WorkEvent) error, interval time.Duration) (string, error) {
	seen := map[string]string{}
	answer := ""
	for {
		// Read status first and evidence second: once terminal, the last progress
		// write necessarily preceded this snapshot. The opposite order loses tails.
		runs, err := read(ctx, "workRunForOwner", runID)
		if err != nil {
			return answer, err
		}
		if len(runs) != 1 {
			return answer, fmt.Errorf("work run is unavailable")
		}
		run := runs[0]
		observations, err := read(ctx, "workObservationsForOwnerRun", runID)
		if err != nil {
			return answer, err
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
		completedResponse := ""
		for _, p := range progress {
			event := p.event
			if event.Kind == "response" {
				// A draft can be rejected, interrupted by a question, or replaced
				// by a structured response. It is not an append-only answer.
				if event.Phase == "completed" {
					completedResponse = event.Text
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
					return answer, err
				}
			}
		}
		switch run["status"] {
		case "succeeded":
			steps, err := read(ctx, "workStepsForOwnerRun", runID)
			if err != nil {
				return answer, err
			}
			for _, file := range workResultFiles(steps) {
				if onEvent != nil {
					if err := onEvent(WorkEvent{ID: "file-" + fmt.Sprint(file["fileId"]), Kind: "artifact", Phase: "completed", At: time.Now().UTC(), Name: fmt.Sprint(file["name"]), App: "files", Arguments: file}); err != nil {
						return answer, err
					}
				}
			}
			// Delivery starts only after the run commits success. The journal
			// identifies the successful attempt even when another replica's
			// clock makes an older response event appear newer.
			answer = workResultText(run, steps, completedResponse)
			if onText != nil {
				onText(answer)
			}
			return answer, nil
		case "waiting":
			waiting, _ := run["waitingOn"].(map[string]any)
			kind, _ := waiting["kind"].(string)
			if kind != "retry" && kind != "replan" && kind != "repair" {
				message := "Work is waiting for input; inspect this run in Nexus."
				if onEvent != nil {
					if err := onEvent(WorkEvent{ID: "work:" + runID, Kind: "run", Phase: "waiting", Name: "Request", Error: message}); err != nil {
						return answer, err
					}
				}
				return answer, &workPending{Waiting: true}
			}
		case "failed", "abandoned", "cancelled":
			message, _ := run["errorMessage"].(string)
			if message == "" {
				message = "work " + fmt.Sprint(run["status"])
			}
			if onEvent != nil {
				phase := "failed"
				if run["status"] == "cancelled" {
					phase = "cancelled"
				}
				if err := onEvent(WorkEvent{ID: "work:" + runID, Kind: "run", Phase: phase, Name: "Request", Error: message}); err != nil {
					return answer, err
				}
			}
			return answer, fmt.Errorf("%s", message)
		}
		mode, _ := ctx.Value(workFollowModeKey{}).(string)
		outcome, _ := run["classification"].(map[string]any)
		workload, _ := outcome["workload"].(string)
		title, _ := outcome["workTitle"].(string)
		ack, _ := outcome["acknowledgement"].(string)
		started, hasStart := askTimestamp(run["startedAt"])
		queuedClassification := run["status"] == "compiling" && hasStart && time.Since(started) >= 15*time.Second
		if mode == followSnapshot || (mode == followBackground && ((workload != "" && workload != "quick") || queuedClassification)) {
			return answer, &workPending{Title: title, Workload: workload, Acknowledgement: ack}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return answer, ctx.Err()
		case <-timer.C:
		}
	}
}

func workResultText(run map[string]any, steps []map[string]any, completedResponse string) string {
	// The query is intentionally unordered. Use the journal's sequence,
	// not result arrival order or wall clocks, to select the final reply.
	ordered := append([]map[string]any(nil), steps...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return intFromAny(ordered[i]["seq"]) < intFromAny(ordered[j]["seq"])
	})
	for i := len(ordered) - 1; i >= 0; i-- {
		if ordered[i]["status"] != "done" {
			continue
		}
		result, _ := ordered[i]["result"].(map[string]any)
		for _, row := range MaterializeRows(result["value"]) {
			if reply, ok := row["reply"].(string); ok && strings.TrimSpace(reply) != "" {
				return reply
			}
		}
	}
	if completedResponse != "" {
		return completedResponse
	}
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
			if sourceID, _ := row["sourceFileId"].(string); sourceID != "" && !seen[sourceID] {
				seen[sourceID] = true
				files = append(files, map[string]any{"fileId": BareShortId(sourceID), "name": "Source ZIP for " + name})
			}
		}
	}
	return files
}
