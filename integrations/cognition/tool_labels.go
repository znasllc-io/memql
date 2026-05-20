package cognition

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// humanizeToolCall converts a raw tool name and arguments into a user-facing label.
// e.g., clawExecuteTask({"task":"write unit tests"}) → "Writing unit tests"
//
//	clawReadFile({"path":"/src/main.go"})        → "Reading main.go"
func humanizeToolCall(toolName, rawArgs string) string {
	var args map[string]any
	if rawArgs != "" {
		_ = json.Unmarshal([]byte(rawArgs), &args)
	}

	switch toolName {
	case "clawExecuteTask":
		if task, ok := args["task"].(string); ok && task != "" {
			task = strings.TrimSpace(task)
			if len(task) > 60 {
				task = task[:57] + "..."
			}
			return task
		}
		return "Executing task..."

	case "clawReadFile":
		if path, ok := args["path"].(string); ok && path != "" {
			return fmt.Sprintf("Reading %s", filepath.Base(path))
		}
		return "Reading file..."

	case "clawListFiles":
		if path, ok := args["path"].(string); ok && path != "" {
			return fmt.Sprintf("Listing %s", filepath.Base(path))
		}
		return "Listing files..."

	case "clawSearchCode":
		if query, ok := args["query"].(string); ok && query != "" {
			q := strings.TrimSpace(query)
			if len(q) > 40 {
				q = q[:37] + "..."
			}
			return fmt.Sprintf("Searching for %q", q)
		}
		return "Searching code..."

	case "searchUsers":
		return "Looking up users..."

	default:
		// Generic: capitalize the tool name.
		if toolName != "" {
			return fmt.Sprintf("Using %s...", toolName)
		}
		return "Working..."
	}
}

// agentTaskTracker tracks concurrent tasks per agent for presence reporting.
// It is safe for concurrent use.
type agentTaskTracker struct {
	mu    sync.Mutex
	tasks map[string]*agentTask
	seq   int // monotonic task ID counter
}

type agentTask struct {
	ID        string
	ToolName  string
	Label     string
	StartedAt time.Time
	Cancel    func() // heartbeat cancellation
}

// newAgentTaskTracker creates a new task tracker.
func newAgentTaskTracker() *agentTaskTracker {
	return &agentTaskTracker{tasks: make(map[string]*agentTask)}
}

// Start registers a new running task. Returns the task ID.
func (t *agentTaskTracker) Start(toolName, label string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	id := fmt.Sprintf("task-%d", t.seq)
	t.tasks[id] = &agentTask{
		ID:        id,
		ToolName:  toolName,
		Label:     label,
		StartedAt: time.Now(),
	}
	return id
}

// End removes a task by ID. Also cancels any heartbeat goroutine.
func (t *agentTaskTracker) End(taskId string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task, ok := t.tasks[taskId]; ok {
		if task.Cancel != nil {
			task.Cancel()
		}
		delete(t.tasks, taskId)
	}
}

// SetCancel attaches a heartbeat cancellation function to a task.
func (t *agentTaskTracker) SetCancel(taskId string, cancel func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task, ok := t.tasks[taskId]; ok {
		task.Cancel = cancel
	}
}

// Count returns the number of currently running tasks.
func (t *agentTaskTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.tasks)
}

// ActiveLabel returns the label of the most recently started task.
func (t *agentTaskTracker) ActiveLabel() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var latest *agentTask
	for _, task := range t.tasks {
		if latest == nil || task.StartedAt.After(latest.StartedAt) {
			latest = task
		}
	}
	if latest != nil {
		return latest.Label
	}
	return ""
}

// TaskStartedAt returns the start time for a task, or zero time if not found.
func (t *agentTaskTracker) TaskStartedAt(taskId string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task, ok := t.tasks[taskId]; ok {
		return task.StartedAt
	}
	return time.Time{}
}
